import CRecorder
import Foundation

/// 一路声音的来源：麦克风、电脑里的声音，或者测试用的文件。
/// 采集那边往 ring 里写单声道、sampleRate 的采样（尽量带上时间戳，见 zr_stamp），处理线程读。
public protocol CaptureSource: AnyObject {
    var kind: SourceKind { get }
    /// 给用户看的名字（麦克风的名字）
    var name: String { get }
    /// zr_ring*
    var ring: OpaquePointer { get }
    /// 现在的采样率。设备换了格式时会变，处理线程每次取数据前看一眼
    var sampleRate: Double { get }
    /// 麦克风这类要看有没有声音的来源（见 SilenceWatch）：刚开始录就一直全是 0 时提醒用户的话。
    /// nil 表示不检查（电脑没在放声音时本来就全是 0；读文件测试时也不检查）
    var silenceWarning: String? { get }
    func start() throws
    /// 停下来。返回以后不会再往 ring 里写
    func stop()
}

/// 录音引擎：现场录音（LiveEngine），或者测试时直接读文件（FileEngine）
public protocol RecordingEngine: AnyObject {
    /// 结束时（收到 stop、文件读完了、服务器能收的最后一片写不下了、出错了）调用一次，在引擎自己的线程上
    var onFinished: ((Result<RecordingResult, RecorderFailure>) -> Void)? { get set }
    func start()
    func pause()
    func resume()
    func stop()
}

/// 现场录音时回声消除怎么做（RecordSession 按命令行参数准备；--echo-cancel off 时不传）
public struct EchoSetup {
    /// auto 或 on
    public var mode: EchoCancelMode
    /// 参考比麦克风提前多少毫秒（见 EchoStage）
    public var leadMilliseconds = 170
    /// 建回声消除器。麦克风和电脑声音都在录时才调用（在开始录音前，不在音频线程上）；建不起来返回 nil
    public var makeCanceller: () -> EchoCanceller?
    /// auto 模式看的输出路线；nil 当成外放
    public var route: OutputRouteSource?
    /// --aec-dump 的目录
    public var dumpDirectory: String?

    public init(mode: EchoCancelMode, leadMilliseconds: Int = 170, makeCanceller: @escaping () -> EchoCanceller?,
                route: OutputRouteSource? = nil, dumpDirectory: String? = nil) {
        self.mode = mode
        self.leadMilliseconds = leadMilliseconds
        self.makeCanceller = makeCanceller
        self.route = route
        self.dumpDirectory = dumpDirectory
    }
}

/// 换麦克风（LiveEngine.switchMic）的结果
public enum MicSwitchResult: Equatable {
    /// 换好了，发了 mic_changed
    case switched
    /// 新的麦克风的采样率处理不了：没换，接着录原来的
    case unsupportedRate(Double)
    /// 没在录麦克风，或者录音已经结束了（正在结束）：没换
    case notRecording
}

/// 现场录音：每 20 毫秒从各路的环形缓冲里取出新的采样，转成 48 kHz，按经过的时间混成一路
/// （见 TimeMixer），交给 RecordingPipeline 编码、切分片。
/// 打开了回声消除、麦克风和电脑声音都在录时，麦克风转好的声音先经过 EchoStage 再进它那一路；
/// 打开了麦克风自动增益（默认）时，麦克风的声音最后再经过 MicGainStage（在回声消除之后）。
/// 录系统默认的麦克风时，默认的麦克风换了可以换一个麦克风接着录（switchMic，见 MicFollower）。
public final class LiveEngine: RecordingEngine {
    static let tickInterval = DispatchTimeInterval.milliseconds(20)
    /// 一次最多混 1 秒；处理线程卡住很久以后分几次补上
    static let maxMixSamples = 48_000
    /// 换麦克风时两个麦克风重叠的那一段最多这么长，再长就是时间戳不对，不去掉（见 trimOverlap）
    static let maxOverlap = 2.0

    public var onFinished: ((Result<RecordingResult, RecorderFailure>) -> Void)?
    let queue = DispatchQueue(label: "io.github.zanexo.zsmeet.recorder.engine", qos: .userInitiated)

    /// 在录的各路。采样率处理不了的那一路，开始时就去掉了（见 init）。换麦克风时麦克风那一路的来源换成新的
    /// （在 queue 上换；开始录以后别的线程不要读它）
    private(set) var sources: [CaptureSource]
    private(set) var resamplers: [Resampler]
    let mixer: TimeMixer
    /// 回声消除：started 事件里报的模式（要的模式没能打开是 unavailable）
    public private(set) var echoStatus = EchoCancelStatus.off
    /// 在做回声消除时（auto、on，两路都在录，建得起来）才有
    private(set) var echoStage: EchoStage?
    /// 麦克风自动增益：started 事件里报的模式（要调却没能打开是 unavailable）
    public private(set) var micGainStatus = MicGainStatus.off
    /// 在调麦克风的音量时（auto，录着麦克风，建得起来）才有
    private(set) var micGain: MicGainStage?
    /// 回声消除和自动增益都有时，回声消除交出来的麦克风声音先放这里，再交给自动增益
    private var echoed = SampleFIFO()
    /// auto、on 模式，录着麦克风却没录电脑声音：开始时报一次 no_reference
    private var echoWithoutReference = false
    private let micIndex: Int?
    private let pipeline: RecordingPipeline
    private let events: EventSink
    /// 纳秒，不算电脑睡眠的时间（睡醒以后不会一下子补出一大段静音）。和 IOProc 时间戳是同一个时钟
    private let clock: () -> UInt64
    private var timer: DispatchSourceTimer?
    private var startTime: UInt64 = 0
    private var produced: Int64 = 0
    private var scratch = [Float](repeating: 0, count: 16_384)
    private var mixed = [Float](repeating: 0, count: LiveEngine.maxMixSamples)
    private var energy: [Double]
    private var silence: [SilenceWatch]
    /// 每一路上次换不过去的采样率，免得每次取数据都再试一遍、再写一遍日志
    private var rejectedRates: [Double?]
    /// 每一路是不是已经提醒过「转换出错，少了一小段」（只提醒一次）
    private var conversionWarned: [Bool]
    /// 回声消除用的时间戳：每一路读出过多少个原始采样、转出过多少个 48 kHz 的采样，
    /// 现在的转换器从哪个原始采样、哪个 48 kHz 的采样开始（时间戳的序号按它换算成 48 kHz 的序号）
    private var rawRead: [Int64]
    private var converted48: [Int64]
    private var stampOrigins: [(raw: Int64, out: Int64, ratio: Double)]
    private var stampBuffer = [zr_stamp](repeating: zr_stamp(), count: 64)
    /// 每一路最近的一个时间戳：第几个原始采样是什么时候（秒）采到的。换麦克风时算旧的最后一个采样之后的时间
    private var lastStamps: [(raw: Int64, time: Double)?]
    /// 刚换了麦克风：新的麦克风在这个时间（秒，旧的最后一个采样之后）以前采到的，旧的已经录了，不要（见 trimOverlap）
    private var overlapUntil: [Double?]
    /// 换了几次麦克风（日志用）
    private var micSwitchCount = 0
    /// 有回声消除时，转好的采样先放这里，再分给 EchoStage 和各自那一路
    private var converted = SampleFIFO()
    /// 在结束了（stopNow 里），tick 不用再看最后一片写不写得下
    private var stopping = false
    private var finished = false

    /// 每一路配一个转换器。采样率处理不了的那一路停掉、发 warning，只录别的；一路都不剩时抛出 no_source。
    /// echo 是回声消除的设置（nil：不做），micGain 是麦克风自动增益的设置（nil：不调）
    public init(sources: [CaptureSource], pipeline: RecordingPipeline, events: EventSink,
                clock: @escaping () -> UInt64 = { DispatchTime.now().uptimeNanoseconds }, echo: EchoSetup? = nil,
                micGain: MicGainSetup? = nil) throws {
        var usable: [CaptureSource] = []
        var resamplers: [Resampler] = []
        for source in sources {
            let rate = source.sampleRate
            guard let resampler = try? Resampler(inputRate: rate) else {
                log("\(source.name) 的采样率是 \(Resampler.describe(rate))，处理不了，不录这一路")
                source.stop()
                events.send(Self.unsupportedRateWarning(source, rate: rate))
                continue
            }
            usable.append(source)
            resamplers.append(resampler)
        }
        guard !usable.isEmpty else { throw RecorderFailure.noSource }
        self.sources = usable
        self.resamplers = resamplers
        mixer = TimeMixer(kinds: usable.map(\.kind))
        micIndex = usable.firstIndex { $0.kind == .mic }
        self.pipeline = pipeline
        self.events = events
        self.clock = clock
        energy = [Double](repeating: 0, count: usable.count)
        silence = [SilenceWatch](repeating: SilenceWatch(), count: usable.count)
        rejectedRates = [Double?](repeating: nil, count: usable.count)
        conversionWarned = [Bool](repeating: false, count: usable.count)
        rawRead = [Int64](repeating: 0, count: usable.count)
        converted48 = [Int64](repeating: 0, count: usable.count)
        stampOrigins = usable.map { (raw: 0, out: 0, ratio: Resampler.outputRate / $0.sampleRate) }
        lastStamps = [(raw: Int64, time: Double)?](repeating: nil, count: usable.count)
        overlapUntil = [Double?](repeating: nil, count: usable.count)
        if let echo, echo.mode != .off {
            setUpEcho(echo)
        }
        if let micGain {
            setUpMicGain(micGain)
        }
    }

    /// 录着麦克风才调它的音量。建不起来就原样录麦克风，started 里报 unavailable（录音照常，不另外提醒：
    /// 声音一直很小的话，录音页上会提示把电脑挪近一些、调大输入音量）
    private func setUpMicGain(_ setup: MicGainSetup) {
        micGainStatus = .auto
        guard micIndex != nil else { return }
        guard let controller = setup.makeController() else {
            micGainStatus = .unavailable
            log("麦克风自动增益没能打开，麦克风的声音原样录")
            return
        }
        micGain = MicGainStage(controller: controller, settings: setup.settings)
    }

    /// 两路都在录才做回声消除（没有电脑声音就没有参考）。建不起来就原样录麦克风，started 里报 unavailable，
    /// 是用户要求一直开着（on）的话再提醒一下
    private func setUpEcho(_ echo: EchoSetup) {
        echoStatus = echo.mode == .on ? .on : .auto
        let kinds = sources.map(\.kind)
        guard kinds.contains(.mic) else { return }
        guard kinds.contains(.system) else {
            echoWithoutReference = true
            return
        }
        guard let canceller = echo.makeCanceller() else {
            echoStatus = .unavailable
            log("回声消除没能打开，麦克风的声音原样录")
            if echo.mode == .on {
                events.send(.warning(.echoUnavailable, EchoMessages.unavailable))
            }
            return
        }
        let dump = echo.dumpDirectory.flatMap { EchoDump(directory: $0) }
        echoStage = EchoStage(configuration: EchoStage.Configuration(mode: echo.mode, leadMilliseconds: echo.leadMilliseconds),
                              canceller: canceller, route: echo.route, events: events, dump: dump, now: seconds(clock()))
    }

    private static func unsupportedRateWarning(_ source: CaptureSource, rate: Double) -> RecorderEvent {
        let rate = Resampler.describe(rate)
        switch source.kind {
        case .mic:
            return .warning(.micUnavailable, "麦克风「\(source.name)」的采样率是 \(rate)，处理不了，录不到你这边的声音。到「音频 MIDI 设置」里把它改成 48000 Hz，或者换一个麦克风，再重新开始录音。")
        case .system:
            return .warning(.systemUnavailable, "电脑声音的采样率是 \(rate)，处理不了，录不到会议软件里其他人的声音。到「音频 MIDI 设置」里把正在用的扬声器或耳机改成 48000 Hz，再重新开始录音。")
        }
    }

    public func start() {
        queue.async {
            self.startClock()
            let timer = DispatchSource.makeTimerSource(queue: self.queue)
            timer.schedule(deadline: .now() + Self.tickInterval, repeating: Self.tickInterval, leeway: .milliseconds(5))
            timer.setEventHandler { [weak self] in self?.tick() }
            timer.resume()
            self.timer = timer
        }
    }

    public func pause() {
        queue.async {
            guard !self.finished, !self.pipeline.isPaused else { return }
            // 点暂停之前采到的声音都编进去：先按时间混到现在，回声消除那里压着的麦克风声音原样先交出来，
            // 再把各路攒着的（约 0.15 秒）也混出来
            self.tick()
            guard !self.finished else { return }
            if let stage = self.echoStage {
                self.fromEcho { stage.previewHeld(into: &$0) }
            }
            if let gain = self.micGain, let mic = self.micIndex {
                gain.flush(into: &self.mixer.lanes[mic].queue)
            }
            do {
                try self.feedQueued()
                try self.pipeline.pause()
            } catch {
                self.fail(error)
            }
        }
    }

    public func resume() {
        queue.async {
            guard !self.finished, self.pipeline.isPaused else { return }
            // 暂停期间采到的声音都丢掉：按时间混到现在（还暂停着，混出来的不编码），回声消除那里压着的
            // （暂停期间采到的）处理完也不要，再清掉各路攒着的。继续以后重新攒，编进去的只有点了继续以后采到的声音
            self.tick()
            // 最后一片写不下了的话，tick 已经结束了录音
            guard !self.finished else { return }
            self.echoStage?.discardHeld()
            self.micGain?.discardHeld()
            self.mixer.reset()
            self.pipeline.resume()
        }
    }

    public func stop() {
        queue.async { self.stopNow() }
    }

    /// 换一个麦克风接着录（跟着系统默认的麦克风换了，见 MicFollower）。replacement 已经开始录了。
    /// 不能在 queue 上调用（会等 queue 上正在做的做完，再在 queue 上换）。
    ///
    /// 先停掉旧的，把它剩下的都取出来、转完（转换器也收尾），再从新的那里接着取：麦克风那一路接着往下，
    /// 录音的时间轴不断，只少了换的时候两个麦克风都没在录的那一点。新的是先开始录、再换的，两个麦克风同时在录的
    /// 那一小段按时间戳去掉新的那边的，不重复。有回声消除时，新的麦克风的时间戳从新的一段开始，处理到新的麦克风的
    /// 声音时 AEC3 从头开始（回声的路径全变了）。新的麦克风的「有没有声音」从头看。换好了发 mic_changed。
    /// 没换成时 replacement 由调用方停掉
    public func switchMic(to replacement: CaptureSource) -> MicSwitchResult {
        dispatchPrecondition(condition: .notOnQueue(queue))
        return queue.sync { switchMicNow(to: replacement) }
    }

    private func switchMicNow(to replacement: CaptureSource) -> MicSwitchResult {
        guard !finished, !stopping, let index = micIndex, replacement.kind == .mic else { return .notRecording }
        let rate = replacement.sampleRate
        guard let resampler = try? Resampler(inputRate: rate) else {
            log("新的麦克风「\(replacement.name)」的采样率是 \(Resampler.describe(rate))，处理不了，不换")
            return .unsupportedRate(rate)
        }
        let now = seconds(clock())
        // 旧的先停掉：之后它的环形缓冲不会再变，剩下的都取出来、转完，接在麦克风那一路后面
        let old = sources[index]
        old.stop()
        pull(index, now: now, final: true)
        // 旧的一个时间戳都没有的话（比如上次换上来的还没交过数据），还按上次换的时候的时间去掉重叠
        overlapUntil[index] = endTime(index) ?? overlapUntil[index]
        sources[index] = replacement
        resamplers[index] = resampler
        rejectedRates[index] = nil
        conversionWarned[index] = false
        rawRead[index] = 0
        stampOrigins[index] = (raw: 0, out: converted48[index], ratio: Resampler.outputRate / rate)
        lastStamps[index] = nil
        silence[index] = SilenceWatch()
        micSwitchCount += 1
        echoStage?.micDeviceChanged(at: converted48[index])
        micGain?.micDeviceChanged(at: converted48[index])
        log("麦克风从「\(old.name)」换成了「\(replacement.name)」（\(Resampler.describe(rate))），接着录")
        events.send(.micChanged(name: replacement.name))
        // 新的已经采到的现在就取（先去掉和旧的重叠的那一段）
        pull(index, now: now, final: false)
        return .switched
    }

    /// 结束（在 queue 上）：收到了 stop，或者服务器能收的最后一片写不下了（见 tick）
    private func stopNow() {
        guard !finished, !stopping else { return }
        stopping = true
        timer?.cancel()
        timer = nil
        tick()
        for source in sources {
            source.stop()
        }
        // 设备停了，环形缓冲不会再变：剩下的都取出来、混完、编完
        do {
            let now = seconds(clock())
            for index in sources.indices {
                pull(index, now: now, final: true)
            }
            if let stage = echoStage {
                fromEcho { stage.finish(now: now, into: &$0) }
            }
            if let gain = micGain, let mic = micIndex {
                gain.finish(into: &mixer.lanes[mic].queue)
            }
            try feedQueued()
            let result = try pipeline.finish()
            logStats()
            finish(.success(result))
        } catch {
            fail(error)
        }
    }

    /// 从现在开始计时，回声消除报一次现在的状态（echo 事件，在 started 之后）。
    /// 测试里不开定时器，直接调用它和 tick（在 queue 上）
    func startClock() {
        startTime = clock()
        produced = 0
        if let stage = echoStage {
            stage.begin(now: seconds(startTime))
        } else if echoWithoutReference {
            events.send(.echo(active: false, reason: .noReference))
            log("回声消除：没在录电脑声音，不做")
        }
    }

    /// 取新数据、按时间混到现在。正常由定时器调用
    func tick() {
        guard !finished else { return }
        let now = seconds(clock())
        for index in sources.indices {
            pull(index, now: now, final: false)
        }
        if let stage = echoStage {
            fromEcho { stage.process(now: now, into: &$0) }
        }
        let elapsed = clock() &- startTime
        let target = Int64(Double(elapsed) * Double(AACEncoder.sampleRate) / 1_000_000_000)
        var due = target - produced
        do {
            while due > 0 {
                let count = Int(min(due, Int64(Self.maxMixSamples)))
                mixed.withUnsafeMutableBufferPointer { buffer in
                    // 积压按这一次还要出的全部算：卡住期间录下的声音要分几次补上，不能当成积压丢掉
                    mixer.mix(count: count, due: Int(due), into: buffer.baseAddress!, energy: &energy)
                }
                try feed(mixed, count: count)
                produced += Int64(count)
                due -= Int64(count)
            }
        } catch {
            fail(error)
        }
        // 服务器能收的最后一片写不下了，后面的声音没地方放：自己结束录音，和收到 stop 一样
        if pipeline.isFull && !stopping {
            stopNow()
        }
    }

    /// 各路攒着、还没混的声音都混出来交给 pipeline（暂停、结束时）。已经暂停着的话 pipeline 会丢掉它们
    private func feedQueued() throws {
        var rest: [Float] = []
        let count = mixer.drain(into: &rest, energy: &energy)
        try feed(rest, count: count)
    }

    private func feed(_ samples: [Float], count: Int) throws {
        guard count > 0 else { return }
        var micEnergy = 0.0
        var systemEnergy = 0.0
        for (index, source) in sources.enumerated() {
            switch source.kind {
            case .mic: micEnergy += energy[index]
            case .system: systemEnergy += energy[index]
            }
        }
        try samples.withUnsafeBufferPointer { buffer in
            try pipeline.feed(UnsafeBufferPointer(rebasing: buffer[0..<count]), micEnergy: micEnergy, systemEnergy: systemEnergy)
        }
    }

    /// 把一路的新采样从环形缓冲里取出来，转成 48 kHz 放进混音队列（有回声消除时麦克风的先交给它）。
    /// final：这一路结束了，转换器也收尾
    private func pull(_ index: Int, now: Double, final: Bool) {
        let source = sources[index]
        followSampleRate(index, now: now)
        // 刚换了麦克风、新的还没交来数据：先不取（取的时候要先去掉和旧的重叠的那一段）
        if overlapUntil[index] != nil && !trimOverlap(index) && !final {
            return
        }
        var arrived = false
        while true {
            let count = scratch.withUnsafeMutableBufferPointer { buffer in
                zr_ring_read(source.ring, buffer.baseAddress!, buffer.count)
            }
            if count == 0 { break }
            arrived = true
            rawRead[index] += Int64(count)
            scratch.withUnsafeBufferPointer { buffer in
                let samples = UnsafeBufferPointer(rebasing: buffer[0..<count])
                watchSilence(index, samples)
                convert(index, now: now) { try resamplers[index].process(samples, into: &$0) }
            }
        }
        readStamps(index, now: now, arrived: arrived)
        if final {
            finishResampler(index, now: now)
        }
    }

    /// 刚换了麦克风，新的交来了第一批数据：它在 overlapUntil（旧的最后一个采样之后）以前采到的，旧的已经录了，去掉。
    /// 按新的第一个时间戳算；新的没有时间戳的话不去掉（最多重复换的那一下的一点点）。
    /// 返回 false 表示还没有数据（或者要去掉的比现在有的还多），下次再看
    private func trimOverlap(_ index: Int) -> Bool {
        guard let until = overlapUntil[index] else { return true }
        let ring = sources[index].ring
        // 先看有没有采样、再看时间戳：IOProc 先记时间戳再写采样，看得到的采样的时间戳一定已经在了（没有就是设备没给）
        let available = Int(zr_ring_available(ring))
        guard available > 0 else { return false }
        overlapUntil[index] = nil
        var first = zr_stamp()
        guard zr_ring_peek_stamp(ring, &first) else {
            log("新的麦克风没有时间戳，和原来的麦克风重叠的那一点（如果有）没去掉")
            return true
        }
        let rate = resamplers[index].inputRate
        // 下一个要取的采样是什么时候采到的
        let next = Double(first.host_ns) / 1_000_000_000 + Double(rawRead[index] - Int64(first.index)) / rate
        let overlap = until - next
        guard overlap > 0 else { return true }
        guard overlap < Self.maxOverlap else {
            log("新的麦克风和原来的麦克风重叠了 \(milliseconds(overlap)) 毫秒，时间戳不对，不去掉")
            return true
        }
        let needed = Int((overlap * rate).rounded())
        let dropped = discard(index, count: min(needed, available))
        // 之后的时间戳从留下的第一个采样算（readStamps 里，这一批前面去掉的部分也会跳过）
        stampOrigins[index].raw = rawRead[index]
        log("去掉了新的麦克风和原来的麦克风重叠的 \(dropped) 个采样（\(milliseconds(Double(dropped) / rate)) 毫秒）")
        guard dropped >= needed else {
            // 还没交够：后面来的接着去掉
            overlapUntil[index] = until
            return false
        }
        return true
    }

    /// 从环形缓冲里取出 count 个采样扔掉，返回扔掉了几个
    private func discard(_ index: Int, count: Int) -> Int {
        var remaining = count
        while remaining > 0 {
            let read = scratch.withUnsafeMutableBufferPointer { buffer in
                zr_ring_read(sources[index].ring, buffer.baseAddress!, min(buffer.count, remaining))
            }
            if read == 0 { break }
            rawRead[index] += Int64(read)
            remaining -= read
        }
        return count - remaining
    }

    /// 这一路最后一个采样之后的时间（秒），按最近的时间戳算（取完以后用）。没有时间戳时是 nil
    private func endTime(_ index: Int) -> Double? {
        guard let stamp = lastStamps[index] else { return nil }
        return stamp.time + Double(rawRead[index] - stamp.raw) / resamplers[index].inputRate
    }

    /// 用这一路的转换器转换（body 把结果追加到给它的队列里）。没有回声消除、这一路也不调音量时直接进这一路的
    /// 混音队列，和没有这两个功能以前一样；不然先放在 converted 里，再分给 EchoStage、自动增益和各自那一路
    /// （有回声消除时麦克风的声音先交给 EchoStage，它处理好的在 tick 里再交给自动增益）
    private func convert(_ index: Int, now: Double, _ body: (inout SampleFIFO) throws -> Void) {
        let kind = sources[index].kind
        let gain = kind == .mic ? micGain : nil
        guard echoStage != nil || gain != nil else {
            do {
                try body(&mixer.lanes[index].queue)
            } catch {
                conversionFailed(index, error)
            }
            return
        }
        do {
            try body(&converted)
        } catch {
            conversionFailed(index, error)
        }
        let count = converted.count
        guard count > 0 else { return }
        converted48[index] += Int64(count)
        let lane = mixer.lanes[index]
        let end = converted48[index]
        converted.withUnsafeSamples { samples in
            switch kind {
            case .mic:
                if let stage = echoStage {
                    stage.appendMic(samples, now: now)
                } else {
                    gain?.process(samples, end: end, into: &lane.queue)
                }
            case .system:
                lane.queue.append(samples)
                echoStage?.appendReference(samples, now: now)
            }
        }
        converted.removeAll()
    }

    /// 回声消除交出来的麦克风声音（body 把它们追加到给它的队列里）：有自动增益时先经过它，再进麦克风那一路。
    /// EchoStage 一次交出来的是麦克风那一路连着的一段，到 deliveredEnd 为止（自动增益靠它找到换麦克风的地方）
    private func fromEcho(_ body: (inout SampleFIFO) -> Void) {
        guard let mic = micIndex, let stage = echoStage else { return }
        guard let gain = micGain else {
            body(&mixer.lanes[mic].queue)
            return
        }
        body(&echoed)
        guard !echoed.isEmpty else { return }
        let end = stage.deliveredEnd
        echoed.withUnsafeSamples { gain.process($0, end: end, into: &mixer.lanes[mic].queue) }
        echoed.removeAll()
    }

    /// 取出这一路的时间戳：记下最近的一个（换麦克风时要用），有回声消除时换算成 48 kHz 的序号交给 EchoStage。
    /// arrived：这次取到了新采样
    private func readStamps(_ index: Int, now: Double, arrived: Bool) {
        let source = sources[index]
        let origin = stampOrigins[index]
        let rate = Resampler.outputRate / origin.ratio
        var stamps: [(index: Double, time: Double)] = []
        while true {
            let count = stampBuffer.withUnsafeMutableBufferPointer { buffer in
                zr_ring_read_stamps(source.ring, buffer.baseAddress!, buffer.count)
            }
            if count == 0 { break }
            for stamp in stampBuffer[0..<count] {
                var raw = Int64(stamp.index)
                var time = Double(stamp.host_ns) / 1_000_000_000
                if raw < origin.raw {
                    // origin.raw 之前的不要了（换麦克风时和原来的重叠的那一段）：整批都在前面的跳过，
                    // 跨过去的那一批从留下的第一个采样算
                    guard raw + Int64(stamp.frames) > origin.raw else { continue }
                    time += Double(origin.raw - raw) / rate
                    raw = origin.raw
                }
                lastStamps[index] = (raw: raw, time: time)
                if echoStage != nil {
                    stamps.append((index: Double(origin.out) + Double(raw - origin.raw) * origin.ratio, time: time))
                }
            }
        }
        guard let stage = echoStage else { return }
        stage.addStamps(source.kind, stamps, now: now)
        if arrived {
            stage.noteArrival(source.kind, now: now)
        }
    }

    /// 这一路的转换器收尾：压着的声音都取出来
    private func finishResampler(_ index: Int, now: Double) {
        convert(index, now: now) { try resamplers[index].finish(into: &$0) }
    }

    /// 转换出错，这一路少了一小段（混音时补成静音）。录音照常，但要告诉用户，每一路只提醒一次
    private func conversionFailed(_ index: Int, _ error: Error) {
        let source = sources[index]
        log("\(source.name)：\((error as? RecorderFailure)?.message ?? "\(error)")")
        guard !conversionWarned[index] else { return }
        conversionWarned[index] = true
        switch source.kind {
        case .mic:
            events.send(.warning(.micUnavailable, "处理麦克风「\(source.name)」的声音时出错，中间有一小段没录上，录音还在继续。经常这样的话，结束录音，换一个麦克风再录。"))
        case .system:
            events.send(.warning(.systemUnavailable, "处理电脑声音时出错，中间有一小段没录上，录音还在继续。经常这样的话，结束录音再重新开始。"))
        }
    }

    /// 设备换了采样率（比如蓝牙耳机切换了模式）：旧的转换器收尾，换一个新的
    private func followSampleRate(_ index: Int, now: Double) {
        let source = sources[index]
        let rate = source.sampleRate
        guard rate != resamplers[index].inputRate, rate != rejectedRates[index] else { return }
        do {
            let replacement = try Resampler(inputRate: rate)
            finishResampler(index, now: now)
            resamplers[index] = replacement
            rejectedRates[index] = nil
            // 之后的时间戳按新的转换器换算；回声消除的时间轴从头来
            stampOrigins[index] = (raw: rawRead[index], out: converted48[index], ratio: Resampler.outputRate / rate)
            echoStage?.streamRestarted(source.kind)
            log("\(source.name) 的采样率变成了 \(Resampler.describe(rate))")
        } catch {
            rejectedRates[index] = rate
            log("\(source.name) 换成了不支持的采样率 \(Resampler.describe(rate))，先按原来的处理")
        }
    }

    /// 麦克风有没有声音（见 SilenceWatch）：刚开始录的几秒一直全是 0（多半是被静音了或者没有权限），
    /// 或者录音中（不算暂停）很长一段时间都是静音（多半是被会议软件占住了），各提醒一次
    private func watchSilence(_ index: Int, _ samples: UnsafeBufferPointer<Float>) {
        let source = sources[index]
        guard let startupMessage = source.silenceWarning else { return }
        let alerts = silence[index].feed(samples, sampleRate: resamplers[index].inputRate, paused: pipeline.isPaused)
        for alert in alerts {
            switch alert {
            case .startup:
                log("麦克风「\(source.name)」刚开始录的 \(JSON.format(SilenceWatch.startupSeconds)) 秒一直全是 0，提醒用户")
                events.send(.warning(.micUnavailable, startupMessage))
            case .silent(let digital, let seconds):
                log("麦克风「\(source.name)」已经 \(JSON.format(seconds)) 秒\(digital ? "全是 0" : "几乎没有声音")（不算暂停），提醒用户")
                events.send(.warning(.micSilent, SilenceWatch.message(name: source.name, digital: digital)))
            case .recovered(let seconds):
                log("麦克风「\(source.name)」又有声音了（之前 \(JSON.format(seconds)) 秒几乎没有声音）")
            }
        }
    }

    /// 结束时写一行日志：每一路补了多少静音、丢了多少，排查「录音断断续续」用
    private func logStats() {
        let rate = Double(AACEncoder.sampleRate)
        for (index, lane) in mixer.lanes.enumerated() {
            let padded = JSON.format(Double(lane.paddedSamples) / rate)
            let dropped = JSON.format(Double(lane.droppedSamples) / rate)
            let overflow = zr_ring_total_dropped(sources[index].ring)
            log("\(sources[index].name)：补了 \(padded) 秒静音，丢了 \(dropped) 秒积压，缓冲满时丢了 \(overflow) 个采样")
        }
        if micSwitchCount > 0 {
            log("这次录音跟着系统默认的麦克风换了 \(micSwitchCount) 次麦克风")
        }
    }

    private func fail(_ error: Error) {
        let failure = error as? RecorderFailure ?? RecorderFailure("internal", "录音出错了：\(error.localizedDescription)")
        timer?.cancel()
        timer = nil
        for source in sources {
            source.stop()
        }
        pipeline.abandon()
        finish(.failure(failure))
    }

    private func finish(_ result: Result<RecordingResult, RecorderFailure>) {
        guard !finished else { return }
        finished = true
        onFinished?(result)
    }
}

/// 纳秒换成秒
func seconds(_ nanoseconds: UInt64) -> Double {
    Double(nanoseconds) / 1_000_000_000
}

/// 测试模式：读文件，尽快编完，读完就结束（和收到 stop 一样）。和现场录音用同一个 RecordingPipeline。
/// 文件当成麦克风：打开了麦克风自动增益时，转好的声音也经过 MicGainStage（和现场录音一样）。
public final class FileEngine: RecordingEngine {
    public var onFinished: ((Result<RecordingResult, RecorderFailure>) -> Void)?

    private let queue = DispatchQueue(label: "io.github.zanexo.zsmeet.recorder.file-engine", qos: .userInitiated)
    private let reader: AudioFileReader
    private let resampler: Resampler
    private let pipeline: RecordingPipeline
    private var converted = SampleFIFO()
    private var block = [Float](repeating: 0, count: RecordingPipeline.levelInterval)
    private var stopRequested = false
    private var finished = false
    /// 麦克风自动增益：started 事件里报的模式（要调却没能打开是 unavailable）
    public private(set) var micGainStatus = MicGainStatus.off
    private var micGain: MicGainStage?
    /// 有自动增益时，转好的先放这里，经过它再进 converted；一共转出过多少个采样
    private var resampled = SampleFIFO()
    private var resampledTotal: Int64 = 0

    /// micGain 是麦克风自动增益的设置（nil：不调）
    public init(reader: AudioFileReader, pipeline: RecordingPipeline, micGain: MicGainSetup? = nil) throws {
        self.reader = reader
        resampler = try Resampler(inputRate: reader.sampleRate)
        self.pipeline = pipeline
        if let micGain {
            micGainStatus = .auto
            if let controller = micGain.makeController() {
                self.micGain = MicGainStage(controller: controller, settings: micGain.settings)
            } else {
                micGainStatus = .unavailable
                log("麦克风自动增益没能打开，文件的声音原样录")
            }
        }
    }

    public func start() {
        queue.async { self.step() }
    }

    public func pause() {
        queue.async {
            guard !self.finished else { return }
            do {
                try self.pipeline.pause()
            } catch {
                self.fail(error)
            }
        }
    }

    public func resume() {
        queue.async {
            guard !self.finished else { return }
            self.pipeline.resume()
        }
    }

    public func stop() {
        queue.async { self.stopRequested = true }
    }

    /// 每次读约 1 秒，读完排到队尾，让排着队的命令（暂停、停止）先执行。
    /// 读文件、转换采样率出错都以出错结束（测试模式，不能悄悄少一段还算录好了）
    private func step() {
        guard !finished else { return }
        do {
            // 收到了 stop，或者服务器能收的最后一片写不下了：和文件读完了一样结束
            let samples = stopRequested || pipeline.isFull ? [] : try reader.read(maxFrames: 48_000)
            if samples.isEmpty {
                try resample { try resampler.finish(into: &$0) }
                micGain?.finish(into: &converted)
                try feedConverted(all: true)
                finish(.success(try pipeline.finish()))
                return
            }
            try samples.withUnsafeBufferPointer { buffer in try resample { try resampler.process(buffer, into: &$0) } }
            try feedConverted(all: false)
            queue.async { self.step() }
        } catch {
            fail(error)
        }
    }

    /// 转换采样率（body 把结果追加到给它的队列里），转好的进 converted：有自动增益时先经过它
    private func resample(_ body: (inout SampleFIFO) throws -> Void) throws {
        guard let gain = micGain else {
            try body(&converted)
            return
        }
        try body(&resampled)
        guard !resampled.isEmpty else { return }
        resampledTotal += Int64(resampled.count)
        let end = resampledTotal
        resampled.withUnsafeSamples { gain.process($0, end: end, into: &converted) }
        resampled.removeAll()
    }

    /// 每 0.1 秒的声音喂一次，音量事件和现场录音一样每 0.1 秒一个
    private func feedConverted(all: Bool) throws {
        while converted.count >= block.count || (all && !converted.isEmpty) {
            let count = min(block.count, converted.count)
            try block.withUnsafeMutableBufferPointer { buffer in
                converted.take(count, into: buffer.baseAddress!)
                var energy = 0.0
                for index in 0..<count {
                    energy += Double(buffer[index] * buffer[index])
                }
                try pipeline.feed(UnsafeBufferPointer(rebasing: buffer[0..<count]), micEnergy: energy, systemEnergy: 0)
            }
        }
    }

    private func fail(_ error: Error) {
        pipeline.abandon()
        finish(.failure(error as? RecorderFailure ?? RecorderFailure("internal", "录音出错了：\(error.localizedDescription)")))
    }

    private func finish(_ result: Result<RecordingResult, RecorderFailure>) {
        guard !finished else { return }
        finished = true
        onFinished?(result)
    }
}

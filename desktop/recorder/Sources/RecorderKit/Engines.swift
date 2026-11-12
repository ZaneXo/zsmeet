import CRecorder
import Foundation

/// 一路声音的来源：麦克风、电脑里的声音，或者测试用的文件。
/// 采集那边往 ring 里写单声道、sampleRate 的采样，处理线程读。
public protocol CaptureSource: AnyObject {
    var kind: SourceKind { get }
    /// 给用户看的名字（麦克风的名字）
    var name: String { get }
    /// zr_ring*
    var ring: OpaquePointer { get }
    /// 现在的采样率。设备换了格式时会变，处理线程每次取数据前看一眼
    var sampleRate: Double { get }
    /// 一直收到全是 0 的声音时提醒用户的话；nil 表示不检查（电脑没在放声音时本来就全是 0）
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

/// 现场录音：每 20 毫秒从各路的环形缓冲里取出新的采样，转成 48 kHz，按经过的时间混成一路
/// （见 TimeMixer），交给 RecordingPipeline 编码、切分片。
public final class LiveEngine: RecordingEngine {
    static let tickInterval = DispatchTimeInterval.milliseconds(20)
    /// 一次最多混 1 秒；处理线程卡住很久以后分几次补上
    static let maxMixSamples = 48_000
    /// 麦克风连续这么多秒全是 0 就提醒一次
    static let silenceWarningSeconds = 5.0

    public var onFinished: ((Result<RecordingResult, RecorderFailure>) -> Void)?
    let queue = DispatchQueue(label: "io.github.zanexo.zsmeet.recorder.engine", qos: .userInitiated)

    /// 在录的各路。采样率处理不了的那一路，开始时就去掉了（见 init）
    let sources: [CaptureSource]
    private(set) var resamplers: [Resampler]
    let mixer: TimeMixer
    private let pipeline: RecordingPipeline
    private let events: EventSink
    /// 纳秒，不算电脑睡眠的时间（睡醒以后不会一下子补出一大段静音）
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
    /// 在结束了（stopNow 里），tick 不用再看最后一片写不写得下
    private var stopping = false
    private var finished = false

    private struct SilenceWatch {
        var samples = 0
        var heardSomething = false
    }

    /// 每一路配一个转换器。采样率处理不了的那一路停掉、发 warning，只录别的；一路都不剩时抛出 no_source
    public init(sources: [CaptureSource], pipeline: RecordingPipeline, events: EventSink,
                clock: @escaping () -> UInt64 = { DispatchTime.now().uptimeNanoseconds }) throws {
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
        self.pipeline = pipeline
        self.events = events
        self.clock = clock
        energy = [Double](repeating: 0, count: usable.count)
        silence = [SilenceWatch](repeating: SilenceWatch(), count: usable.count)
        rejectedRates = [Double?](repeating: nil, count: usable.count)
        conversionWarned = [Bool](repeating: false, count: usable.count)
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
            // 点暂停之前采到的声音都编进去：先按时间混到现在，再把各路攒着的（约 0.15 秒）也混出来
            self.tick()
            guard !self.finished else { return }
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
            // 暂停期间采到的声音都丢掉：按时间混到现在（还暂停着，混出来的不编码），再清掉各路攒着的。
            // 继续以后重新攒，编进去的只有点了继续以后采到的声音
            self.tick()
            // 最后一片写不下了的话，tick 已经结束了录音
            guard !self.finished else { return }
            self.mixer.reset()
            self.pipeline.resume()
        }
    }

    public func stop() {
        queue.async { self.stopNow() }
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
            for index in sources.indices {
                pull(index, final: true)
            }
            try feedQueued()
            let result = try pipeline.finish()
            logStats()
            finish(.success(result))
        } catch {
            fail(error)
        }
    }

    /// 从现在开始计时。测试里不开定时器，直接调用它和 tick（在 queue 上）
    func startClock() {
        startTime = clock()
        produced = 0
    }

    /// 取新数据、按时间混到现在。正常由定时器调用
    func tick() {
        guard !finished else { return }
        for index in sources.indices {
            pull(index, final: false)
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

    /// 把一路的新采样从环形缓冲里取出来，转成 48 kHz 放进混音队列。final：这一路结束了，转换器也收尾
    private func pull(_ index: Int, final: Bool) {
        let source = sources[index]
        let lane = mixer.lanes[index]
        followSampleRate(index)
        while true {
            let count = scratch.withUnsafeMutableBufferPointer { buffer in
                zr_ring_read(source.ring, buffer.baseAddress!, buffer.count)
            }
            if count == 0 { break }
            scratch.withUnsafeBufferPointer { buffer in
                let samples = UnsafeBufferPointer(rebasing: buffer[0..<count])
                watchSilence(index, samples)
                do {
                    try resamplers[index].process(samples, into: &lane.queue)
                } catch {
                    conversionFailed(index, error)
                }
            }
        }
        if final {
            finishResampler(index)
        }
    }

    /// 这一路的转换器收尾：压着的声音都取出来
    private func finishResampler(_ index: Int) {
        do {
            try resamplers[index].finish(into: &mixer.lanes[index].queue)
        } catch {
            conversionFailed(index, error)
        }
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
    private func followSampleRate(_ index: Int) {
        let source = sources[index]
        let rate = source.sampleRate
        guard rate != resamplers[index].inputRate, rate != rejectedRates[index] else { return }
        do {
            let replacement = try Resampler(inputRate: rate)
            finishResampler(index)
            resamplers[index] = replacement
            rejectedRates[index] = nil
            log("\(source.name) 的采样率变成了 \(Resampler.describe(rate))")
        } catch {
            rejectedRates[index] = rate
            log("\(source.name) 换成了不支持的采样率 \(Resampler.describe(rate))，先按原来的处理")
        }
    }

    /// 麦克风刚开始的几秒一直全是 0（一点底噪都没有），多半是被静音了或者没有权限，提醒一次
    private func watchSilence(_ index: Int, _ samples: UnsafeBufferPointer<Float>) {
        guard let message = sources[index].silenceWarning, !silence[index].heardSomething else { return }
        if samples.contains(where: { $0 != 0 }) {
            silence[index].heardSomething = true
            return
        }
        silence[index].samples += samples.count
        if Double(silence[index].samples) >= Self.silenceWarningSeconds * sources[index].sampleRate {
            // 只提醒一次
            silence[index].heardSomething = true
            events.send(.warning(.micUnavailable, message))
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

/// 测试模式：读文件，尽快编完，读完就结束（和收到 stop 一样）。和现场录音用同一个 RecordingPipeline。
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

    public init(reader: AudioFileReader, pipeline: RecordingPipeline) throws {
        self.reader = reader
        resampler = try Resampler(inputRate: reader.sampleRate)
        self.pipeline = pipeline
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
                try resampler.finish(into: &converted)
                try feedConverted(all: true)
                finish(.success(try pipeline.finish()))
                return
            }
            try samples.withUnsafeBufferPointer { try resampler.process($0, into: &converted) }
            try feedConverted(all: false)
            queue.async { self.step() }
        } catch {
            fail(error)
        }
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

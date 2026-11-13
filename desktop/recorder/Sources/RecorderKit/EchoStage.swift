import Foundation

/// 回声消除这一步。LiveEngine 里，每一路转成 48 kHz 以后、按时间混音（TimeMixer）之前：
///
///     麦克风 → Resampler → EchoStage（AEC3）→ MicGainStage（自动增益，开着时）→ 麦克风那一路 → TimeMixer
///     电脑声音 → Resampler → 电脑声音那一路（不动）→ TimeMixer
///                         └→ 复制一份给 EchoStage 当参考
///
/// 外放开会时，麦克风会录到扬声器放出来的会议声音，转写里同一句话就有两遍。电脑声音（进程音频捕获）就是
/// 扬声器放的东西，拿它当参考，WebRTC 的 AEC3 把它从麦克风里减掉。电脑声音那一路本身不动。
///
/// 对齐：两路的每个采样都有时间（Timeline，来自 IOProc 的时间戳）。麦克风一帧（480 个采样，10 毫秒）
/// 从 tm 开始，就配上参考里从 tm + lead 开始的 480 个采样，先喂参考、再处理麦克风，一比一。
/// 参考提前 lead（默认 170 毫秒，和 Chromium 拿系统声音做参考时一样），AEC3 看到的延迟就是
/// lead + 真正的回声延迟，它能找到 0 到约 608 毫秒（6 个滤波器），所以回声比时间戳早到 170 毫秒以内、
/// 晚到约 438 毫秒以内都消得掉。代价是麦克风要多等 lead（加上等参考的时间）才出来。
/// 两路时钟快慢不一样（不同的设备能差 100 ppm 以上）时，偏出一个采样就在参考里补一个或者丢一个，
/// 麦克风的采样一个都不动。时间轴断开（丢了采样、设备重新开始）就重新对齐，偏得多（超过 20 毫秒）
/// 时 AEC3 从头开始学。
///
/// 不做回声消除的时候（auto 模式戴着耳机、电脑声音断了、AEC3 出错关掉了）麦克风原样通过，
/// 但还是等差不多一样久，开、关切换时不会多出或者少掉一段。
///
/// 不管出什么事，麦克风进来多少个采样就出去多少个，顺序不变，录音不会因为回声消除停下来或者变短。
/// 只在 LiveEngine 的处理线程上用。
final class EchoStage {
    static let frameSize = 480
    static let sampleRate = Resampler.outputRate
    static let frameDuration = Double(frameSize) / sampleRate

    struct Configuration {
        var mode = EchoCancelMode.auto
        /// 参考比麦克风提前多少秒
        var lead = 0.170
        /// 参考来晚了，麦克风在 lead 之外最多再等多久，再晚的帧直接用原声。
        /// 不能等太久：等完一下子交出去，TimeMixer 那一路积压超过 0.5 秒会丢掉
        var maxWait = 0.25
        /// 不做回声消除时麦克风在 lead 之外多等一会儿（参考一般比麦克风晚到几十毫秒，这样和做的时候等得差不多，
        /// 开、关切换时麦克风那一路交得快慢变化小）。电脑声音又跟上了没有不靠它判断，见 referenceCaughtUp
        var bypassExtra = 0.06
        /// 电脑声音断了以后又来了：比麦克风晚得不到 maxWait - recoverMargin 秒才接着做（见 referenceCaughtUp）。
        /// 正好在边上的话，刚打开又因为一直来晚了关掉，来回开、关
        var recoverMargin = 0.02
        /// 电脑声音这么久没来新的就算断了
        var stallTimeout = 0.3
        /// 刚开始录的这一段时间里电脑声音还没来，不算断了（设备启动有快有慢）
        var startupGrace = 1.5
        /// 参考连续这么多帧来得太晚，也算断了
        var lateFramesLimit = 20
        /// 输出路线变了要稳定这么久才跟着变（插拔耳机时设备会抖几下）
        var routeDebounce = 1.0
        /// 每隔多久写一次统计日志
        var statsInterval = 60.0
        /// 参考最多攒这么多秒（麦克风不往前走时，比如麦克风断了）
        var maxReferenceSeconds = 10.0
        /// 对齐偏了这么多个采样以上（2 毫秒）就直接重新对齐，小于它时一次挪一个采样
        var realignSamples = 96
        /// 偏了这么多个采样以上（20 毫秒），AEC3 学到的回声路径不能用了，从头开始
        var resetSamples = 960
        /// AEC3 连续出错这么多次就不再做回声消除（第一次出错先从头开始试一次）
        var maxConsecutiveFailures = 3

        init(mode: EchoCancelMode = .auto, leadMilliseconds: Int = 170) {
            self.mode = mode
            lead = Double(leadMilliseconds) / 1_000
        }
    }

    /// 统计（日志、测试用）
    struct Counters: Equatable {
        /// 做了回声消除的帧、直接用原声的帧（最后不满一帧的那一点也算一帧）
        var processed = 0
        var bypassed = 0
        /// 为了跟上时钟差，参考里补的、丢的采样
        var slips = 0
        /// 重新对齐的次数（时间轴断开）、AEC3 从头开始的次数
        var realigns = 0
        var resets = 0
        /// 电脑声音断了（或者一直来得太晚）的次数；来得太晚、直接用了原声的帧
        var stalls = 0
        var lateFrames = 0
        /// 电脑声音断开过的那段时间里，喂了 0 当参考的帧
        var silentReference = 0
        /// 接着上一帧往下喂参考时，和该喂的位置最多差了几个采样（不算重新对齐的）
        var maxAlignmentError: Double?
        /// AEC3 出错的次数
        var failures = 0
        /// 麦克风最多等了多少秒
        var maxHeld = 0.0
    }

    private let config: Configuration
    let canceller: EchoCanceller
    private let route: OutputRouteSource?
    private let events: EventSink
    private let dump: EchoDump?
    private var startedAt: Double

    /// 麦克风：还没处理的采样（48 kHz），第一个的序号（从这一路的第一个采样算起），时间轴
    private(set) var mic = SampleFIFO()
    private(set) var micHead: Int64 = 0
    private(set) var micTimeline = Timeline()
    private var micLastStamp: Double?
    /// 上次收到麦克风新采样的时间（处理线程的时钟）
    private var micLastArrival: Double?
    /// 参考：留着的采样，第一个的序号，时间轴，上次收到新采样的时间（处理线程的时钟）
    private(set) var reference = SampleFIFO()
    private(set) var referenceHead: Int64 = 0
    private(set) var referenceTimeline = Timeline()
    private var referenceLastStamp: Double?
    private var referenceLastArrival: Double?
    /// 下一个要喂给 AEC3 的参考采样的序号。nil：还没对齐（刚开始、没做回声消除的帧之后）
    private(set) var referencePosition: Int64?
    /// 换了麦克风的地方（新的麦克风的第一个采样的序号，按先后）：处理到这里时 AEC3 从头开始，见 micDeviceChanged
    private(set) var micSwitches: [Int64] = []

    /// 接下来这么多个处理好的采样不要：暂停时已经先交出了原声；继续时是暂停期间采到的
    private(set) var skipOutput = 0
    /// emit 下一次交的第一个采样是麦克风的第几个采样（从这一路的第一个采样算起，要跳过的也算）。交出去的和送进来的
    /// 一个对一个、顺序不变（做没做回声消除都一样），所以它一直是 micHead - shadow.count
    private var emitted: Int64 = 0
    /// 交给麦克风那一路的采样，最后一个之后是麦克风的第几个采样。暂停期间采到的、点继续时不要的那些（discardHeld）
    /// 不交，序号在那里跳过去；别的都是接着的，一次调用交出去的是连着的一段。后面的自动增益（MicGainStage）
    /// 靠它找到换麦克风的地方
    private(set) var deliveredEnd: Int64 = 0
    /// AEC3 处理过的声音晚 latency 个采样。最近送进去的 latency 个麦克风原声在这里留一份：
    /// 不做回声消除的帧先交出它们，再交这一帧的前 480 - latency 个，自己留下最后 latency 个。
    /// 这样不管做不做，交出去的声音都晚 latency 个采样，开、关切换、出错、结束时不多不少、一个不重
    private let latency: Int
    private(set) var shadow: [Float] = []
    /// 下一帧 AEC3 交出来的前 latency 个不能用（刚开始、从头开始过、上一帧没经过它），改用 shadow 里的原声
    private var restartPending = true

    private(set) var active: Bool
    private(set) var reason: EchoReason
    private var healthy = true
    private var referenceMissing = false
    private var lateFrames = 0
    private var routeKind: OutputRouteKind
    private var routeCandidate: (kind: OutputRouteKind, since: Double)?
    private var consecutiveFailures = 0
    private(set) var counters = Counters()
    private var lastStatsLog: Double

    private var micFrame = [Float](repeating: 0, count: EchoStage.frameSize)
    private var referenceFrame = [Float](repeating: 0, count: EchoStage.frameSize)
    private var outputFrame = [Float](repeating: 0, count: EchoStage.frameSize)
    private let silence = [Float](repeating: 0, count: EchoStage.frameSize)

    /// now：处理线程的时钟（秒）。route 是 nil 时当外放
    init(configuration: Configuration, canceller: EchoCanceller, route: OutputRouteSource?, events: EventSink,
         dump: EchoDump? = nil, now: Double) {
        config = configuration
        self.canceller = canceller
        self.route = route
        self.events = events
        self.dump = dump
        startedAt = now
        lastStatsLog = now
        latency = min(max(canceller.latency, 0), EchoStage.frameSize - 1)
        routeKind = configuration.mode == .auto ? route?.current ?? .speakers : .speakers
        active = false
        reason = .off
        (active, reason) = desiredState()
    }

    // MARK: 输入

    /// 麦克风新转好的 48 kHz 采样
    func appendMic(_ samples: UnsafeBufferPointer<Float>, now: Double) {
        guard !samples.isEmpty else { return }
        mic.append(samples)
        micLastArrival = now
    }

    /// 电脑声音新转好的 48 kHz 采样（电脑声音那一路自己的一份不经过这里）
    func appendReference(_ samples: UnsafeBufferPointer<Float>, now: Double) {
        guard !samples.isEmpty else { return }
        reference.append(samples)
        referenceLastArrival = now
    }

    /// 时间戳：这一路第 index 个采样（48 kHz 的序号，可以带小数）是在 time 秒采到的
    func addStamps(_ kind: SourceKind, _ stamps: [(index: Double, time: Double)], now: Double) {
        guard !stamps.isEmpty else { return }
        for stamp in stamps {
            switch kind {
            case .mic:
                if micTimeline.add(index: stamp.index, time: stamp.time), Self.worthLogging(micTimeline.restarts) {
                    log("回声消除：麦克风的时间戳断开了（第 \(micTimeline.restarts) 次，差 \(milliseconds(micTimeline.lastJump)) 毫秒），重新对齐")
                }
            case .system:
                if referenceTimeline.add(index: stamp.index, time: stamp.time), Self.worthLogging(referenceTimeline.restarts) {
                    log("回声消除：电脑声音的时间戳断开了（第 \(referenceTimeline.restarts) 次，差 \(milliseconds(referenceTimeline.lastJump)) 毫秒），重新对齐")
                }
            }
        }
        switch kind {
        case .mic: micLastStamp = now
        case .system: referenceLastStamp = now
        }
    }

    /// 这一路刚收到了数据。很久（1 秒）没有时间戳的话，按收到的时间粗略对齐（没有时间戳的设备）
    func noteArrival(_ kind: SourceKind, now: Double) {
        switch kind {
        case .mic:
            if micLastStamp.map({ now - $0 > 1 }) ?? true {
                micTimeline.addArrival(endIndex: Double(micHead) + Double(mic.count), time: now)
            }
        case .system:
            if referenceLastStamp.map({ now - $0 > 1 }) ?? true {
                referenceTimeline.addArrival(endIndex: Double(referenceHead) + Double(reference.count), time: now)
            }
        }
    }

    /// 这一路换了采样率（换了转换器）：序号和时间的关系变了，时间轴从头来，AEC3 也从头开始
    func streamRestarted(_ kind: SourceKind) {
        switch kind {
        case .mic:
            micTimeline.reset()
            micLastStamp = nil
        case .system:
            referenceTimeline.reset()
            referenceLastStamp = nil
        }
        referencePosition = nil
        if active {
            resetCanceller()
        }
    }

    /// 换了麦克风（跟着系统默认的麦克风换了，见 MicFollower）：从 boundary 起是新的麦克风的声音。
    /// 新的设备有自己的时钟和时间戳，麦克风的时间轴从它的第一个时间戳开始新的一段；在这之前压着的旧麦克风的声音
    /// 照旧按旧的时间配参考。处理到新的麦克风的第一帧时 AEC3 从头开始（麦克风换了，回声的路径全变了），参考重新对齐
    func micDeviceChanged(at boundary: Int64) {
        micTimeline.split()
        // 新的麦克风没有时间戳的话，按收到数据的时间对齐（见 noteArrival）
        micLastStamp = nil
        micSwitches.append(boundary)
    }

    // MARK: 处理

    /// 把能处理的麦克风帧都处理掉，结果追加到 lane（麦克风那一路的队列）。每次 tick 调用
    func process(now: Double, into lane: inout SampleFIFO) {
        updateRoute(now)
        checkReference(now)
        // 麦克风 0.5 秒没来新的了（断开了）：压着的不再等参考，先交出去，免得到结束时才出来
        let micStalled = micLastArrival.map { now - $0 > 0.5 } ?? false
        while mic.count >= Self.frameSize {
            guard step(now: now, flushing: micStalled, into: &lane) else { break }
        }
        capReference()
        if now - lastStatsLog >= config.statsInterval {
            lastStatsLog = now
            logStats(final: false)
        }
    }

    /// 暂停：点暂停之前采到、还压在这里的麦克风声音先原样交出去（这样它们会被编码进去），
    /// 之后处理它们的结果不要。AEC3 照常往下处理，一直学着
    func previewHeld(into lane: inout SampleFIFO) {
        // 还没交出去的：shadow 里的（AEC3 里正压着的那几个的原声），再加上还没处理的
        let total = shadow.count + mic.count
        guard total > skipOutput else { return }
        if skipOutput < shadow.count {
            lane.append(shadow[skipOutput...])
        }
        let fromQueue = max(skipOutput - shadow.count, 0)
        mic.withUnsafeSamples { all in
            lane.append(UnsafeBufferPointer(rebasing: all[fromQueue...]))
        }
        // 调试用的录音（--aec-dump）这里不写：之后处理出来、跳过的那一份照样写（见 emit），一个采样对一个采样
        skipOutput = total
        deliveredEnd = emitted + Int64(total)
    }

    /// 继续：现在压着的都是点继续之前采到的（暂停期间的），处理完不要
    func discardHeld() {
        skipOutput = max(skipOutput, shadow.count + mic.count)
    }

    /// 结束：剩下的全部处理完（参考没到的直接用原声，最后不满一帧的原样交出），一个采样都不留
    func finish(now: Double, into lane: inout SampleFIFO) {
        while mic.count >= Self.frameSize {
            _ = step(now: now, flushing: true, into: &lane)
        }
        // 最后：AEC3 里压着的那几个（用原声）和不满一帧的，原样交出去
        emit(shadow[...], into: &lane)
        shadow.removeAll()
        let rest = mic.count
        if rest > 0 {
            micFrame.withUnsafeMutableBufferPointer { frame in
                frame.baseAddress!.initialize(repeating: 0, count: Self.frameSize)
                mic.take(rest, into: frame.baseAddress!)
            }
            micHead += Int64(rest)
            counters.bypassed += 1
            dump?.writeInput(mic: micFrame, reference: silence, count: rest)
            emit(micFrame[0..<rest], into: &lane)
        }
        dump?.finish()
        logStats(final: true)
    }

    /// 开始录音了（在 started 事件之后）：从现在算刚开始的那段宽限时间，报一次现在的状态
    func begin(now: Double) {
        startedAt = now
        lastStatsLog = now
        if let route, config.mode == .auto {
            log("回声消除：输出路线：\(route.currentDescription)")
        }
        announce()
    }

    /// 发 echo 事件说明现在的状态（开始录音时一次，之后状态一变就发）
    func announce() {
        events.send(.echo(active: active, reason: reason))
        log("回声消除：\(active ? "开着" : "没开")（\(reason.rawValue)）")
    }

    /// 处理麦克风队列最前面的一帧。返回 false 表示要等（参考还没到）
    private func step(now: Double, flushing: Bool, into lane: inout SampleFIFO) -> Bool {
        if let boundary = micSwitches.first, micHead >= boundary {
            // 这一帧是新的麦克风的了（跨着换麦克风那一刻的那一帧还算旧的）
            micSwitches.removeAll { $0 <= micHead }
            restartForNewMic()
        }
        let held = Double(mic.count) / Self.sampleRate
        let frameTime = micTimeline.time(at: Double(micHead))
        let location = frameTime.map { referenceTimeline.locate($0 + config.lead) } ?? .unknown
        var ideal: Double?
        if case .index(let index) = location {
            ideal = index
        }
        if active {
            if location == .gap {
                // 这段时间电脑声音断开过（现在已经接上了）：没有参考，喂 0，AEC3 接着跑，不用从头开始
                lateFrames = 0
                counters.maxHeld = max(counters.maxHeld, held)
                cancel(ideal: nil, now: now, into: &lane)
                return true
            }
            if let ideal, covers(ideal) || flushing {
                // 结束时最后这一点（提前量那么长）的参考还没放出来，没有的部分喂 0：消这几帧的回声要的是
                // 以前的参考，AEC3 那里都有
                lateFrames = 0
                counters.maxHeld = max(counters.maxHeld, held)
                cancel(ideal: ideal, now: now, into: &lane)
                return true
            }
            if !flushing && held < config.lead + config.maxWait + Self.frameDuration {
                return false
            }
            if !flushing {
                noteLateFrame(now)
            }
            bypass(ideal: ideal, into: &lane)
            return true
        }
        // 没在消除。是因为电脑声音断了的话，看它是不是又跟上了
        if !flushing, referenceMissing, referenceCaughtUp(now: now) {
            referenceMissing = false
            lateFrames = 0
            log("回声消除：电脑声音又跟上了")
            applyState()
            if active {
                return true
            }
        }
        if !flushing && held < config.lead + config.bypassExtra + Self.frameDuration {
            return false
        }
        bypass(ideal: ideal, into: &lane)
        return true
    }

    /// 参考已经到了从 start 开始的一帧（多留两个采样，补、丢一个采样时也够）
    private func covers(_ start: Double) -> Bool {
        Double(referenceHead) + Double(reference.count) >= start + Double(Self.frameSize) + 2
    }

    /// 电脑声音断了（或者一直来得太晚）以后又跟上了没有：它又在来了，并且按时间戳，最新的参考采样比最新的麦克风采样
    /// 晚不到 maxWait - recoverMargin。做回声消除时每一帧最多压 lead + maxWait，参考比麦克风晚 maxWait 以内都等得到，
    /// 跟上了就是这个意思。不能看最前面那一帧的参考到没到：不做的时候麦克风只压 lead + bypassExtra 就交出去，
    /// 电脑声音一直比麦克风晚 0.07 到 0.25 秒时（做的时候等得到），就再也打不开了；麦克风卡了一下、一下子来了一大批时，
    /// 最前面那一帧又早得参考已经到了，打开了马上又因为来晚了关掉
    private func referenceCaughtUp(now: Double) -> Bool {
        guard let last = referenceLastArrival, now - last <= config.stallTimeout,
              let micTime = micTimeline.time(at: Double(micHead) + Double(mic.count)),
              let referenceTime = referenceTimeline.time(at: Double(referenceHead) + Double(reference.count))
        else { return false }
        // 麦克风有一阵没交数据了（卡了一下、一批一批地交）的话，这段时间里采到的还没到这里，也算上
        let micPending = now - (micLastArrival ?? now)
        return micTime + micPending - referenceTime <= config.maxWait - config.recoverMargin
    }

    /// 做回声消除：喂一帧参考，处理一帧麦克风。ideal 是这一帧该从参考的哪个采样开始，nil：这段时间没有参考，喂 0
    private func cancel(ideal: Double?, now: Double, into lane: inout SampleFIFO) {
        let position = ideal.map(align)
        referenceFrame.withUnsafeMutableBufferPointer { frame in
            if let position {
                fillReference(from: position, into: frame.baseAddress!)
            } else {
                frame.baseAddress!.initialize(repeating: 0, count: Self.frameSize)
            }
        }
        micFrame.withUnsafeMutableBufferPointer { frame in
            mic.take(Self.frameSize, into: frame.baseAddress!)
        }
        micHead += Int64(Self.frameSize)
        let rendered = referenceFrame.withUnsafeBufferPointer { canceller.render($0.baseAddress!) }
        let captured = micFrame.withUnsafeBufferPointer { input in
            outputFrame.withUnsafeMutableBufferPointer { output in
                canceller.capture(input.baseAddress!, into: output.baseAddress!)
            }
        }
        if let position {
            referencePosition = position + Int64(Self.frameSize)
            trimReference(before: position + Int64(Self.frameSize))
        } else {
            // 空当过去以后从新的一段重新对齐（不算偏了）
            referencePosition = nil
            counters.silentReference += 1
        }
        counters.processed += 1
        dump?.writeInput(mic: micFrame, reference: referenceFrame, count: Self.frameSize)
        guard rendered && captured else {
            // 出错：这一帧当成没经过 AEC3，交原声（照样晚 latency 个采样）
            passThrough(into: &lane)
            failed(now)
            return
        }
        consecutiveFailures = 0
        if restartPending {
            // AEC3 交出来的前 latency 个是它刚开始时里面的 0（或者上一帧没经过它之前的），用 shadow 里的原声
            restartPending = false
            emit(shadow[...], into: &lane)
            emit(outputFrame[latency...], into: &lane)
        } else {
            emit(outputFrame[...], into: &lane)
        }
        keepShadow()
    }

    /// 这一帧该从参考的哪个采样开始：跟着上一帧接着往下，偏出一个采样以上就补一个或者丢一个，
    /// 偏得多（时间轴断开了）就直接跳过去
    private func align(_ ideal: Double) -> Int64 {
        let target = Int64(ideal.rounded())
        guard let current = referencePosition else { return target }
        let offset = Double(current) - ideal
        if abs(offset) > Double(config.resetSamples) {
            counters.realigns += 1
            if Self.worthLogging(counters.realigns) {
                log("回声消除：参考和麦克风的对齐差了 \(milliseconds(offset / Self.sampleRate)) 毫秒，重新对齐，AEC3 从头开始")
            }
            resetCanceller()
            return target
        }
        if abs(offset) > Double(config.realignSamples) {
            counters.realigns += 1
            return target
        }
        counters.maxAlignmentError = max(counters.maxAlignmentError ?? 0, abs(offset))
        if offset > 1.5 {
            // 参考走快了：退一个采样（上一帧的最后一个再喂一次）
            counters.slips += 1
            return current - 1
        }
        if offset < -1.5 {
            // 参考走慢了：跳过一个采样
            counters.slips += 1
            return current + 1
        }
        return current
    }

    /// 参考里从 position 开始的一帧，没有的地方（还没到、已经丢掉了）是 0
    private func fillReference(from position: Int64, into out: UnsafeMutablePointer<Float>) {
        out.initialize(repeating: 0, count: Self.frameSize)
        let available = referenceHead + Int64(reference.count)
        let start = max(position, referenceHead)
        let end = min(position + Int64(Self.frameSize), available)
        guard end > start else { return }
        reference.peek(from: Int(start - referenceHead), count: Int(end - start), into: out + Int(start - position))
    }

    /// 不做回声消除：这一帧原样交出去
    private func bypass(ideal: Double?, into lane: inout SampleFIFO) {
        micFrame.withUnsafeMutableBufferPointer { frame in
            mic.take(Self.frameSize, into: frame.baseAddress!)
        }
        micHead += Int64(Self.frameSize)
        // 中间有没喂给 AEC3 的帧，下次重新对齐
        referencePosition = nil
        if let ideal {
            trimReference(before: Int64(ideal.rounded()) + Int64(Self.frameSize))
        }
        counters.bypassed += 1
        dump?.writeInput(mic: micFrame, reference: silence, count: Self.frameSize)
        passThrough(into: &lane)
    }

    /// 这一帧（micFrame）不经过 AEC3：先交 shadow 里的，再交这一帧的前 480 - latency 个，最后 latency 个留进 shadow。
    /// 下一帧要是经过 AEC3，它交出来的前 latency 个不能用
    private func passThrough(into lane: inout SampleFIFO) {
        emit(shadow[...], into: &lane)
        emit(micFrame[0..<(Self.frameSize - latency)], into: &lane)
        keepShadow()
        restartPending = true
    }

    /// 这一帧的最后 latency 个原声留着（AEC3 里正压着它们）
    private func keepShadow() {
        shadow.removeAll(keepingCapacity: true)
        shadow.append(contentsOf: micFrame[(Self.frameSize - latency)...])
    }

    /// 交给麦克风那一路（要跳过的先跳过）。调试用的录音（--aec-dump）里跳过的也写：暂停时已经先原样交出去的、
    /// 继续时不要的（暂停期间采到的）都写处理出来的这一份，mic-out.wav 的第 n 个采样一直是 mic-in.wav 的
    /// 第 n 个采样处理出来的，暂停几次都对得上
    private func emit(_ samples: ArraySlice<Float>, into lane: inout SampleFIFO) {
        guard !samples.isEmpty else { return }
        dump?.writeOutput(samples)
        emitted += Int64(samples.count)
        var start = samples.startIndex
        if skipOutput > 0 {
            let skipped = min(skipOutput, samples.count)
            skipOutput -= skipped
            start += skipped
        }
        guard start < samples.endIndex else { return }
        lane.append(samples[start...])
        deliveredEnd = emitted
    }

    /// 参考里 end 之前的采样不会再用了（留 20 毫秒，补采样、往回对齐时要用），丢掉
    private func trimReference(before end: Int64) {
        let keep: Int64 = 960
        let excess = end - keep - referenceHead
        guard excess > 0 else { return }
        let n = min(Int(excess), reference.count)
        reference.drop(n)
        referenceHead += Int64(n)
    }

    /// 参考最多留 maxReferenceSeconds 秒
    private func capReference() {
        let limit = Int(config.maxReferenceSeconds * Self.sampleRate)
        guard reference.count > limit else { return }
        let n = reference.count - limit
        reference.drop(n)
        referenceHead += Int64(n)
    }

    // MARK: 状态

    /// 现在该不该做、为什么。AEC3 出错关掉了 > 戴耳机、AirPlay > 电脑声音断了 > 做
    private func desiredState() -> (Bool, EchoReason) {
        if !healthy {
            return (false, .off)
        }
        if config.mode == .auto {
            switch routeKind {
            case .headphones: return (false, .headphones)
            case .airplay: return (false, .airplay)
            case .speakers: break
            }
        }
        if referenceMissing {
            return (false, .noReference)
        }
        return (true, config.mode == .on ? .forced : .speakers)
    }

    /// 按 desiredState 切换，变了就发 echo 事件。重新打开时 AEC3 从头开始（中间没喂过它，回声路径也可能变了）
    private func applyState() {
        let (newActive, newReason) = desiredState()
        guard newActive != active || newReason != reason else { return }
        if newActive && !active {
            resetCanceller()
            lateFrames = 0
        }
        referencePosition = nil
        active = newActive
        reason = newReason
        announce()
    }

    /// auto 模式：输出路线变了，并且稳定了 routeDebounce 秒，才跟着变
    private func updateRoute(_ now: Double) {
        guard config.mode == .auto, let route else { return }
        let kind = route.current
        guard kind != routeKind else {
            routeCandidate = nil
            return
        }
        if let candidate = routeCandidate, candidate.kind == kind {
            guard now - candidate.since >= config.routeDebounce else { return }
            routeCandidate = nil
            routeKind = kind
            log("回声消除：输出路线变了，\(route.currentDescription)")
            applyState()
        } else {
            routeCandidate = (kind, now)
        }
    }

    /// 电脑声音断了（stallTimeout 秒没来新的；刚开始录时 startupGrace 秒还没来）
    private func checkReference(_ now: Double) {
        guard !referenceMissing else { return }
        let stalled: Bool
        if let last = referenceLastArrival {
            stalled = now - last > config.stallTimeout
        } else {
            stalled = now - startedAt > config.startupGrace
        }
        guard stalled else { return }
        counters.stalls += 1
        referenceMissing = true
        log("回声消除：电脑声音\(referenceLastArrival == nil ? "一直没来" : "断了")，先不做")
        applyState()
    }

    /// 参考来得太晚，这一帧只好用原声。一直这样就当成断了（不再等，麦克风也就不用多等）
    private func noteLateFrame(_ now: Double) {
        counters.lateFrames += 1
        lateFrames += 1
        guard lateFrames >= config.lateFramesLimit, now - startedAt >= config.startupGrace, !referenceMissing else { return }
        counters.stalls += 1
        referenceMissing = true
        log("回声消除：电脑声音一直比麦克风晚 \(milliseconds(config.maxWait)) 毫秒以上，先不做")
        applyState()
    }

    /// 处理到了新的麦克风的声音：参考重新对齐，在做回声消除的话 AEC3 从头开始
    private func restartForNewMic() {
        referencePosition = nil
        lateFrames = 0
        log("回声消除：换了麦克风\(active ? "，AEC3 从头开始" : "")")
        if active {
            resetCanceller()
        }
    }

    private func resetCanceller() {
        counters.resets += 1
        // 从头开始以后 AEC3 里压着的没了，下一帧交出来的前 latency 个改用原声
        restartPending = true
        if !canceller.reset() {
            log("回声消除：AEC3 没能从头开始，接着用原来的")
        }
    }

    /// AEC3 出错（这一帧已经用了原声）。第一次先从头开始试一次，连续出错就关掉，录音照常
    private func failed(_ now: Double) {
        counters.failures += 1
        consecutiveFailures += 1
        if consecutiveFailures >= config.maxConsecutiveFailures {
            disable("AEC3 连续出错 \(consecutiveFailures) 次")
            return
        }
        if Self.worthLogging(counters.failures) {
            log("回声消除：AEC3 出错了（第 \(counters.failures) 次），这一帧用原声，从头开始再试")
        }
        counters.resets += 1
        restartPending = true
        if !canceller.reset() {
            disable("AEC3 出错，也没能从头开始")
        }
    }

    private func disable(_ why: String) {
        guard healthy else { return }
        healthy = false
        log("回声消除：\(why)，这次录音不再做回声消除，麦克风的声音原样录")
        if config.mode == .on {
            events.send(.warning(.echoUnavailable, EchoMessages.failedDuringRecording))
        }
        applyState()
    }

    // MARK: 日志

    func logStats(final: Bool) {
        let seconds = Double(counters.processed) * Self.frameDuration
        var parts = [
            "回声消除\(final ? "（结束）" : "")：\(config.mode.rawValue) 模式，\(active ? "开着" : "没开")（\(reason.rawValue)）",
            "处理了 \(counters.processed) 帧（\(JSON.format(seconds)) 秒），直接用原声 \(counters.bypassed) 帧",
            "参考补、丢了 \(counters.slips) 个采样，重新对齐 \(counters.realigns) 次，从头开始 \(counters.resets) 次",
            "电脑声音断过 \(counters.stalls) 次、来晚 \(counters.lateFrames) 帧、断开的空当喂 0 \(counters.silentReference) 帧，出错 \(counters.failures) 次",
            "麦克风最多等了 \(milliseconds(counters.maxHeld)) 毫秒",
            "时钟：麦克风 \(ppm(micTimeline))、电脑声音 \(ppm(referenceTimeline))，时间戳断开 \(micTimeline.restarts)/\(referenceTimeline.restarts) 次",
        ]
        if let route {
            parts.append("输出路线：\(route.currentDescription)")
        }
        if let stats = canceller.stats() {
            parts.append(stats.summary)
        }
        log(parts.joined(separator: "；"))
    }

    /// 同一件事反复发生时，日志只写前几次和之后每 100 次一次
    static func worthLogging(_ count: Int) -> Bool {
        count <= 5 || count % 100 == 0
    }

    private func ppm(_ timeline: Timeline) -> String {
        guard timeline.isAnchored else { return "没有时间" }
        return "\(JSON.format((timeline.rate / Self.sampleRate - 1) * 1_000_000)) ppm"
    }
}

/// 回声消除的提醒（warning 的 message，给用户看）
enum EchoMessages {
    static let unavailable = "回声消除没能打开，这次录音里外放的会议声音可能会录两遍。戴耳机可以避免。"
    static let failedDuringRecording = "回声消除出错，已经关掉了，后面外放的会议声音可能会录两遍。录音照常进行，戴耳机可以避免。"
}

/// 秒换成毫秒的文字（日志用）
func milliseconds(_ seconds: Double) -> String {
    JSON.format(seconds * 1_000)
}

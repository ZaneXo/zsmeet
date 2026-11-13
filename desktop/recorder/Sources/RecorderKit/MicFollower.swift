import CoreAudio
import Foundation

/// 跟着系统默认的麦克风换（MicFollower）要用到的系统功能。正式运行时是 Core Audio（SystemMicrophones），
/// 测试里换成假的：跟、换的逻辑和正式运行时走的是同一套
protocol MicrophoneSystem: AnyObject {
    /// 系统设置里选的默认输入设备（在能录音的设备里找），没有时是 nil
    func defaultMicrophone() -> Microphone?
    /// 打开并开始录这个麦克风。lost：它断开了（在别的线程上调用）
    func open(_ microphone: Microphone, lost: @escaping () -> Void) throws -> CaptureSource
    /// 系统默认的输入设备变了就调用 changed（在别的线程上）。返回的函数用来取消监听
    func watchDefault(_ changed: @escaping () -> Void) -> () -> Void
}

/// 换麦克风的一方：LiveEngine.switchMic
protocol MicSwitcher: AnyObject {
    func switchMic(to replacement: CaptureSource) -> MicSwitchResult
}

extension LiveEngine: MicSwitcher {}

/// 录「系统默认的麦克风」（--mic default）时跟着系统设置走：系统默认的输入设备换了（连上了 AirPods、插上了
/// USB 麦克风、在系统设置里换了一个），或者正在录的麦克风断开了、系统换了一个默认的，就打开新的那个，交给
/// LiveEngine 接着录（switchMic：麦克风那一路不断，不重复），发 mic_changed。指定了麦克风（--mic <uid>）时不跟，
/// 断开了只提醒（见 RecordSession）。
///
/// - 默认的设备变了先等 settle 秒（插拔、连蓝牙的时候会连着变几下）再看，1 秒左右就换过去了。
/// - 新的打不开（蓝牙设备刚连上时可能还用不了）：过 retryDelay 秒再试，一共试 maxAttempts 次，都不行才提醒，
///   接着录原来的；默认的设备再变时再看。采样率处理不了的不用再试，直接提醒。
/// - 正在录的麦克风断开了、系统一时没有换一个默认的：断开 lostGrace 秒以后还没有才提醒（录不到你这边的声音了），
///   之后有了新的默认设备照样换过去。
///
/// 状态都在自己的队列上改；打开设备、换麦克风也在这个队列上做（打开蓝牙设备可能要等一会儿），这期间照常录原来的
final class MicFollower {
    struct Timing {
        /// 默认的设备变了（或者麦克风断开了）以后等多久再看
        var settle = 0.3
        /// 麦克风断开了、系统没有别的默认设备，等多久再提醒
        var lostGrace = 2.0
        /// 新的打不开时，过多久再试
        var retryDelay = 1.0
        /// 一共试几次
        var maxAttempts = 3
    }

    private let system: MicrophoneSystem
    private let events: EventSink
    private let timing: Timing
    private let queue = DispatchQueue(label: "io.github.zanexo.zsmeet.recorder.mic-follower")
    private var engine: MicSwitcher?
    private var onSwitched: ((Microphone) -> Void)?
    /// 现在录的麦克风和它的来源；它还在不在，什么时候断开的
    private var current: Microphone?
    private var capture: CaptureSource?
    private var currentAlive = true
    private var lostAt: Double?
    /// 断开了、换不过去，已经提醒过了（换过去以后清掉）
    private var lostWarned = false
    /// 开始跟之前就断开了的麦克风
    private var lostEarly: Set<AudioObjectID> = []
    /// 正在试着换过去的默认设备试了几次（试够了就等默认的设备再变）
    private var attempt: (id: AudioObjectID, count: Int)?
    /// 等着做的那次检查
    private var pending: DispatchWorkItem?
    private var stopWatching: (() -> Void)?
    private var started = false
    private var stopped = false

    init(system: MicrophoneSystem, events: EventSink, timing: Timing = Timing()) {
        self.system = system
        self.events = events
        self.timing = timing
    }

    /// 开始跟（started 事件之后）：microphone 是现在录的麦克风，capture 是它的来源，engine 换麦克风，
    /// onSwitched 在换好以后调用（在自己的队列上；更新回声消除看的输出路线）
    func start(current microphone: Microphone, capture: CaptureSource, engine: MicSwitcher,
               onSwitched: @escaping (Microphone) -> Void) {
        queue.async { [self] in
            guard !stopped, !started else { return }
            started = true
            current = microphone
            self.capture = capture
            self.engine = engine
            self.onSwitched = onSwitched
            if lostEarly.contains(microphone.id) {
                currentAlive = false
                lostAt = clock()
            }
            lostEarly.removeAll()
            stopWatching = system.watchDefault { [weak self] in self?.defaultChanged() }
            // 等权限弹窗、打开设备的时候默认的麦克风可能已经换了：开始时也看一次
            schedule(after: timing.settle)
        }
    }

    /// 录着的麦克风断开了（MicCapture 的 onLost，在别的线程上调用）
    func lost(_ microphone: Microphone) {
        queue.async { [self] in
            guard !stopped else { return }
            guard started else {
                lostEarly.insert(microphone.id)
                return
            }
            // 已经换掉的麦克风后来断开了：不用管
            guard microphone.id == current?.id, currentAlive else { return }
            currentAlive = false
            lostAt = clock()
            // 之前换不过去的设备也再试一次
            attempt = nil
            log("麦克风「\(microphone.name)」断开了，看系统有没有换一个默认的麦克风")
            schedule(after: timing.settle)
        }
    }

    /// 不跟了（录音结束时）：取消监听，停掉换上来的麦克风（LiveEngine 结束时也会停掉它，重复停没关系）。
    /// 正在换的话等它换完（不能在自己的队列上调用）
    func stop() {
        dispatchPrecondition(condition: .notOnQueue(queue))
        queue.sync {
            stopped = true
            pending?.cancel()
            pending = nil
            stopWatching?()
            stopWatching = nil
            capture?.stop()
            capture = nil
            engine = nil
            onSwitched = nil
        }
    }

    /// 测试用：等着的那次检查现在就做，做完才返回（没有等着的就什么都不做）
    func checkNow() {
        queue.sync {
            guard let item = pending else { return }
            item.cancel()
            pending = nil
            evaluate()
        }
    }

    /// 测试用：有没有等着做的检查
    var hasPendingCheck: Bool {
        queue.sync { pending != nil }
    }

    private func defaultChanged() {
        queue.async { [self] in
            guard started, !stopped else { return }
            schedule(after: timing.settle)
        }
    }

    /// delay 秒以后检查一次。之前等着的那次不要了：连着变几下时只在最后一下之后看
    private func schedule(after delay: Double) {
        pending?.cancel()
        let item = DispatchWorkItem { [weak self] in
            guard let self else { return }
            self.pending = nil
            self.evaluate()
        }
        pending = item
        queue.asyncAfter(deadline: .now() + max(delay, 0), execute: item)
    }

    /// 看系统默认的麦克风是不是换了，换了就换过去（在 queue 上）
    private func evaluate() {
        guard started, !stopped, let current else { return }
        guard let candidate = system.defaultMicrophone(), candidate.id != current.id else {
            // 默认的还是现在录的这个，或者没有默认的输入设备
            if !currentAlive {
                warnLost(current)
            }
            return
        }
        if let attempt, attempt.id == candidate.id, attempt.count >= timing.maxAttempts {
            // 这个试够了、提醒过了：等默认的设备再变
            if !currentAlive {
                warnLost(current)
            }
            return
        }
        let replacement: CaptureSource
        do {
            replacement = try system.open(candidate) { [weak self] in self?.lost(candidate) }
        } catch {
            log("打开麦克风「\(candidate.name)」：\(error)")
            failed(candidate, current: current, unsupportedRate: nil)
            return
        }
        switch engine?.switchMic(to: replacement) ?? .notRecording {
        case .switched:
            self.current = candidate
            capture = replacement
            currentAlive = true
            lostAt = nil
            lostWarned = false
            attempt = nil
            onSwitched?(candidate)
        case .unsupportedRate(let rate):
            replacement.stop()
            failed(candidate, current: current, unsupportedRate: rate)
        case .notRecording:
            // 录音已经结束了
            replacement.stop()
            stopped = true
        }
    }

    /// 换不过去：打不开（unsupportedRate 是 nil，过一会儿再试），或者采样率处理不了（不用再试）。试够了提醒一次
    private func failed(_ candidate: Microphone, current: Microphone, unsupportedRate rate: Double?) {
        let tried = attempt?.id == candidate.id ? attempt?.count ?? 0 : 0
        let count = rate == nil ? tried + 1 : timing.maxAttempts
        attempt = (candidate.id, count)
        guard count >= timing.maxAttempts else {
            schedule(after: timing.retryDelay)
            return
        }
        let name = candidate.name
        if currentAlive {
            if let rate {
                warn("系统默认的麦克风换成了「\(name)」，可是它的采样率是 \(Resampler.describe(rate))，处理不了，接着录「\(current.name)」。"
                    + "要录「\(name)」的话，到「音频 MIDI 设置」里把它改成 48000 Hz，再重新开始录音。")
            } else {
                warn("系统默认的麦克风换成了「\(name)」，可是它打不开，接着录「\(current.name)」。要录「\(name)」的话，结束这次录音再重新开始。")
            }
            return
        }
        lostWarned = true
        let problem = rate.map { "的采样率是 \(Resampler.describe($0))，处理不了" } ?? "又打不开"
        warn("麦克风「\(current.name)」断开了，系统默认的麦克风「\(name)」\(problem)，现在录不到你这边的声音。"
            + "在「系统设置 → 声音 → 输入」里换一个麦克风，录音会自动换过去接着录。")
    }

    /// 正在录的麦克风断开了，系统还没有换一个能用的默认麦克风：断开 lostGrace 秒以后还是这样才提醒（只提醒一次），
    /// 之后有了新的默认设备照样换过去
    private func warnLost(_ current: Microphone) {
        guard !lostWarned else { return }
        let waited = clock() - (lostAt ?? clock())
        guard waited >= timing.lostGrace else {
            schedule(after: timing.lostGrace - waited)
            return
        }
        lostWarned = true
        warn("麦克风「\(current.name)」断开了，现在录不到你这边的声音。接上麦克风，或者在「系统设置 → 声音 → 输入」里选一个麦克风，"
            + "录音会自动换过去接着录。")
    }

    private func warn(_ message: String) {
        log(message)
        events.send(.warning(.micUnavailable, message))
    }

    private func clock() -> Double {
        seconds(DispatchTime.now().uptimeNanoseconds)
    }
}

/// 正式运行时：读 Core Audio 的默认输入设备、打开真的麦克风
final class SystemMicrophones: MicrophoneSystem {
    private let queue = DispatchQueue(label: "io.github.zanexo.zsmeet.recorder.default-input")

    func defaultMicrophone() -> Microphone? {
        Microphones.list().first(where: \.isDefault)
    }

    func open(_ microphone: Microphone, lost: @escaping () -> Void) throws -> CaptureSource {
        let capture = try MicCapture(microphone)
        capture.onLost = lost
        try capture.start()
        return capture
    }

    func watchDefault(_ changed: @escaping () -> Void) -> () -> Void {
        let listener = PropertyListener(object: AudioProperty.system, selector: kAudioHardwarePropertyDefaultInputDevice,
                                        queue: queue, handler: changed)
        return { listener.remove() }
    }
}

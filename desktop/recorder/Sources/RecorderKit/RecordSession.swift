import CoreAudio
import Foundation

/// 程序入口：解析参数，执行 devices 或 record
public enum RecorderMain {
    public static func run(_ arguments: [String]) -> Never {
        let command: Command
        do {
            command = try CommandLineParser.parse(arguments)
        } catch {
            let message = (error as? UsageError)?.message ?? "\(error)"
            StdoutEvents().send(.error(RecorderFailure("bad_args", message)))
            log(message)
            fputs(CommandLineParser.usage + "\n", stderr)
            exit(2)
        }
        switch command {
        case .help:
            print(CommandLineParser.usage)
            exit(0)
        case .devices:
            print(Microphones.json(Microphones.list()).text)
            exit(0)
        case .record(let options):
            RecordSession(options: options).run()
        }
    }
}

/// 一次录音从开始到结束：准备目录、问权限、打开声音来源、跑录音引擎，处理 App 发来的命令和信号，
/// 最后收尾，发 stopped（或者 error）后退出。状态都在主线程上改。
final class RecordSession {
    private enum Phase {
        case preparing
        case running
        case stopping
        case done
    }

    /// 准备好的录音：引擎、声音来源，以及 started 事件里要说的
    private struct Setup {
        let engine: RecordingEngine
        let sources: [CaptureSource]
        let micName: String?
        let system: Bool
        let echo: EchoCancelStatus
        /// 麦克风自动增益的模式（要调却没能打开是 unavailable）
        let micGain: MicGainStatus
        /// 回声消除 auto 模式看的输出路线（结束时停掉它的监听）
        var route: OutputRouteSource?
        /// 录系统默认的麦克风时跟着系统设置换麦克风（结束时停掉）
        var follower: MicFollower?
        /// 开始跟（发了 started 以后调用，mic_changed 一定在 started 后面）
        var startFollowing: (() -> Void)?
    }

    /// 准备的时候 App 让停止了
    private struct Cancelled: Error {}

    /// 结束时最多等这么久。超时多半是 Core Audio 卡住了，不等了。
    /// App 那边（desktop/src-tauri 的 RecorderOptions.stop_timeout，10 秒）要等得比这个久，改了这里那边也要跟着改
    static let stopTimeout: TimeInterval = 8

    private let options: RecordOptions
    private let events = StdoutEvents()
    private var phase = Phase.preparing
    private var engine: RecordingEngine?
    private var sources: [CaptureSource] = []
    private var route: OutputRouteSource?
    private var follower: MicFollower?
    private var signalSources: [DispatchSourceSignal] = []
    private var activity: NSObjectProtocol?

    /// 准备是在后台线程上做的（要等用户点权限弹窗），这期间 App 让停止的话记在这里
    private let stopLock = NSLock()
    private var stopWanted = false

    init(options: RecordOptions) {
        self.options = options
    }

    func run() -> Never {
        // App 退出后再写标准输出会收到 SIGPIPE，忽略它，改由 write 的返回值发现
        signal(SIGPIPE, SIG_IGN)
        events.onClosed = { [weak self] in
            DispatchQueue.main.async {
                log("标准输出断了，App 可能已经退出，结束录音")
                self?.requestStop()
            }
        }
        installSignalHandlers()
        readCommands()
        DispatchQueue.global(qos: .userInitiated).async { self.prepare() }
        dispatchMain()
    }

    // MARK: 准备（后台线程）

    private var isStopWanted: Bool {
        stopLock.lock()
        defer { stopLock.unlock() }
        return stopWanted
    }

    private func prepare() {
        do {
            let directory = try OutputDirectory.prepare(options.outDir, startSeq: options.startSeq)
            let chunks = ChunkWriter(directory: directory, startSeq: options.startSeq, chunkSeconds: options.chunkSeconds)
            let pipeline = RecordingPipeline(encoder: try AACEncoder(), chunks: chunks, events: events)
            let setup = try options.inputFile.map { try prepareFile($0, pipeline: pipeline) } ?? prepareDevices(pipeline: pipeline)
            DispatchQueue.main.async { self.begin(setup) }
        } catch is Cancelled {
            DispatchQueue.main.async { self.finishWithoutRecording() }
        } catch {
            let failure = error as? RecorderFailure ?? RecorderFailure("internal", "录音没能开始：\(error)")
            DispatchQueue.main.async { self.fail(failure) }
        }
    }

    private func prepareFile(_ path: String, pipeline: RecordingPipeline) throws -> Setup {
        let reader = try AudioFileReader(path: path)
        guard options.realtime else {
            // 尽快读完的测试模式只有一路，不做回声消除，started 里照样报要的模式。文件当成麦克风，照样调音量
            let engine = try FileEngine(reader: reader, pipeline: pipeline, micGain: micGainSetup())
            return Setup(engine: engine, sources: [], micName: reader.name, system: false,
                         echo: EchoCancelStatus(rawValue: options.echoCancel.rawValue) ?? .off, micGain: engine.micGainStatus)
        }
        // 文件的采样率处理不了时直接报错（unsupported_format），和不加 --realtime 时一样
        _ = try Resampler(inputRate: reader.sampleRate)
        let source = try RealtimeFileSource(reader: reader)
        var sources = [source]
        // --reference-file：再拿一个文件当电脑声音（回声消除的参考）。它比「麦克风」短的话，后面当成电脑声音断了
        if let referencePath = options.referenceFile {
            let referenceReader = try AudioFileReader(path: referencePath)
            _ = try Resampler(inputRate: referenceReader.sampleRate)
            sources.append(try RealtimeFileSource(reader: referenceReader, kind: .system))
        }
        // 「麦克风」的文件读完了就结束
        source.onEnded = { DispatchQueue.main.async { self.requestStop() } }
        for file in sources {
            // 读到一半出错：以 error 结束，不能当成读完了、正常结束（后面的声音就悄悄没了）
            file.onFailed = { failure in DispatchQueue.main.async { self.fail(failure) } }
        }
        // 读文件时不看真的输出设备，auto 当成外放
        let engine = try LiveEngine(sources: sources, pipeline: pipeline, events: events, echo: echoSetup(route: FixedRoute(.speakers)),
                                    micGain: micGainSetup())
        // 两个文件从同一个时间开始，时间戳严格对齐
        let startTime = DispatchTime.now().uptimeNanoseconds
        for file in sources {
            file.startTime = startTime
            try file.start()
        }
        return Setup(engine: engine, sources: sources, micName: reader.name, system: sources.count > 1, echo: engine.echoStatus,
                     micGain: engine.micGainStatus)
    }

    /// 回声消除的设置（--echo-cancel off 时是 nil）。route：auto 模式看的输出路线
    private func echoSetup(route: OutputRouteSource?) -> EchoSetup? {
        guard options.echoCancel != .off else { return nil }
        let filters = options.aecFilters
        let hint = options.aecDelayHintMilliseconds
        return EchoSetup(mode: options.echoCancel, leadMilliseconds: options.aecLeadMilliseconds,
                         makeCanceller: { WebRTCEchoCanceller(filters: filters, streamDelayMilliseconds: hint) },
                         route: route, dumpDirectory: options.aecDump)
    }

    /// 麦克风自动增益的设置（--mic-gain off 时是 nil）
    private func micGainSetup() -> MicGainSetup? {
        options.micGain == .off ? nil : .webRTC()
    }

    private func prepareDevices(pipeline: RecordingPipeline) throws -> Setup {
        var sources: [CaptureSource] = []
        // 录系统默认的麦克风时跟着系统设置换（指定了麦克风时不跟）
        let follower = options.mic == .systemDefault ? MicFollower(system: SystemMicrophones(), events: events) : nil
        var opened: (capture: MicCapture, microphone: Microphone)?
        if options.mic != .none, let mic = openMicrophone(follower: follower) {
            sources.append(mic.capture)
            opened = mic
        }
        if isStopWanted { throw Cancelled() }
        if options.system, let capture = openSystemAudio() {
            sources.append(capture)
        }
        if isStopWanted { throw Cancelled() }
        if sources.isEmpty {
            // 每一路为什么录不了，前面已经各发了一个 warning（App 和它一起显示）
            throw RecorderFailure.noSource
        }
        // 回声消除的 auto 模式要跟着输出设备（外放还是耳机）。两路都在录才用得上
        let route: OutputRouteSource? = options.echoCancel == .auto && opened != nil && sources.count == 2
            ? OutputRoute(micDevice: opened?.capture.deviceID) : nil
        // 采样率处理不了的那一路，LiveEngine 停掉它、发 warning，只录剩下的（一路都不剩时抛出 no_source）
        let engine = try LiveEngine(sources: sources, pipeline: pipeline, events: events, echo: echoSetup(route: route),
                                    micGain: micGainSetup())
        let recorded = engine.sources
        // 麦克风真的在录才跟（没开起来、采样率处理不了的不跟）
        var startFollowing: (() -> Void)?
        if let follower, let opened, recorded.contains(where: { $0 === opened.capture }) {
            startFollowing = {
                follower.start(current: opened.microphone, capture: opened.capture, engine: engine) { microphone in
                    // 判断输出是不是耳麦要看录的是哪个麦克风
                    route?.updateMicDevice(microphone.id)
                }
            }
        }
        return Setup(engine: engine, sources: recorded,
                     micName: recorded.first { $0.kind == .mic }?.name,
                     system: recorded.contains { $0.kind == .system },
                     echo: engine.echoStatus, micGain: engine.micGainStatus, route: route, follower: follower,
                     startFollowing: startFollowing)
    }

    /// 打开麦克风。follower 不是 nil（录系统默认的麦克风）时，麦克风断开了交给它（看系统有没有换一个默认的）
    private func openMicrophone(follower: MicFollower?) -> (capture: MicCapture, microphone: Microphone)? {
        switch MicPermission.ensure(cancelled: { self.isStopWanted }) {
        case .granted, .unknown:
            break
        case .cancelled:
            return nil
        case .denied:
            warn(.micDenied, "没有麦克风权限，录不到你这边的声音。到「系统设置 → 隐私与安全性 → 麦克风」里打开 zsmeet，再重新开始录音。")
            return nil
        }

        var microphone = Microphones.find(options.mic)
        if microphone == nil, case .device = options.mic, let fallback = Microphones.find(.systemDefault) {
            warn(.micUnavailable, "没找到选的麦克风（可能已经拔掉了），改用「\(fallback.name)」。")
            microphone = fallback
        }
        guard let microphone else {
            warn(.micUnavailable, "这台电脑上没有能用的麦克风，录不到你这边的声音。接上麦克风后重新开始录音。")
            return nil
        }
        do {
            let capture = try MicCapture(microphone)
            if let follower {
                // 先看系统有没有换一个默认的麦克风，换过去接着录；换不过去它会提醒
                capture.onLost = { [weak follower] in follower?.lost(microphone) }
            } else {
                capture.onLost = { [events] in
                    events.send(.warning(.micUnavailable, "麦克风「\(microphone.name)」断开了，后面录不到你这边的声音。接好以后结束这次录音，再重新开始。"))
                }
            }
            try capture.start()
            return (capture, microphone)
        } catch {
            log("打开麦克风「\(microphone.name)」：\(error)")
            warn(.micUnavailable, "麦克风「\(microphone.name)」打不开，录不到你这边的声音。换一个麦克风再试。")
            return nil
        }
    }

    private func openSystemAudio() -> SystemAudioCapture? {
        switch SystemAudioPermission.ensure(cancelled: { self.isStopWanted }) {
        case .granted, .unknown:
            break
        case .cancelled:
            return nil
        case .denied:
            warn(.systemDenied, "没有录电脑声音的权限，录不到会议软件里其他人的声音。到「系统设置 → 隐私与安全性 → 屏幕与系统录音」，在「仅系统录音」里打开 zsmeet，再重新开始录音。")
            return nil
        }
        do {
            let capture = try SystemAudioCapture()
            try capture.start()
            return capture
        } catch {
            log("录电脑声音：\(error)")
            warn(.systemUnavailable, "录不了电脑里的声音，录不到会议软件里其他人的声音。重新开始录音试试，还不行的话重启一下电脑。")
            return nil
        }
    }

    private func warn(_ code: WarningCode, _ message: String) {
        log(message)
        events.send(.warning(code, message))
    }

    // MARK: 录音中（主线程）

    private func begin(_ setup: Setup) {
        sources = setup.sources
        route = setup.route
        follower = setup.follower
        if isStopWanted {
            finishWithoutRecording()
            return
        }
        engine = setup.engine
        phase = .running
        // 录音期间不让系统把我们当成闲着：App Nap 会拖慢定时器，闲置睡眠会让录音停下来
        activity = ProcessInfo.processInfo.beginActivity(options: .userInitiated, reason: "录音")
        events.send(.started(sampleRate: AACEncoder.sampleRate, channels: 1, mic: setup.micName, system: setup.system,
                             echoCancellation: setup.echo, micGain: setup.micGain))
        setup.engine.onFinished = { result in
            DispatchQueue.main.async { self.finish(result) }
        }
        setup.engine.start()
        setup.startFollowing?()
    }

    private func handle(_ command: ControlCommand) {
        switch command {
        case .stop:
            requestStop()
        case .pause:
            if phase == .running {
                engine?.pause()
            } else {
                log("还没开始录音，不能暂停")
            }
        case .resume:
            if phase == .running {
                engine?.resume()
            }
        }
    }

    private func requestStop() {
        switch phase {
        case .preparing:
            stopLock.lock()
            stopWanted = true
            stopLock.unlock()
        case .running:
            phase = .stopping
            DispatchQueue.main.asyncAfter(deadline: .now() + Self.stopTimeout) {
                guard self.phase == .stopping else { return }
                self.abandon(RecorderFailure("stop_timeout", "录音没能正常结束，最后几秒可能没有保存下来。"))
            }
            engine?.stop()
        case .stopping, .done:
            break
        }
    }

    private func finish(_ result: Result<RecordingResult, RecorderFailure>) {
        switch result {
        case .success(let recording):
            quit(0, after: .stopped(nextSeq: recording.nextSeq, seconds: recording.seconds))
        case .failure(let failure):
            fail(failure)
        }
    }

    /// 还没开始录就让停止了
    private func finishWithoutRecording() {
        quit(0, after: .stopped(nextSeq: options.startSeq, seconds: 0))
    }

    private func fail(_ failure: RecorderFailure) {
        log(failure.message)
        quit(1, after: .error(failure))
    }

    /// 发出最后一个事件（这时分片都已经落盘了），拆掉录音用的设备，退出。最后一个事件之后什么都不再写
    /// （比如一路声音读文件出错时，引擎的定时器还在走，不能在 error 后面又冒出一个音量）
    private func quit(_ code: Int32, after event: RecorderEvent) {
        guard phase != .done else { return }
        phase = .done
        events.sendLast(event)
        // 先不跟了（停掉换上来的麦克风），再停开始时打开的
        follower?.stop()
        for source in sources {
            source.stop()
        }
        route?.stop()
        AudioCleanup.runAll()
        if let activity {
            ProcessInfo.processInfo.endActivity(activity)
        }
        exit(code)
    }

    /// 结束卡住了（多半是 Core Audio 没有响应）：不再碰音频设备（会跟着卡住），发完事件直接退出。
    /// 本进程私有的聚合设备和 tap，系统会在进程退出时收回
    private func abandon(_ failure: RecorderFailure) {
        guard phase != .done else { return }
        phase = .done
        log(failure.message)
        events.sendLast(.error(failure))
        _exit(1)
    }

    // MARK: 命令和信号

    /// 标准输入一行一个命令。读到头（App 关掉了管道，或者 App 不在了）等于 stop
    private func readCommands() {
        let thread = Thread {
            while let line = readLine() {
                if let command = ControlCommand(line: line) {
                    DispatchQueue.main.async { self.handle(command) }
                } else if !line.trimmingCharacters(in: .whitespaces).isEmpty {
                    log("不认识的命令：\(line)")
                }
            }
            DispatchQueue.main.async { self.requestStop() }
        }
        thread.name = "stdin"
        thread.start()
    }

    /// SIGINT、SIGTERM、SIGHUP 都按 stop 处理：最后一片写完、拆掉录音用的设备再退出。
    /// 结束的过程中又收到一次就不等了。
    private func installSignalHandlers() {
        for number in [SIGINT, SIGTERM, SIGHUP] {
            signal(number, SIG_IGN)
            let source = DispatchSource.makeSignalSource(signal: number, queue: .main)
            source.setEventHandler {
                if self.phase == .stopping {
                    self.abandon(RecorderFailure("killed", "录音被强制结束了，最后几秒可能没有保存下来。"))
                } else {
                    log("收到信号 \(number)，结束录音")
                    self.requestStop()
                }
            }
            source.resume()
            signalSources.append(source)
        }
    }
}

/// 放分片的目录
enum OutputDirectory {
    /// 建好目录（返回绝对路径），并确认不会覆盖已经录好的分片
    static func prepare(_ path: String, startSeq: Int) throws -> URL {
        let url = URL(fileURLWithPath: path).standardizedFileURL
        do {
            // 这里新建的目录（包括中间的）只有自己能进，和 App 建的一样（0700）；已经有的目录不动
            try FileManager.default.createDirectory(at: url, withIntermediateDirectories: true,
                                                    attributes: [.posixPermissions: 0o700])
        } catch {
            throw RecorderFailure("out_dir", "建不了录音目录 \(url.path)：\(error.localizedDescription)")
        }
        guard access(url.path, W_OK) == 0 else {
            throw RecorderFailure("out_dir", "录音目录 \(url.path) 写不进去，请检查权限。")
        }
        // 列不出里面有什么（比如只能写、不能读）就不录：看不到已经录好的分片，接着录可能把它们覆盖掉
        let names: [String]
        do {
            names = try FileManager.default.contentsOfDirectory(atPath: url.path)
        } catch {
            log("列出录音目录 \(url.path)：\(error.localizedDescription)")
            throw RecorderFailure("out_dir", "录音目录 \(url.path) 读不了，看不到里面已经录好的分片，接着录可能会覆盖掉它们。请检查权限。")
        }
        if let taken = names.compactMap(ChunkWriter.seq(fromFileName:)).filter({ $0 >= startSeq }).min() {
            throw RecorderFailure("chunk_exists", "录音目录里已经有 \(taken).aac 了，从第 \(startSeq) 片接着录会把它覆盖掉。")
        }
        return url
    }
}

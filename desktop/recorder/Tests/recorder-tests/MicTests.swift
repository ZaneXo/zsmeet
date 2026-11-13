import CoreAudio
import CRecorder
import Foundation
@testable import RecorderKit

/// 麦克风：有没有声音（SilenceWatch）、跟着系统默认的麦克风换（MicFollower、LiveEngine.switchMic）。
/// 都不要真的设备：换麦克风用假的「系统」（FakeMicrophones）驱动，跟、换的逻辑和正式运行时走的是同一套
let micTests: [(String, () throws -> Void)] = [
    ("没有声音：刚开始录就一直全是 0，5 秒时提醒一次（mic_unavailable），同一段静音不再提醒", testSilenceWatchStartup),
    ("没有声音：录音中连续 30 秒全是 0 提醒一次（mic_silent）；有声音了算恢复，再静音 30 秒再提醒", testSilenceWatchZeros),
    ("没有声音：比 -90 dBFS 还小的底噪 1 分钟才提醒；安静房间的底噪（-70 dBFS）不提醒", testSilenceWatchQuiet),
    ("没有声音：暂停的时间不算；暂停时有声音也算恢复", testSilenceWatchPause),
    ("没有声音：16 kHz、44.1 kHz 的麦克风也按秒算", testSilenceWatchRates),
    ("没有声音：提醒里说了可能的原因、怎么检查", testSilenceWatchMessage),
    ("没有声音：现场录音里麦克风静音 30 秒（不算暂停）发 mic_silent，电脑声音不检查", testEngineSilenceWarning),
    ("回声消除：换了设备，时间轴从新设备的第一个时间戳开始新的一段（差不到 2 毫秒也是）", testTimelineSplit),
    ("换麦克风：旧的剩下的都录进去，新的接着录，两个同时在录的那一段按时间戳去掉，一个不重一个不少", testEngineSwitchMicIsSeamless),
    ("换麦克风：新的是 44.1 kHz，换转换器，重叠的按它的采样率去掉", testEngineSwitchMicOtherRate),
    ("换麦克风：新的采样率处理不了时不换；录音结束了不换", testEngineSwitchMicRefused),
    ("换麦克风（回声消除）：压着的旧麦克风的声音照旧处理，处理到新的麦克风时 AEC3 从头开始，按新的时间戳配参考", testEngineSwitchMicWithEcho),
    ("换麦克风（回声消除这一步）：新的麦克风按它自己的时间配参考，进多少出多少", testStageMicDeviceChanged),
    ("跟着默认的麦克风：系统默认的换了就换过去（mic_changed），抖一下又回来不换；不跟了停掉换上来的", testFollowerFollowsDefault),
    ("跟着默认的麦克风：在录的断开了、系统换了一个默认的，换过去，不提醒", testFollowerReplacesLostMic),
    ("跟着默认的麦克风：断开了、一时没有别的麦克风，等一会儿才提醒；之后有了默认的照样换过去", testFollowerWarnsWhenNothingToSwitchTo),
    ("跟着默认的麦克风：新的打不开时再试，试了 3 次都不行才提醒，接着录原来的", testFollowerRetriesThenWarns),
    ("跟着默认的麦克风：新的采样率处理不了，提醒，接着录原来的；录音结束了不再换", testFollowerUnsupportedRateAndFinished),
    ("跟着默认的麦克风：默认的换了，1 秒以内换过去，录到的是新的麦克风的声音，时长照样跟着时钟走", testFollowerSwitchesWithinASecond),
]

// MARK: 工具

/// 从 offset + 1 开始编号的采样：看得出哪个采样到了哪里（Float 能精确表示到 1600 多万）
private func counting(_ count: Int, from offset: Int = 0) -> [Float] {
    (0..<count).map { Float($0 + offset + 1) }
}

/// 一段指定均方根的噪声（可重复；均匀分布 ±a 的均方根是 a/√3）
private func noise(_ count: Int, decibels: Double, seed: UInt64) -> [Float] {
    var random = SplitMix(seed: seed)
    let amplitude = Float(pow(10, decibels / 20) * 3.0.squareRoot())
    return (0..<count).map { _ in (random.uniform() * 2 - 1) * amplitude }
}

/// 喂 seconds 秒（每次 20 毫秒），第 i 个采样是 sample(i)，返回这期间的提醒
private func feed(_ watch: inout SilenceWatch, seconds: Double, rate: Double = 48_000, paused: Bool = false,
                  _ sample: (Int) -> Float) -> [SilenceWatch.Alert] {
    let block = Int(rate / 50)
    let total = Int((seconds * rate).rounded())
    var buffer = [Float](repeating: 0, count: block)
    var alerts: [SilenceWatch.Alert] = []
    var done = 0
    while done < total {
        let count = min(block, total - done)
        for index in 0..<count {
            buffer[index] = sample(done + index)
        }
        alerts += buffer.withUnsafeBufferPointer { all in
            watch.feed(UnsafeBufferPointer(rebasing: all[0..<count]), sampleRate: rate, paused: paused)
        }
        done += count
    }
    return alerts
}

/// 提醒的种类：startup（开头全是 0）、zero（全是 0）、quiet（几乎没有声音）、recovered（恢复了）
private func kinds(_ alerts: [SilenceWatch.Alert]) -> [String] {
    alerts.map { alert in
        switch alert {
        case .startup: return "startup"
        case .silent(let digital, _): return digital ? "zero" : "quiet"
        case .recovered: return "recovered"
        }
    }
}

/// 提醒里说的时长（第一个 silent）
private func silentSeconds(_ alerts: [SilenceWatch.Alert]) -> Double {
    for case .silent(_, let seconds) in alerts {
        return seconds
    }
    return -1
}

/// 等到条件成立（最多 timeout 秒），返回成没成立
private func waitUntil(_ timeout: Double, _ condition: () -> Bool) -> Bool {
    let deadline = Date().addingTimeInterval(timeout)
    while Date() < deadline {
        if condition() { return true }
        usleep(5_000)
    }
    return condition()
}

/// 事件里的 warning（JSON 解出来的 code、message）
private func warnings(_ events: RecordedEvents) -> [(code: String, message: String)] {
    events.all.compactMap { line in
        guard let object = (try? JSONSerialization.jsonObject(with: Data(line.utf8))) as? [String: Any],
              object["event"] as? String == "warning"
        else { return nil }
        return (object["code"] as? String ?? "", object["message"] as? String ?? "")
    }
}

/// 事件里的 mic_changed 换成的名字
private func micChanges(_ events: RecordedEvents) -> [String] {
    events.all.compactMap { line in
        guard let object = (try? JSONSerialization.jsonObject(with: Data(line.utf8))) as? [String: Any],
              object["event"] as? String == "mic_changed"
        else { return nil }
        return object["name"] as? String
    }
}

/// 第 sample 个采样（48 kHz）在 origin 之后多少纳秒
private func nanoseconds(_ sample: Int, rate: Double = 48_000) -> UInt64 {
    UInt64((Double(sample) / rate * 1_000_000_000).rounded())
}

/// 第一个对不上的地方（说明用）
private func firstDifference(_ a: [Float], _ b: [Float]) -> String {
    guard let index = zip(a, b).enumerated().first(where: { $0.element.0 != $0.element.1 })?.offset else {
        return a.count == b.count ? "一样" : "长度不一样：\(a.count)、\(b.count)"
    }
    return "第 \(index) 个：\(a[index])、\(b[index])（长度 \(a.count)、\(b.count)）"
}

/// 现场录音，时钟手动拨（每次 20 毫秒）
private final class EngineRun {
    let clock = ManualClock()
    let events = RecordedEvents()
    let pipeline: RecordingPipeline
    let engine: LiveEngine

    init(_ sources: [CaptureSource], echo: EchoSetup? = nil) throws {
        pipeline = RecordingPipeline(encoder: try AACEncoder(),
                                     chunks: ChunkWriter(directory: try temporaryDirectory("mic"), startSeq: 0, chunkSeconds: 10),
                                     events: events)
        let clock = clock
        engine = try LiveEngine(sources: sources, pipeline: pipeline, events: events, clock: { clock.now }, echo: echo)
        engine.queue.sync { engine.startClock() }
    }

    func run(ticks: Int, deliver: () -> Void) {
        for _ in 0..<ticks {
            clock.advance(milliseconds: 20)
            deliver()
            engine.queue.sync { engine.tick() }
        }
    }

    func stop() -> Result<RecordingResult, RecorderFailure>? {
        let finished = DispatchSemaphore(value: 0)
        var result: Result<RecordingResult, RecorderFailure>?
        engine.onFinished = {
            result = $0
            finished.signal()
        }
        engine.stop()
        return finished.wait(timeout: .now() + 10) == .success ? result : nil
    }
}

/// 时钟不走的现场录音：不混音，麦克风那一路收到的都留在它的队列里，每次 tick 以后取出来看
private final class FrozenRun {
    let events = RecordedEvents()
    let engine: LiveEngine
    private(set) var lane: [Float] = []

    init(_ sources: [CaptureSource]) throws {
        let pipeline = RecordingPipeline(encoder: try AACEncoder(),
                                         chunks: ChunkWriter(directory: try temporaryDirectory("mic-frozen"), startSeq: 0, chunkSeconds: 10),
                                         events: events)
        engine = try LiveEngine(sources: sources, pipeline: pipeline, events: events, clock: { 0 })
    }

    func tick() {
        engine.queue.sync {
            engine.tick()
            engine.mixer.lanes[0].queue.withUnsafeSamples { lane.append(contentsOf: $0) }
            engine.mixer.lanes[0].queue.removeAll()
        }
    }

    func stop() {
        let finished = DispatchSemaphore(value: 0)
        engine.onFinished = { _ in finished.signal() }
        engine.stop()
        _ = finished.wait(timeout: .now() + 10)
    }
}

/// 假的「系统」：测试里换默认的麦克风、让打开失败、指定采样率。打开的麦克风是 FakeSource
final class FakeMicrophones: MicrophoneSystem {
    private let lock = NSLock()
    private var defaultMic: Microphone?
    private var changed: (() -> Void)?
    private var _opened: [FakeSource] = []
    private var _attempts: [AudioObjectID] = []
    /// 打开时出错的设备
    var failing: Set<AudioObjectID> = []
    /// 设备的采样率（没写的是 48 kHz）
    var rates: [AudioObjectID: Double] = [:]

    init(default microphone: Microphone?) {
        defaultMic = microphone
    }

    var opened: [FakeSource] {
        lock.lock()
        defer { lock.unlock() }
        return _opened
    }

    /// 试着打开过的设备（按先后）
    var attempts: [AudioObjectID] {
        lock.lock()
        defer { lock.unlock() }
        return _attempts
    }

    var watching: Bool {
        lock.lock()
        defer { lock.unlock() }
        return changed != nil
    }

    /// 系统默认的输入设备换了（像 Core Audio 的监听一样通知）
    func setDefault(_ microphone: Microphone?) {
        lock.lock()
        defaultMic = microphone
        let handler = changed
        lock.unlock()
        handler?()
    }

    func defaultMicrophone() -> Microphone? {
        lock.lock()
        defer { lock.unlock() }
        return defaultMic
    }

    func open(_ microphone: Microphone, lost: @escaping () -> Void) throws -> CaptureSource {
        lock.lock()
        defer { lock.unlock() }
        _attempts.append(microphone.id)
        if failing.contains(microphone.id) {
            throw CoreAudioError(action: "开始录麦克风", status: kAudioHardwareNotRunningError)
        }
        let source = FakeSource(kind: .mic, sampleRate: rates[microphone.id] ?? 48_000, name: microphone.name)
        _opened.append(source)
        return source
    }

    func watchDefault(_ changed: @escaping () -> Void) -> () -> Void {
        lock.lock()
        self.changed = changed
        lock.unlock()
        return { [weak self] in
            guard let self else { return }
            self.lock.lock()
            self.changed = nil
            self.lock.unlock()
        }
    }
}

private let builtIn = Microphone(id: 41, uid: "BuiltInMicrophoneDevice", name: "MacBook Pro 麦克风", isDefault: true)
private let airPods = Microphone(id: 77, uid: "airpods", name: "AirPods Pro", isDefault: true)
private let usb = Microphone(id: 90, uid: "usb", name: "USB 麦克风", isDefault: true)

/// 测试里不自己到时间：等着的检查都由测试调用 checkNow 做（断开以后不用等就提醒）
private let manual = MicFollower.Timing(settle: 60, lostGrace: 0, retryDelay: 60, maxAttempts: 3)

/// 录着 builtIn（FakeSource）和电脑声音、时钟不走的现场录音，加上跟着默认麦克风的 follower
private final class FollowFixture {
    let system: FakeMicrophones
    let mic = FakeSource(kind: .mic, name: builtIn.name)
    let run: FrozenRun
    let follower: MicFollower
    /// onSwitched 收到的设备
    private(set) var switched: [AudioObjectID] = []

    init(default microphone: Microphone? = builtIn, timing: MicFollower.Timing = manual, start: Bool = true) throws {
        system = FakeMicrophones(default: microphone)
        run = try FrozenRun([mic, FakeSource(kind: .system)])
        follower = MicFollower(system: system, events: run.events, timing: timing)
        if start {
            self.start()
        }
    }

    func start() {
        follower.start(current: builtIn, capture: mic, engine: run.engine) { [weak self] in self?.switched.append($0.id) }
    }

    var events: RecordedEvents { run.events }
}

// MARK: 没有声音

func testSilenceWatchStartup() {
    var watch = SilenceWatch()
    expectEqual(kinds(feed(&watch, seconds: 4.9) { _ in 0 }), [])
    expectEqual(kinds(feed(&watch, seconds: 0.2) { _ in 0 }), ["startup"], "第 5 秒")
    expectEqual(kinds(feed(&watch, seconds: 120) { _ in 0 }), [], "同一段静音只提醒一次")
    // 有声音了（恢复了，只写日志）：之后再静音 30 秒提醒 mic_silent
    expectEqual(kinds(feed(&watch, seconds: 0.1) { _ in 0.1 }), ["recovered"])
    expectEqual(kinds(feed(&watch, seconds: 30.05) { _ in 0 }), ["zero"])

    // 开头有一点底噪（-80 dBFS）：不是全是 0，不提醒
    var noisy = SilenceWatch()
    expectEqual(kinds(feed(&noisy, seconds: 10) { _ in 0.0001 }), [])
}

func testSilenceWatchZeros() {
    var watch = SilenceWatch()
    expectEqual(kinds(feed(&watch, seconds: 1) { _ in 0.05 }), [], "在说话")
    expectEqual(kinds(feed(&watch, seconds: 29.9) { _ in 0 }), [])
    let alerts = feed(&watch, seconds: 0.2) { _ in 0 }
    expectEqual(kinds(alerts), ["zero"], "连续 30 秒全是 0")
    expectClose(silentSeconds(alerts), 30, within: 0.11)
    expectEqual(kinds(feed(&watch, seconds: 60) { _ in 0 }), [], "同一段只提醒一次")
    // 「咔」一下（0.1 秒）就算恢复了；之后再静音 30 秒再提醒
    expectEqual(kinds(feed(&watch, seconds: 0.1) { _ in 0.05 }), ["recovered"])
    expectEqual(kinds(feed(&watch, seconds: 29.9) { _ in 0 }), [])
    expectEqual(kinds(feed(&watch, seconds: 0.2) { _ in 0 }), ["zero"])

    // 极小的数（-120 dBFS 以下：有的设备给的不是正好 0）也算全是 0
    var dust = SilenceWatch()
    _ = feed(&dust, seconds: 1) { _ in 0.05 }
    expectEqual(kinds(feed(&dust, seconds: 30.1) { $0 % 2 == 0 ? 5e-7 : -5e-7 }), ["zero"])
}

func testSilenceWatchQuiet() {
    // -95 dBFS 的底噪：比 -90 dBFS 小，但不是 0。30 秒不提醒，1 分钟提醒
    let faint = noise(960, decibels: -95, seed: 5)
    var watch = SilenceWatch()
    _ = feed(&watch, seconds: 1) { _ in 0.05 }
    expectEqual(kinds(feed(&watch, seconds: 59.9) { faint[$0 % faint.count] }), [])
    let alerts = feed(&watch, seconds: 0.2) { faint[$0 % faint.count] }
    expectEqual(kinds(alerts), ["quiet"], "连续 1 分钟比 -90 dBFS 还小")
    expectClose(silentSeconds(alerts), 60, within: 0.11)

    // 安静房间的底噪（-70 dBFS）、很安静的设备（-85 dBFS）：录 3 分钟都不提醒
    for (decibels, seed) in [(-70.0, UInt64(6)), (-85.0, UInt64(7))] {
        let room = noise(960, decibels: decibels, seed: seed)
        var quietRoom = SilenceWatch()
        expectEqual(kinds(feed(&quietRoom, seconds: 180) { room[$0 % room.count] }), [], "\(decibels) dBFS 的底噪")
    }
}

func testSilenceWatchPause() {
    var watch = SilenceWatch()
    _ = feed(&watch, seconds: 1) { _ in 0.05 }
    expectEqual(kinds(feed(&watch, seconds: 20) { _ in 0 }), [])
    expectEqual(kinds(feed(&watch, seconds: 60, paused: true) { _ in 0 }), [], "暂停的时间不算")
    expectEqual(kinds(feed(&watch, seconds: 9.9) { _ in 0 }), [])
    expectEqual(kinds(feed(&watch, seconds: 0.2) { _ in 0 }), ["zero"], "不算暂停一共 30 秒")
    // 暂停时有声音也算恢复
    expectEqual(kinds(feed(&watch, seconds: 0.1, paused: true) { _ in 0.05 }), ["recovered"])
    expectEqual(kinds(feed(&watch, seconds: 30.1) { _ in 0 }), ["zero"])
}

func testSilenceWatchRates() {
    for rate in [16_000.0, 44_100.0] {
        var watch = SilenceWatch()
        expectEqual(kinds(feed(&watch, seconds: 5.1, rate: rate) { _ in 0 }), ["startup"], "\(rate) Hz：开头 5 秒")
        _ = feed(&watch, seconds: 1, rate: rate) { _ in 0.05 }
        expectEqual(kinds(feed(&watch, seconds: 29.9, rate: rate) { _ in 0 }), [], "\(rate) Hz")
        expectEqual(kinds(feed(&watch, seconds: 0.2, rate: rate) { _ in 0 }), ["zero"], "\(rate) Hz：30 秒")
    }
}

func testSilenceWatchMessage() {
    let zero = SilenceWatch.message(name: "MacBook Pro 麦克风", digital: true)
    let quiet = SilenceWatch.message(name: "MacBook Pro 麦克风", digital: false)
    expect(zero.hasPrefix("麦克风「MacBook Pro 麦克风」已经 30 秒录到的全是静音"), zero)
    expect(quiet.hasPrefix("麦克风「MacBook Pro 麦克风」已经 1 分钟几乎录不到声音"), quiet)
    for text in [zero, quiet] {
        for part in ["如果这段时间你说过话", "会议软件占住了麦克风", "被静音了", "录的不是你正在用的麦克风", "「系统设置 → 声音 → 输入」",
                     "换一个麦克风"] {
            expect(text.contains(part), "提醒里要有「\(part)」：\(text)")
        }
    }
    expectEqual(SilenceWatch.duration(30), "30 秒")
    expectEqual(SilenceWatch.duration(60), "1 分钟")
    expectEqual(SilenceWatch.duration(90), "90 秒")
}

func testEngineSilenceWarning() throws {
    let mic = FakeSource(kind: .mic, silenceWarning: "开头全是 0", name: "USB 麦克风")
    let system = FakeSource(kind: .system)
    let run = try EngineRun([mic, system])
    // 说了 0.5 秒，之后麦克风全是 0（电脑声音也一直是 0，它不检查）
    run.run(ticks: 25) {
        mic.push(960, value: 0.05)
        system.push(960, value: 0)
    }
    let silent = {
        mic.push(960, value: 0)
        system.push(960, value: 0)
    }
    run.run(ticks: 1_000, deliver: silent)
    // 暂停的 30 秒不算
    run.engine.pause()
    run.engine.queue.sync {}
    run.run(ticks: 1_500, deliver: silent)
    run.engine.resume()
    run.engine.queue.sync {}
    run.run(ticks: 495, deliver: silent)
    expectEqual(warnings(run.events).count, 0, "不算暂停才 29.9 秒")
    run.run(ticks: 10, deliver: silent)
    let got = warnings(run.events)
    expectEqual(got.map(\.code), ["mic_silent"], "不算暂停 30 秒了")
    expectEqual(got.first?.message, SilenceWatch.message(name: "USB 麦克风", digital: true))
    _ = run.stop()
}

// MARK: 换麦克风

func testTimelineSplit() {
    var timeline = Timeline()
    for n in 0..<200 {
        timeline.add(index: Double(n * 480), time: 10 + Double(n) * 0.01)
    }
    // 换了设备：新设备的第一个时间戳只差 0.5 毫秒（不换设备时算同一段），也从它开始新的一段
    timeline.split()
    expect(!timeline.add(index: 96_000, time: 12.0005), "换设备不算断开")
    expectEqual(timeline.line?.startIndex, 96_000)
    expectEqual(timeline.restarts, 0)
    expectClose(timeline.time(at: 96_000) ?? 0, 12.0005, within: 1e-9, "新的一段按新设备的时间")
    expectClose(timeline.time(at: 95_520) ?? 0, 11.99, within: 1e-6, "之前的采样照旧按上一段算")
    // 之后照常：离直线 0.5 毫秒的不算断开
    expect(!timeline.add(index: 96_480, time: 12.0105 + 0.0005))
    expectEqual(timeline.line?.startIndex, 96_000)
}

func testEngineSwitchMicIsSeamless() throws {
    let old = FakeSource(kind: .mic, name: "MacBook Pro 麦克风")
    let run = try FrozenRun([old, FakeSource(kind: .system)])
    let origin: UInt64 = 5_000_000_000
    // 原来的麦克风录了 2 秒（每 10 毫秒一批，带时间戳），第 k 个采样在 origin + k/48000 采到，值是 k + 1
    for start in stride(from: 0, to: 96_000, by: 480) {
        old.push(counting(480, from: start), at: origin + nanoseconds(start))
        run.tick()
    }
    // 系统默认的换成了 AirPods：它比原来的最后一个采样早 30 毫秒（1440 个采样）开始录，换之前已经录了 50 毫秒。
    // 它的第 j 个采样和原来的第 94560 + j 个是同一个时间，值也这么编（不重不少的话录出来的就是连着的 1、2、3……）
    let new = FakeSource(kind: .mic, name: "AirPods Pro")
    let newOrigin = origin + nanoseconds(96_000) - 30_000_000
    func pushNew(_ start: Int, _ count: Int) {
        new.push(counting(count, from: 94_560 + start), at: newOrigin + nanoseconds(start))
    }
    for start in stride(from: 0, to: 2_400, by: 480) {
        pushNew(start, 480)
    }
    expectEqual(run.engine.switchMic(to: new), .switched)
    expect(old.stopped, "原来的停掉了")
    expect(run.engine.sources[0] === new)
    expectEqual(micChanges(run.events), ["AirPods Pro"])
    run.tick()
    for start in stride(from: 2_400, to: 48_000, by: 480) {
        pushNew(start, 480)
        run.tick()
    }
    let expected = counting(96_000 - 1_440 + 48_000)
    expect(run.lane == expected, "原来的 2 秒一个不少，接着是新的从原来的结束那一刻起的：\(firstDifference(run.lane, expected))")
    run.stop()
}

func testEngineSwitchMicOtherRate() throws {
    let old = FakeSource(kind: .mic)
    let run = try FrozenRun([old, FakeSource(kind: .system)])
    let origin: UInt64 = 5_000_000_000
    for start in stride(from: 0, to: 48_000, by: 480) {
        old.push(counting(480, from: start), at: origin + nanoseconds(start))
        run.tick()
    }
    // 新的是 44.1 kHz：比原来的结束早 30 毫秒开始（1323 个采样），换之前录了 20 毫秒（882 个），之后 1 秒
    let new = FakeSource(kind: .mic, sampleRate: 44_100, name: "USB 麦克风")
    let newOrigin = origin + nanoseconds(48_000) - 30_000_000
    let level: Float = 0.25
    new.push([Float](repeating: level, count: 882), at: newOrigin)
    expectEqual(run.engine.switchMic(to: new), .switched)
    for start in stride(from: 882, to: 882 + 44_100, by: 441) {
        new.push([Float](repeating: level, count: 441), at: newOrigin + nanoseconds(start, rate: 44_100))
        run.tick()
    }
    expect(Array(run.lane.prefix(48_000)) == counting(48_000), "原来的 1 秒原样")
    let rest = Array(run.lane.dropFirst(48_000))
    // 去掉重叠的 1323 个，剩下的 43659 个转成 48 kHz 约 47520 个（转换器里还压着一点点）
    expect(abs(rest.count - 47_520) < 200, "新的转出来 \(rest.count) 个")
    let middle = rest[1_000..<(rest.count - 1_000)]
    expect(middle.allSatisfy { abs($0 - level) < 0.001 }, "新的按 44.1 kHz 转换：\(middle.min() ?? 0)…\(middle.max() ?? 0)")
    run.stop()
}

func testEngineSwitchMicRefused() throws {
    let old = FakeSource(kind: .mic)
    let run = try FrozenRun([old, FakeSource(kind: .system)])
    old.push(960, value: 0.1)
    run.tick()
    // 4 kHz 处理不了：不换，接着录原来的
    let low = FakeSource(kind: .mic, sampleRate: 4_000)
    expectEqual(run.engine.switchMic(to: low), .unsupportedRate(4_000))
    expect(!old.stopped && run.engine.sources[0] === old, "接着录原来的")
    old.push(960, value: 0.1)
    run.tick()
    expectEqual(run.lane.count, 1_920)
    expectEqual(micChanges(run.events), [])
    // 只录电脑声音：没有麦克风可换
    let systemOnly = try FrozenRun([FakeSource(kind: .system)])
    expectEqual(systemOnly.engine.switchMic(to: FakeSource(kind: .mic)), .notRecording)
    systemOnly.stop()
    // 录音结束了：不换
    run.stop()
    expectEqual(run.engine.switchMic(to: FakeSource(kind: .mic)), .notRecording)
}

func testEngineSwitchMicWithEcho() throws {
    let old = FakeSource(kind: .mic, name: "MacBook Pro 麦克风")
    let system = FakeSource(kind: .system)
    let fake = FakeCanceller(latency: Int(ZR_AEC_LATENCY))
    let run = try EngineRun([old, system], echo: EchoSetup(mode: .on, makeCanceller: { fake }, route: TestRoute(.speakers)))
    guard let stage = run.engine.echoStage else {
        Harness.fail("没有建回声消除这一步")
        return
    }
    // 两路的第 k 个采样都在 origin + k/48000 采到（比交给处理线程早 50 毫秒左右）；电脑声音的值是 5000000 + k + 1
    let origin = run.clock.now - 30_000_000
    var micSent = 0
    var systemSent = 0
    let reference = {
        system.push(counting(960, from: 5_000_000 + systemSent), at: origin + nanoseconds(systemSent))
        systemSent += 960
    }
    run.run(ticks: 100) {
        old.push(counting(960, from: micSent), at: origin + nanoseconds(micSent))
        micSent += 960
        reference()
    }
    // 换成 AirPods：比原来的最后一个采样早 30 毫秒开始录，时间和值都接着原来的编（同 testEngineSwitchMicIsSeamless）
    let new = FakeSource(kind: .mic, name: "AirPods Pro")
    let newOrigin = origin + nanoseconds(micSent) - 30_000_000
    var newSent = 1_920
    new.push(counting(newSent, from: micSent - 1_440), at: newOrigin)
    expectEqual(run.engine.switchMic(to: new), .switched)
    expectEqual(fake.resets, 0, "回声消除那里还压着原来的麦克风的声音，先照旧处理")
    run.run(ticks: 100) {
        new.push(counting(960, from: micSent - 1_440 + newSent), at: newOrigin + nanoseconds(newSent))
        newSent += 960
        reference()
    }
    expectEqual(fake.resets, 1, "处理到新的麦克风的声音时 AEC3 从头开始")
    expectEqual(micChanges(run.events), ["AirPods Pro"])
    // 每一帧都配上了 170 毫秒以后的参考：换之前按原来的时间戳，换之后按新的（去掉重叠以后接得上）
    let offsets = Set(fake.pairs.filter { $0.reference != 0 }.map { Int($0.reference) - 5_000_000 - Int($0.mic) })
    expectEqual(offsets, [8_160])
    expect(fake.pairs.contains { $0.mic > Float(micSent) }, "新的麦克风的声音也做了回声消除")
    expect(stage.micTimeline.previous != nil, "原来的麦克风的那一段时间轴留着")
    guard case .success? = run.stop() else {
        Harness.fail("没有正常结束")
        return
    }
    expectEqual(stage.mic.count, 0, "结束时压着的都交出去了")
}

func testStageMicDeviceChanged() {
    // 原来的麦克风 2 秒；换成新的，新的第一个采样比原来的最后一个采样之后晚 1 毫秒：差不到 2 毫秒，
    // 不说换了麦克风的话会当成同一段，配参考差 48 个采样
    let count = 4 * 48_000
    let fake = FakeCanceller(latency: Int(ZR_AEC_LATENCY))
    var mic = SimStream(counting(count), segments: [SimStream.Segment(start: 0, time: 0), SimStream.Segment(start: 96_000, time: 2.001)])
    mic.count = 96_000
    let run = StageRun(mic: mic, reference: SimStream(counting(count + 96_000, from: 5_000_000)), canceller: fake)
    run.run()
    expectEqual(run.mic.delivered, 96_000, "原来的麦克风交完了")
    run.stage.micDeviceChanged(at: 96_000)
    run.mic.count = count
    run.run()
    run.finish()
    expect(run.output == counting(count), "进多少出多少、顺序不变：\(firstDifference(run.output, counting(count)))")
    expectEqual(fake.resets, 1, "处理到新的麦克风的第一帧时 AEC3 从头开始")
    let offsets = Set(fake.pairs.filter { $0.reference != 0 }.map { Int($0.reference) - 5_000_000 - Int($0.mic) })
    expectEqual(offsets, [8_160, 8_160 + 48], "原来的按原来的时间、新的按新的时间配参考")
    expect(run.stage.micTimeline.previous != nil, "原来的那一段留着")
    expectEqual(run.stage.micSwitches, [])
}

// MARK: 跟着系统默认的麦克风

func testFollowerFollowsDefault() throws {
    let fixture = try FollowFixture()
    let system = fixture.system
    fixture.follower.checkNow()
    expectEqual(system.attempts, [], "开始时看了一次：默认的还是它")
    expect(system.watching, "在听默认的输入设备")
    // 连上了 AirPods，系统默认的输入设备换成了它
    system.setDefault(airPods)
    fixture.follower.checkNow()
    expectEqual(system.attempts, [airPods.id])
    expectEqual(micChanges(fixture.events), ["AirPods Pro"])
    expect(fixture.mic.stopped, "原来的停掉了")
    expect(fixture.run.engine.sources[0] === system.opened.last, "现在录的是 AirPods")
    expectEqual(fixture.switched, [airPods.id], "回声消除看的输出路线也知道换了麦克风")
    // 默认的抖了一下（换成 USB 又换回 AirPods）：只在最后一下之后看，还是它，不换
    system.setDefault(usb)
    system.setDefault(airPods)
    fixture.follower.checkNow()
    expectEqual(system.attempts, [airPods.id])
    expectEqual(warnings(fixture.events).count, 0)
    // 不跟了：不再听，停掉换上来的
    fixture.follower.stop()
    expect(!system.watching)
    expect(system.opened.last?.stopped == true)
    system.setDefault(usb)
    fixture.follower.checkNow()
    expectEqual(system.attempts, [airPods.id], "不跟了")
    fixture.run.stop()
}

func testFollowerReplacesLostMic() throws {
    let fixture = try FollowFixture()
    fixture.follower.checkNow()
    // 在录的 MacBook 麦克风断开了（MicCapture 的 onLost），系统把默认的换成了 USB 麦克风
    fixture.follower.lost(builtIn)
    fixture.system.setDefault(usb)
    fixture.follower.checkNow()
    expectEqual(micChanges(fixture.events), ["USB 麦克风"])
    expectEqual(warnings(fixture.events).count, 0, "换过去了，不提醒")
    // 换掉的那个后来又报断开：不用管
    fixture.follower.lost(builtIn)
    fixture.follower.checkNow()
    expectEqual(fixture.system.attempts, [usb.id])
    fixture.follower.stop()
    fixture.run.stop()

    // 开始跟之前就断开了：开始时看，系统已经换了一个默认的就换过去
    let early = try FollowFixture(default: usb, start: false)
    early.follower.lost(builtIn)
    early.start()
    early.follower.checkNow()
    expectEqual(micChanges(early.events), ["USB 麦克风"])
    expectEqual(warnings(early.events).count, 0)
    early.follower.stop()
    early.run.stop()
}

func testFollowerWarnsWhenNothingToSwitchTo() throws {
    // 断开了等 0.3 秒还没有别的默认麦克风才提醒
    var timing = manual
    timing.lostGrace = 0.3
    let fixture = try FollowFixture(timing: timing)
    fixture.follower.checkNow()
    fixture.follower.lost(builtIn)
    fixture.system.setDefault(nil)
    fixture.follower.checkNow()
    expectEqual(warnings(fixture.events).count, 0, "刚断开，先等一会儿")
    expect(fixture.follower.hasPendingCheck, "过一会儿再看")
    expect(waitUntil(2) { !warnings(fixture.events).isEmpty }, "等了一会儿还是没有别的麦克风：提醒")
    let lostMessage = "麦克风「MacBook Pro 麦克风」断开了，现在录不到你这边的声音。接上麦克风，或者在「系统设置 → 声音 → 输入」里选一个麦克风，录音会自动换过去接着录。"
    expectEqual(warnings(fixture.events).map(\.code), ["mic_unavailable"])
    expectEqual(warnings(fixture.events).first?.message, lostMessage)
    // 默认的又变了几下（还是没有能用的）：不再提醒
    fixture.system.setDefault(builtIn)
    fixture.follower.checkNow()
    expectEqual(warnings(fixture.events).count, 1)
    // 接上了 USB 麦克风，系统把它设成了默认的：换过去
    fixture.system.setDefault(usb)
    fixture.follower.checkNow()
    expectEqual(micChanges(fixture.events), ["USB 麦克风"])
    fixture.follower.stop()
    fixture.run.stop()
}

func testFollowerRetriesThenWarns() throws {
    let fixture = try FollowFixture()
    let system = fixture.system
    fixture.follower.checkNow()
    // AirPods 刚连上，打不开：过一会儿再试（测试里由 checkNow 做），3 次都不行才提醒
    system.failing = [airPods.id]
    system.setDefault(airPods)
    fixture.follower.checkNow()
    expectEqual(warnings(fixture.events).count, 0, "第 1 次打不开，先不提醒")
    expect(fixture.follower.hasPendingCheck, "过一会儿再试")
    fixture.follower.checkNow()
    fixture.follower.checkNow()
    expectEqual(system.attempts, [airPods.id, airPods.id, airPods.id])
    expectEqual(warnings(fixture.events).map(\.message), [
        "系统默认的麦克风换成了「AirPods Pro」，可是它打不开，接着录「MacBook Pro 麦克风」。要录「AirPods Pro」的话，结束这次录音再重新开始。",
    ])
    expect(!fixture.follower.hasPendingCheck, "试够了不再试")
    expect(!fixture.mic.stopped && fixture.run.engine.sources[0] === fixture.mic, "接着录原来的")
    // 默认的又报了一次变（还是它）：不再试、不再提醒
    system.setDefault(airPods)
    fixture.follower.checkNow()
    expectEqual(system.attempts.count, 3)
    expectEqual(warnings(fixture.events).count, 1)
    // 换成了 USB 麦克风：换过去
    system.setDefault(usb)
    fixture.follower.checkNow()
    expectEqual(micChanges(fixture.events), ["USB 麦克风"])
    fixture.follower.stop()
    fixture.run.stop()

    // 第 1 次打不开，第 2 次就好了：换过去，不提醒
    let flaky = try FollowFixture()
    flaky.follower.checkNow()
    flaky.system.failing = [airPods.id]
    flaky.system.setDefault(airPods)
    flaky.follower.checkNow()
    flaky.system.failing = []
    flaky.follower.checkNow()
    expectEqual(micChanges(flaky.events), ["AirPods Pro"])
    expectEqual(warnings(flaky.events).count, 0)
    flaky.follower.stop()
    flaky.run.stop()

    // 在录的断开了，系统默认的又打不开：试够了提醒录不到了；之后换了默认的照样换过去
    let lost = try FollowFixture()
    lost.follower.checkNow()
    lost.system.failing = [airPods.id]
    lost.follower.lost(builtIn)
    lost.system.setDefault(airPods)
    for _ in 0..<3 {
        lost.follower.checkNow()
    }
    expectEqual(warnings(lost.events).map(\.message), [
        "麦克风「MacBook Pro 麦克风」断开了，系统默认的麦克风「AirPods Pro」又打不开，现在录不到你这边的声音。在「系统设置 → 声音 → 输入」里换一个麦克风，录音会自动换过去接着录。",
    ])
    lost.system.setDefault(usb)
    lost.follower.checkNow()
    expectEqual(micChanges(lost.events), ["USB 麦克风"])
    expectEqual(warnings(lost.events).count, 1)
    lost.follower.stop()
    lost.run.stop()
}

func testFollowerUnsupportedRateAndFinished() throws {
    let fixture = try FollowFixture()
    let system = fixture.system
    fixture.follower.checkNow()
    // 默认的换成了 768 kHz 的设备：处理不了，不用再试，提醒一次，接着录原来的
    system.rates[usb.id] = 768_000
    system.setDefault(usb)
    fixture.follower.checkNow()
    expectEqual(system.attempts, [usb.id])
    expectEqual(warnings(fixture.events).map(\.message), [
        "系统默认的麦克风换成了「USB 麦克风」，可是它的采样率是 768000 Hz，处理不了，接着录「MacBook Pro 麦克风」。要录「USB 麦克风」的话，到「音频 MIDI 设置」里把它改成 48000 Hz，再重新开始录音。",
    ])
    expect(system.opened.last?.stopped == true, "打开的又停掉了")
    expect(!fixture.mic.stopped, "接着录原来的")
    expect(!fixture.follower.hasPendingCheck)
    // 录音结束了：默认的再换也不换了
    fixture.run.stop()
    system.setDefault(airPods)
    fixture.follower.checkNow()
    expectEqual(micChanges(fixture.events), [])
    expect(system.opened.last?.stopped == true, "打开了也马上停掉")
    system.setDefault(builtIn)
    fixture.follower.checkNow()
    expectEqual(system.attempts, [usb.id, airPods.id], "录音结束以后不再看")
    fixture.follower.stop()
}

func testFollowerSwitchesWithinASecond() throws {
    // 真的计时（等 0.3 秒再看），现场录音的时钟照常走：原来的麦克风声音很轻，AirPods 的很响
    let system = FakeMicrophones(default: builtIn)
    let old = FakeSource(kind: .mic, name: builtIn.name)
    let computer = FakeSource(kind: .system)
    let run = try EngineRun([old, computer])
    let follower = MicFollower(system: system, events: run.events)
    follower.start(current: builtIn, capture: old, engine: run.engine) { _ in }
    run.run(ticks: 50) {
        old.push(960, value: 0.01)
        computer.push(960, value: 0)
    }
    var ticks = 50
    let changedAt = Date()
    system.setDefault(airPods)
    // 处理线程照常每 20 毫秒走一次（真的过 20 毫秒），麦克风交数据
    while micChanges(run.events).isEmpty && Date().timeIntervalSince(changedAt) < 2 {
        usleep(20_000)
        run.run(ticks: 1) {
            (system.opened.last ?? old).push(960, value: system.opened.isEmpty ? 0.01 : 0.5)
            computer.push(960, value: 0)
        }
        ticks += 1
    }
    let elapsed = Date().timeIntervalSince(changedAt)
    expectEqual(micChanges(run.events), ["AirPods Pro"])
    expect(elapsed >= 0.25 && elapsed < 1, "默认的换了以后 \(elapsed) 秒才换过去")
    guard let airPodsSource = system.opened.last else { return }
    run.run(ticks: 100) {
        airPodsSource.push(960, value: 0.5)
        computer.push(960, value: 0)
    }
    ticks += 100
    // 换了以后的音量是 AirPods 的（0.5，-6 dB，音量约 0.9），原来的只有 0.01（-40 dB，约 0.33）。
    // 换的时候混音队列里还攒着原来的麦克风的一点声音（0.15 秒左右），头几个音量里还有它
    let objects = run.events.all.compactMap { (try? JSONSerialization.jsonObject(with: Data($0.utf8))) as? [String: Any] }
    let changed = objects.firstIndex { $0["event"] as? String == "mic_changed" } ?? objects.count
    let after = objects[changed...].filter { $0["event"] as? String == "level" }.compactMap { $0["mic"] as? Double }
    expect(after.count >= 19 && after.dropFirst(4).allSatisfy { $0 > 0.85 }, "换了以后录到的是 AirPods 的声音：\(after)")
    let before = objects[..<changed].filter { $0["event"] as? String == "level" }.compactMap { $0["mic"] as? Double }
    expect(before.suffix(10).allSatisfy { $0 < 0.4 }, "换之前是原来的麦克风：\(before.suffix(10))")
    follower.stop()
    guard case .success(let recording)? = run.stop() else {
        Harness.fail("没有正常结束")
        return
    }
    // 换麦克风不影响时长：时钟走了多少就录了多少（加上结束时混出来的各路攒着的）
    let ticked = Double(ticks) * 0.02
    expect(recording.seconds >= ticked - 0.05 && recording.seconds < ticked + 0.6, "录了 \(recording.seconds) 秒，时钟走了 \(ticked) 秒")
}

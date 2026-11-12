import CoreAudio
import CRecorder
import Foundation
@testable import RecorderKit

/// 回声消除这一步（EchoStage）和它在现场录音里怎么接：用假的回声消除器（输出就是输入）看调用的顺序、
/// 对齐、采样数、状态和事件。AEC3 本身消得好不好见 EchoQualityTests.swift
let echoStageTests: [(String, () throws -> Void)] = [
    ("回声消除（AEC3）：C 接口参数不对时返回错误，不会让程序崩溃", testShimRejectsBadArguments),
    ("回声消除：时间轴按时间戳拟合，跟得上时钟差，断开了重新开始", testTimeline),
    ("回声消除：判断输出路线（外放、耳机、AirPlay）", testRouteClassifier),
    ("回声消除：每一帧麦克风配上提前 170 毫秒的参考，一帧参考一帧麦克风", testStagePairsByTime),
    ("回声消除：麦克风进多少出多少、顺序不变（交付时快时慢、成批到、卡住、电脑声音断了）", testStageConservesSamples),
    ("回声消除：电脑声音断了就不做（no_reference），麦克风不会一直等；恢复了接着做", testStageReferenceStall),
    ("回声消除：电脑声音一直比麦克风晚 0.1 秒时断了、晚开始，跟上了照样接着做；一直晚得等不到的关掉以后不来回开关",
     testStageLateReferenceRecovers),
    ("回声消除：两路时钟差 100 ppm，参考补、丢采样跟上，模拟一小时对齐一直在 1 毫秒以内", testStageDrift),
    ("回声消除：时间戳断开就重新对齐，偏了 20 毫秒以上 AEC3 从头开始", testStageRealign),
    ("回声消除：不做的时候原样通过（一个比特都不变），开、关切换时不多不少", testStageBypassIsExact),
    ("回声消除：输出路线变了要稳定 1 秒才跟着变", testStageRouteDebounce),
    ("回声消除：暂停前压着的先原样交出，暂停期间的不要，结束时全部交出", testStagePauseResumeFinish),
    ("回声消除：--aec-dump 的三个文件在暂停、继续以后也一个采样对一个采样", testStageDumpStaysAligned),
    ("回声消除：麦克风断了（不来新的了），压着的 0.5 秒以后就交出去，不等到结束", testStageMicStall),
    ("回声消除：AEC3 出错时这一帧用原声，连续出错就关掉（on 模式提醒）", testStageCancellerFailure),
    ("回声消除：建不了回声消除器时原样录麦克风，started 报 unavailable，on 模式提醒", testEngineCancellerUnavailable),
    ("回声消除：off 时不经过回声消除，麦克风那一路和以前一模一样", testEngineOffIsUnchanged),
    ("回声消除：只录麦克风时报 no_reference，不建回声消除器", testEngineWithoutReference),
    ("回声消除：现场录音里暂停、继续、结束，时间戳换算成 48 kHz 的序号", testEngineWithEcho),
]

// MARK: 测试用的回声消除器、输出路线

/// 假的回声消除器：输出就是输入（和 AEC3 一样可以晚 latency 个采样，从头开始以后里面是 0），
/// 记下调用的顺序、每次处理麦克风时配的参考
final class FakeCanceller: EchoCanceller {
    let latency: Int
    private var line: [Float]
    /// "r"：喂参考，"c"：处理麦克风
    private(set) var calls = ""
    /// 每次处理麦克风时：最近喂的参考帧的第一个采样、这一帧麦克风的第一个采样
    private(set) var pairs: [(reference: Float, mic: Float)] = []
    private(set) var resets = 0
    private(set) var captures = 0
    private var lastReference: Float = .nan
    /// 第几次处理麦克风（从 0 数）时出错
    var failCapture: (Int) -> Bool = { _ in false }
    var resetSucceeds = true

    init(latency: Int = 0) {
        self.latency = latency
        line = [Float](repeating: 0, count: latency)
    }

    func render(_ frame: UnsafePointer<Float>) -> Bool {
        calls.append("r")
        lastReference = frame[0]
        return true
    }

    func capture(_ input: UnsafePointer<Float>, into output: UnsafeMutablePointer<Float>) -> Bool {
        calls.append("c")
        pairs.append((lastReference, input[0]))
        let frame = EchoStage.frameSize
        // 晚 latency 个采样：先交里面压着的，再交这一帧的前面，这一帧的最后 latency 个压在里面
        for index in 0..<latency {
            output[index] = line[index]
        }
        for index in latency..<frame {
            output[index] = input[index - latency]
        }
        for index in 0..<latency {
            line[index] = input[frame - latency + index]
        }
        defer { captures += 1 }
        return !failCapture(captures)
    }

    func reset() -> Bool {
        resets += 1
        if resetSucceeds {
            line = [Float](repeating: 0, count: latency)
        }
        return resetSucceeds
    }

    func stats() -> EchoStats? {
        nil
    }
}

/// 测试里可以随时改的输出路线
final class TestRoute: OutputRouteSource {
    var current: OutputRouteKind

    init(_ kind: OutputRouteKind) {
        current = kind
    }

    var currentDescription: String {
        "测试的输出路线：\(current.rawValue)"
    }

    func stop() {}
}

// MARK: 模拟两路声音按时交给 EchoStage

/// 一路模拟的声音：第 i 个采样的内容，和它是什么时候采到的（秒）。
/// 时间分几段：每段从第 start 个采样开始，它的时间是 time，每秒 rate 个采样（段和段之间可以断开、跳过一段时间）
struct SimStream {
    struct Segment {
        var start: Int
        var time: Double
        var rate = 48_000.0
    }

    /// 采样的内容。很长的模拟（一小时）不存数组，全是 constant
    var samples: [Float]
    var count: Int
    var constant: Float = 0
    var segments: [Segment]
    var delivered = 0
    /// 一批（一个 IO 缓冲）最多几个采样
    var buffer = 480
    /// 这一路在这些时间段里不交数据（模拟处理线程拿不到、设备卡住）：到了时间以后一起交
    var holds: [ClosedRange<Double>] = []

    init(_ samples: [Float], segments: [Segment] = [Segment(start: 0, time: 0)]) {
        self.samples = samples
        count = samples.count
        self.segments = segments
    }

    init(count: Int, constant: Float = 0, segments: [Segment] = [Segment(start: 0, time: 0)]) {
        samples = []
        self.count = count
        self.constant = constant
        self.segments = segments
    }

    private func segment(containing index: Int) -> Int {
        var found = 0
        for (number, segment) in segments.enumerated() where segment.start <= index {
            found = number
        }
        return found
    }

    func time(_ index: Int) -> Double {
        let segment = segments[segment(containing: index)]
        return segment.time + Double(index - segment.start) / segment.rate
    }

    /// 到 limit 秒为止采到了多少个采样（下一个没采到的序号）
    func end(upTo limit: Double) -> Int {
        var end = 0
        for (number, segment) in segments.enumerated() where segment.time <= limit {
            let last = number + 1 < segments.count ? segments[number + 1].start : count
            end = min(last, segment.start + Int(((limit - segment.time) * segment.rate).rounded(.down)) + 1)
        }
        return min(end, count)
    }

    /// 从 start 起这一批到哪里为止：不超过 end、buffer 个，也不跨段
    func batchEnd(from start: Int, end: Int) -> Int {
        let number = segment(containing: start)
        let segmentEnd = number + 1 < segments.count ? segments[number + 1].start : count
        return min(end, start + buffer, segmentEnd)
    }
}

/// 按模拟的时钟每 20 毫秒交一次数据、处理一次，收集麦克风那一路的输出
final class StageRun {
    let stage: EchoStage
    let events = RecordedEvents()
    var mic: SimStream
    var reference: SimStream?
    var lane = SampleFIFO()
    var output: [Float] = []
    var now = 0.0
    var tick = 0.02
    var micLatency = 0.01
    var referenceLatency = 0.015
    /// 每次 tick 的间隔上下随机抖动这么多秒
    var tickJitter = 0.0
    /// 麦克风在 EchoStage 里最多压了多少个采样
    private(set) var maxHeld = 0
    /// 不留输出（很长的模拟），只数个数
    var keepOutput = true
    private(set) var outputCount = 0
    private var random = SplitMix(seed: 42)
    private var constantBatch: [Float] = []

    init(mic: SimStream, reference: SimStream?, canceller: EchoCanceller, mode: EchoCancelMode = .on,
         lead: Int = 170, route: OutputRouteSource? = nil, dump: EchoDump? = nil,
         configure: (inout EchoStage.Configuration) -> Void = { _ in }) {
        self.mic = mic
        self.reference = reference
        var configuration = EchoStage.Configuration(mode: mode, leadMilliseconds: lead)
        configure(&configuration)
        stage = EchoStage(configuration: configuration, canceller: canceller, route: route, events: events, dump: dump, now: 0)
        stage.begin(now: 0)
    }

    /// 走到 seconds 秒，或者麦克风的数据都交完了
    func run(until seconds: Double = .infinity) {
        while now < seconds && mic.delivered < mic.count {
            step()
        }
    }

    func step() {
        now += tick + (tickJitter > 0 ? Double(random.uniform()) * tickJitter : 0)
        deliver(&mic, .mic, upTo: now - micLatency)
        if var stream = reference {
            deliver(&stream, .system, upTo: now - referenceLatency)
            reference = stream
        }
        stage.process(now: now, into: &lane)
        maxHeld = max(maxHeld, stage.mic.count)
        collect()
    }

    /// 结束：剩下的都交出去
    func finish() {
        stage.finish(now: now, into: &lane)
        collect()
    }

    func collect() {
        outputCount += lane.count
        if keepOutput {
            lane.withUnsafeSamples { output.append(contentsOf: $0) }
        }
        lane.removeAll()
    }

    private func deliver(_ stream: inout SimStream, _ kind: SourceKind, upTo limit: Double) {
        if stream.holds.contains(where: { $0.contains(now) }) { return }
        let end = stream.end(upTo: limit)
        guard end > stream.delivered else { return }
        while stream.delivered < end {
            let start = stream.delivered
            let batchEnd = stream.batchEnd(from: start, end: end)
            let count = batchEnd - start
            if stream.samples.isEmpty {
                if constantBatch.count != count {
                    constantBatch = [Float](repeating: stream.constant, count: count)
                }
                constantBatch.withUnsafeBufferPointer { append($0, kind) }
            } else {
                stream.samples.withUnsafeBufferPointer { append(UnsafeBufferPointer(rebasing: $0[start..<batchEnd]), kind) }
            }
            stage.addStamps(kind, [(index: Double(start), time: stream.time(start))], now: now)
            stream.delivered = batchEnd
        }
        stage.noteArrival(kind, now: now)
    }

    private func append(_ samples: UnsafeBufferPointer<Float>, _ kind: SourceKind) {
        switch kind {
        case .mic: stage.appendMic(samples, now: now)
        case .system: stage.appendReference(samples, now: now)
        }
    }
}

/// 可重复的随机数（SplitMix64）
struct SplitMix {
    var state: UInt64

    init(seed: UInt64) {
        state = seed
    }

    mutating func next() -> UInt64 {
        state &+= 0x9E37_79B9_7F4A_7C15
        var z = state
        z = (z ^ (z >> 30)) &* 0xBF58_476D_1CE4_E5B9
        z = (z ^ (z >> 27)) &* 0x94D0_49BB_1331_11EB
        return z ^ (z >> 31)
    }

    /// 0…1
    mutating func uniform() -> Float {
        Float(next() >> 40) / Float(1 << 24)
    }
}

/// 采样的值就是它的序号（从 1 开始），看得出哪个采样到了哪里
private func numbered(_ count: Int, offset: Int = 0) -> [Float] {
    (0..<count).map { Float($0 + offset + 1) }
}

// MARK: C 接口、时间轴、输出路线

func testShimRejectsBadArguments() {
    expect(zr_aec_create(nil) == nil, "没有配置")
    for (filters, delay) in [(0, -1), (17, -1), (6, -2), (6, 501)] {
        var config = zr_aec_config(num_filters: Int32(filters), stream_delay_ms: Int32(delay))
        expect(zr_aec_create(&config) == nil, "配置不对：\(filters) 个滤波器，延迟 \(delay)")
    }
    var config = zr_aec_config(num_filters: 6, stream_delay_ms: -1)
    guard let aec = zr_aec_create(&config) else {
        Harness.fail("建不了回声消除器")
        return
    }
    defer { zr_aec_destroy(aec) }
    zr_aec_destroy(nil)
    var frame = [Float](repeating: 0.1, count: 481)
    var output = [Float](repeating: 0, count: 481)
    expectEqual(zr_aec_render(aec, frame, 479), Int32(ZR_AEC_BAD_ARGUMENT), "不是 480 个采样")
    expectEqual(zr_aec_render(aec, nil, 480), Int32(ZR_AEC_BAD_ARGUMENT), "空指针")
    expectEqual(zr_aec_render(nil, frame, 480), Int32(ZR_AEC_BAD_ARGUMENT))
    expectEqual(zr_aec_capture(aec, frame, &output, 481), Int32(ZR_AEC_BAD_ARGUMENT))
    expectEqual(zr_aec_capture(aec, frame, nil, 480), Int32(ZR_AEC_BAD_ARGUMENT))
    expectEqual(zr_aec_capture(aec, nil, &output, 480), Int32(ZR_AEC_BAD_ARGUMENT))
    expectEqual(zr_aec_reset(nil), Int32(ZR_AEC_BAD_ARGUMENT))
    expectEqual(zr_aec_get_stats(aec, nil), Int32(ZR_AEC_BAD_ARGUMENT))
    // 还没处理过：统计都没有（NaN）
    var stats = zr_aec_stats()
    expectEqual(zr_aec_get_stats(aec, &stats), Int32(ZR_AEC_OK))
    expect(stats.erle_db.isNaN && stats.delay_ms.isNaN, "还没有统计")
    // 不是有限的数、超出 -1…1 的：照样处理，出来的都是有限的数，不会把滤波器弄坏
    frame[0] = .nan
    frame[1] = .infinity
    frame[2] = -40
    for _ in 0..<50 {
        expectEqual(zr_aec_render(aec, frame, 480), Int32(ZR_AEC_OK))
        expectEqual(zr_aec_capture(aec, frame, &output, 480), Int32(ZR_AEC_OK))
    }
    expect(output[0..<480].allSatisfy(\.isFinite), "输出都是有限的数")
    // 输入和输出可以是同一块内存
    var inPlace = [Float](repeating: 0.01, count: 480)
    let status = inPlace.withUnsafeMutableBufferPointer { zr_aec_capture(aec, $0.baseAddress!, $0.baseAddress!, 480) }
    expectEqual(status, Int32(ZR_AEC_OK))
    expectEqual(zr_aec_reset(aec), Int32(ZR_AEC_OK))
    expectEqual(zr_aec_capture(aec, frame, &output, 480), Int32(ZR_AEC_OK), "从头开始以后照样能用")
    // Swift 那层也一样：参数不对时建不起来
    expect(WebRTCEchoCanceller(filters: 0) == nil)
    expect(WebRTCEchoCanceller(filters: 6, streamDelayMilliseconds: 600) == nil)
    expect(WebRTCEchoCanceller(filters: 6, streamDelayMilliseconds: 230) != nil)
}

func testTimeline() {
    // 设备的时钟比主机快 100 ppm，时间戳每 10 毫秒一个，带 ±20 微秒的抖动
    var timeline = Timeline()
    var random = SplitMix(seed: 7)
    let rate = 48_000 * (1 + 100e-6)
    for n in 0..<3_000 {
        let index = Double(n * 480)
        let jitter = Double(random.uniform() - 0.5) * 40e-6
        expect(!timeline.add(index: index, time: 100 + index / rate + jitter), "第 \(n) 个时间戳不该算断开")
    }
    expectClose(timeline.rate, rate, within: 0.5, "测得的采样率")
    // 拟合出来的时间和真的差不到 0.05 毫秒
    let probe = 3_000.0 * 480
    expectClose(timeline.time(at: probe) ?? 0, 100 + probe / rate, within: 5e-5)
    expectClose(timeline.index(at: 100 + probe / rate) ?? 0, probe, within: 3)
    // 丢了 50 毫秒（序号接着，时间跳了）：从这里重新开始一段，采样率接着用
    let gapIndex = 3_001.0 * 480
    expect(timeline.add(index: gapIndex, time: 100 + gapIndex / rate + 0.05), "跳了 50 毫秒")
    expectEqual(timeline.restarts, 1)
    expectClose(timeline.lastJump, 0.05, within: 1e-4)
    expectClose(timeline.time(at: gapIndex) ?? 0, 100 + gapIndex / rate + 0.05, within: 1e-9)
    expectClose(timeline.rate, rate, within: 0.5, "断开以后采样率还是测出来的")
    // 时钟差得离谱（1%）不信，按 48 kHz 算
    var wild = Timeline()
    for n in 0..<200 {
        wild.add(index: Double(n * 480), time: Double(n * 480) / (48_000 * 1.01))
    }
    expectClose(wild.rate, 48_000, within: 0.001)
    // 没有时间戳的来源：按收到的时间对齐，差 0.1 秒以上才重新对齐
    var arrival = Timeline()
    expect(arrival.index(at: 1) == nil, "还没对上")
    arrival.addArrival(endIndex: 960, time: 10)
    expectClose(arrival.time(at: 0) ?? 0, 10 - 0.02, within: 1e-9)
    expect(!arrival.addArrival(endIndex: 1_920, time: 10.05), "抖了 30 毫秒不算")
    expect(arrival.addArrival(endIndex: 2_880, time: 10.5), "差了 0.44 秒，重新对齐")
    arrival.reset()
    expect(arrival.time(at: 0) == nil, "从头来")
}

func testRouteClassifier() {
    func kind(_ info: OutputDeviceInfo?) -> OutputRouteKind {
        OutputRouteClassifier.classify(info).kind
    }
    let code = OutputRouteClassifier.fourCharCode
    expectEqual(kind(nil), .speakers, "读不到就当外放")
    expectEqual(kind(OutputDeviceInfo(name: "MacBook Pro扬声器", transport: kAudioDeviceTransportTypeBuiltIn,
                                      terminalTypes: [kAudioStreamTerminalTypeSpeaker])), .speakers)
    expectEqual(kind(OutputDeviceInfo(name: "外置耳机", transport: kAudioDeviceTransportTypeBuiltIn)), .headphones, "苹果芯片的耳机孔")
    expectEqual(kind(OutputDeviceInfo(name: "Built-in Output", transport: kAudioDeviceTransportTypeBuiltIn,
                                      dataSource: code("hdpn"))), .headphones, "Intel 的 Mac 插着耳机")
    expectEqual(kind(OutputDeviceInfo(name: "Built-in Output", transport: kAudioDeviceTransportTypeBuiltIn,
                                      dataSource: code("ispk"))), .speakers)
    expectEqual(kind(OutputDeviceInfo(name: "X", transport: kAudioDeviceTransportTypeUSB,
                                      terminalTypes: [kAudioStreamTerminalTypeHeadphones])), .headphones)
    expectEqual(kind(OutputDeviceInfo(name: "USB Audio", transport: kAudioDeviceTransportTypeUSB, terminalTypes: [0x0402])), .headphones,
                "USB 耳麦的终端类型")
    expectEqual(kind(OutputDeviceInfo(name: "小明的AirPods Pro", transport: kAudioDeviceTransportTypeBluetooth)), .headphones)
    expectEqual(kind(OutputDeviceInfo(name: "WH-1000XM5", transport: kAudioDeviceTransportTypeBluetooth)), .headphones, "蓝牙当耳机")
    expectEqual(kind(OutputDeviceInfo(name: "JBL Speaker", transport: kAudioDeviceTransportTypeBluetoothLE)), .speakers, "名字是音箱")
    expectEqual(kind(OutputDeviceInfo(name: "客厅", transport: kAudioDeviceTransportTypeAirPlay)), .airplay)
    expectEqual(kind(OutputDeviceInfo(name: "Jabra Evolve 20", transport: kAudioDeviceTransportTypeUSB, isMicDevice: true)), .headphones,
                "和麦克风是同一个设备：耳麦")
    expectEqual(kind(OutputDeviceInfo(name: "Studio Display", transport: kAudioDeviceTransportTypeUSB, isMicDevice: true)), .speakers,
                "显示器的扬声器和麦克风在一个设备里也是外放")
    expectEqual(kind(OutputDeviceInfo(name: "DELL U2723QE", transport: kAudioDeviceTransportTypeHDMI)), .speakers)
    expectEqual(kind(OutputDeviceInfo(name: "Multi-Output Device", transport: kAudioDeviceTransportTypeAggregate)), .speakers, "认不出的当外放")
}

// MARK: EchoStage

func testStagePairsByTime() {
    let seconds = 4
    let fake = FakeCanceller()
    let run = StageRun(mic: SimStream(numbered(seconds * 48_000)),
                       reference: SimStream(numbered(seconds * 48_000 + 48_000, offset: 1_000_000)), canceller: fake)
    run.run()
    run.finish()
    expectEqual(run.output, numbered(seconds * 48_000), "麦克风原样（假的回声消除器）、一个不少")
    expectEqual(run.events.all, [#"{"event":"echo","active":true,"reason":"forced"}"#])
    // 一帧参考一帧麦克风，参考在前
    expect(fake.calls.count > 300 && fake.calls == String(repeating: "rc", count: fake.calls.count / 2), "调用顺序：\(fake.calls.prefix(20))")
    // 麦克风从第 k 个采样开始的一帧，配的参考从第 k + 8160 个（170 毫秒）开始（结束时参考还没到的几帧喂的是 0，不算）
    let offsets = Set(fake.pairs.filter { $0.reference != 0 }.map { Int($0.reference) - 1_000_000 - Int($0.mic) })
    expectEqual(offsets, [8_160])
    // 麦克风在这里最多压着：170 毫秒 + 参考比麦克风晚到的 5 毫秒 + 一次 tick 的 20 毫秒 + 一帧
    expect(run.maxHeld <= 8_160 + 240 + 960 + 480, "麦克风压了 \(run.maxHeld) 个采样")
    expectEqual(run.stage.counters.slips, 0)
    expectEqual(run.stage.counters.realigns, 0)
}

func testStageConservesSamples() {
    // 假的回声消除器不晚，和像 AEC3 一样晚 431 个采样，各跑一遍
    for latency in [0, Int(ZR_AEC_LATENCY)] {
        conserveSamples(latency: latency)
    }
}

private func conserveSamples(latency: Int) {
    let count = 20 * 48_000
    let fake = FakeCanceller(latency: latency)
    var mic = SimStream(numbered(count))
    mic.buffer = 4_096  // 成批地到（有的 USB 设备、聚合设备一次给 85 毫秒）
    mic.holds = [3.0...3.5, 9.2...9.25]
    var reference = SimStream(numbered(count + 48_000, offset: 5_000_000))
    reference.holds = [6.0...7.0]  // 电脑声音停了 1 秒，之后一起到
    // 再断一次：12 秒起断了 2 秒（序号接着，时间跳过 2 秒）
    reference.segments = [SimStream.Segment(start: 0, time: 0), SimStream.Segment(start: 12 * 48_000, time: 14)]
    let run = StageRun(mic: mic, reference: reference, canceller: fake)
    run.tickJitter = 0.03
    run.run()
    run.finish()
    expectEqual(run.output.count, count, "进多少出多少（AEC3 晚 \(latency) 个采样）")
    expect(run.output == numbered(count), "顺序不变、一个都不错（AEC3 晚 \(latency) 个采样）")
    let counters = run.stage.counters
    expectEqual(counters.processed + counters.bypassed, count / 480, "每一帧不是消除了就是原样通过")
    expect(counters.processed > 1_000 && counters.bypassed > 100, "有消除的也有原样通过的：\(counters)")
    // 麦克风压着的不超过：提前量 + 最多再等 0.25 秒 + 两次 tick（抖动）+ 一批数据
    expect(run.maxHeld <= 8_160 + 12_000 + 2 * 2_400 + 4_096 + 480, "麦克风压了 \(run.maxHeld) 个采样")
}

func testStageReferenceStall() {
    let count = 12 * 48_000
    let fake = FakeCanceller()
    var reference = SimStream(numbered(count + 48_000, offset: 5_000_000))
    // 4 秒到 6 秒电脑声音断了：这两秒的采样没有（序号接着，时间跳过去）
    reference.segments = [SimStream.Segment(start: 0, time: 0), SimStream.Segment(start: 4 * 48_000, time: 6)]
    let run = StageRun(mic: SimStream(numbered(count)), reference: reference, canceller: fake)
    run.run()
    run.finish()
    expectEqual(run.output, numbered(count))
    expectEqual(run.events.all, [
        #"{"event":"echo","active":true,"reason":"forced"}"#,
        #"{"event":"echo","active":false,"reason":"no_reference"}"#,
        #"{"event":"echo","active":true,"reason":"forced"}"#,
    ])
    let counters = run.stage.counters
    expectEqual(counters.stalls, 1)
    // 断的时候麦克风原样通过，最多多等 0.25 秒
    expect(run.maxHeld <= 8_160 + 12_000 + 960 + 960, "麦克风压了 \(run.maxHeld) 个采样")
    // 断开前、恢复后都按时间对齐：恢复后配的参考还是提前 170 毫秒（序号差了断掉的 2 秒）
    let offsets = Set(fake.pairs.filter { $0.reference != 0 }.map { Int($0.reference) - 5_000_000 - Int($0.mic) })
    expectEqual(offsets, [8_160, 8_160 - 96_000])
    expect(fake.resets >= 1, "恢复时 AEC3 从头开始")
}

func testStageLateReferenceRecovers() {
    // 电脑声音一直比麦克风晚 0.1 秒交来（输出的缓冲大，比如蓝牙音箱）：做回声消除时麦克风多等一会儿就等得到。
    // 不做的时候麦克风只压 lead + bypassExtra（0.24 秒）就交出去，断了以后跟上了没有不能看那时参考到没到，
    // 不然这样晚的电脑声音一断，这场录音后面就再也不做了
    let on = #"{"event":"echo","active":true,"reason":"forced"}"#
    let off = #"{"event":"echo","active":false,"reason":"no_reference"}"#
    let count = 30 * 48_000
    let fake = FakeCanceller()
    // 10 秒时交不过来 0.5 秒（之后一起到）；20 秒时设备停了 0.5 秒（这段时间的采样没有，序号接着、时间跳过去）
    var reference = SimStream(numbered(count + 96_000, offset: 5_000_000),
                              segments: [SimStream.Segment(start: 0, time: 0), SimStream.Segment(start: 20 * 48_000, time: 20.5)])
    reference.holds = [10.0...10.5]
    let run = StageRun(mic: SimStream(numbered(count)), reference: reference, canceller: fake)
    run.referenceLatency = run.micLatency + 0.1
    run.run()
    run.finish()
    expectEqual(run.output, numbered(count), "进多少出多少")
    expectEqual(run.events.all, [on, off, on, off, on], "两次断了都接着做")
    let counters = run.stage.counters
    expectEqual(counters.stalls, 2)
    // 每次断了不到一秒不做（断 0.5 秒，0.3 秒没来才算断了，之前压着的那些帧也等不到参考）
    expect(counters.processed >= count / 480 - 200, "做了回声消除的帧太少：\(counters)")
    // 恢复以后照样按时间配参考（设备停过的那次，序号差了停掉的 0.5 秒）
    let offsets = Set(fake.pairs.filter { $0.reference != 0 }.map { Int($0.reference) - 5_000_000 - Int($0.mic) })
    expectEqual(offsets, [8_160, 8_160 - 24_000])

    // 电脑声音 2 秒才开始来（过了刚开始录时等它的 1.5 秒），也晚 0.1 秒：先不做，来了就做
    let lateStart = StageRun(mic: SimStream(numbered(10 * 48_000)),
                             reference: SimStream(numbered(10 * 48_000, offset: 5_000_000), segments: [SimStream.Segment(start: 0, time: 2)]),
                             canceller: FakeCanceller())
    lateStart.referenceLatency = lateStart.micLatency + 0.1
    lateStart.run()
    lateStart.finish()
    expectEqual(lateStart.events.all, [on, off, on])
    expectEqual(lateStart.output, numbered(10 * 48_000))
    expect(lateStart.stage.counters.processed >= 10 * 100 - 260, "来了以后都在做：\(lateStart.stage.counters)")

    // 一直晚 0.3 秒（做的时候最多等 0.25 秒，也等不到）：关掉以后不来回开、关。麦克风卡了一下（交不过来，之后
    // 一下子都到了）的时候也不开：那时压着的最前面那一帧早，参考已经到了，看起来像是跟上了
    var stuttering = SimStream(numbered(count))
    stuttering.holds = [10.0...10.3, 20.0...20.45]
    let behind = StageRun(mic: stuttering, reference: SimStream(numbered(count + 96_000, offset: 5_000_000)), canceller: FakeCanceller())
    behind.referenceLatency = behind.micLatency + 0.3
    behind.run()
    behind.finish()
    expectEqual(behind.events.all, [on, off])
    expectEqual(behind.output, numbered(count))
    expectEqual(behind.stage.counters.stalls, 1)
}

func testStageDrift() {
    // 电脑声音的时钟比麦克风快 100 ppm：同样一秒，它多出 4.8 个采样。模拟一个小时（只看对齐，不处理声音）
    let ppm = 100e-6
    let count = 3_600 * 48_000
    let fake = FakeCanceller()
    var mic = SimStream(count: count)
    mic.buffer = 4_800
    var reference = SimStream(count: Int(Double(count) * (1 + ppm)) + 96_000, segments: [SimStream.Segment(start: 0, time: 0, rate: 48_000 * (1 + ppm))])
    reference.buffer = 4_800
    // 统计日志一分钟一行，模拟一小时太吵，不写
    let run = StageRun(mic: mic, reference: reference, canceller: fake) { $0.statsInterval = .infinity }
    run.keepOutput = false
    run.tick = 0.5  // 一次走 0.5 秒，模拟得快一些
    run.run()
    run.finish()
    let counters = run.stage.counters
    // 一小时多出 17280 个采样，每次丢一个
    expectClose(Double(counters.slips), Double(count) * ppm, within: Double(count) * ppm * 0.05, "补、丢的采样数")
    expectEqual(counters.realigns, 0, "不用重新对齐")
    expect(counters.bypassed <= 20, "只有结束时参考还没到的那 170 毫秒原样通过：\(counters.bypassed) 帧")
    expectEqual(run.outputCount, count)
    expectClose(run.stage.referenceTimeline.rate, 48_000 * (1 + ppm), within: 0.1, "测出了电脑声音的时钟")
    // 对齐的误差：结束前最后喂的参考和该喂的差不到 1 毫秒（48 个采样）
    guard let worst = run.stage.counters.maxAlignmentError else {
        Harness.fail("没有对齐的信息")
        return
    }
    expect(worst < 3, "最多偏了 \(worst) 个采样")
}

func testStageRealign() {
    // 麦克风 5 秒时丢了 10 毫秒（时间跳了，序号接着）：小于 20 毫秒，重新对齐但不用从头开始；
    // 8 秒时又丢了 100 毫秒：AEC3 从头开始
    let count = 12 * 48_000
    let fake = FakeCanceller()
    let mic = SimStream(numbered(count), segments: [SimStream.Segment(start: 0, time: 0), SimStream.Segment(start: 5 * 48_000, time: 5.01),
                                                    SimStream.Segment(start: 8 * 48_000, time: 8.11)])
    let run = StageRun(mic: mic, reference: SimStream(numbered(count + 96_000, offset: 5_000_000)), canceller: fake)
    run.run()
    run.finish()
    expectEqual(run.output, numbered(count))
    expectEqual(run.stage.counters.realigns, 2)
    expectEqual(run.stage.micTimeline.restarts, 2)
    expectEqual(fake.resets, 1, "只有偏了 100 毫秒的那次从头开始")
    // 每一段里都是按时间配的参考（结束时最后几帧参考还没到，喂的是 0，不算）
    let offsets = Set(fake.pairs.filter { $0.reference != 0 }.map { Int($0.reference) - 5_000_000 - Int($0.mic) })
    expectEqual(offsets, [8_160, 8_160 + 480, 8_160 + 5_280])
}

func testStageBypassIsExact() {
    // 真的声音（带小数），戴着耳机：auto 模式不做，原样通过，一个比特都不变
    var random = SplitMix(seed: 3)
    let count = 10 * 48_000
    let samples = (0..<count).map { _ in random.uniform() * 0.6 - 0.3 }
    let route = TestRoute(.headphones)
    let fake = FakeCanceller()
    let run = StageRun(mic: SimStream(samples), reference: SimStream(numbered(count + 48_000)), canceller: fake, mode: .auto, route: route)
    run.run()
    run.finish()
    expect(run.output == samples, "原样通过")
    expectEqual(fake.calls, "", "没有调用回声消除器")
    expectEqual(run.events.all, [#"{"event":"echo","active":false,"reason":"headphones"}"#])

    // 开、关来回切换（每 2 秒换一次输出路线）：麦克风的采样一个不多一个不少，顺序不变
    let switching = TestRoute(.speakers)
    let mic = numbered(count)
    let toggled = StageRun(mic: SimStream(mic), reference: SimStream(numbered(count + 48_000, offset: 5_000_000)),
                           canceller: FakeCanceller(latency: Int(ZR_AEC_LATENCY)), mode: .auto, route: switching)
    while toggled.mic.delivered < toggled.mic.count {
        switching.current = Int(toggled.now / 2) % 2 == 0 ? .speakers : .headphones
        toggled.step()
    }
    toggled.finish()
    expect(toggled.output == mic, "切换时不多不少")
    expect(toggled.events.all.count >= 4, "切换了几次：\(toggled.events.all)")
}

func testStageRouteDebounce() {
    let route = TestRoute(.speakers)
    let count = 8 * 48_000
    let run = StageRun(mic: SimStream(numbered(count)), reference: SimStream(numbered(count + 48_000)), canceller: FakeCanceller(),
                       mode: .auto, route: route)
    run.run(until: 1)
    route.current = .headphones  // 抖了 0.5 秒（插拔的时候）
    run.run(until: 1.5)
    route.current = .speakers
    run.run(until: 3)
    expectEqual(run.events.all, [#"{"event":"echo","active":true,"reason":"speakers"}"#], "0.5 秒的抖动不算")
    route.current = .airplay
    run.run(until: 3.9)
    expectEqual(run.events.all.count, 1, "还没稳定 1 秒")
    run.run(until: 4.2)
    expectEqual(run.events.all.last, #"{"event":"echo","active":false,"reason":"airplay"}"#)
    route.current = .speakers
    run.run()
    run.finish()
    expectEqual(run.events.all.last, #"{"event":"echo","active":true,"reason":"speakers"}"#)
    expectEqual(run.output, numbered(count))
}

func testStagePauseResumeFinish() {
    // 0 到 2 秒：暂停前；2 到 3 秒：暂停期间；3 到 5 秒：继续以后
    let count = 5 * 48_000
    let fake = FakeCanceller(latency: Int(ZR_AEC_LATENCY))
    let run = StageRun(mic: SimStream(numbered(count)), reference: SimStream(numbered(count + 48_000, offset: 5_000_000)), canceller: fake)
    run.run(until: 2.01)
    // 点暂停：已经交到 EchoStage 的麦克风采样（2 秒以前采到的）都要编进去，压着的先原样交出
    let beforePause = run.mic.delivered
    run.stage.previewHeld(into: &run.lane)
    run.collect()
    expect(run.output == numbered(beforePause), "暂停前采到的一个不少：\(run.output.count) / \(beforePause)")
    let pausedAt = run.output.count
    // 暂停期间照常处理（AEC3 一直学着），交出来的 TimeMixer 会丢掉（这里直接扔掉）
    run.run(until: 3.01)
    run.output.removeSubrange(pausedAt...)
    // 点继续：压着的（暂停期间采到的）处理完也不要，各路攒着的也清掉
    let beforeResume = run.mic.delivered
    run.stage.discardHeld()
    run.run()
    run.finish()
    let after = Array(run.output[pausedAt...])
    expect(after == Array(numbered(count)[beforeResume...]), "继续以后只有点了继续以后采到的：\(after.first ?? -1) 起，\(after.count) 个")
    expectEqual(fake.calls, String(repeating: "rc", count: fake.calls.count / 2), "暂停期间也一直在处理，一帧参考一帧麦克风")
    expectEqual(run.stage.skipOutput, 0)
}

func testStageDumpStaysAligned() throws {
    // 真机上量消掉了多少靠 --aec-dump 的三个文件一个采样对一个采样。暂停、继续以后（包括压着的还没处理完就又暂停、
    // 又继续）也要对得上：录音里不要的那些，mic-out 里照样写处理出来的。假的回声消除器原样交出（像 AEC3 一样晚
    // 431 个采样，EchoStage 补齐），mic-out 应该和 mic-in 一模一样。采样的值是序号乘 2 的 -22 次方（在 -1…1 以内，
    // Float 里是精确的），写进 WAV、读出来不走样
    let count = 8 * 48_000
    let scale = Float(1) / 4_194_304
    let mic = (0..<count).map { Float($0 + 1) * scale }
    let directory = try temporaryDirectory("aec-dump")
    autoreleasepool {
        guard let dump = EchoDump(directory: directory.path) else {
            Harness.fail("建不了调试用的录音文件")
            return
        }
        let run = StageRun(mic: SimStream(mic), reference: SimStream(numbered(count + 48_000).map { $0 * scale }),
                           canceller: FakeCanceller(latency: Int(ZR_AEC_LATENCY)), dump: dump)
        run.run(until: 2.01)
        run.stage.previewHeld(into: &run.lane)  // 暂停
        run.run(until: 3.01)
        run.stage.discardHeld()  // 继续
        run.run(until: 3.5)
        // 暂停了马上继续，再马上暂停：压着的都还没处理
        run.stage.previewHeld(into: &run.lane)
        run.stage.discardHeld()
        run.stage.previewHeld(into: &run.lane)
        run.run(until: 4.6)
        run.stage.discardHeld()
        run.run()
        run.finish()
    }
    let micIn = try readWAV(directory.appendingPathComponent("mic-in.wav"))
    let reference = try readWAV(directory.appendingPathComponent("reference.wav"))
    let micOut = try readWAV(directory.appendingPathComponent("mic-out.wav"))
    expectEqual(micIn.count, count)
    expectEqual(reference.count, count)
    expectEqual(micOut.count, count)
    expect(micIn == mic, "mic-in 是送进去的麦克风原声")
    let mismatch = (0..<min(micIn.count, micOut.count)).first { micIn[$0] != micOut[$0] }
    expect(mismatch == nil, "mic-out 和 mic-in 从第 \(mismatch ?? -1) 个采样起对不上了")
}

func testStageMicStall() {
    // 麦克风只有 3 秒（之后断开了），电脑声音一直有
    let fake = FakeCanceller(latency: Int(ZR_AEC_LATENCY))
    let run = StageRun(mic: SimStream(numbered(3 * 48_000)), reference: SimStream(numbered(10 * 48_000)), canceller: fake)
    run.run()
    let delivered = run.now
    // 麦克风不来了，处理线程照常每 20 毫秒走一次
    while run.now < delivered + 0.6 {
        run.step()
    }
    expectEqual(run.stage.mic.count, 0, "压着的都处理了")
    expectEqual(run.output.count, 3 * 48_000 - Int(ZR_AEC_LATENCY), "除了 AEC3 里压着的那几个（结束时用原声交出），都交出去了")
    run.finish()
    expectEqual(run.output, numbered(3 * 48_000))
}

func testStageCancellerFailure() {
    // auto 模式：偶尔出错一次，这一帧用原声，从头开始再试，接着做
    let count = 6 * 48_000
    let flaky = FakeCanceller(latency: Int(ZR_AEC_LATENCY))
    flaky.failCapture = { $0 == 100 }
    let run = StageRun(mic: SimStream(numbered(count)), reference: SimStream(numbered(count + 48_000)), canceller: flaky, mode: .auto,
                       route: TestRoute(.speakers))
    run.run()
    run.finish()
    expectEqual(run.output, numbered(count), "出错的那一帧用的是原声")
    expectEqual(run.stage.counters.failures, 1)
    expectEqual(flaky.resets, 1)
    expectEqual(run.events.all, [#"{"event":"echo","active":true,"reason":"speakers"}"#], "auto 模式偶尔出错不提醒")

    // on 模式：连续出错，关掉、提醒，录音照常
    let broken = FakeCanceller(latency: Int(ZR_AEC_LATENCY))
    broken.failCapture = { $0 >= 50 }
    let forced = StageRun(mic: SimStream(numbered(count)), reference: SimStream(numbered(count + 48_000)), canceller: broken)
    forced.run()
    forced.finish()
    expectEqual(forced.output, numbered(count), "一个采样都不少")
    expectEqual(forced.events.all, [
        #"{"event":"echo","active":true,"reason":"forced"}"#,
        #"{"event":"warning","code":"echo_unavailable","message":"\#(EchoMessages.failedDuringRecording)"}"#,
        #"{"event":"echo","active":false,"reason":"off"}"#,
    ])
    expectEqual(broken.captures, 53, "连续出错 3 次就不再调用")

    // 出错以后连从头开始都不行：马上关掉
    let stuck = FakeCanceller()
    stuck.failCapture = { $0 >= 10 }
    stuck.resetSucceeds = false
    let hopeless = StageRun(mic: SimStream(numbered(count)), reference: SimStream(numbered(count + 48_000)), canceller: stuck, mode: .auto,
                            route: TestRoute(.speakers))
    hopeless.run()
    hopeless.finish()
    expectEqual(hopeless.output, numbered(count))
    expectEqual(stuck.captures, 11)
    expectEqual(hopeless.events.all.last, #"{"event":"echo","active":false,"reason":"off"}"#, "auto 模式不提醒，只报状态")
}

// MARK: 接在现场录音里

/// 现场录音（LiveEngine）加回声消除，两路都是假的、带时间戳的来源
private final class EchoEngineFixture {
    let mic = FakeSource(kind: .mic)
    let system = FakeSource(kind: .system)
    let clock = ManualClock()
    let events = RecordedEvents()
    let pipeline: RecordingPipeline
    let engine: LiveEngine
    var fake: FakeCanceller?
    private var micSent = 0
    private var systemSent = 0

    init(echo: EchoCancelMode, canceller: FakeCanceller? = FakeCanceller(), sources: [SourceKind] = [.mic, .system]) throws {
        let directory = try temporaryDirectory("live-echo")
        pipeline = RecordingPipeline(encoder: try AACEncoder(),
                                     chunks: ChunkWriter(directory: directory, startSeq: 0, chunkSeconds: 10), events: events)
        fake = canceller
        let setup = echo == .off ? nil : EchoSetup(mode: echo, makeCanceller: { canceller }, route: TestRoute(.speakers))
        let clock = clock
        let (mic, system) = (mic, system)
        let chosen: [CaptureSource] = sources.map { $0 == .mic ? mic : system }
        engine = try LiveEngine(sources: chosen, pipeline: pipeline, events: events, clock: { clock.now }, echo: setup)
        engine.queue.sync { engine.startClock() }
    }

    /// 走 ticks 次，每次 20 毫秒；两路各交 20 毫秒的声音，带时间戳（麦克风早 5 毫秒采到，电脑声音早 10 毫秒）
    func run(ticks: Int, mic micValue: Float = 0.1, system systemValue: Float = 0.05) {
        for _ in 0..<ticks {
            clock.advance(milliseconds: 20)
            mic.push([Float](repeating: micValue, count: 960), at: clock.now - 25_000_000)
            micSent += 960
            system.push([Float](repeating: systemValue, count: 960), at: clock.now - 30_000_000)
            systemSent += 960
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

func testEngineCancellerUnavailable() throws {
    // on 模式建不起来：提醒，started 报 unavailable，麦克风直接进它那一路
    let forced = try EchoEngineFixture(echo: .on, canceller: nil)
    expectEqual(forced.engine.echoStatus, .unavailable)
    expect(forced.engine.echoStage == nil)
    expectEqual(forced.events.all, [#"{"event":"warning","code":"echo_unavailable","message":"\#(EchoMessages.unavailable)"}"#])
    forced.run(ticks: 100)
    expectEqual(forced.pipeline.recordedSamples, 96_000, "照常录")
    _ = forced.stop()
    // auto 模式建不起来：不提醒，也不报 echo 事件
    let auto = try EchoEngineFixture(echo: .auto, canceller: nil)
    expectEqual(auto.engine.echoStatus, .unavailable)
    expectEqual(auto.events.all, [])
    _ = auto.stop()
}

func testEngineOffIsUnchanged() throws {
    // 同样的输入，off 和没有回声消除这个功能时（不传 echo）麦克风那一路收到的完全一样，也没有 echo 事件
    var random = SplitMix(seed: 11)
    let chunks = (0..<50).map { _ in (0..<960).map { _ in random.uniform() - 0.5 } }
    func micLane(_ echo: EchoSetup?) throws -> [Float] {
        let mic = FakeSource(kind: .mic)
        let system = FakeSource(kind: .system)
        let events = RecordedEvents()
        let pipeline = RecordingPipeline(encoder: try AACEncoder(),
                                         chunks: ChunkWriter(directory: try temporaryDirectory("off"), startSeq: 0, chunkSeconds: 10), events: events)
        let engine = try LiveEngine(sources: [mic, system], pipeline: pipeline, events: events, clock: { 0 }, echo: echo)
        expect(engine.echoStage == nil, "不经过回声消除")
        var lane: [Float] = []
        for (index, chunk) in chunks.enumerated() {
            mic.push(chunk, at: UInt64(index) * 20_000_000)
            engine.queue.sync {
                engine.tick()
                // 时钟不走，混音不取，麦克风那一路收到的都在队列里
                engine.mixer.lanes[0].queue.withUnsafeSamples { lane.append(contentsOf: $0) }
                engine.mixer.lanes[0].queue.removeAll()
            }
        }
        expectEqual(events.all, [], "没有 echo 事件")
        return lane
    }
    let plain = try micLane(nil)
    let off = try micLane(EchoSetup(mode: .off, makeCanceller: { Harness.fail("off 时不该建回声消除器"); return nil }))
    expectEqual(plain.count, 48_000)
    expect(off == plain && plain == chunks.flatMap { $0 }, "一个比特都不变")
}

func testEngineWithoutReference() throws {
    var built = false
    let mic = FakeSource(kind: .mic)
    let events = RecordedEvents()
    let pipeline = RecordingPipeline(encoder: try AACEncoder(),
                                     chunks: ChunkWriter(directory: try temporaryDirectory("no-ref"), startSeq: 0, chunkSeconds: 10), events: events)
    let engine = try LiveEngine(sources: [mic], pipeline: pipeline, events: events, clock: { 0 },
                                echo: EchoSetup(mode: .auto, makeCanceller: { built = true; return FakeCanceller() }))
    expect(!built, "没有电脑声音就不建回声消除器")
    expectEqual(engine.echoStatus, .auto)
    engine.queue.sync { engine.startClock() }
    expectEqual(events.all, [#"{"event":"echo","active":false,"reason":"no_reference"}"#])
    // 只录电脑声音：什么都不报
    let system = FakeSource(kind: .system)
    let quiet = RecordedEvents()
    let other = try LiveEngine(sources: [system], pipeline: pipeline, events: quiet, clock: { 0 },
                               echo: EchoSetup(mode: .on, makeCanceller: { FakeCanceller() }))
    other.queue.sync { other.startClock() }
    expectEqual(quiet.all, [])
}

func testEngineWithEcho() throws {
    let fixture = try EchoEngineFixture(echo: .on)
    guard let stage = fixture.engine.echoStage, let fake = fixture.fake else {
        Harness.fail("没有建回声消除这一步")
        return
    }
    expectEqual(fixture.engine.echoStatus, .on)
    expectEqual(fixture.events.all, [#"{"event":"echo","active":true,"reason":"forced"}"#], "开始时报一次")
    // 暂停前、继续以后的声音很轻，暂停期间的很响：编进去一点就看得出来（和不做回声消除时一样）
    fixture.run(ticks: 100, mic: 0.01)
    expect(stage.counters.processed > 50, "在做回声消除：\(stage.counters)")
    expectEqual(stage.micTimeline.restarts + stage.referenceTimeline.restarts, 0, "时间戳换算成 48 kHz 的序号，前后接得上")
    // 时间戳：麦克风的第一个采样在第 1 个 tick 之前 25 毫秒采到
    expectClose(stage.micTimeline.time(at: 0) ?? 0, seconds(1_000_000_000 + 20_000_000 - 25_000_000), within: 1e-6)
    fixture.engine.pause()
    fixture.engine.queue.sync {}
    let beforePause = fixture.pipeline.recordedSamples
    fixture.run(ticks: 50, mic: 1)
    expectEqual(fixture.pipeline.recordedSamples, beforePause, "暂停期间什么都不编")
    fixture.engine.resume()
    fixture.engine.queue.sync {}
    fixture.run(ticks: 100, mic: 0.01)
    expectEqual(fixture.pipeline.recordedSamples - beforePause, 96_000, "继续以后的 2 秒")
    let objects = fixture.events.all.compactMap { (try? JSONSerialization.jsonObject(with: Data($0.utf8))) as? [String: Any] }
    let resumed = objects.firstIndex { $0["event"] as? String == "resumed" } ?? objects.count
    let loud = objects[resumed...].filter { $0["event"] as? String == "level" && ($0["mic"] as? Double ?? 0) > 0.5 }
    expectEqual(loud.count, 0, "暂停期间采到的声音编进去了")
    guard case .success(let recording)? = fixture.stop() else {
        Harness.fail("没有正常结束")
        return
    }
    // 4 秒，加上暂停时、结束时混出来的各路攒着的、回声消除压着的
    expect(recording.seconds > 4 && recording.seconds < 5, "录了 \(recording.seconds) 秒")
    expectEqual(stage.mic.count, 0, "结束时压着的都交出去了")
    expectEqual(fake.calls, String(repeating: "rc", count: fake.calls.count / 2))
}

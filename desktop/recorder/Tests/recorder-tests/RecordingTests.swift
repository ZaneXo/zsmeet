import CRecorder
import Foundation
@testable import RecorderKit

/// 录音的流程：分片、目录、暂停、按时钟混音
let recordingTests: [(String, () throws -> Void)] = [
    ("分片：按包切，写的时候是 .part，写满落盘改名", testChunkWriter),
    ("分片：从文件名认出序号", testChunkSeqFromFileName),
    ("录音目录：建好，不覆盖已经录好的分片", testOutputDirectory),
    ("录音目录：列不出里面有什么时不录（看不到已经录好的分片，会覆盖掉）", testOutputDirectoryUnreadable),
    ("分片和录音小程序新建的目录只有自己能读写", testPrivatePermissions),
    ("暂停：丢掉暂停期间的声音，正在写的分片先落盘", testPipelinePause),
    ("现场录音：按时钟出声音，两路时钟快慢不一也对得上", testLiveEngineFollowsClock),
    ("现场录音：处理线程卡住 2 秒，补上时一点声音都不丢", testLiveEngineCatchesUpAfterStall),
    ("现场录音：点暂停前采到的都编进去，暂停期间采到的一点都不编", testLiveEnginePause),
    ("现场录音：麦克风一直全是 0 时提醒一次", testLiveEngineSilenceWarning),
    ("现场录音：一路的采样率处理不了时只去掉这一路并提醒，另一路照常录", testLiveEngineDropsUnsupportedSource),
    ("现场录音：转换采样率出错时提醒一次，录音不停", testLiveEngineConversionError),
]

/// 数一数 ADTS 帧，不是完整的 ADTS 流时返回 nil
func adtsFrameCount(_ data: Data) -> Int? {
    let bytes = [UInt8](data)
    var offset = 0
    var frames = 0
    while offset < bytes.count {
        guard offset + ADTS.headerLength <= bytes.count, bytes[offset] == 0xFF, bytes[offset + 1] & 0xF0 == 0xF0 else { return nil }
        let length = (Int(bytes[offset + 3] & 0x3) << 11) | (Int(bytes[offset + 4]) << 3) | Int(bytes[offset + 5] >> 5)
        guard length >= ADTS.headerLength else { return nil }
        offset += length
        frames += 1
    }
    return offset == bytes.count ? frames : nil
}

func testChunkWriter() throws {
    let directory = try temporaryDirectory("chunks")
    expectEqual(ChunkWriter(directory: directory, startSeq: 0, chunkSeconds: 10).packetsPerChunk, 469, "10 秒 = 468.75 包，向上取整")
    // 0.1 秒 = 4.69 包 → 每片 5 包
    let writer = ChunkWriter(directory: directory, startSeq: 4, chunkSeconds: 0.1)
    expectEqual(writer.packetsPerChunk, 5)

    let packet = [UInt8](repeating: 0xAB, count: 100)
    var chunks: [ChunkInfo] = []
    for index in 0..<12 {
        if let chunk = try packet.withUnsafeBytes({ try writer.append(packet: $0) }) {
            chunks.append(chunk)
        }
        if index == 2 {
            expect(FileManager.default.fileExists(atPath: directory.appendingPathComponent("4.aac.part").path), "写的时候是 .part")
            expect(!FileManager.default.fileExists(atPath: directory.appendingPathComponent("4.aac").path), "没写完不能有正式的文件")
        }
    }
    expectEqual(chunks.map(\.seq), [4, 5])
    let first = ChunkWriter.url(directory: directory, seq: 4)
    expectEqual(chunks[0].path, first.path)
    expectEqual(chunks[0].bytes, 5 * 107)
    expectEqual(chunks[0].packets, 5)
    expectClose(chunks[0].seconds, 5 * 1_024 / 48_000, within: 1e-9)
    let data = try Data(contentsOf: first)
    expectEqual(data.count, 5 * 107)
    expectEqual(Array(data.prefix(7)), ADTS.header(payloadLength: 100))
    expectEqual(adtsFrameCount(data), 5)

    // 结束时剩下的 2 包写成最后一片；再结束一次不会写出空文件
    let last = try writer.finishChunk()
    expectEqual(last?.seq, 6)
    expectEqual(last?.packets, 2)
    expectEqual(try writer.finishChunk(), nil)
    expectEqual(writer.nextSeq, 7)
    let names = try FileManager.default.contentsOfDirectory(atPath: directory.path).sorted()
    expectEqual(names, ["4.aac", "5.aac", "6.aac"])
}

func testChunkSeqFromFileName() {
    expectEqual(ChunkWriter.seq(fromFileName: "0.aac"), 0)
    expectEqual(ChunkWriter.seq(fromFileName: "12.aac"), 12)
    for name in ["12.aac.part", "a.aac", ".aac", "12.m4a", "-1.aac", "1 2.aac", "١٢.aac", "recording.json"] {
        expectEqual(ChunkWriter.seq(fromFileName: name), nil, name)
    }
}

func testOutputDirectory() throws {
    let base = try temporaryDirectory("out")
    let target = base.appendingPathComponent("a/b")
    let url = try OutputDirectory.prepare(target.path, startSeq: 0)
    expectEqual(url.path, target.standardizedFileURL.path)
    var isDirectory: ObjCBool = false
    expect(FileManager.default.fileExists(atPath: url.path, isDirectory: &isDirectory) && isDirectory.boolValue, "目录建好了")

    // 已经有 3.aac：从 3 和 3 以前开始都会覆盖它；没写完的 .part 不算
    FileManager.default.createFile(atPath: url.appendingPathComponent("3.aac").path, contents: Data([1]))
    FileManager.default.createFile(atPath: url.appendingPathComponent("9.aac.part").path, contents: Data([1]))
    expectThrows(RecorderFailure("chunk_exists", "录音目录里已经有 3.aac 了，从第 2 片接着录会把它覆盖掉。")) {
        _ = try OutputDirectory.prepare(url.path, startSeq: 2)
    }
    expectThrows(RecorderFailure("chunk_exists", "录音目录里已经有 3.aac 了，从第 3 片接着录会把它覆盖掉。")) {
        _ = try OutputDirectory.prepare(url.path, startSeq: 3)
    }
    _ = try OutputDirectory.prepare(url.path, startSeq: 4)

    // 相对路径换成绝对路径（事件里的 path 要是绝对路径）
    let previous = FileManager.default.currentDirectoryPath
    FileManager.default.changeCurrentDirectoryPath(base.path)
    defer { FileManager.default.changeCurrentDirectoryPath(previous) }
    let relative = try OutputDirectory.prepare("rel", startSeq: 0)
    expect(relative.path.hasPrefix("/") && relative.path.hasSuffix("/rel"), relative.path)
}

func testOutputDirectoryUnreadable() throws {
    let url = try temporaryDirectory("unreadable")
    FileManager.default.createFile(atPath: url.appendingPathComponent("3.aac").path, contents: Data([1]))
    // 只能写、不能读（-wx------）：列不出里面有什么，看不到 3.aac，从第 0 片录到第 3 片时会覆盖它
    expectEqual(chmod(url.path, 0o300), 0)
    defer { chmod(url.path, 0o700) }
    let path = url.standardizedFileURL.path
    expectThrows(RecorderFailure("out_dir", "录音目录 \(path) 读不了，看不到里面已经录好的分片，接着录可能会覆盖掉它们。请检查权限。")) {
        _ = try OutputDirectory.prepare(url.path, startSeq: 0)
    }
}

/// 文件或目录的权限，八进制，比如 "600"
private func permissions(_ url: URL) -> String {
    guard let mode = (try? FileManager.default.attributesOfItem(atPath: url.path))?[.posixPermissions] as? Int else { return "读不到" }
    return String(mode & 0o777, radix: 8)
}

func testPrivatePermissions() throws {
    let base = try temporaryDirectory("private")
    // 录音小程序自己建的目录，包括中间的：0700，和 App 建的一样
    let url = try OutputDirectory.prepare(base.appendingPathComponent("a/b").path, startSeq: 0)
    expectEqual(permissions(url), "700", "新建的录音目录")
    expectEqual(permissions(url.deletingLastPathComponent()), "700", "新建的中间目录")

    // 分片写的时候（.part）和改名以后都是 0600；上次没写完留下来的 .part（0644）接着用时也改成 0600
    let leftover = url.appendingPathComponent("0.aac.part")
    FileManager.default.createFile(atPath: leftover.path, contents: Data([1]), attributes: [.posixPermissions: 0o644])
    let writer = ChunkWriter(directory: url, startSeq: 0, chunkSeconds: 10)
    let packet = [UInt8](repeating: 0xAB, count: 100)
    _ = try packet.withUnsafeBytes { try writer.append(packet: $0) }
    expectEqual(permissions(leftover), "600", "接着用的 .part")
    let chunk = try writer.finishChunk()
    expectEqual(permissions(URL(fileURLWithPath: chunk?.path ?? "")), "600", "写好的分片")
    _ = try packet.withUnsafeBytes { try writer.append(packet: $0) }
    expectEqual(permissions(url.appendingPathComponent("1.aac.part")), "600", "新建的 .part")
    writer.abandon()
}

private func sine(seconds: Double, frequency: Double = 300) -> [Float] {
    (0..<Int(seconds * 48_000)).map { Float(0.3 * sin(2 * Double.pi * frequency * Double($0) / 48_000)) }
}

func testPipelinePause() throws {
    let directory = try temporaryDirectory("pipeline")
    let events = RecordedEvents()
    let pipeline = RecordingPipeline(encoder: try AACEncoder(),
                                     chunks: ChunkWriter(directory: directory, startSeq: 0, chunkSeconds: 1),
                                     events: events)
    let audio = sine(seconds: 1)
    func feed(seconds: Double) throws {
        var remaining = Int(seconds * 48_000)
        while remaining > 0 {
            let n = min(960, remaining)
            try audio.withUnsafeBufferPointer { buffer in
                try pipeline.feed(UnsafeBufferPointer(rebasing: buffer[0..<n]), micEnergy: 1, systemEnergy: 0)
            }
            remaining -= n
        }
    }

    try feed(seconds: 1.5)
    try pipeline.pause()
    try feed(seconds: 1)
    expectEqual(pipeline.recordedSamples, 72_000, "暂停期间的声音不算")
    pipeline.resume()
    try feed(seconds: 0.5)
    let result = try pipeline.finish()
    expectEqual(result, RecordingResult(nextSeq: 3, seconds: 2))

    // 第 1 秒满了一片；暂停时把剩下的先落盘，再发 paused；结束时最后一片
    expectEqual(events.names.filter { $0 != "level" }, ["chunk", "chunk", "paused", "resumed", "chunk"])
    expectEqual(events.names.filter { $0 == "level" }.count, 20, "每 0.1 秒一次，暂停期间没有")
    // 每次喂 960 个采样、平方和记 1：均方根 0.032，-29.8 dB
    expect(events.all.contains(#"{"event":"level","seconds":2,"mic":0.503,"system":0}"#), "音量带着录了多少秒")

    // 三片拼起来的包数和一路连续编码的一样：暂停、切片都没有多出预热
    let frames = try (0..<3).map { try adtsFrameCount(Data(contentsOf: ChunkWriter.url(directory: directory, seq: $0))) ?? -1 }
    expectEqual(frames[0], 47, "1 秒 = 46.9 包，向上取整")
    expectEqual(frames.reduce(0, +), Int((Double(96_000 + 2_112) / 1_024).rounded(.up)) - 2)
}

/// 测试用的声音来源：测试直接往环形缓冲里写
final class FakeSource: CaptureSource {
    let kind: SourceKind
    let name: String
    let ring: OpaquePointer
    let sampleRate: Double
    let silenceWarning: String?
    private(set) var stopped = false

    init(kind: SourceKind, sampleRate: Double = 48_000, silenceWarning: String? = nil) {
        self.kind = kind
        name = kind == .mic ? "测试麦克风" : "测试电脑声音"
        ring = zr_ring_create(1 << 18, 1)!
        self.sampleRate = sampleRate
        self.silenceWarning = silenceWarning
    }

    deinit {
        zr_ring_destroy(ring)
    }

    func start() {}

    func stop() {
        stopped = true
    }

    func push(_ count: Int, value: Float = 0.1) {
        let samples = [Float](repeating: value, count: count)
        _ = samples.withUnsafeBufferPointer { zr_ring_write(ring, $0.baseAddress!, count) }
    }
}

/// 手动拨的时钟（纳秒）
final class ManualClock {
    var now: UInt64 = 1_000_000_000

    func advance(milliseconds: UInt64) {
        now += milliseconds * 1_000_000
    }
}

private struct LiveFixture {
    let engine: LiveEngine
    let pipeline: RecordingPipeline
    let clock = ManualClock()
    let events = RecordedEvents()

    init(_ sources: [FakeSource]) throws {
        let directory = try temporaryDirectory("live")
        pipeline = RecordingPipeline(encoder: try AACEncoder(),
                                     chunks: ChunkWriter(directory: directory, startSeq: 0, chunkSeconds: 10),
                                     events: events)
        let clock = clock
        engine = try LiveEngine(sources: sources, pipeline: pipeline, events: events, clock: { clock.now })
        engine.queue.sync { engine.startClock() }
    }

    /// 走 ticks 次，每次 20 毫秒，每次之前先让各路交数据
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

func testLiveEngineFollowsClock() throws {
    // 麦克风的时钟比电脑慢 5%，电脑声音的快 5%（真实的设备差得少得多，这里放大了好测）
    let mic = FakeSource(kind: .mic)
    let system = FakeSource(kind: .system)
    let fixture = try LiveFixture([mic, system])
    fixture.run(ticks: 1_000) {
        mic.push(912)
        system.push(1_008)
    }
    // 出来的长度只跟着时钟走：20 秒就是 20 秒
    expectEqual(fixture.pipeline.recordedSamples, 960_000)
    let lanes = fixture.engine.mixer.lanes
    expect(lanes[0].paddedSamples > 0, "慢的一路要补静音")
    expect(lanes[1].droppedSamples > 0, "快的一路要丢掉积压")
    expect(lanes[1].queue.count <= fixture.engine.mixer.maxBacklog, "积压不超过 0.5 秒")

    guard case .success(let recording)? = fixture.stop() else {
        Harness.fail("没有正常结束")
        return
    }
    expect(mic.stopped && system.stopped, "结束时两路都停了")
    // 结束时把队列里剩下的（不到 0.5 秒）也编进去
    expect(recording.seconds >= 20 && recording.seconds < 20.6, "录了 \(recording.seconds) 秒")
    expectEqual(recording.nextSeq, 3)
}

func testLiveEngineCatchesUpAfterStall() throws {
    let mic = FakeSource(kind: .mic)
    let fixture = try LiveFixture([mic])
    fixture.run(ticks: 50) { mic.push(960) }
    let lane = fixture.engine.mixer.lanes[0]
    let padded = lane.paddedSamples
    // 处理线程卡了 2 秒（比如分片落盘时硬盘很忙）：麦克风照常往环形缓冲里写了 2 秒，下一次一起补上
    fixture.clock.advance(milliseconds: 2_000)
    mic.push(96_000)
    fixture.engine.queue.sync { fixture.engine.tick() }
    fixture.run(ticks: 50) { mic.push(960) }
    expectEqual(lane.droppedSamples, 0, "卡住期间录下的声音不能当成积压丢掉")
    expectEqual(lane.paddedSamples, padded, "也不用补静音")
    expectEqual(zr_ring_total_dropped(mic.ring), 0)
    expectEqual(fixture.pipeline.recordedSamples, 192_000, "出来的长度还是跟着时钟走")
    _ = fixture.stop()
}

/// 某个事件之后的音量事件里，麦克风的音量
private func micLevels(_ events: RecordedEvents, after name: String) -> [Double] {
    let objects = events.all.compactMap { (try? JSONSerialization.jsonObject(with: Data($0.utf8))) as? [String: Any] }
    guard let start = objects.firstIndex(where: { $0["event"] as? String == name }) else { return [] }
    return objects[(start + 1)...].filter { $0["event"] as? String == "level" }.compactMap { $0["mic"] as? Double }
}

func testLiveEnginePause() throws {
    let mic = FakeSource(kind: .mic)
    let fixture = try LiveFixture([mic])
    // 暂停前、继续以后的声音很轻（音量 0.33），暂停期间的很响（音量 1）：编进去一点就看得出来
    fixture.run(ticks: 100) { mic.push(960, value: 0.01) }
    fixture.engine.pause()
    fixture.engine.queue.sync {}
    let lane = fixture.engine.mixer.lanes[0]
    // 点暂停之前采到的 2 秒都编进去了（加上开头攒缓冲时补的静音），没有哪一点留在队列里
    expectEqual(fixture.pipeline.recordedSamples, 96_000 + Int64(lane.paddedSamples), "点暂停前采到的声音要都编进去")
    let beforePause = fixture.pipeline.recordedSamples

    fixture.run(ticks: 50) { mic.push(960, value: 1) }
    expectEqual(fixture.pipeline.recordedSamples, beforePause, "暂停期间什么都不编")
    fixture.engine.resume()
    fixture.engine.queue.sync {}
    fixture.run(ticks: 100) { mic.push(960, value: 0.01) }
    expectEqual(fixture.pipeline.recordedSamples - beforePause, 96_000, "继续以后的 2 秒")
    expectEqual(fixture.events.names.filter { $0 == "paused" || $0 == "resumed" }, ["paused", "resumed"])
    let levels = micLevels(fixture.events, after: "resumed")
    expectEqual(levels.count, 20, "继续以后每 0.1 秒一个音量")
    expectEqual(levels.filter { $0 > 0.5 }, [], "暂停期间采到的声音编进去了")

    guard case .success(let recording)? = fixture.stop() else {
        Harness.fail("没有正常结束")
        return
    }
    expectClose(recording.seconds, 4.28, within: 0.01, "4 秒，加上暂停时、结束时各混出来的、队列里攒着的 0.14 秒")
}

func testLiveEngineSilenceWarning() throws {
    let silent = FakeSource(kind: .mic, silenceWarning: "麦克风一直没有声音")
    let quiet = try LiveFixture([silent])
    quiet.run(ticks: 400) { silent.push(960, value: 0) }
    let warnings = quiet.events.all.filter { $0.contains(#""event":"warning""#) }
    expectEqual(warnings, [#"{"event":"warning","code":"mic_unavailable","message":"麦克风一直没有声音"}"#])
    _ = quiet.stop()

    // 有一点点底噪就不提醒
    let noisy = FakeSource(kind: .mic, silenceWarning: "麦克风一直没有声音")
    let normal = try LiveFixture([noisy])
    normal.run(ticks: 400) { noisy.push(960, value: 0.0001) }
    expectEqual(normal.events.names.filter { $0 == "warning" }, [])
    _ = normal.stop()
}

func testLiveEngineDropsUnsupportedSource() throws {
    // 电脑的输出设备设成了 768 kHz：转换器只支持 8 kHz 到 384 kHz
    let mic = FakeSource(kind: .mic)
    let system = FakeSource(kind: .system, sampleRate: 768_000)
    let fixture = try LiveFixture([mic, system])
    expect(fixture.engine.sources.count == 1 && fixture.engine.sources.first === mic, "只录麦克风")
    expect(system.stopped, "处理不了的那一路停掉")
    expect(!mic.stopped, "另一路照常录")
    expectEqual(fixture.events.all, [#"{"event":"warning","code":"system_unavailable","message":"电脑声音的采样率是 768000 Hz，处理不了，录不到会议软件里其他人的声音。到「音频 MIDI 设置」里把正在用的扬声器或耳机改成 48000 Hz，再重新开始录音。"}"#])
    fixture.run(ticks: 50) { mic.push(960) }
    expectEqual(fixture.pipeline.recordedSamples, 48_000, "照常录")
    guard case .success? = fixture.stop() else {
        Harness.fail("没有正常结束")
        return
    }

    // 一路都不剩：录不了（no_source），处理不了的那一路也提醒了
    let events = RecordedEvents()
    let low = FakeSource(kind: .mic, sampleRate: 4_000)
    let pipeline = RecordingPipeline(encoder: try AACEncoder(),
                                     chunks: ChunkWriter(directory: try temporaryDirectory("unsupported"), startSeq: 0, chunkSeconds: 10),
                                     events: events)
    expectThrows(RecorderFailure.noSource) {
        _ = try LiveEngine(sources: [low], pipeline: pipeline, events: events)
    }
    expect(low.stopped, "处理不了的那一路停掉")
    expectEqual(events.all, [#"{"event":"warning","code":"mic_unavailable","message":"麦克风「测试麦克风」的采样率是 4000 Hz，处理不了，录不到你这边的声音。到「音频 MIDI 设置」里把它改成 48000 Hz，或者换一个麦克风，再重新开始录音。"}"#])
}

func testLiveEngineConversionError() throws {
    let mic = FakeSource(kind: .mic, sampleRate: 44_100)
    let fixture = try LiveFixture([mic])
    fixture.run(ticks: 50) { mic.push(882) }
    // 接下来两次转换出错（真的转换器几乎不出错，只能假装），出错的那两段声音丢了
    fixture.engine.queue.sync { fixture.engine.resamplers[0].simulatedFailures = 2 }
    fixture.run(ticks: 50) { mic.push(882) }
    let warnings = fixture.events.all.filter { $0.contains(#""event":"warning""#) }
    expectEqual(warnings, [#"{"event":"warning","code":"mic_unavailable","message":"处理麦克风「测试麦克风」的声音时出错，中间有一小段没录上，录音还在继续。经常这样的话，结束录音，换一个麦克风再录。"}"#],
                "要提醒，而且只提醒一次")
    expectEqual(fixture.pipeline.recordedSamples, 96_000, "录音没停，还是跟着时钟走")
    guard case .success? = fixture.stop() else {
        Harness.fail("没有正常结束")
        return
    }
}

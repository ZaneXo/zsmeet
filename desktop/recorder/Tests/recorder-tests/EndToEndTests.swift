import AVFoundation
import Foundation
@testable import RecorderKit

/// 端到端：真的跑 zsmeet-recorder（路径在环境变量 ZSMEET_RECORDER 里），用 --input-file 代替
/// 麦克风和电脑声音（不要权限、不弹窗），再用 ffmpeg、ffprobe（服务器也是用它解码的）检查分片。
let endToEndTests: [(String, () throws -> Void)] = [
    ("端到端：读文件录音，事件完整，分片拼起来时长对得上、解码没有错", testEndToEndFile),
    ("端到端：拼起来的声音和录音的时间轴对得上（标记重点要用）", testEndToEndAlignment),
    ("端到端：录 10 分钟，分片拼起来不会越录越长", testEndToEndLong),
    ("端到端：从指定的序号接着录，不覆盖已有的分片", testEndToEndStartSeq),
    ("端到端：录到第 3999 片（服务器能收的最后一片）不再切，提醒，后面的声音都写进它", testEndToEndChunkLimit),
    ("端到端：按实际时长读，暂停、继续，读完自动结束", testEndToEndPauseResume),
    ("端到端：录到一半收到 stop", testEndToEndStop),
    ("端到端：按实际时长读时文件读到一半出错，以 error 结束，不当成读完了", testEndToEndInputFileReadError),
    ("端到端：参数写错时发 error 事件", testEndToEndBadArguments),
    ("端到端：回声消除（--reference-file）：录出来的声音里外放的回声消掉了，--echo-cancel off 时原样", testEndToEndEchoCancel),
    ("端到端：回声消除 auto 但只录麦克风：started 报 auto，接着报 no_reference；off 时不报", testEndToEndEchoWithoutReference),
]

enum EndToEnd {
    static var binary: String? {
        ProcessInfo.processInfo.environment["ZSMEET_RECORDER"]
    }
}

/// 跑一次 zsmeet-recorder：事件写到文件里，标准输入是管道（发命令；关掉等于 stop）
final class RecorderRun {
    let directory: URL
    private let process = Process()
    private let input = Pipe()
    private let eventsURL: URL

    init(_ arguments: [String], in directory: URL) throws {
        self.directory = directory
        eventsURL = directory.appendingPathComponent("events-\(UUID().uuidString).jsonl")
        FileManager.default.createFile(atPath: eventsURL.path, contents: nil)
        process.executableURL = URL(fileURLWithPath: EndToEnd.binary!)
        process.arguments = arguments
        process.currentDirectoryURL = directory
        process.standardInput = input
        process.standardOutput = try FileHandle(forWritingTo: eventsURL)
        process.standardError = FileHandle.nullDevice
        try process.run()
    }

    func send(_ command: String) {
        try? input.fileHandleForWriting.write(contentsOf: Data((command + "\n").utf8))
    }

    /// 等它退出，返回退出码；超时就杀掉，返回 nil
    func exitStatus(within seconds: Double) -> Int32? {
        let deadline = Date().addingTimeInterval(seconds)
        while process.isRunning && Date() < deadline {
            usleep(10_000)
        }
        defer { try? input.fileHandleForWriting.close() }
        if process.isRunning {
            kill(process.processIdentifier, SIGKILL)
            process.waitUntilExit()
            return nil
        }
        return process.terminationStatus
    }

    /// 等某个事件出现
    func waitFor(_ name: String, within seconds: Double) -> Bool {
        let deadline = Date().addingTimeInterval(seconds)
        while Date() < deadline {
            if events.contains(where: { $0["event"] as? String == name }) {
                return true
            }
            usleep(10_000)
        }
        return false
    }

    /// 输出的每一行都要是 JSON 对象
    var events: [[String: Any]] {
        let text = (try? String(contentsOf: eventsURL, encoding: .utf8)) ?? ""
        return text.split(separator: "\n").map { line in
            (try? JSONSerialization.jsonObject(with: Data(line.utf8))) as? [String: Any] ?? ["event": "不是 JSON：\(line)"]
        }
    }

    var names: [String] {
        events.map { $0["event"] as? String ?? "?" }
    }
}

/// 调用 ffmpeg、ffprobe（按 PATH 找）
enum FFmpeg {
    struct Output {
        let data: Data
        let errors: String
        let status: Int32
    }

    static func run(_ tool: String, _ arguments: [String]) throws -> Output {
        let process = Process()
        process.executableURL = URL(fileURLWithPath: "/usr/bin/env")
        // ffprobe 不认 -nostdin（它本来就不读标准输入）
        process.arguments = [tool] + (tool == "ffmpeg" ? ["-nostdin"] : []) + ["-hide_banner", "-v", "error"] + arguments
        let output = Pipe()
        let errorsURL = FileManager.default.temporaryDirectory.appendingPathComponent("ffmpeg-\(UUID().uuidString).txt")
        FileManager.default.createFile(atPath: errorsURL.path, contents: nil)
        defer { try? FileManager.default.removeItem(at: errorsURL) }
        process.standardOutput = output
        process.standardError = try FileHandle(forWritingTo: errorsURL)
        try process.run()
        let data = output.fileHandleForReading.readDataToEndOfFile()
        process.waitUntilExit()
        let errors = (try? String(contentsOf: errorsURL, encoding: .utf8)) ?? ""
        return Output(data: data, errors: errors.trimmingCharacters(in: .whitespacesAndNewlines), status: process.terminationStatus)
    }

    /// 用 lavfi 生成测试用的声音
    static func generate(_ url: URL, _ source: String, channels: Int) throws {
        let result = try run("ffmpeg", ["-y", "-f", "lavfi", "-i", source, "-ac", String(channels), url.path])
        guard result.status == 0 else { throw RecorderFailure("ffmpeg", "生成测试音频失败：\(result.errors)") }
    }

    /// ffmpeg 把整个文件解码一遍，返回它报的错（空的表示没有错）
    static func decodeErrors(_ url: URL) throws -> String {
        let result = try run("ffmpeg", ["-i", url.path, "-f", "null", "-"])
        return result.status == 0 ? result.errors : "退出码 \(result.status)：\(result.errors)"
    }

    /// 解出来有多少秒：ffprobe 逐帧解码数出帧数（AAC-LC 每帧 1024 个采样）。
    /// 不用 format=duration：裸的 ADTS 文件它是按码率估的，差得很远
    static func decodedSeconds(_ url: URL) throws -> Double {
        let result = try run("ffprobe", ["-count_frames", "-select_streams", "a:0", "-show_entries", "stream=nb_read_frames",
                                         "-of", "csv=p=0", url.path])
        let text = String(decoding: result.data, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
        guard result.status == 0, let frames = Int(text) else {
            throw RecorderFailure("ffprobe", "ffprobe 读不出帧数：\(result.errors) \(text)")
        }
        return Double(frames * 1_024) / 48_000
    }

    /// 解码成 48 kHz 单声道的采样
    static func decodeSamples(_ url: URL) throws -> [Float] {
        let result = try run("ffmpeg", ["-i", url.path, "-f", "f32le", "-ac", "1", "-ar", "48000", "-"])
        guard result.status == 0 else { throw RecorderFailure("ffmpeg", "解码失败：\(result.errors)") }
        return result.data.withUnsafeBytes { Array($0.bindMemory(to: Float.self)) }
    }
}

private func requireBinary() -> Bool {
    if EndToEnd.binary == nil {
        print("    （没有设置 ZSMEET_RECORDER，跳过端到端测试）")
        return false
    }
    return true
}

/// 按序号把分片首尾相接（和服务器做的一样）
private func concatenate(_ chunks: [[String: Any]], into url: URL) throws {
    var data = Data()
    for chunk in chunks {
        data.append(try Data(contentsOf: URL(fileURLWithPath: chunk["path"] as! String)))
    }
    try data.write(to: url)
}

private func chunkEvents(_ run: RecorderRun) -> [[String: Any]] {
    run.events.filter { $0["event"] as? String == "chunk" }
}

/// 检查分片事件：序号连续、文件在、大小对、没有剩下 .part；返回所有分片的秒数之和
@discardableResult
private func checkChunks(_ run: RecorderRun, out: URL, firstSeq: Int) -> Double {
    let chunks = chunkEvents(run)
    expectEqual(chunks.map { $0["seq"] as? Int ?? -1 }, Array(firstSeq..<(firstSeq + chunks.count)), "分片序号连续")
    var seconds = 0.0
    for chunk in chunks {
        let seq = chunk["seq"] as? Int ?? -1
        let path = chunk["path"] as? String ?? ""
        expectEqual(path, out.appendingPathComponent("\(seq).aac").path, "分片是绝对路径")
        let size = (try? FileManager.default.attributesOfItem(atPath: path)[.size] as? Int) ?? -1
        expectEqual(size, chunk["bytes"] as? Int, "\(seq).aac 的大小")
        seconds += chunk["seconds"] as? Double ?? 0
    }
    let leftovers = ((try? FileManager.default.contentsOfDirectory(atPath: out.path)) ?? []).filter { $0.hasSuffix(".part") }
    expectEqual(leftovers, [], "没有没写完的分片")
    return seconds
}

private func lastEvent(_ run: RecorderRun) -> [String: Any] {
    run.events.last ?? [:]
}

func testEndToEndFile() throws {
    guard requireBinary() else { return }
    let directory = try temporaryDirectory("e2e-file")
    let input = directory.appendingPathComponent("in.wav")
    // 44.1 kHz 立体声：顺带测采样率转换和混成单声道
    try FFmpeg.generate(input, "sine=frequency=440:sample_rate=44100:duration=7.3", channels: 2)
    let out = directory.appendingPathComponent("out")

    let run = try RecorderRun(["record", "--out", out.path, "--chunk-seconds", "2", "--input-file", input.path], in: directory)
    expectEqual(run.exitStatus(within: 30), 0)
    let events = run.events
    expectEqual(events.first?["event"] as? String, "started")
    expectEqual(events.first?["sample_rate"] as? Int, 48_000)
    expectEqual(events.first?["channels"] as? Int, 1)
    expectEqual(events.first?["mic"] as? String, "in.wav")
    expectEqual(events.first?["system"] as? Bool, false)
    expectEqual(run.names.filter { $0 != "level" }, ["started", "chunk", "chunk", "chunk", "chunk", "stopped"])

    // 音量每 0.1 秒一个，秒数一直往上走
    let levels = events.filter { $0["event"] as? String == "level" }.map { $0["seconds"] as? Double ?? -1 }
    // 7.3 秒约 73 个（转换采样率时最后差一两个采样就是 72 个）
    expect((72...73).contains(levels.count), "音量事件有 \(levels.count) 个")
    expectEqual(levels.first, 0.1)
    expect(zip(levels, levels.dropFirst()).allSatisfy { abs($1 - $0 - 0.1) < 0.0011 }, "音量的秒数每次加 0.1")

    let chunkSeconds = checkChunks(run, out: out, firstSeq: 0)
    let stopped = lastEvent(run)
    expectEqual(stopped["next_seq"] as? Int, 4)
    let recorded = stopped["seconds"] as? Double ?? -1
    expectClose(recorded, 7.3, within: 0.002, "录到的秒数")

    let all = directory.appendingPathComponent("all.aac")
    try concatenate(chunkEvents(run), into: all)
    expectEqual(try FFmpeg.decodeErrors(all), "", "拼起来解码没有错")
    let decoded = try FFmpeg.decodedSeconds(all)
    expectClose(decoded, 7.3, within: 0.05, "拼起来的时长和输入一样")
    expect(decoded >= recorded && decoded - recorded < 0.05, "拼起来的时长 \(decoded) 和录到的 \(recorded) 对得上")
    // 事件里每片的秒数保留 3 位小数，每片最多差 0.0005
    expectClose(chunkSeconds, decoded, within: 0.0005 * Double(chunkEvents(run).count), "各片的秒数加起来就是总时长")
    // 每一片单独也能解码（ADTS 每帧都带着格式）
    for chunk in chunkEvents(run) {
        expectEqual(try FFmpeg.decodeErrors(URL(fileURLWithPath: chunk["path"] as! String)), "", "单独解码 \(chunk["seq"]!)")
    }
}

/// 写一个 48 kHz 单声道的 WAV
func writeWAV(_ url: URL, _ samples: [Float]) throws {
    let format = AVAudioFormat(commonFormat: .pcmFormatFloat32, sampleRate: 48_000, channels: 1, interleaved: false)!
    let file = try AVAudioFile(forWriting: url, settings: format.settings, commonFormat: .pcmFormatFloat32, interleaved: false)
    let buffer = AVAudioPCMBuffer(pcmFormat: format, frameCapacity: AVAudioFrameCount(samples.count))!
    buffer.frameLength = AVAudioFrameCount(samples.count)
    samples.withUnsafeBufferPointer { buffer.floatChannelData![0].update(from: $0.baseAddress!, count: samples.count) }
    try file.write(from: buffer)
}

func testEndToEndAlignment() throws {
    guard requireBinary() else { return }
    let directory = try temporaryDirectory("e2e-align")
    // 安静的 3 秒，第 1.000 秒和第 2.000 秒各有一下「咔」
    var samples = [Float](repeating: 0, count: 144_000)
    for start in [48_000, 96_000] {
        for index in start..<(start + 24) {
            samples[index] = 0.9
        }
    }
    let input = directory.appendingPathComponent("clicks.wav")
    try writeWAV(input, samples)
    let out = directory.appendingPathComponent("out")
    let run = try RecorderRun(["record", "--out", out.path, "--chunk-seconds", "1", "--input-file", input.path], in: directory)
    expectEqual(run.exitStatus(within: 30), 0)

    let all = directory.appendingPathComponent("all.aac")
    try concatenate(chunkEvents(run), into: all)
    let decoded = try FFmpeg.decodeSamples(all)
    for expected in [48_000, 96_000] {
        let window = (expected - 4_800)..<min(expected + 4_800, decoded.count)
        let peak = window.max { abs(decoded[$0]) < abs(decoded[$1]) } ?? -1
        // 编码器的预热丢掉了整包，只剩 64 个采样（1.3 毫秒）
        expect(abs(peak - expected) <= 150, "第 \(expected / 48_000) 秒的「咔」解出来在 \(peak)，差了 \(peak - expected) 个采样")
    }
}

func testEndToEndLong() throws {
    guard requireBinary() else { return }
    let directory = try temporaryDirectory("e2e-long")
    let input = directory.appendingPathComponent("long.wav")
    // 16 kHz 单声道，文件小一些，也测一种转换比例
    try FFmpeg.generate(input, "sine=frequency=300:sample_rate=16000:duration=600", channels: 1)
    let out = directory.appendingPathComponent("out")
    let run = try RecorderRun(["record", "--out", out.path, "--input-file", input.path], in: directory)
    expectEqual(run.exitStatus(within: 120), 0)
    expectEqual(chunkEvents(run).count, 60, "默认每片 10 秒")
    checkChunks(run, out: out, firstSeq: 0)
    let all = directory.appendingPathComponent("all.aac")
    try concatenate(chunkEvents(run), into: all)
    let decoded = try FFmpeg.decodedSeconds(all)
    // 每片都重新开编码器的话会多出 60 × 44 毫秒 = 2.6 秒
    expectClose(decoded, 600, within: 0.05, "10 分钟拼起来还是 10 分钟")
    expectEqual(try FFmpeg.decodeErrors(all), "")
}

func testEndToEndStartSeq() throws {
    guard requireBinary() else { return }
    let directory = try temporaryDirectory("e2e-seq")
    let input = directory.appendingPathComponent("in.wav")
    try FFmpeg.generate(input, "sine=frequency=500:sample_rate=48000:duration=2.5", channels: 1)
    let out = directory.appendingPathComponent("out")

    let first = try RecorderRun(["record", "--out", out.path, "--start-seq", "5", "--chunk-seconds", "1", "--input-file", input.path], in: directory)
    expectEqual(first.exitStatus(within: 30), 0)
    checkChunks(first, out: out, firstSeq: 5)
    expectEqual(lastEvent(first)["next_seq"] as? Int, 8)

    // 从 6 开始会覆盖 6.aac、7.aac：不录，报错
    let clash = try RecorderRun(["record", "--out", out.path, "--start-seq", "6", "--input-file", input.path], in: directory)
    expectEqual(clash.exitStatus(within: 30), 1)
    expectEqual(clash.names, ["error"])
    expectEqual(lastEvent(clash)["code"] as? String, "chunk_exists")

    // 从 8 接着录
    let next = try RecorderRun(["record", "--out", out.path, "--start-seq", "8", "--chunk-seconds", "1", "--input-file", input.path], in: directory)
    expectEqual(next.exitStatus(within: 30), 0)
    checkChunks(next, out: out, firstSeq: 8)
}

func testEndToEndChunkLimit() throws {
    guard requireBinary() else { return }
    let directory = try temporaryDirectory("e2e-limit")
    let input = directory.appendingPathComponent("in.wav")
    try FFmpeg.generate(input, "sine=frequency=500:sample_rate=48000:duration=3", channels: 1)
    let out = directory.appendingPathComponent("out")
    // 从倒数第 2 片开始录，每片 0.5 秒。没有 App 来结束录音：第 3999 片够长了也不切，读完时才写好
    let run = try RecorderRun(["record", "--out", out.path, "--start-seq", "3998", "--chunk-seconds", "0.5", "--input-file", input.path], in: directory)
    expectEqual(run.exitStatus(within: 30), 0)
    expectEqual(run.names.filter { $0 != "level" }, ["started", "chunk", "warning", "chunk", "stopped"])
    let warning = run.events.first { $0["event"] as? String == "warning" } ?? [:]
    expectEqual(warning["code"] as? String, "chunk_limit")
    expectEqual(warning["message"] as? String, RecordingPipeline.chunkLimitMessage)
    checkChunks(run, out: out, firstSeq: 3998)
    expectEqual(lastEvent(run)["next_seq"] as? Int, 4000)
    expectEqual(try FileManager.default.contentsOfDirectory(atPath: out.path).sorted(), ["3998.aac", "3999.aac"], "不会有第 4000 片")
    // 最后一片装下了后面的 2.5 秒，拼起来还是完整的 3 秒
    let all = directory.appendingPathComponent("all.aac")
    try concatenate(chunkEvents(run), into: all)
    expectEqual(try FFmpeg.decodeErrors(all), "")
    expectClose(try FFmpeg.decodedSeconds(all), 3, within: 0.05, "拼起来的时长和输入一样")
}

func testEndToEndPauseResume() throws {
    guard requireBinary() else { return }
    let directory = try temporaryDirectory("e2e-pause")
    let input = directory.appendingPathComponent("in.wav")
    try FFmpeg.generate(input, "sine=frequency=300:sample_rate=48000:duration=6", channels: 1)
    let out = directory.appendingPathComponent("out")

    let run = try RecorderRun(["record", "--out", out.path, "--chunk-seconds", "2", "--input-file", input.path, "--realtime"], in: directory)
    expect(run.waitFor("started", within: 10), "开始了")
    usleep(2_000_000)
    run.send(#"{"cmd":"pause"}"#)
    expect(run.waitFor("paused", within: 5), "暂停了")
    usleep(1_500_000)
    run.send(#"{"cmd":"resume"}"#)
    expectEqual(run.exitStatus(within: 20), 0, "读完自动结束")

    let names = run.names.filter { $0 != "level" }
    expectEqual(names.first, "started")
    expectEqual(names.last, "stopped")
    // 暂停时先把正在写的那片落盘
    if let paused = names.firstIndex(of: "paused") {
        expectEqual(names[paused - 1], "chunk", "暂停前先发分片")
        expectEqual(names[paused + 1], "resumed")
    }
    checkChunks(run, out: out, firstSeq: 0)
    let recorded = lastEvent(run)["seconds"] as? Double ?? -1
    // 6 秒的文件暂停了约 1.5 秒；开头先攒 0.15 秒的缓冲（静音），暂停时、结束时把攒着的各混出来 0.15 秒
    expectClose(recorded, 4.8, within: 0.4, "录到的秒数不算暂停")
    let all = directory.appendingPathComponent("all.aac")
    try concatenate(chunkEvents(run), into: all)
    expectEqual(try FFmpeg.decodeErrors(all), "")
    let decoded = try FFmpeg.decodedSeconds(all)
    expect(decoded >= recorded && decoded - recorded < 0.05, "拼起来的时长 \(decoded) 和录到的 \(recorded) 对得上")
}

func testEndToEndStop() throws {
    guard requireBinary() else { return }
    let directory = try temporaryDirectory("e2e-stop")
    let input = directory.appendingPathComponent("in.wav")
    try FFmpeg.generate(input, "sine=frequency=300:sample_rate=48000:duration=30", channels: 1)
    let out = directory.appendingPathComponent("out")
    let run = try RecorderRun(["record", "--out", out.path, "--input-file", input.path, "--realtime"], in: directory)
    expect(run.waitFor("started", within: 10), "开始了")
    usleep(1_000_000)
    run.send(#"{"cmd":"stop"}"#)
    expectEqual(run.exitStatus(within: 5), 0, "很快就结束")
    expectEqual(run.names.filter { $0 != "level" }, ["started", "chunk", "stopped"], "最后一片不满也写出来")
    expectEqual(lastEvent(run)["next_seq"] as? Int, 1)
    let recorded = lastEvent(run)["seconds"] as? Double ?? -1
    expect(recorded > 0.8 && recorded < 2, "录了 \(recorded) 秒")
}

func testEndToEndInputFileReadError() throws {
    guard requireBinary() else { return }
    let directory = try temporaryDirectory("e2e-read-error")
    let input = directory.appendingPathComponent("in.wav")
    try FFmpeg.generate(input, "sine=frequency=300:sample_rate=48000:duration=30", channels: 1)
    let out = directory.appendingPathComponent("out")
    let run = try RecorderRun(["record", "--out", out.path, "--input-file", input.path, "--realtime"], in: directory)
    expect(run.waitFor("started", within: 10), "开始了")
    usleep(500_000)
    // 读到一半文件被截短了：后面的都读不出来
    expectEqual(truncate(input.path, 4_096), 0)
    expectEqual(run.exitStatus(within: 10), 1, "以出错结束")
    expectEqual(run.names.last, "error")
    expect(!run.names.contains("stopped"), "不能当成读完了、正常结束")
    expectEqual(lastEvent(run)["code"] as? String, "input_file")
}

func testEndToEndBadArguments() throws {
    guard requireBinary() else { return }
    let directory = try temporaryDirectory("e2e-args")
    let run = try RecorderRun(["record", "--out", directory.path, "--chunk-seconds", "0"], in: directory)
    expectEqual(run.exitStatus(within: 10), 2)
    expectEqual(run.names, ["error"])
    expectEqual(lastEvent(run)["code"] as? String, "bad_args")
    expectEqual(lastEvent(run)["message"] as? String, "--chunk-seconds 要在 0.5 到 600 秒之间，现在是 0")
}

/// 读 WAV（mono，48 kHz 的 float），回声消除的 --aec-dump 写的就是这种
private func readWAV(_ url: URL) throws -> [Float] {
    let reader = try AudioFileReader(path: url.path)
    var samples: [Float] = []
    while true {
        let chunk = try reader.read(maxFrames: 48_000)
        if chunk.isEmpty { return samples }
        samples += chunk
    }
}

/// 只有对方在说话的那些 10 毫秒（near 静音、mic 够响），a 和 b 的能量比（dB）
private func energyRatio(_ a: [Float], _ b: [Float], near: [Float], from: Double, to: Double) -> Double {
    var ea = 0.0
    var eb = 0.0
    for frame in Int(from * 100)..<Int(to * 100) where (frame + 1) * 480 <= min(a.count, b.count, near.count) {
        let range = (frame * 480)..<((frame + 1) * 480)
        guard energy(near[range]) < 1e-9, energy(a[range]) > 480 * 1e-5 else { continue }
        ea += energy(a[range])
        eb += energy(b[range])
    }
    return 10 * log10(ea / max(eb, 1e-20))
}

func testEndToEndEchoCancel() throws {
    guard requireBinary() else { return }
    let directory = try temporaryDirectory("e2e-echo")
    // 12 秒：对方一直在说（电脑声音），这边 7 到 9 秒说了一句；外放时麦克风录到对方的声音（晚 80 毫秒，经过房间）
    let s = scene(seconds: 12, far: [0..<12], near: [7..<9], delay: 0.08, seed: 21)
    let micFile = directory.appendingPathComponent("mic.wav")
    let farFile = directory.appendingPathComponent("far.wav")
    try writeWAV(micFile, s.mic)
    try writeWAV(farFile, s.far)
    let dump = directory.appendingPathComponent("dump")
    func run(_ mode: String) throws -> RecorderRun {
        try RecorderRun(["record", "--out", directory.appendingPathComponent(mode).path, "--echo-cancel", mode]
                            + (mode == "on" ? ["--aec-dump", dump.path] : [])
                            + ["--input-file", micFile.path, "--realtime", "--reference-file", farFile.path], in: directory)
    }
    // 两次一起跑，省一半时间
    let on = try run("on")
    let off = try run("off")
    expectEqual(on.exitStatus(within: 60), 0)
    expectEqual(off.exitStatus(within: 60), 0)
    expectEqual(on.events.first?["echo_cancellation"] as? String, "on")
    expectEqual(on.events.first?["system"] as? Bool, true, "参考文件当电脑声音录")
    expect(on.events.contains { $0["event"] as? String == "echo" && $0["active"] as? Bool == true && $0["reason"] as? String == "forced" },
           "报了在做回声消除：\(on.names)")
    expectEqual(off.events.first?["echo_cancellation"] as? String, "off")
    expect(!off.names.contains("echo"), "off 时不报 echo")
    expectEqual(on.names.last, "stopped")

    // 回声消除这一步本身（--aec-dump 写的处理前、处理后的麦克风）：只有对方在说话的时候消掉了多少
    let micIn = try readWAV(dump.appendingPathComponent("mic-in.wav"))
    let micOut = try readWAV(dump.appendingPathComponent("mic-out.wav"))
    let stage = energyRatio(micIn, micOut, near: s.near, from: 4, to: 7)
    expect(micIn.count >= 11 * 48_000 && micOut.count == micIn.count, "调试录音有 \(micIn.count)、\(micOut.count) 个采样")

    // 录出来的分片：两次都能完整解码，时长和输入一样
    for (run, name) in [(on, "on"), (off, "off")] {
        let all = directory.appendingPathComponent("\(name).aac")
        try concatenate(chunkEvents(run), into: all)
        expectEqual(try FFmpeg.decodeErrors(all), "", "\(name) 解码没有错")
        expectClose(try FFmpeg.decodedSeconds(all), 12, within: 0.6, "\(name) 的时长")
    }
    // 混音前麦克风那一路的音量（level 事件，两路分开算的）：只有对方在说话的那段时间（这边 7 秒才开始说；
    // 麦克风那一路在录出来的时间里晚 0.3 秒左右），off 时是回声的音量，on 时是消剩下的。
    // 不拿解码出来的声音减掉电脑声音那一路来比：AAC 编码保留的是听起来一样，不是波形一样，减不干净
    func micLevel(_ run: RecorderRun) -> Double {
        let levels = run.events.filter {
            $0["event"] as? String == "level" && ($0["seconds"] as? Double).map { (4.0...6.5).contains($0) } == true
        }.compactMap { $0["mic"] as? Double }
        return levels.isEmpty ? -1 : levels.reduce(0, +) / Double(levels.count)
    }
    let echoLevel = micLevel(off)
    let cancelledLevel = micLevel(on)
    print("    测得：回声消除这一步消掉 \(String(format: "%.1f", stage)) dB；麦克风那一路的音量（0 到 1，1/60 是 1 dB）：off \(String(format: "%.2f", echoLevel))、on \(String(format: "%.2f", cancelledLevel))")
    expect(stage >= 20, "回声消除这一步只消掉了 \(stage) dB")
    expect(echoLevel > 0.3, "off 时麦克风那一路应该有回声：\(echoLevel)")
    // 音量 0.25 是 15 dB
    expect(cancelledLevel >= 0 && cancelledLevel < echoLevel - 0.25, "on 时麦克风那一路的回声没小多少：\(cancelledLevel)，off 时 \(echoLevel)")
}

func testEndToEndEchoWithoutReference() throws {
    guard requireBinary() else { return }
    let directory = try temporaryDirectory("e2e-echo-mic")
    let input = directory.appendingPathComponent("in.wav")
    try FFmpeg.generate(input, "sine=frequency=300:sample_rate=48000:duration=1.5", channels: 1)
    let auto = try RecorderRun(["record", "--out", directory.appendingPathComponent("auto").path, "--echo-cancel", "auto",
                                "--input-file", input.path, "--realtime"], in: directory)
    expectEqual(auto.exitStatus(within: 20), 0)
    expectEqual(auto.events.first?["echo_cancellation"] as? String, "auto")
    let echo = auto.events.filter { $0["event"] as? String == "echo" }
    expectEqual(echo.count, 1)
    expectEqual(echo.first?["active"] as? Bool, false)
    expectEqual(echo.first?["reason"] as? String, "no_reference", "没在录电脑声音")
    // 尽快读完的测试模式（只有一路）：报要的模式，不做、也不报 echo
    let file = try RecorderRun(["record", "--out", directory.appendingPathComponent("file").path, "--echo-cancel", "on",
                                "--input-file", input.path], in: directory)
    expectEqual(file.exitStatus(within: 20), 0)
    expectEqual(file.events.first?["echo_cancellation"] as? String, "on")
    expect(!file.names.contains("echo"))
}

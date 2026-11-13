import Foundation
@testable import RecorderKit

/// 和 App 之间的约定：事件、命令、命令行参数、ADTS 帧头
let formatTests: [(String, () throws -> Void)] = [
    ("JSON：转义、数字最多 3 位小数、键按顺序", testJSON),
    ("事件的 JSON 和约定的一字不差", testEventJSON),
    ("标准输入的命令", testControlCommands),
    ("命令行参数：默认值和各种写法", testParseCommandLine),
    ("命令行参数：写错时的提示", testParseErrors),
    ("ADTS 帧头：AAC-LC、48 kHz、单声道、帧长", testADTSHeader),
    ("标准输出：最后一个事件（stopped、error）之后别的线程再发的都不写", testLastEventIsLast),
]

func testLastEventIsLast() throws {
    var fds: [Int32] = [0, 0]
    expectEqual(pipe(&fds), 0)
    let events = StdoutEvents(fd: fds[1])
    events.send(.paused)
    events.sendLast(.error(RecorderFailure("input_file", "读不下去了")))
    // 引擎的定时器还在走：之后的音量、再一个结束都不写
    events.send(.level(seconds: 1, mic: 0.5, system: 0))
    events.sendLast(.stopped(nextSeq: 1, seconds: 1))
    close(fds[1])
    let data = FileHandle(fileDescriptor: fds[0], closeOnDealloc: true).readDataToEndOfFile()
    expectEqual(String(decoding: data, as: UTF8.self),
                #"{"event":"paused"}"# + "\n" + #"{"event":"error","code":"input_file","message":"读不下去了"}"# + "\n")
}

func testJSON() {
    let value = JSON.object([
        ("event", .string("x")),
        ("text", .string("引号\"、反斜杠\\、换行\n、制表\t、\u{01}")),
        ("n", .number(1.23456)),
        ("negativeZero", .number(-0.0001)),
        ("whole", .number(3)),
        ("i", .int(-3)),
        ("b", .bool(true)),
        ("none", .optional(nil)),
        ("list", .array([.number(2.5), .optional("a")])),
    ])
    expectEqual(value.text, #"{"event":"x","text":"引号\"、反斜杠\\、换行\n、制表\t、\u0001","n":1.235,"negativeZero":0,"whole":3,"i":-3,"b":true,"none":null,"list":[2.5,"a"]}"#)
    expectEqual(JSON.number(.nan).text, "0")
}

func testEventJSON() {
    let cases: [(RecorderEvent, String)] = [
        (.started(sampleRate: 48_000, channels: 1, mic: "MacBook Pro 麦克风", system: true),
         #"{"event":"started","sample_rate":48000,"channels":1,"mic":"MacBook Pro 麦克风","system":true,"echo_cancellation":"off"}"#),
        (.started(sampleRate: 48_000, channels: 1, mic: nil, system: false),
         #"{"event":"started","sample_rate":48000,"channels":1,"mic":null,"system":false,"echo_cancellation":"off"}"#),
        (.started(sampleRate: 48_000, channels: 1, mic: "m", system: true, echoCancellation: .auto),
         #"{"event":"started","sample_rate":48000,"channels":1,"mic":"m","system":true,"echo_cancellation":"auto"}"#),
        (.started(sampleRate: 48_000, channels: 1, mic: "m", system: true, echoCancellation: .on),
         #"{"event":"started","sample_rate":48000,"channels":1,"mic":"m","system":true,"echo_cancellation":"on"}"#),
        (.started(sampleRate: 48_000, channels: 1, mic: "m", system: true, echoCancellation: .unavailable),
         #"{"event":"started","sample_rate":48000,"channels":1,"mic":"m","system":true,"echo_cancellation":"unavailable"}"#),
        (.echo(active: true, reason: .speakers), #"{"event":"echo","active":true,"reason":"speakers"}"#),
        (.echo(active: false, reason: .headphones), #"{"event":"echo","active":false,"reason":"headphones"}"#),
        (.echo(active: false, reason: .airplay), #"{"event":"echo","active":false,"reason":"airplay"}"#),
        (.echo(active: false, reason: .noReference), #"{"event":"echo","active":false,"reason":"no_reference"}"#),
        (.micChanged(name: "AirPods Pro"), #"{"event":"mic_changed","name":"AirPods Pro"}"#),
        (.warning(.micSilent, "很久没有声音"), #"{"event":"warning","code":"mic_silent","message":"很久没有声音"}"#),
        (.echo(active: true, reason: .forced), #"{"event":"echo","active":true,"reason":"forced"}"#),
        (.echo(active: false, reason: .off), #"{"event":"echo","active":false,"reason":"off"}"#),
        (.warning(.echoUnavailable, "e"), #"{"event":"warning","code":"echo_unavailable","message":"e"}"#),
        (.level(seconds: 12.3456, mic: 0.5, system: 0),
         #"{"event":"level","seconds":12.346,"mic":0.5,"system":0}"#),
        (.chunk(seq: 3, path: "/录音/3.aac", bytes: 80_000, seconds: 10.005333),
         #"{"event":"chunk","seq":3,"path":"/录音/3.aac","bytes":80000,"seconds":10.005}"#),
        (.paused, #"{"event":"paused"}"#),
        (.resumed, #"{"event":"resumed"}"#),
        (.warning(.micDenied, "没有麦克风权限"), #"{"event":"warning","code":"mic_denied","message":"没有麦克风权限"}"#),
        (.warning(.micUnavailable, "m"), #"{"event":"warning","code":"mic_unavailable","message":"m"}"#),
        (.warning(.systemDenied, "s"), #"{"event":"warning","code":"system_denied","message":"s"}"#),
        (.warning(.systemUnavailable, "s"), #"{"event":"warning","code":"system_unavailable","message":"s"}"#),
        (.warning(.chunkLimit, "c"), #"{"event":"warning","code":"chunk_limit","message":"c"}"#),
        (.error(RecorderFailure("no_source", "都录不了")), #"{"event":"error","code":"no_source","message":"都录不了"}"#),
        (.stopped(nextSeq: 7, seconds: 61.5), #"{"event":"stopped","next_seq":7,"seconds":61.5}"#),
    ]
    for (event, expected) in cases {
        expectEqual(event.json.text, expected)
    }
}

func testControlCommands() {
    expectEqual(ControlCommand(line: #"{"cmd":"pause"}"#), .pause)
    expectEqual(ControlCommand(line: "  {\"cmd\": \"resume\"}\r\n"), .resume)
    expectEqual(ControlCommand(line: #"{"cmd":"stop","extra":1}"#), .stop)
    expectEqual(ControlCommand(line: #"{"cmd":"mark"}"#), nil)
    expectEqual(ControlCommand(line: #"{"command":"stop"}"#), nil)
    expectEqual(ControlCommand(line: "stop"), nil)
    expectEqual(ControlCommand(line: ""), nil)
    expectEqual(ControlCommand(line: "[1]"), nil)
}

func testParseCommandLine() throws {
    var defaults = RecordOptions()
    defaults.outDir = "/tmp/录音"
    expectEqual(try CommandLineParser.parse(["record", "--out", "/tmp/录音"]), .record(defaults))
    expectEqual(defaults.startSeq, 0)
    expectEqual(defaults.chunkSeconds, 10)
    expectEqual(defaults.mic, .systemDefault)
    expectEqual(defaults.system, true)

    var full = RecordOptions()
    full.outDir = "d"
    full.startSeq = 12
    full.chunkSeconds = 2.5
    full.mic = .device(uid: "BuiltInMicrophoneDevice")
    full.system = false
    expectEqual(try CommandLineParser.parse(["record", "--out", "d", "--start-seq", "12", "--chunk-seconds", "2.5",
                                             "--mic", "BuiltInMicrophoneDevice", "--no-system"]), .record(full))

    var noMic = RecordOptions()
    noMic.outDir = "d"
    noMic.mic = .none
    expectEqual(try CommandLineParser.parse(["record", "--no-mic", "--out", "d"]), .record(noMic))

    var defaultMic = RecordOptions()
    defaultMic.outDir = "d"
    expectEqual(try CommandLineParser.parse(["record", "--out", "d", "--mic", "default"]), .record(defaultMic))

    // 服务器最多收 4000 片，最后一片的序号是 3999
    var lastSeq = RecordOptions()
    lastSeq.outDir = "d"
    lastSeq.startSeq = 3_999
    expectEqual(try CommandLineParser.parse(["record", "--out", "d", "--start-seq", "3999"]), .record(lastSeq))

    // 测试模式：文件代替两路声音，所以 --no-mic --no-system 也可以
    var file = RecordOptions()
    file.outDir = "d"
    file.mic = .none
    file.system = false
    file.inputFile = "in.wav"
    file.realtime = true
    expectEqual(try CommandLineParser.parse(["record", "--out", "d", "--no-mic", "--no-system", "--input-file", "in.wav", "--realtime"]),
                .record(file))

    // 回声消除，和调它用的参数
    var echo = RecordOptions()
    echo.outDir = "d"
    expectEqual(echo.echoCancel, .off, "默认不做回声消除")
    echo.echoCancel = .auto
    expectEqual(try CommandLineParser.parse(["record", "--out", "d", "--echo-cancel", "auto"]), .record(echo))
    echo.echoCancel = .on
    echo.aecLeadMilliseconds = 120
    echo.aecFilters = 8
    echo.aecDelayHintMilliseconds = 230
    echo.aecDump = "/tmp/aec"
    expectEqual(try CommandLineParser.parse(["record", "--out", "d", "--echo-cancel", "on", "--aec-lead-ms", "120", "--aec-filters", "8",
                                             "--aec-delay-hint-ms", "230", "--aec-dump", "/tmp/aec"]), .record(echo))
    var reference = RecordOptions()
    reference.outDir = "d"
    reference.inputFile = "mic.wav"
    reference.realtime = true
    reference.referenceFile = "far.wav"
    reference.echoCancel = .off
    expectEqual(try CommandLineParser.parse(["record", "--out", "d", "--input-file", "mic.wav", "--realtime", "--reference-file", "far.wav",
                                             "--echo-cancel", "off"]), .record(reference))

    expectEqual(try CommandLineParser.parse(["devices"]), .devices)
    expectEqual(try CommandLineParser.parse(["--help"]), .help)
}

func testParseErrors() {
    let cases: [([String], String)] = [
        ([], "缺少子命令：devices 或 record"),
        (["play"], "不认识的子命令：play"),
        (["devices", "--json"], "devices 不接受参数：--json"),
        (["record"], "缺少 --out：分片放在哪个目录"),
        (["record", "--out"], "--out 后面缺少值"),
        (["record", "--out", ""], "--out 后面缺少值"),
        (["record", "--out", "d", "--start-seq", "-1"], "--start-seq 要是 0 到 3999 之间的整数（服务器一场会议最多收 4000 片），现在是 -1"),
        (["record", "--out", "d", "--start-seq", "1.5"], "--start-seq 要是 0 到 3999 之间的整数（服务器一场会议最多收 4000 片），现在是 1.5"),
        (["record", "--out", "d", "--start-seq", "4000"], "--start-seq 要是 0 到 3999 之间的整数（服务器一场会议最多收 4000 片），现在是 4000"),
        // 不限制的话，写完这一片、序号加 1 时溢出，程序崩溃
        (["record", "--out", "d", "--start-seq", "\(Int.max)"],
         "--start-seq 要是 0 到 3999 之间的整数（服务器一场会议最多收 4000 片），现在是 \(Int.max)"),
        (["record", "--out", "d", "--chunk-seconds", "0.1"], "--chunk-seconds 要在 0.5 到 600 秒之间，现在是 0.1"),
        (["record", "--out", "d", "--chunk-seconds", "abc"], "--chunk-seconds 要在 0.5 到 600 秒之间，现在是 abc"),
        (["record", "--out", "d", "--mic", "x", "--no-mic"], "--mic 和 --no-mic 只能用一个"),
        (["record", "--out", "d", "--no-mic", "--no-system"], "麦克风和电脑声音至少要录一个：--no-mic 和 --no-system 不能一起用"),
        (["record", "--out", "d", "--realtime"], "--realtime 只能和 --input-file 一起用"),
        (["record", "--out", "d", "--loud"], "不认识的参数：--loud"),
        (["record", "--out", "d", "--echo-cancel"], "--echo-cancel 后面缺少值"),
        (["record", "--out", "d", "--echo-cancel", "yes"], "--echo-cancel 只能是 off、auto 或者 on，现在是 yes"),
        (["record", "--out", "d", "--aec-lead-ms", "401"], "--aec-lead-ms 要是 0 到 400 之间的整数，现在是 401"),
        (["record", "--out", "d", "--aec-filters", "0"], "--aec-filters 要是 1 到 16 之间的整数，现在是 0"),
        (["record", "--out", "d", "--aec-delay-hint-ms", "-1"], "--aec-delay-hint-ms 要是 0 到 500 之间的整数，现在是 -1"),
        (["record", "--out", "d", "--reference-file", "far.wav"], "--reference-file 只能和 --input-file、--realtime 一起用"),
        (["record", "--out", "d", "--input-file", "a.wav", "--reference-file", "far.wav"], "--reference-file 只能和 --input-file、--realtime 一起用"),
    ]
    for (arguments, message) in cases {
        expectThrows(UsageError(message)) {
            _ = try CommandLineParser.parse(arguments)
        }
    }
}

func testADTSHeader() {
    // 100 字节的包，帧长 107
    expectEqual(ADTS.header(payloadLength: 100), [0xFF, 0xF1, 0x4C, 0x40, 0x0D, 0x7F, 0xFC])

    for length in [0, 1, 170, 768, ADTS.maxFrameLength - ADTS.headerLength] {
        let header = ADTS.header(payloadLength: length).map(Int.init)
        expectEqual(header.count, 7)
        expectEqual((header[0] << 4) | (header[1] >> 4), 0xFFF, "同步字")
        expectEqual((header[1] >> 3) & 1, 0, "MPEG-4")
        expectEqual(header[1] & 1, 1, "没有 CRC")
        expectEqual(header[2] >> 6, 1, "AAC-LC")
        expectEqual((header[2] >> 2) & 0xF, 3, "48 kHz")
        expectEqual(((header[2] & 1) << 2) | (header[3] >> 6), 1, "单声道")
        expectEqual(((header[3] & 0x3) << 11) | (header[4] << 3) | (header[5] >> 5), length + 7, "帧长")
        expectEqual(((header[5] & 0x1F) << 6) | (header[6] >> 2), 0x7FF, "可变码率")
        expectEqual(header[6] & 0x3, 0, "一帧一个 AAC 块")
    }
}

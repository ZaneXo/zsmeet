import Foundation

/// 录音时的提醒，App 把 message 显示给用户。录音照常进行。
public enum WarningCode: String {
    /// 这四个：有一路声音录不了或者出了问题，另一路照常录
    case micDenied = "mic_denied"
    case micUnavailable = "mic_unavailable"
    case systemDenied = "system_denied"
    case systemUnavailable = "system_unavailable"
    /// 分片数到了服务器能收的上限，App 该结束录音了（见 ChunkWriter）。只发一次
    case chunkLimit = "chunk_limit"
    /// 要求一直做回声消除（--echo-cancel on），却没能打开或者中途出错关掉了，麦克风的声音原样录
    case echoUnavailable = "echo_unavailable"
    /// 录音中（不算暂停）麦克风很长一段时间录到的全是静音，或者比安静房间的底噪还小得多（见 SilenceWatch）：
    /// 多半是会议软件的语音处理占住了麦克风、麦克风被静音了、录的不是正在用的麦克风。
    /// 一段静音只提醒一次，有了声音以后又静音那么久会再提醒
    case micSilent = "mic_silent"
}

/// started 事件里的回声消除（echo_cancellation）：用的哪种模式；要的模式没能打开是 unavailable
public enum EchoCancelStatus: String, Equatable {
    case off
    case auto
    case on
    case unavailable
}

/// echo 事件里说的原因：现在为什么在做、为什么没做回声消除
public enum EchoReason: String, Equatable {
    /// auto 模式，声音从扬声器（外放）出来：在做
    case speakers
    /// auto 模式，戴着耳机（包括耳麦、蓝牙耳机）：没做
    case headphones
    /// auto 模式，声音从 AirPlay 出来（延迟太长，消不掉）：没做
    case airplay
    /// 电脑声音没在录、断了或者一直来得太晚：没做
    case noReference = "no_reference"
    /// on 模式：在做
    case forced
    /// 回声消除出错，这次录音不再做了
    case off
}

/// 录不下去了：发一个 error 事件，然后以非 0 退出。code 给 App 判断，message 给用户看。
public struct RecorderFailure: Error, Equatable {
    public let code: String
    public let message: String

    public init(_ code: String, _ message: String) {
        self.code = code
        self.message = message
    }

    /// 麦克风和电脑声音一路都录不了。每一路为什么录不了，前面已经各发了一个 warning（App 和它一起显示）
    static let noSource = RecorderFailure("no_source", "麦克风和电脑声音都录不了，没法开始录音。按提示处理好以后再试。")
}

/// 写到标准输出的事件，一行一个 JSON。日志不在这里，写在标准错误。
public enum RecorderEvent {
    case started(sampleRate: Int, channels: Int, mic: String?, system: Bool, echoCancellation: EchoCancelStatus = .off)
    /// 回声消除实际做没做变了（开始录音时先报一次）。只在 --echo-cancel 是 auto、on 时有
    case echo(active: Bool, reason: EchoReason)
    /// 录的是系统默认的麦克风（--mic default），默认的麦克风换了（或者原来的断开了、系统换了一个），
    /// 已经换过去接着录了：name 是现在在录的麦克风（见 MicFollower）
    case micChanged(name: String)
    /// seconds 是录了多久（不算暂停），mic、system 是两路的音量（0…1）
    case level(seconds: Double, mic: Double, system: Double)
    /// 一个分片写好了（已经落盘、改成正式的文件名）
    case chunk(seq: Int, path: String, bytes: Int, seconds: Double)
    case paused
    case resumed
    case warning(WarningCode, String)
    case error(RecorderFailure)
    case stopped(nextSeq: Int, seconds: Double)

    public var json: JSON {
        switch self {
        case let .started(sampleRate, channels, mic, system, echoCancellation):
            return .object([
                ("event", .string("started")),
                ("sample_rate", .int(sampleRate)),
                ("channels", .int(channels)),
                ("mic", .optional(mic)),
                ("system", .bool(system)),
                ("echo_cancellation", .string(echoCancellation.rawValue)),
            ])
        case let .echo(active, reason):
            return .object([
                ("event", .string("echo")),
                ("active", .bool(active)),
                ("reason", .string(reason.rawValue)),
            ])
        case let .micChanged(name):
            return .object([
                ("event", .string("mic_changed")),
                ("name", .string(name)),
            ])
        case let .level(seconds, mic, system):
            return .object([
                ("event", .string("level")),
                ("seconds", .number(seconds)),
                ("mic", .number(mic)),
                ("system", .number(system)),
            ])
        case let .chunk(seq, path, bytes, seconds):
            return .object([
                ("event", .string("chunk")),
                ("seq", .int(seq)),
                ("path", .string(path)),
                ("bytes", .int(bytes)),
                ("seconds", .number(seconds)),
            ])
        case .paused:
            return .object([("event", .string("paused"))])
        case .resumed:
            return .object([("event", .string("resumed"))])
        case let .warning(code, message):
            return .object([
                ("event", .string("warning")),
                ("code", .string(code.rawValue)),
                ("message", .string(message)),
            ])
        case let .error(failure):
            return .object([
                ("event", .string("error")),
                ("code", .string(failure.code)),
                ("message", .string(failure.message)),
            ])
        case let .stopped(nextSeq, seconds):
            return .object([
                ("event", .string("stopped")),
                ("next_seq", .int(nextSeq)),
                ("seconds", .number(seconds)),
            ])
        }
    }
}

/// App 从标准输入发来的命令，一行一个：{"cmd":"pause"}、{"cmd":"resume"}、{"cmd":"stop"}。
public enum ControlCommand: String, Equatable {
    case pause
    case resume
    case stop

    /// 空行、不是 JSON、不认识的命令都返回 nil
    public init?(line: String) {
        let trimmed = line.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty,
              let data = trimmed.data(using: .utf8),
              let object = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any],
              let name = object["cmd"] as? String
        else { return nil }
        self.init(rawValue: name)
    }
}

/// 收事件的一方：正式运行时写到标准输出，测试时记在内存里。会在不同的线程上调用。
public protocol EventSink: AnyObject {
    func send(_ event: RecorderEvent)
}

/// 把事件一行一行写到标准输出。好几个线程都会发事件，加锁保证每一行是完整的。
public final class StdoutEvents: EventSink {
    private let lock = NSLock()
    private let fd: Int32
    private var closed = false
    /// 发过最后一个事件（stopped、error）了：之后别的线程再发的（比如还在走的音量）都不写，最后一个一定在最后
    private var sealed = false

    /// 标准输出断了（App 不在了，没人听了）时调用一次，在发事件的那个线程上
    public var onClosed: (() -> Void)?

    public init(fd: Int32 = STDOUT_FILENO) {
        self.fd = fd
    }

    public func send(_ event: RecorderEvent) {
        write(event, last: false)
    }

    /// 发最后一个事件（stopped、error），之后什么都不再写
    public func sendLast(_ event: RecorderEvent) {
        write(event, last: true)
    }

    private func write(_ event: RecorderEvent, last: Bool) {
        let line = Array((event.json.text + "\n").utf8)
        lock.lock()
        guard !sealed else {
            lock.unlock()
            return
        }
        sealed = last
        let justClosed = !closed && !writeAll(line)
        if justClosed { closed = true }
        lock.unlock()
        if justClosed {
            onClosed?()
        }
    }

    private func writeAll(_ bytes: [UInt8]) -> Bool {
        var offset = 0
        while offset < bytes.count {
            let written = bytes.withUnsafeBytes { buffer in
                Foundation.write(fd, buffer.baseAddress! + offset, buffer.count - offset)
            }
            if written < 0 {
                if errno == EINTR { continue }
                return false
            }
            offset += written
        }
        return true
    }
}

/// 日志写到标准错误。标准输出只放给 App 读的事件，别的什么都不能往那里写。
public func log(_ message: String) {
    fputs("zsmeet-recorder: \(message)\n", stderr)
}

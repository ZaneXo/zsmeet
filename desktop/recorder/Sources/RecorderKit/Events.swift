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
    case started(sampleRate: Int, channels: Int, mic: String?, system: Bool)
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
        case let .started(sampleRate, channels, mic, system):
            return .object([
                ("event", .string("started")),
                ("sample_rate", .int(sampleRate)),
                ("channels", .int(channels)),
                ("mic", .optional(mic)),
                ("system", .bool(system)),
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

    /// 标准输出断了（App 不在了，没人听了）时调用一次，在发事件的那个线程上
    public var onClosed: (() -> Void)?

    public init(fd: Int32 = STDOUT_FILENO) {
        self.fd = fd
    }

    public func send(_ event: RecorderEvent) {
        let line = Array((event.json.text + "\n").utf8)
        lock.lock()
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

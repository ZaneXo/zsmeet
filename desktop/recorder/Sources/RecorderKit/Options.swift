import Foundation

/// 录不录麦克风、录哪一个。
public enum MicChoice: Equatable {
    case none
    case systemDefault
    case device(uid: String)
}

public struct RecordOptions: Equatable {
    /// 分片写到哪个目录（不存在就建）
    public var outDir = ""
    /// 第一个分片的序号。接着录一段中断了的录音时，从上次的下一个序号开始
    public var startSeq = 0
    /// 每片大约多少秒（按录到的声音算，不算暂停）
    public var chunkSeconds = 10.0
    public var mic = MicChoice.systemDefault
    /// 录不录电脑里的声音
    public var system = true
    /// 测试用：拿这个音频文件代替麦克风和电脑声音（不要权限、不弹窗），尽快读完，读完自动结束
    public var inputFile: String?
    /// 测试用：和 --input-file 一起用，按文件的实际时长读（像真的在录），用来测暂停、继续
    public var realtime = false

    public init() {}
}

public enum Command: Equatable {
    case devices
    case record(RecordOptions)
    case help
}

public struct UsageError: Error, Equatable {
    public let message: String

    public init(_ message: String) {
        self.message = message
    }
}

public enum CommandLineParser {
    public static let usage = """
        用法：
          zsmeet-recorder devices
              列出麦克风，输出一行 JSON：{"microphones":[{"uid":"…","name":"…","default":true}]}
          zsmeet-recorder record --out <目录> [--start-seq N] [--chunk-seconds 10]
                                 [--mic <uid> | --mic default | --no-mic] [--no-system]
                                 [--input-file <音频文件> [--realtime]]
              录音：标准输出一行一个事件（JSON），标准输入一行一个命令：
              {"cmd":"pause"} {"cmd":"resume"} {"cmd":"stop"}，标准输入关掉等于 stop。
              --input-file 是测试用的：拿文件代替麦克风和电脑声音，尽快读完后自动结束；
              再加 --realtime 就按文件的实际时长读。
        """

    /// --start-seq 要小于它（服务器一场会议最多收的片数）：从后面开始录的分片都传不上去。
    /// 录的过程中也不会超出，见 ChunkWriter
    static let maxChunks = ChunkWriter.maxChunks

    /// args 不含程序名
    public static func parse(_ args: [String]) throws -> Command {
        guard let subcommand = args.first else {
            throw UsageError("缺少子命令：devices 或 record")
        }
        switch subcommand {
        case "-h", "--help", "help":
            return .help
        case "devices":
            if args.count > 1 {
                throw UsageError("devices 不接受参数：\(args[1])")
            }
            return .devices
        case "record":
            return .record(try parseRecord(Array(args.dropFirst())))
        default:
            throw UsageError("不认识的子命令：\(subcommand)")
        }
    }

    static func parseRecord(_ args: [String]) throws -> RecordOptions {
        var options = RecordOptions()
        var micGiven = false
        var noMic = false
        var index = 0

        func value(of flag: String) throws -> String {
            index += 1
            guard index < args.count, !args[index].isEmpty else {
                throw UsageError("\(flag) 后面缺少值")
            }
            return args[index]
        }

        while index < args.count {
            let flag = args[index]
            switch flag {
            case "--out":
                options.outDir = try value(of: flag)
            case "--start-seq":
                let text = try value(of: flag)
                guard let seq = Int(text), seq >= 0, seq < maxChunks else {
                    throw UsageError("--start-seq 要是 0 到 \(maxChunks - 1) 之间的整数（服务器一场会议最多收 \(maxChunks) 片），现在是 \(text)")
                }
                options.startSeq = seq
            case "--chunk-seconds":
                let text = try value(of: flag)
                guard let seconds = Double(text), seconds.isFinite, seconds >= 0.5, seconds <= 600 else {
                    throw UsageError("--chunk-seconds 要在 0.5 到 600 秒之间，现在是 \(text)")
                }
                options.chunkSeconds = seconds
            case "--mic":
                let text = try value(of: flag)
                options.mic = text == "default" ? .systemDefault : .device(uid: text)
                micGiven = true
            case "--no-mic":
                noMic = true
            case "--no-system":
                options.system = false
            case "--input-file":
                options.inputFile = try value(of: flag)
            case "--realtime":
                options.realtime = true
            default:
                throw UsageError("不认识的参数：\(flag)")
            }
            index += 1
        }

        if micGiven && noMic {
            throw UsageError("--mic 和 --no-mic 只能用一个")
        }
        if noMic {
            options.mic = .none
        }
        if options.outDir.isEmpty {
            throw UsageError("缺少 --out：分片放在哪个目录")
        }
        if options.realtime && options.inputFile == nil {
            throw UsageError("--realtime 只能和 --input-file 一起用")
        }
        if options.inputFile == nil && options.mic == .none && !options.system {
            throw UsageError("麦克风和电脑声音至少要录一个：--no-mic 和 --no-system 不能一起用")
        }
        return options
    }
}

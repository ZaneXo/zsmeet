import Foundation

/// 录不录麦克风、录哪一个。
public enum MicChoice: Equatable {
    case none
    /// 系统默认的麦克风：录音中默认的换了跟着换（见 MicFollower）
    case systemDefault
    /// 只录这一个（断开了只提醒，不换）
    case device(uid: String)
}

/// 回声消除（--echo-cancel）。外放开会时麦克风会录到会议里其他人的声音，用录到的电脑声音把它从麦克风里去掉
public enum EchoCancelMode: String, Equatable {
    /// 不做（默认）
    case off
    /// 麦克风和电脑声音都在录、声音不是从耳机（包括耳麦、AirPods）或 AirPlay 出来时才做
    case auto
    /// 麦克风和电脑声音都在录就做（比如用蓝牙音箱外放，auto 会把它当成耳机）
    case on
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
    /// 回声消除
    public var echoCancel = EchoCancelMode.off
    /// 调回声消除用（--aec-lead-ms）：参考比麦克风提前多少毫秒，0 到 400
    public var aecLeadMilliseconds = 170
    /// 调回声消除用（--aec-filters）：AEC3 找回声延迟用的滤波器个数，1 到 16
    public var aecFilters = 6
    /// 调回声消除用（--aec-delay-hint-ms）：告诉 AEC3 大概的回声延迟（毫秒，包括提前量），0 到 500；nil 不告诉
    public var aecDelayHintMilliseconds: Int?
    /// 排查回声消除用（--aec-dump）：把麦克风原声、参考、处理后的麦克风写成 WAV 放到这个目录
    public var aecDump: String?
    /// 测试用：拿这个音频文件代替麦克风和电脑声音（不要权限、不弹窗），尽快读完，读完自动结束
    public var inputFile: String?
    /// 测试用：和 --input-file 一起用，按文件的实际时长读（像真的在录），用来测暂停、继续
    public var realtime = false
    /// 测试用：和 --input-file、--realtime 一起用，拿这个音频文件当电脑声音（这时 --input-file 只当麦克风），
    /// 测回声消除
    public var referenceFile: String?

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
                                 [--echo-cancel off|auto|on]
                                 [--input-file <音频文件> [--realtime [--reference-file <音频文件>]]]
              录音：标准输出一行一个事件（JSON），标准输入一行一个命令：
              {"cmd":"pause"} {"cmd":"resume"} {"cmd":"stop"}，标准输入关掉等于 stop。
              --mic default（默认）录系统默认的麦克风：录音中系统默认的输入设备换了，或者在录的麦克风断开了、
              系统换了一个默认的，就换过去接着录（mic_changed 事件）；--mic <uid> 只录这一个，不跟着换。
              --echo-cancel：回声消除（默认 off）。auto：麦克风和电脑声音都在录、声音不是从耳机或 AirPlay
              出来时，用电脑声音把外放录进麦克风的会议声音去掉；on：两路都在录就做。
              --input-file 是测试用的：拿文件代替麦克风和电脑声音，尽快读完后自动结束；
              再加 --realtime 就按文件的实际时长读；再加 --reference-file 就拿它当电脑声音（测回声消除）。
              调回声消除用：--aec-lead-ms 170、--aec-filters 6、--aec-delay-hint-ms <毫秒>、
              --aec-dump <目录>（把麦克风原声、参考、处理后的麦克风写成 WAV）。
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

        /// 范围内的整数
        func integer(of flag: String, in range: ClosedRange<Int>) throws -> Int {
            let text = try value(of: flag)
            guard let number = Int(text), range.contains(number) else {
                throw UsageError("\(flag) 要是 \(range.lowerBound) 到 \(range.upperBound) 之间的整数，现在是 \(text)")
            }
            return number
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
            case "--echo-cancel":
                let text = try value(of: flag)
                guard let mode = EchoCancelMode(rawValue: text) else {
                    throw UsageError("--echo-cancel 只能是 off、auto 或者 on，现在是 \(text)")
                }
                options.echoCancel = mode
            case "--aec-lead-ms":
                options.aecLeadMilliseconds = try integer(of: flag, in: 0...400)
            case "--aec-filters":
                options.aecFilters = try integer(of: flag, in: 1...16)
            case "--aec-delay-hint-ms":
                options.aecDelayHintMilliseconds = try integer(of: flag, in: 0...500)
            case "--aec-dump":
                options.aecDump = try value(of: flag)
            case "--input-file":
                options.inputFile = try value(of: flag)
            case "--realtime":
                options.realtime = true
            case "--reference-file":
                options.referenceFile = try value(of: flag)
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
        if options.referenceFile != nil && !options.realtime {
            throw UsageError("--reference-file 只能和 --input-file、--realtime 一起用")
        }
        if options.inputFile == nil && options.mic == .none && !options.system {
            throw UsageError("麦克风和电脑声音至少要录一个：--no-mic 和 --no-system 不能一起用")
        }
        return options
    }
}

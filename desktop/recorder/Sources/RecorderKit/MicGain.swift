import CRecorder
import Foundation

/// 麦克风自动增益（--mic-gain）。原生录音直接录麦克风的原始声音，没有浏览器录音时的自动增益：MacBook 自带的麦克风
/// 离说话的人一两米时，录到的说话声比正常的录音小 16 到 20 dB，服务器的 VAD 会把小声的话当成没人说话丢掉
public enum MicGainMode: String, Equatable {
    /// 不调，麦克风的声音原样录
    case off
    /// 按说话的声音自动调到正常的音量（默认）
    case auto
}

/// started 事件里的 mic_gain：用的哪种模式；要调却没能打开是 unavailable（麦克风的声音原样录）
public enum MicGainStatus: String, Equatable {
    case off
    case auto
    case unavailable
}

/// 自动增益的参数（WebRTC 的 AGC2，见 zr_agc.h）。说话的音量调到 -22 dBFS 左右（均方根，按说话的时候算），
/// 峰值在 -5 dBFS 左右；量到的数见 Tests/recorder-tests/MicGainTests.swift
public struct GainSettings: Equatable {
    /// 最多调大多少 dB：小声到 -52 dBFS 的说话也能调到正常的音量，再小的多半是离得太远，调大了也全是底噪
    public var maxGain = 30.0
    /// 一开始调大多少 dB，之后按说话的声音调。MacBook 自带的麦克风要调大 20 dB 左右，耳机、USB 麦克风本来就够响：
    /// 取中间，够响的麦克风开头一两秒偏响（限幅器压着，不会削顶），自带的麦克风有人说话一两秒就调够
    public var initialGain = 10.0
    /// 说话的峰值离满幅留多少 dB（AGC2 的 headroom_db）
    public var headroom = 5.0
    /// 增益每秒最多变多少 dB（AGC2 的默认值）
    public var maxChangePerSecond = 6.0
    /// 调大以后的底噪最多多大（dBFS）。AGC2 的默认值是 -50，开会的房间里自带的麦克风录到的底噪一般在 -60 dBFS
    /// 上下，按 -50 只能调大 10 dB，说话还是小。放宽到 -40：底噪跟着说话一起调大，信噪比不变，转写不受影响
    public var maxOutputNoise = -40.0

    public init() {}
}

/// 自动增益：一帧（480 个采样，48 kHz 的 10 毫秒）一帧地调音量，处理过的声音不晚（第 n 个采样出来还是第 n 个）。
/// 正式用的是 WebRTCGainController（AGC2），测试里换成假的。只在一个线程上用（录音引擎的处理线程）
public protocol GainController: AnyObject {
    /// 调一帧，写到 output（可以和 input 是同一块内存）。返回 false 表示出错了，这时 output 里的不能用
    func process(_ input: UnsafePointer<Float>, into output: UnsafeMutablePointer<Float>) -> Bool
    /// 从头开始：之前估计的说话音量、底噪都不要了，一开始调大 initialGain dB。返回 false 表示没能重新开始
    func reset(initialGain: Double) -> Bool
}

/// WebRTC 的 AGC2（zr_agc.cc 包的 C 接口）。参数都在 C 那边查过，不会让 WebRTC abort 整个进程
public final class WebRTCGainController: GainController {
    public static let frameSize = Int(ZR_AGC_FRAME)

    private let agc: OpaquePointer
    private let maxGain: Double

    /// 参数不对、建不起来时返回 nil（原因写在标准错误里）
    public init?(settings: GainSettings) {
        var config = zr_agc_config(max_gain_db: Float(settings.maxGain), initial_gain_db: Float(settings.initialGain),
                                   headroom_db: Float(settings.headroom),
                                   max_gain_change_db_per_second: Float(settings.maxChangePerSecond),
                                   max_output_noise_level_dbfs: Float(settings.maxOutputNoise))
        guard let agc = zr_agc_create(&config) else { return nil }
        self.agc = agc
        maxGain = settings.maxGain
    }

    deinit {
        zr_agc_destroy(agc)
    }

    public func process(_ input: UnsafePointer<Float>, into output: UnsafeMutablePointer<Float>) -> Bool {
        zr_agc_process(agc, input, output, Self.frameSize) == ZR_AGC_OK
    }

    public func reset(initialGain: Double) -> Bool {
        zr_agc_reset(agc, Float(min(max(initialGain, 0), maxGain))) == ZR_AGC_OK
    }
}

/// 现场录音时麦克风自动增益怎么做（RecordSession 按命令行参数准备；--mic-gain off 时不传）
public struct MicGainSetup {
    public var settings: GainSettings
    /// 建自动增益。录着麦克风时才调用（在开始录音前，不在音频线程上）；建不起来返回 nil
    public var makeController: () -> GainController?

    public init(settings: GainSettings = GainSettings(), makeController: @escaping () -> GainController?) {
        self.settings = settings
        self.makeController = makeController
    }

    /// 正式用的：WebRTC 的 AGC2
    public static func webRTC(_ settings: GainSettings = GainSettings()) -> MicGainSetup {
        MicGainSetup(settings: settings) { WebRTCGainController(settings: settings) }
    }
}

/// 麦克风自动增益这一步（--mic-gain auto）。LiveEngine 里在回声消除之后、按时间混音之前，只动麦克风那一路：
///
///     麦克风 → Resampler → EchoStage（AEC3，开着回声消除时）→ MicGainStage（AGC2）→ 麦克风那一路 → TimeMixer
///     电脑声音 → Resampler → 电脑声音那一路（不动）→ TimeMixer
///
/// 线上会议时电脑声音那一路（会议里其他人）音量正常，麦克风那一路小 20 dB 左右，所以要在混音之前把麦克风单独调到
/// 正常的说话音量。先去掉回声再调：调大了的回声更难消，增益一直在变 AEC3 也学不准回声的路径。
///
/// 按 10 毫秒一帧处理，凑不满一帧的先压着（最多 479 个采样），处理过的不晚。AGC2 只在有人说话时把增益往上调，
/// 没人说话时不会把底噪越调越大；增益在一帧里平滑地变（每秒最多 6 dB），限幅器保证不超过满幅。
/// 怎么分批交进来，出来的都一样（同一套 10 毫秒的帧）。
///
/// 不管出什么事，进来多少个采样就出去多少个，顺序不变，录音不会因为它停下来：建不起来就不经过它（录原声）；
/// 某一帧出错了，这一帧的增益平滑地回到原样（原声），AGC2 从 0 dB 重新开始，接着调；连续出错就不再调，后面都是原声。
/// 只在录音引擎的处理线程上用
final class MicGainStage {
    static let frameSize = 480
    /// 连续出错这么多次就不再调
    static let maxConsecutiveFailures = 3
    /// 每处理这么多帧（录到的 60 秒）写一次统计日志
    static let statsFrames = 6_000
    /// 统计「有声音的时候」的音量、增益时只算比它响的帧（-60 dBFS，安静房间的底噪附近）
    private static let loudEnergy = Double(frameSize) * 1e-6
    /// 估计现在的增益时只看比它响的帧（-100 dBFS，再小的看不准）
    private static let audibleEnergy = Double(frameSize) * 1e-10

    /// 统计（日志、测试用）
    struct Counters: Equatable {
        /// 经过 AGC2 的帧（不满一帧的也算一帧）、出错的帧、直接用原声的帧（不再调以后）
        var processed = 0
        var failures = 0
        var bypassed = 0
        /// 从头开始的次数（换了麦克风、出错以后）
        var restarts = 0
        /// 暂停、换麦克风、结束时补了 0 凑成一帧处理的次数
        var padded = 0
    }

    let controller: GainController
    private let settings: GainSettings
    /// 还没凑满一帧的采样
    private var pending = [Float](repeating: 0, count: MicGainStage.frameSize)
    private var pendingCount = 0
    private var frame = [Float](repeating: 0, count: MicGainStage.frameSize)
    private var output = [Float](repeating: 0, count: MicGainStage.frameSize)
    /// 换了麦克风的地方（新的麦克风的第一个采样是麦克风那一路的第几个采样，按先后），处理到那里时从头开始
    private(set) var micSwitches: [Int64] = []
    /// 现在大概调大了多少倍（上一帧处理后和处理前的均方根之比）。出错时从它平滑地回到原样
    private(set) var currentGain: Double
    private(set) var healthy = true
    private var consecutiveFailures = 0
    private(set) var counters = Counters()
    /// 这一段（上次写日志以后）的统计：有声音的帧处理前、处理后的能量，帧数，最大的增益
    private var period = Period()

    private struct Period {
        var frames = 0
        var loudFrames = 0
        var inputEnergy = 0.0
        var outputEnergy = 0.0
        var maxGain = 0.0
    }

    init(controller: GainController, settings: GainSettings) {
        self.controller = controller
        self.settings = settings
        currentGain = pow(10, settings.initialGain / 20)
    }

    /// 麦克风那一路新来的采样（48 kHz）。end：这一批最后一个之后是麦克风那一路的第几个采样（从第一个采样算起；
    /// 暂停期间采到的、点继续时不要的那些没交过来，序号会跳过去），用来找换麦克风的地方。调好的追加到 lane
    func process(_ samples: UnsafeBufferPointer<Float>, end: Int64, into lane: inout SampleFIFO) {
        guard !samples.isEmpty else { return }
        let start = end - Int64(samples.count)
        var offset = 0
        while offset < samples.count {
            let position = start + Int64(offset)
            if let boundary = micSwitches.first, boundary <= position {
                // 从这里起是新的麦克风的声音（换的那一刻之前的已经都交过来了，或者在暂停期间、不要了）
                micSwitches.removeFirst()
                restartForNewMic(into: &lane)
                continue
            }
            var count = samples.count - offset
            if let boundary = micSwitches.first {
                count = min(count, Int(boundary - position))
            }
            append(UnsafeBufferPointer(rebasing: samples[offset..<(offset + count)]), into: &lane)
            offset += count
        }
    }

    /// 换了麦克风（跟着系统默认的麦克风换了，见 MicFollower）：从 boundary（麦克风那一路的第几个采样）起是新的麦克风。
    /// 处理到那里时 AGC2 从头开始（从一开始的增益重新调）：新的麦克风灵敏度不一样，旧的增益不合适
    func micDeviceChanged(at boundary: Int64) {
        micSwitches.append(boundary)
    }

    /// 暂停：压着的不满一帧的现在就处理掉交出去（这样点暂停之前采到的都编进去）
    func flush(into lane: inout SampleFIFO) {
        guard pendingCount > 0 else { return }
        counters.padded += 1
        processPending(into: &lane)
    }

    /// 继续：压着的是暂停期间采到的，不要
    func discardHeld() {
        pendingCount = 0
    }

    /// 结束：压着的都交出去，写最后一行统计
    func finish(into lane: inout SampleFIFO) {
        flush(into: &lane)
        logStats(final: true)
    }

    // MARK: 处理

    private func append(_ samples: UnsafeBufferPointer<Float>, into lane: inout SampleFIFO) {
        var offset = 0
        while offset < samples.count {
            let count = min(Self.frameSize - pendingCount, samples.count - offset)
            pending.withUnsafeMutableBufferPointer { buffer in
                (buffer.baseAddress! + pendingCount).update(from: samples.baseAddress! + offset, count: count)
            }
            pendingCount += count
            offset += count
            if pendingCount == Self.frameSize {
                processPending(into: &lane)
            }
        }
    }

    /// 处理压着的一帧（不满一帧的后面补 0），交出压着的那几个
    private func processPending(into lane: inout SampleFIFO) {
        let count = pendingCount
        pendingCount = 0
        frame.withUnsafeMutableBufferPointer { buffer in
            pending.withUnsafeBufferPointer { buffer.baseAddress!.update(from: $0.baseAddress!, count: count) }
            (buffer.baseAddress! + count).initialize(repeating: 0, count: Self.frameSize - count)
        }
        if healthy {
            counters.processed += 1
            let succeeded = frame.withUnsafeBufferPointer { input in
                output.withUnsafeMutableBufferPointer { out in
                    controller.process(input.baseAddress!, into: out.baseAddress!)
                }
            }
            if succeeded {
                consecutiveFailures = 0
                measure(count: count)
            } else {
                fadeToRaw(count: count)
                failed()
            }
            lane.append(output[0..<count])
        } else {
            counters.bypassed += 1
            lane.append(frame[0..<count])
        }
        period.frames += 1
        if period.frames >= Self.statsFrames {
            logStats(final: false)
        }
    }

    /// 处理好的一帧：记下现在的增益，算统计
    private func measure(count: Int) {
        let input = energy(frame, count: count)
        let processed = energy(output, count: count)
        if input > Self.audibleEnergy * Double(count) / Double(Self.frameSize) {
            currentGain = (processed / input).squareRoot()
        }
        if input > Self.loudEnergy * Double(count) / Double(Self.frameSize) {
            period.loudFrames += 1
            period.inputEnergy += input
            period.outputEnergy += processed
        }
        period.maxGain = max(period.maxGain, currentGain)
    }

    /// 出错的这一帧：增益从现在的平滑地回到 1（原声），超出 -1…1 的削掉。不是有限的数的换成 0
    private func fadeToRaw(count: Int) {
        let from = currentGain.isFinite ? currentGain : 1
        let span = Double(max(count, 1))
        for index in 0..<count {
            let gain = from + (1 - from) * Double(index + 1) / span
            let x = frame[index].isFinite ? Double(frame[index]) * gain : 0
            output[index] = Float(min(1, max(-1, x)))
        }
        currentGain = 1
    }

    /// AGC2 出错（这一帧已经用了原声）。从 0 dB 重新开始（接着原声往上调，不会一下子变响），连续出错就不再调
    private func failed() {
        counters.failures += 1
        consecutiveFailures += 1
        if consecutiveFailures >= Self.maxConsecutiveFailures {
            disable("AGC2 连续出错 \(consecutiveFailures) 次")
            return
        }
        if EchoStage.worthLogging(counters.failures) {
            log("麦克风自动增益：AGC2 出错了（第 \(counters.failures) 次），这一帧用原声，从头开始再调")
        }
        counters.restarts += 1
        if !controller.reset(initialGain: 0) {
            disable("AGC2 出错，也没能从头开始")
        }
    }

    private func disable(_ why: String) {
        guard healthy else { return }
        healthy = false
        log("麦克风自动增益：\(why)，这次录音不再调，麦克风的声音原样录")
    }

    /// 处理到了新的麦克风的声音：旧的麦克风压着的先交出去，AGC2 从头开始（从一开始的增益重新调）
    private func restartForNewMic(into lane: inout SampleFIFO) {
        if pendingCount > 0 {
            counters.padded += 1
            processPending(into: &lane)
        }
        guard healthy else { return }
        counters.restarts += 1
        if controller.reset(initialGain: settings.initialGain) {
            currentGain = pow(10, settings.initialGain / 20)
            log("麦克风自动增益：换了麦克风，从头开始调（一开始调大 \(JSON.format(settings.initialGain)) dB）")
        } else {
            log("麦克风自动增益：换了麦克风，AGC2 没能从头开始，接着用原来的")
        }
    }

    private func energy(_ samples: [Float], count: Int) -> Double {
        var sum = 0.0
        for index in 0..<count {
            sum += Double(samples[index]) * Double(samples[index])
        }
        return sum
    }

    // MARK: 日志

    /// 每 60 秒（按处理的声音算）和结束时一行：调大了多少、有声音的时候处理前后的音量、出错的次数
    func logStats(final: Bool) {
        func decibels(_ ratio: Double) -> String {
            ratio > 0 ? JSON.format(10 * log10(ratio)) : "没有"
        }
        let frames = Double(Self.frameSize)
        var parts = [
            "麦克风自动增益\(final ? "（结束）" : "")：\(healthy ? "在调" : "不调了，原样录")",
            "处理了 \(counters.processed) 帧（\(JSON.format(Double(counters.processed) / 100)) 秒），原声 \(counters.bypassed) 帧",
            "现在调大 \(decibels(currentGain * currentGain)) dB，这一段最多 \(decibels(period.maxGain * period.maxGain)) dB",
        ]
        if period.loudFrames > 0 {
            let count = Double(period.loudFrames) * frames
            parts.append("有声音的 \(period.loudFrames) 帧（-60 dBFS 以上）：处理前 \(decibels(period.inputEnergy / count)) dBFS、"
                + "处理后 \(decibels(period.outputEnergy / count)) dBFS，平均调大 \(decibels(period.outputEnergy / period.inputEnergy)) dB")
        } else {
            parts.append("这一段没有 -60 dBFS 以上的声音")
        }
        parts.append("出错 \(counters.failures) 次，从头开始 \(counters.restarts) 次")
        log(parts.joined(separator: "；"))
        period = Period()
    }
}

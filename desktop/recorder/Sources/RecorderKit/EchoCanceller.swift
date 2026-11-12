import CRecorder
import Foundation

/// 回声消除器：一帧（480 个采样，48 kHz 的 10 毫秒）一帧地处理。只在一个线程上用（LiveEngine 的处理线程）。
/// 正式用的是 WebRTCEchoCanceller，测试里换成假的，能看到调用的顺序。
public protocol EchoCanceller: AnyObject {
    /// 处理过的声音比送进去的晚多少个采样（固定的，小于一帧）。EchoStage 靠它把做过、没做回声消除的声音无缝接起来
    var latency: Int { get }
    /// 喂一帧参考（电脑里放出来的声音）。返回 false 表示出错了
    func render(_ frame: UnsafePointer<Float>) -> Bool
    /// 处理一帧麦克风的声音，写到 output（晚 latency 个采样：前 latency 个是上一帧的）。
    /// 返回 false 表示出错了，这时 output 里的不能用
    func capture(_ input: UnsafePointer<Float>, into output: UnsafeMutablePointer<Float>) -> Bool
    /// 从头开始：之前学到的回声路径都不要了（里面压着的那 latency 个采样也没了）。返回 false 表示没能重新开始
    func reset() -> Bool
    /// 现在的统计，没有时是 nil
    func stats() -> EchoStats?
}

/// AEC3 报的统计，写日志用。没有的值是 nil
public struct EchoStats: Equatable {
    /// 估计的回声延迟（毫秒），包括 EchoStage 让参考提前的那一段
    public var delayMilliseconds: Double?
    /// 回声消掉了多少（dB）
    public var erle: Double?
    /// 回声本来比参考小多少（dB）
    public var erl: Double?
    /// 处理后还有回声的可能性（0…1）
    public var residualEchoLikelihood: Double?
    public var divergentFilterFraction: Double?

    public init(delayMilliseconds: Double? = nil, erle: Double? = nil, erl: Double? = nil,
                residualEchoLikelihood: Double? = nil, divergentFilterFraction: Double? = nil) {
        self.delayMilliseconds = delayMilliseconds
        self.erle = erle
        self.erl = erl
        self.residualEchoLikelihood = residualEchoLikelihood
        self.divergentFilterFraction = divergentFilterFraction
    }

    /// 一行日志：延迟 230 毫秒，ERLE 25.3 dB……
    var summary: String {
        func value(_ number: Double?, _ unit: String) -> String {
            number.map { "\(JSON.format($0))\(unit)" } ?? "没有"
        }
        return "AEC3 估计的延迟 \(value(delayMilliseconds, " 毫秒"))，ERLE \(value(erle, " dB"))，ERL \(value(erl, " dB"))，"
            + "残余回声的可能性 \(value(residualEchoLikelihood, ""))"
    }
}

/// WebRTC 的 AEC3（zr_aec.cc 包的 C 接口）。参数都在 C 那边查过，不会让 WebRTC abort 整个进程
public final class WebRTCEchoCanceller: EchoCanceller {
    public static let frameSize = Int(ZR_AEC_FRAME)

    /// 分频的滤波器组加上 AEC3 按 4 毫秒一块处理，处理过的声音晚 431 个采样（约 9 毫秒），测试里量过
    public let latency = Int(ZR_AEC_LATENCY)

    private let aec: OpaquePointer

    /// filters：AEC3 找延迟用的滤波器个数（1 到 16）；streamDelayMilliseconds：告诉 AEC3 的大概延迟，nil 不告诉。
    /// 建不起来时返回 nil（原因写在标准错误里）
    public init?(filters: Int, streamDelayMilliseconds: Int? = nil) {
        guard (1...16).contains(filters), (streamDelayMilliseconds ?? 0) >= 0, (streamDelayMilliseconds ?? 0) <= 500 else {
            return nil
        }
        var config = zr_aec_config(num_filters: Int32(filters), stream_delay_ms: Int32(streamDelayMilliseconds ?? -1))
        guard let aec = zr_aec_create(&config) else { return nil }
        self.aec = aec
    }

    deinit {
        zr_aec_destroy(aec)
    }

    public func render(_ frame: UnsafePointer<Float>) -> Bool {
        zr_aec_render(aec, frame, Self.frameSize) == ZR_AEC_OK
    }

    public func capture(_ input: UnsafePointer<Float>, into output: UnsafeMutablePointer<Float>) -> Bool {
        zr_aec_capture(aec, input, output, Self.frameSize) == ZR_AEC_OK
    }

    public func reset() -> Bool {
        zr_aec_reset(aec) == ZR_AEC_OK
    }

    public func stats() -> EchoStats? {
        var raw = zr_aec_stats()
        guard zr_aec_get_stats(aec, &raw) == ZR_AEC_OK else { return nil }
        func value(_ number: Double) -> Double? {
            number.isFinite ? number : nil
        }
        return EchoStats(delayMilliseconds: value(raw.delay_ms), erle: value(raw.erle_db), erl: value(raw.erl_db),
                         residualEchoLikelihood: value(raw.residual_echo_likelihood),
                         divergentFilterFraction: value(raw.divergent_filter_fraction))
    }
}

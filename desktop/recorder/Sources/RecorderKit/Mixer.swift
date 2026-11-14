import Foundation

/// 一路声音是麦克风还是电脑里的声音
public enum SourceKind: Equatable {
    case mic
    case system
}

/// 按时间混音。两路声音来自不同的设备，时钟不一样快，交数据也是一批一批、时快时慢的，
/// 所以不能「两边各有多少就混多少」。调用方按经过的时间算出这一次该出多少个采样（48 kHz），
/// 每一路从自己的队列里取：
/// - 刚开始（或者断过一次）先攒够 cushion 再开始取，抵消设备交数据的快慢；
/// - 取的时候不够就补静音（设备卡了、断了），下次重新攒；
/// - 积压（这一次该出的之外还攒着的）超过 maxBacklog 就丢掉最早的，只留 cushion。
/// 这样两路一直对得上，录一个小时也不会越录越偏。
/// 两路加起来超过满幅时由限幅器（MixLimiter）平滑地压下来，不削顶。
public final class TimeMixer {
    public final class Lane {
        public let kind: SourceKind
        /// 已经转成 48 kHz、等着混的采样
        public var queue = SampleFIFO()
        /// 攒够了，正在取
        public internal(set) var primed = false
        /// 补过多少个静音采样、丢过多少个积压的采样（写日志用）
        public internal(set) var paddedSamples = 0
        public internal(set) var droppedSamples = 0

        init(kind: SourceKind) {
            self.kind = kind
        }
    }

    public let lanes: [Lane]
    public let cushion: Int
    public let maxBacklog: Int
    private var limiter = MixLimiter()

    /// cushion 默认 0.15 秒，maxBacklog 默认 0.5 秒（48 kHz 的采样数）
    public init(kinds: [SourceKind], cushion: Int = 7_200, maxBacklog: Int = 24_000) {
        precondition(cushion >= 0 && maxBacklog > cushion)
        lanes = kinds.map(Lane.init)
        self.cushion = cushion
        self.maxBacklog = maxBacklog
    }

    /// 混出 count 个采样写到 out（先清零），energy[i] 是第 i 路这一段的平方和（算音量用，限幅之前的）。
    /// due：这一次一共该出多少。处理线程卡住以后分几次补上时，它是还没混的全部（包括这 count 个），
    /// 积压按它算：卡住期间录下、等着补上的声音不是积压，不能丢。不给时就是 count
    public func mix(count: Int, due: Int? = nil, into out: UnsafeMutablePointer<Float>, energy: inout [Double]) {
        out.initialize(repeating: 0, count: count)
        let due = max(count, due ?? count)
        for (index, lane) in lanes.enumerated() {
            energy[index] = take(count, due: due, from: lane, into: out)
        }
        limiter.process(out, count: count)
    }

    /// 各路攒着的都不要了，从头重新攒（继续录音时：暂停期间采到的声音不能编进去）
    public func reset() {
        for lane in lanes {
            lane.queue.removeAll()
            lane.primed = false
        }
        limiter.reset()
    }

    /// 暂停、结束录音时：各路剩下的都混出来，长度按最长的那一路（短的后面补静音）。返回混出的个数。
    public func drain(into out: inout [Float], energy: inout [Double]) -> Int {
        let count = lanes.map(\.queue.count).max() ?? 0
        out = [Float](repeating: 0, count: count)
        guard count > 0 else { return 0 }
        out.withUnsafeMutableBufferPointer { buffer in
            for (index, lane) in lanes.enumerated() {
                energy[index] = lane.queue.takeAdding(lane.queue.count, into: buffer.baseAddress!)
            }
            limiter.process(buffer.baseAddress!, count: count)
        }
        return count
    }

    private func take(_ count: Int, due: Int, from lane: Lane, into out: UnsafeMutablePointer<Float>) -> Double {
        if !lane.primed {
            guard lane.queue.count >= cushion else {
                lane.paddedSamples += count
                return 0
            }
            lane.primed = true
        }
        let backlog = lane.queue.count - due
        if backlog > maxBacklog {
            let excess = backlog - cushion
            lane.queue.drop(excess)
            lane.droppedSamples += excess
        }
        let available = min(count, lane.queue.count)
        let energy = lane.queue.takeAdding(available, into: out)
        if available < count {
            // 不够了：这次补静音，下次重新攒够再取
            lane.paddedSamples += count - available
            lane.primed = false
        }
        return energy
    }
}

/// 混音以后的限幅器：麦克风（自动增益调大了的）和电脑声音加起来超过 threshold（-1 dBFS）时，平滑地把音量压下来，
/// 不再把超出的部分直接削掉（削顶的地方听起来是破音，也会让识别认错字）。
///
/// 不加延迟：mix 一次混出多少就处理多少，录音的长度、暂停继续时的采样数都和以前一样。每次处理一整块，
/// 块里的峰值提前开始压（往下压最快 attackSamples 个采样从 1 压到 0，压到峰值那一刻正好够）；
/// 峰值离块的开头太近、来不及提前时，在块的开头压得陡一些（照样不超过 threshold）。
/// 没有峰值以后按 releaseSeconds 慢慢回到 1，回到 1 以后原样通过，一个比特都不变。
struct MixLimiter {
    /// -1 dBFS：离满幅留一点余量，AAC 编码、解码以后也不会超过满幅
    static let threshold: Float = 0.891_250_9
    /// 往下压最快的速度：2 毫秒（96 个采样）从 1 压到 0
    static let attackSamples: Float = 96
    /// 回到 1 的时间常数
    static let releaseSeconds = 0.1
    private static let release = Float(exp(-1 / (releaseSeconds * Double(AACEncoder.sampleRate))))

    /// 当前压低了多少（1 减去增益，0…1），跨块保留。记压低的量而不是增益：快回到 1 时每个采样只变很小一点，
    /// 增益离 1 太近时 Float 表示不了这么小的变化，会停在 0.9998 左右回不去
    private var reduction: Float = 0

    /// 当前的增益（0…1）
    var gain: Float { 1 - reduction }
    /// 每个采样最多能用的增益（处理时用的，留着下次接着用，免得每次都分配）
    private var bound: [Float] = []

    /// 从 1 重新开始（暂停以后继续时）
    mutating func reset() {
        reduction = 0
    }

    /// 原地处理 count 个采样。不是有限的数（NaN、无穷大）换成 0。
    mutating func process(_ samples: UnsafeMutablePointer<Float>, count: Int) {
        guard count > 0 else { return }
        if bound.count < count {
            bound = [Float](repeating: 0, count: count)
        }
        let step = 1 / Self.attackSamples
        var cut = reduction
        bound.withUnsafeMutableBufferPointer { bound in
            // 从后往前：每个采样最多能用多大的增益，才能让它自己和后面的峰值（按最快的压法）都不超过 threshold
            var next = Float.infinity
            for index in stride(from: count - 1, through: 0, by: -1) {
                if !samples[index].isFinite {
                    samples[index] = 0
                }
                let magnitude = abs(samples[index])
                let needed = magnitude > Self.threshold ? Self.threshold / magnitude : 1
                next = min(needed, next + step)
                bound[index] = next
            }
            // 从前往后：要压时按上面算的压，不用压时慢慢回到 1；离 1 很近、又不用压时直接回到 1
            for index in 0..<count {
                cut = max(1 - bound[index], cut * Self.release)
                if cut < 1e-5 && bound[index] >= 1 {
                    cut = 0
                }
                // 照理 1 - cut 不会大于 bound；压得很低时（大得离谱的采样）Float 的 1 - cut 精度不够，
                // 可能比 bound 大一点而超过 threshold，所以再取一次小的
                samples[index] *= min(1 - cut, bound[index])
            }
        }
        reduction = cut
    }
}

/// 音量（0…1）：按一段声音的均方根换算成分贝，-60 dB 及以下是 0，0 dB 是 1，中间按分贝均匀分布。
public enum LevelMeter {
    public static let floorDecibels = -60.0

    public static func level(energy: Double, count: Int) -> Double {
        guard count > 0, energy > 0 else { return 0 }
        let rms = (energy / Double(count)).squareRoot()
        let decibels = 20 * log10(rms)
        return min(1, max(0, (decibels - floorDecibels) / -floorDecibels))
    }
}

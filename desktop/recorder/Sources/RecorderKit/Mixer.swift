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
/// - 积压超过 maxBacklog 就丢掉最早的，只留 cushion。
/// 这样两路一直对得上，录一个小时也不会越录越偏。
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

    /// cushion 默认 0.15 秒，maxBacklog 默认 0.5 秒（48 kHz 的采样数）
    public init(kinds: [SourceKind], cushion: Int = 7_200, maxBacklog: Int = 24_000) {
        precondition(cushion >= 0 && maxBacklog > cushion)
        lanes = kinds.map(Lane.init)
        self.cushion = cushion
        self.maxBacklog = maxBacklog
    }

    /// 混出 count 个采样写到 out（先清零），energy[i] 是第 i 路这一段的平方和（算音量用）
    public func mix(count: Int, into out: UnsafeMutablePointer<Float>, energy: inout [Double]) {
        out.initialize(repeating: 0, count: count)
        for (index, lane) in lanes.enumerated() {
            energy[index] = take(count, from: lane, into: out)
        }
        clip(out, count: count)
    }

    /// 录音结束：各路剩下的都混出来，长度按最长的那一路（短的后面补静音）。返回混出的个数。
    public func drain(into out: inout [Float], energy: inout [Double]) -> Int {
        let count = lanes.map(\.queue.count).max() ?? 0
        out = [Float](repeating: 0, count: count)
        guard count > 0 else { return 0 }
        out.withUnsafeMutableBufferPointer { buffer in
            for (index, lane) in lanes.enumerated() {
                energy[index] = lane.queue.takeAdding(lane.queue.count, into: buffer.baseAddress!)
            }
            clip(buffer.baseAddress!, count: count)
        }
        return count
    }

    private func take(_ count: Int, from lane: Lane, into out: UnsafeMutablePointer<Float>) -> Double {
        if !lane.primed {
            guard lane.queue.count >= cushion else {
                lane.paddedSamples += count
                return 0
            }
            lane.primed = true
        }
        let backlog = lane.queue.count - count
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

    private func clip(_ samples: UnsafeMutablePointer<Float>, count: Int) {
        for index in 0..<count {
            samples[index] = min(1, max(-1, samples[index]))
        }
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

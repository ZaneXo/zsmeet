import Foundation

/// 一路声音（已经转成 48 kHz）的时间轴：第几个采样是什么时候采到的（主机时间，秒，和 DispatchTime 的
/// uptimeNanoseconds 是同一个时钟）。回声消除靠它把麦克风和电脑声音按时间对上（见 EchoStage）。
///
/// 时间戳来自 IOProc（每个缓冲一个，zr_stamp）。用最近 20 秒的时间戳按最小二乘拟合一条直线：
/// 时间戳本身的抖动被平均掉，设备时钟比主机时钟快一点慢一点（不同的设备能差 100 ppm 以上）也跟得上。
/// 新的时间戳离直线超过 2 毫秒（丢了采样、设备停过、睡眠过）就从它开始新的一段。上一段还留着，
/// 新一段开始之前的采样、时间照旧按上一段算；两段之间空出来的那段时间没有采样（locate 返回 gap）。
/// 换了设备（换了麦克风，见 split）时，不管差多少都从新设备的第一个时间戳开始新的一段。
///
/// 没有时间戳的来源（设备给的时间不可信时）按收到数据的时间粗略对齐：差 0.1 秒以上才重新对齐。
struct Timeline {
    static let nominalSecondsPerSample = 1 / Resampler.outputRate
    /// 时钟快慢最多差这么多（相对）。拟合出来超过这个数说明时间戳不可信，按标称的 48 kHz 算
    static let maxRateError = 0.005
    /// 拟合至少要跨这么多秒，太短的话斜率不准，先按标称的算
    static let minimumFitSpan = 0.5
    /// 拟合用最近这么多秒的时间戳
    static let window = 20.0
    /// 最多留这么多个时间戳（IOProc 一般 10 毫秒一个，20 秒是 2000 个）
    static let maxPoints = 4_096
    /// 时间戳离直线超过这么多秒算断开了
    var jumpThreshold = 0.002
    /// 按收到数据的时间对齐时，差这么多秒以上才重新对齐（收到的时间本来就抖 20 毫秒左右）
    var arrivalThreshold = 0.1

    /// 一条直线：采样 originIndex 的时间是 originTime，每个采样 secondsPerSample 秒；这一段从 startIndex
    /// （startTime 秒）开始
    struct Line: Equatable {
        var originIndex: Double
        var originTime: Double
        var secondsPerSample: Double
        var startIndex: Double
        var startTime: Double

        func time(at index: Double) -> Double {
            originTime + (index - originIndex) * secondsPerSample
        }

        func index(at time: Double) -> Double {
            originIndex + (time - originTime) / secondsPerSample
        }
    }

    /// 某个时间在这一路的哪里
    enum Location: Equatable {
        /// 第几个采样（可以带小数）
        case index(Double)
        /// 两段之间断开的那段时间，没有采样
        case gap
        /// 还没有时间戳
        case unknown
    }

    private var indices: [Double] = []
    private var times: [Double] = []
    /// 现在这一段、上一段
    private(set) var line: Line?
    private(set) var previous: Line?
    /// 从头开始过几段（第一次对上不算）
    private(set) var restarts = 0
    /// 上一次重新开始时差了多少秒（日志用）
    private(set) var lastJump = 0.0
    private var sinceFit = 0
    /// 下一个时间戳（或者按收到数据的时间对齐的一次）从新的一段开始，见 split
    private var splitPending = false

    var isAnchored: Bool {
        line != nil
    }

    /// 测得的采样率（每秒多少个采样）
    var rate: Double {
        1 / (line?.secondsPerSample ?? Self.nominalSecondsPerSample)
    }

    /// 采样 index 是什么时候采到的。还没有时间戳时是 nil
    func time(at index: Double) -> Double? {
        guard let line else { return nil }
        if index < line.startIndex, let previous, index >= previous.startIndex {
            return previous.time(at: index)
        }
        return line.time(at: index)
    }

    /// 什么时候采到的是第几个采样（可以带小数）。在断开的空当里、还没有时间戳时是 nil
    func index(at time: Double) -> Double? {
        if case .index(let index) = locate(time) {
            return index
        }
        return nil
    }

    func locate(_ time: Double) -> Location {
        guard let line else { return .unknown }
        // 这一段的第一个时间戳之前一点点（不到一个采样）也算这一段
        if time >= line.startTime - line.secondsPerSample {
            return .index(line.index(at: time))
        }
        // 第一段之前：没有更早的了，照这一段往前推
        guard let previous else { return .index(line.index(at: time)) }
        // 上一段一直到这一段开始的那个采样为止；再往后到这一段开始之前，是断开的空当
        if time <= previous.time(at: line.startIndex) {
            return .index(previous.index(at: time))
        }
        return .gap
    }

    /// 加一个时间戳：采样 index 是在 time 秒采到的。返回 true 表示和之前的对不上，从这里开始了新的一段
    @discardableResult
    mutating func add(index: Double, time: Double) -> Bool {
        guard index.isFinite, time.isFinite else { return false }
        guard let line, !splitPending else {
            splitPending = false
            start(index: index, time: time, secondsPerSample: Self.nominalSecondsPerSample)
            return false
        }
        let error = time - line.time(at: index)
        if abs(error) > jumpThreshold {
            restarts += 1
            lastJump = error
            start(index: index, time: time, secondsPerSample: line.secondsPerSample)
            return true
        }
        indices.append(index)
        times.append(time)
        trim()
        sinceFit += 1
        // 刚开始的时候每个都拟合，之后每 8 个拟合一次（拟合要把留着的都过一遍）
        if indices.count < 64 || sinceFit >= 8 {
            fit()
        }
        return false
    }

    /// 没有时间戳的来源：到 time 秒为止收到了 endIndex 个采样。返回 true 表示重新对齐了
    @discardableResult
    mutating func addArrival(endIndex: Double, time: Double) -> Bool {
        guard endIndex.isFinite, time.isFinite else { return false }
        guard let line, !splitPending else {
            splitPending = false
            start(index: endIndex, time: time, secondsPerSample: Self.nominalSecondsPerSample)
            return false
        }
        let error = time - line.time(at: endIndex)
        guard abs(error) > arrivalThreshold else { return false }
        restarts += 1
        lastJump = error
        start(index: endIndex, time: time, secondsPerSample: Self.nominalSecondsPerSample)
        return true
    }

    /// 全部不要了（设备换了采样率：序号和时间的关系变了），等下一个时间戳重新开始
    mutating func reset() {
        indices.removeAll(keepingCapacity: true)
        times.removeAll(keepingCapacity: true)
        line = nil
        previous = nil
        sinceFit = 0
        splitPending = false
    }

    /// 换了设备（换了麦克风）：下一个时间戳不管和现在这一段对不对得上，都从它开始新的一段（新的设备有自己的时钟，
    /// 采样率从标称的重新测）。现在这一段留着当上一段：新一段开始之前的采样照旧按它算时间
    mutating func split() {
        splitPending = true
    }

    private mutating func start(index: Double, time: Double, secondsPerSample: Double) {
        previous = line
        indices.removeAll(keepingCapacity: true)
        times.removeAll(keepingCapacity: true)
        indices.append(index)
        times.append(time)
        line = Line(originIndex: index, originTime: time, secondsPerSample: secondsPerSample, startIndex: index, startTime: time)
        sinceFit = 0
    }

    /// 只留最近 window 秒、最多 maxPoints 个
    private mutating func trim() {
        guard let newest = times.last else { return }
        var drop = 0
        while drop < times.count - 1 && (newest - times[drop] > Self.window || times.count - drop > Self.maxPoints) {
            drop += 1
        }
        // 攒多一点再删，免得每加一个就挪一遍数组
        if drop >= 256 || times.count > Self.maxPoints + 256 {
            indices.removeFirst(drop)
            times.removeFirst(drop)
        }
    }

    /// 按留着的时间戳拟合直线 time = a + b × index（中心化以后算，数值稳定）
    private mutating func fit() {
        sinceFit = 0
        guard var fitted = line, let newest = times.last else { return }
        // 只用 window 秒以内的（trim 是攒着删的，开头可能还有更旧的）
        var first = 0
        while first < times.count - 1 && newest - times[first] > Self.window {
            first += 1
        }
        let count = Double(times.count - first)
        var meanIndex = 0.0
        var meanTime = 0.0
        for i in first..<times.count {
            meanIndex += indices[i]
            meanTime += times[i]
        }
        meanIndex /= count
        meanTime /= count
        if newest - times[first] >= Self.minimumFitSpan {
            var covariance = 0.0
            var variance = 0.0
            for i in first..<times.count {
                let dk = indices[i] - meanIndex
                covariance += dk * (times[i] - meanTime)
                variance += dk * dk
            }
            if variance > 0 {
                let slope = covariance / variance
                fitted.secondsPerSample = abs(slope / Self.nominalSecondsPerSample - 1) <= Self.maxRateError
                    ? slope : Self.nominalSecondsPerSample
            }
        }
        // 直线一定过这些点的中心；跨的时间还短时斜率沿用原来的
        fitted.originIndex = meanIndex
        fitted.originTime = meanTime
        line = fitted
    }
}

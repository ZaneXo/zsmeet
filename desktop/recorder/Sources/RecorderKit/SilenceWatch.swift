import Foundation

/// 麦克风有没有声音。在 LiveEngine 的处理线程上读环形缓冲里的原始采样（还没转采样率，也没经过回声消除，
/// 所以回声消除影响不到它），每 0.1 秒算一次音量：
///
/// - 这个麦克风刚开始录的 5 秒一直全是 0（一点底噪都没有）：多半是被静音了、没有权限，马上提醒
///   （mic_unavailable，只看开头，暂停的时间也算，和以前一样）。
/// - 录音中（不算暂停）连续 30 秒全是 0，或者连续 1 分钟都比 -90 dBFS 还小：多半是开着的会议软件用它的语音处理
///   占住了麦克风（别的程序录到的就是静音，或者小得多的声音）、麦克风被静音了，或者录的不是正在用的麦克风（mic_silent）。
///   -90 dBFS 比安静房间里麦克风录到的底噪（一般在 -75 到 -60 dBFS）小得多，说话（-40 到 -20 dBFS）更不用说，
///   一般的麦克风在再安静的房间里也不会误报（底噪特别低的专业声卡把增益调到最小时可能会）。有的耳麦、降噪软件的
///   虚拟麦克风在没人说话时给的就是 0，一直不说话超过 30 秒也会提醒，所以提醒里说的是「如果这段时间你说过话」。
///
/// 一段静音只提醒一次（开头那 5 秒的提醒也算这一段的）。中间有 0.1 秒超过 -90 dBFS 就算恢复了，
/// 之后再静音那么久会再提醒。暂停时录到的不算时间，但有声音照样算恢复。
struct SilenceWatch {
    /// 刚开始录的这么多秒一直全是 0 就提醒（mic_unavailable）
    static let startupSeconds = 5.0
    /// 录音中连续这么多秒全是 0 就提醒（mic_silent）
    static let zeroSeconds = 30.0
    /// 录音中连续这么多秒都比 quietDecibels 小就提醒（mic_silent）
    static let quietSeconds = 60.0
    /// 0.1 秒的均方根比它小算「几乎没有声音」
    static let quietDecibels = -90.0
    static let quietLevel = pow(10, quietDecibels / 20)
    /// 每个采样的绝对值都不超过它（-120 dBFS）算「全是 0」：有的设备给的是极小的数，不是正好 0
    static let zeroLevel: Float = 1e-6
    /// 多长算一次音量
    static let windowSeconds = 0.1

    enum Alert: Equatable {
        /// 刚开始录就一直全是 0（mic_unavailable）
        case startup
        /// 录音中很长一段时间没有声音（mic_silent）。digital：全是 0；seconds：已经多久了（不算暂停）
        case silent(digital: Bool, seconds: Double)
        /// 提醒过的那段静音过去了，又有声音了（只写日志）。seconds：静音了多久（不算暂停）
        case recovered(seconds: Double)
    }

    /// 刚开始的检查：还在看（到现在全是 0），一共看了多少个采样
    private var startupPending = true
    private var startupSamples = 0
    /// 这 0.1 秒到现在的采样数、平方和、最大的绝对值
    private var windowCount = 0
    private var windowEnergy = 0.0
    private var windowPeak: Float = 0
    /// 现在这一段静音：一共多少秒（不算暂停），其中最近连续全是 0 的多少秒；这一段提醒过没有
    private(set) var quietRun = 0.0
    private(set) var zeroRun = 0.0
    private(set) var warned = false

    /// 新读到的一批原始采样（sampleRate 是它们的采样率）。paused：录音暂停着。返回要提醒、要写日志的事
    mutating func feed(_ samples: UnsafeBufferPointer<Float>, sampleRate: Double, paused: Bool) -> [Alert] {
        guard !samples.isEmpty, sampleRate.isFinite, sampleRate > 0 else { return [] }
        var alerts: [Alert] = []
        if startupPending {
            checkStartup(samples, sampleRate: sampleRate, alerts: &alerts)
        }
        let size = max(1, Int((sampleRate * Self.windowSeconds).rounded()))
        var offset = 0
        while offset < samples.count {
            // 采样率刚变小时这一段可能已经超过新的长度了：至少再算一个
            let n = min(max(size - windowCount, 1), samples.count - offset)
            for index in offset..<(offset + n) {
                let sample = samples[index]
                windowEnergy += Double(sample * sample)
                windowPeak = max(windowPeak, abs(sample))
            }
            windowCount += n
            offset += n
            if windowCount >= size {
                finishWindow(seconds: Double(windowCount) / sampleRate, paused: paused, alerts: &alerts)
            }
        }
        return alerts
    }

    /// 刚开始录的 5 秒：有一个采样不是 0 就不再看了；一直全是 0 就提醒一次（这一段静音算提醒过了）
    private mutating func checkStartup(_ samples: UnsafeBufferPointer<Float>, sampleRate: Double, alerts: inout [Alert]) {
        // 写成「不是不超过」：NaN 也算有东西（坏了的数据不当成静音）
        if samples.contains(where: { !(abs($0) <= Self.zeroLevel) }) {
            startupPending = false
            return
        }
        startupSamples += samples.count
        if Double(startupSamples) >= Self.startupSeconds * sampleRate {
            startupPending = false
            warned = true
            alerts.append(.startup)
        }
    }

    /// 0.1 秒满了：有声音就结束这一段静音；没有就加上这 0.1 秒（暂停时不加），够长了提醒一次
    private mutating func finishWindow(seconds: Double, paused: Bool, alerts: inout [Alert]) {
        let rms = (windowEnergy / Double(windowCount)).squareRoot()
        let digital = windowPeak <= Self.zeroLevel
        let quiet = digital || rms < Self.quietLevel
        windowCount = 0
        windowEnergy = 0
        windowPeak = 0
        guard quiet else {
            if warned {
                alerts.append(.recovered(seconds: quietRun))
            }
            quietRun = 0
            zeroRun = 0
            warned = false
            return
        }
        guard !paused else { return }
        quietRun += seconds
        zeroRun = digital ? zeroRun + seconds : 0
        guard !warned else { return }
        // 0.1 秒一次地加，加到 30 秒时浮点数可能还差一点点
        if zeroRun >= Self.zeroSeconds - 1e-6 {
            warned = true
            alerts.append(.silent(digital: true, seconds: zeroRun))
        } else if quietRun >= Self.quietSeconds - 1e-6 {
            warned = true
            alerts.append(.silent(digital: false, seconds: quietRun))
        }
    }

    /// mic_silent 的提醒（给用户看）。digital：全是 0
    static func message(name: String, digital: Bool) -> String {
        let what = digital
            ? "已经 \(duration(zeroSeconds))录到的全是静音（连一点底噪都没有）"
            : "已经 \(duration(quietSeconds))几乎录不到声音（比安静房间里的底噪还小得多）"
        return "麦克风「\(name)」\(what)。如果这段时间你说过话，那些话没录进去：可能是开着的会议软件占住了麦克风"
            + "（它的降噪、回声消除会让别的程序录不到声音），也可能是麦克风被静音了，或者录的不是你正在用的麦克风。"
            + "请看看麦克风的静音开关，在「系统设置 → 声音 → 输入」里对着它说话，看「输入电平」动不动；"
            + "是会议软件占着的话，结束这次录音，换一个麦克风（比如耳机上的）再录。"
    }

    /// 给人看的时长：30 秒、1 分钟
    static func duration(_ seconds: Double) -> String {
        let whole = Int(seconds.rounded())
        return whole >= 60 && whole % 60 == 0 ? "\(whole / 60) 分钟" : "\(whole) 秒"
    }
}

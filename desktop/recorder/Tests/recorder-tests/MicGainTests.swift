import Accelerate
import CRecorder
import Foundation
@testable import RecorderKit

/// 麦克风自动增益（MicGainStage + WebRTC 的 AGC2）。调得对不对：真的 AGC2，合成的像说话的声音（见 EchoQualityTests 的
/// speechLike），不放声音、不要设备，门槛是量到的（2026-11，M4 Pro，见每个测试里的注释）留了余量；升级 WebRTC、
/// 改了 GainSettings 以后要重新量。接得对不对：假的 AGC（FakeGain），看帧、采样数、从头开始的地方
let micGainTests: [(String, () throws -> Void)] = [
    ("麦克风自动增益（AGC2）：C 接口参数不对时返回错误，不会让程序崩溃", testGainShim),
    ("麦克风自动增益（AGC2）：-40 dBFS 的小声说话调到 -22 dBFS 左右（-26 到 -19），开头 5 秒就到 -30 以上", testGainQuietSpeech),
    ("麦克风自动增益（AGC2）：-55 dBFS 的很小声也调到 -27 以上，最多调大 30 dB", testGainVeryQuietSpeech),
    ("麦克风自动增益（AGC2）：没人说话时不往上调：只有底噪时不比一开始多调大，说话停下来时增益不变，底噪响时调小",
     testGainNoise),
    ("麦克风自动增益（AGC2）：大声说话不超过满幅、几乎不碰顶，3 秒内增益回到 0 dB", testGainLoudSpeech),
    ("麦克风自动增益（AGC2）：按 10 毫秒一帧，进多少出多少，怎么分批交进来出来的都一样，增益平滑地变（没有跳变）",
     testGainChunkingAndSmoothness),
    ("麦克风自动增益（AGC2）：每 10 毫秒一帧不到一个核的 0.5%（机器很忙时 1%）", testGainSpeed),
    ("麦克风自动增益：出错的那一帧平滑地回到原声，从 0 dB 重新调；连续出错就不再调，录音不停", testGainFailures),
    ("麦克风自动增益：暂停时压着的先交出，继续时暂停期间的不要，结束时全部交出", testGainPauseResumeFinish),
    ("麦克风自动增益：接在回声消除后面，暂停、继续以后进多少出多少，换麦克风时正好从新的麦克风的第一个采样从头调",
     testGainAfterEchoStage),
    ("麦克风自动增益：现场录音里先回声消除再调音量，暂停、继续、换麦克风、结束，电脑声音那一路不动", testEngineGainWithEcho),
    ("麦克风自动增益：off 时不经过它，麦克风那一路一个比特都不变；建不起来时原样录，报 unavailable",
     testEngineGainOffAndUnavailable),
]

// MARK: 测试用的

/// 假的自动增益：每帧乘以 factor（不晚），记下每次处理的帧的第一个、最后一个采样，和从头开始时已经处理过几帧
final class FakeGain: GainController {
    var factor: Float
    private(set) var frames: [(first: Float, last: Float)] = []
    private(set) var resets: [(afterFrames: Int, initialGain: Double)] = []
    /// 第几次处理（从 0 数）时出错
    var failProcess: (Int) -> Bool = { _ in false }
    var resetSucceeds = true

    init(factor: Float = 1) {
        self.factor = factor
    }

    func process(_ input: UnsafePointer<Float>, into output: UnsafeMutablePointer<Float>) -> Bool {
        let call = frames.count
        frames.append((input[0], input[MicGainStage.frameSize - 1]))
        for index in 0..<MicGainStage.frameSize {
            output[index] = input[index] * factor
        }
        return !failProcess(call)
    }

    func reset(initialGain: Double) -> Bool {
        resets.append((frames.count, initialGain))
        return resetSucceeds
    }
}

/// 假的回声消除器：不晚，出来的是送进去的相反数。看得出麦克风的声音先经过了回声消除、再经过自动增益
private final class NegatingCanceller: EchoCanceller {
    let latency = 0
    private(set) var captures = 0

    func render(_ frame: UnsafePointer<Float>) -> Bool {
        true
    }

    func capture(_ input: UnsafePointer<Float>, into output: UnsafeMutablePointer<Float>) -> Bool {
        captures += 1
        for index in 0..<EchoStage.frameSize {
            output[index] = -input[index]
        }
        return true
    }

    func reset() -> Bool {
        true
    }

    func stats() -> EchoStats? {
        nil
    }
}

/// 跑一遍自动增益：input 按 blocks 里的大小一批一批交进去（轮着用），结束时全部交出
func runMicGain(_ input: [Float], controller: GainController, settings: GainSettings = GainSettings(),
                blocks: [Int] = [960]) -> (output: [Float], stage: MicGainStage) {
    let stage = MicGainStage(controller: controller, settings: settings)
    var lane = SampleFIFO()
    var output: [Float] = []
    output.reserveCapacity(input.count)
    var offset = 0
    var batch = 0
    input.withUnsafeBufferPointer { all in
        while offset < all.count {
            let count = min(blocks[batch % blocks.count], all.count - offset)
            batch += 1
            stage.process(UnsafeBufferPointer(rebasing: all[offset..<(offset + count)]), end: Int64(offset + count), into: &lane)
            offset += count
            lane.withUnsafeSamples { output.append(contentsOf: $0) }
            lane.removeAll()
        }
    }
    stage.finish(into: &lane)
    lane.withUnsafeSamples { output.append(contentsOf: $0) }
    return (output, stage)
}

/// 用真的 AGC2（默认参数）跑一遍
private func agc2(_ input: [Float], blocks: [Int] = [960]) -> [Float] {
    guard let controller = WebRTCGainController(settings: GainSettings()) else {
        Harness.fail("建不了 AGC2")
        return input
    }
    let output = runMicGain(input, controller: controller, blocks: blocks).output
    expectEqual(output.count, input.count, "进多少出多少")
    return output
}

/// 合成的说话声：说话的时候均方根是 level dBFS
private func speech(_ level: Double, seconds: Double, seed: UInt64 = 31, active: [Range<Double>]? = nil) -> [Float] {
    speechLike(seconds: seconds, seed: seed, rms: Float(pow(10, level / 20)), active: active ?? [0..<seconds],
               formants: (550, 1_500), pitch: 120)
}

/// 均方根是 level dBFS 的白噪声（-1…1 均匀分布的均方根是 1/√3）
private func noise(_ count: Int, dbfs level: Double, seed: UInt64) -> [Float] {
    scaled(whiteNoise(count, seed: seed), Float(pow(10, level / 20) * 3.0.squareRoot()))
}

private func dbfs(_ energy: Double, count: Int) -> Double {
    10 * log10(max(energy / Double(max(count, 1)), 1e-30))
}

/// 说话的那些 10 毫秒（clean 里比它自己说话时的平均音量小不到 10 dB 的帧）里，signal 的平均音量（dBFS，均方根）
func activeLevel(_ signal: [Float], clean: [Float], from: Double, to: Double) -> Double {
    let frames = min(signal.count, clean.count) / 480
    let energies = (0..<frames).map { energy(clean[($0 * 480)..<(($0 + 1) * 480)]) }
    let voiced = energies.filter { $0 > 0 }
    guard !voiced.isEmpty else { return -.infinity }
    let threshold = voiced.reduce(0, +) / Double(voiced.count) * 0.1
    var sum = 0.0
    var count = 0
    for frame in max(Int(from * 100), 0)..<min(Int(to * 100), frames) where energies[frame] > threshold {
        sum += energy(signal[(frame * 480)..<((frame + 1) * 480)])
        count += 480
    }
    return dbfs(sum, count: count)
}

/// clean 里一点声音都没有的那些 10 毫秒里（只有底噪），signal 的平均音量
private func pauseLevel(_ signal: [Float], clean: [Float], from: Double, to: Double) -> Double {
    var sum = 0.0
    var count = 0
    for frame in Int(from * 100)..<min(Int(to * 100), min(signal.count, clean.count) / 480) {
        let range = (frame * 480)..<((frame + 1) * 480)
        if energy(clean[range]) == 0 {
            sum += energy(signal[range])
            count += 480
        }
    }
    return dbfs(sum, count: count)
}

/// 每 10 毫秒的增益（dB）：处理后和处理前的能量比。处理前太小（-80 dBFS 以下）的是 nil
private func frameGains(input: [Float], output: [Float]) -> [Double?] {
    (0..<(min(input.count, output.count) / 480)).map { frame in
        let range = (frame * 480)..<((frame + 1) * 480)
        let e = energy(input[range])
        return e > 480 * 1e-8 ? 10 * log10(energy(output[range]) / e) : nil
    }
}

private func peakDecibels(_ samples: [Float]) -> Double {
    20 * log10(Double(samples.lazy.map { abs($0) }.max() ?? 0))
}

private func report(_ text: String) {
    print("    测得：\(text)")
}

private func format(_ value: Double) -> String {
    String(format: "%.1f", value)
}

/// 采样的值就是它的序号（从 1 开始），看得出哪个采样到了哪里
private func sequence(_ count: Int, from offset: Int = 0) -> [Float] {
    (0..<count).map { Float($0 + offset + 1) }
}

// MARK: AGC2 调得对不对

func testGainShim() {
    expect(zr_agc_create(nil) == nil, "没有配置")
    let good = zr_agc_config(max_gain_db: 30, initial_gain_db: 10, headroom_db: 5, max_gain_change_db_per_second: 6,
                             max_output_noise_level_dbfs: -40)
    var bad: [(String, zr_agc_config)] = []
    func with(_ name: String, _ change: (inout zr_agc_config) -> Void) {
        var config = good
        change(&config)
        bad.append((name, config))
    }
    with("最大增益 0") { $0.max_gain_db = 0 }
    with("最大增益 51") { $0.max_gain_db = 51 }
    with("最大增益 NaN") { $0.max_gain_db = .nan }
    with("一开始的增益比最大的大") { $0.initial_gain_db = 31 }
    with("一开始的增益是负的") { $0.initial_gain_db = -1 }
    with("留的余量 31 dB") { $0.headroom_db = 31 }
    with("每秒变 0 dB") { $0.max_gain_change_db_per_second = 0 }
    with("每秒变无穷大") { $0.max_gain_change_db_per_second = .infinity }
    with("底噪上限 1 dBFS") { $0.max_output_noise_level_dbfs = 1 }
    with("底噪上限 -91 dBFS") { $0.max_output_noise_level_dbfs = -91 }
    for (name, config) in bad {
        var copy = config
        expect(zr_agc_create(&copy) == nil, "配置不对（\(name)）也建起来了")
    }
    var config = good
    guard let agc = zr_agc_create(&config) else {
        Harness.fail("建不了自动增益")
        return
    }
    defer { zr_agc_destroy(agc) }
    zr_agc_destroy(nil)
    var frame = [Float](repeating: 0.1, count: 481)
    var output = [Float](repeating: 0, count: 481)
    let badArgument = Int32(ZR_AGC_BAD_ARGUMENT)
    expectEqual(zr_agc_process(agc, frame, &output, 479), badArgument, "不是 480 个采样")
    expectEqual(zr_agc_process(agc, nil, &output, 480), badArgument)
    expectEqual(zr_agc_process(agc, frame, nil, 480), badArgument)
    expectEqual(zr_agc_process(nil, frame, &output, 480), badArgument)
    expectEqual(zr_agc_reset(nil, 0), badArgument)
    expectEqual(zr_agc_reset(agc, 31), badArgument, "比最大的增益还大")
    expectEqual(zr_agc_reset(agc, -1), badArgument)
    expectEqual(zr_agc_reset(agc, .nan), badArgument)
    // 不是有限的数、超出 -1…1 的：照样处理，出来的都是有限的数，不超过满幅，不会把估计的音量弄坏
    frame[0] = .nan
    frame[1] = .infinity
    frame[2] = -40
    for _ in 0..<50 {
        expectEqual(zr_agc_process(agc, frame, &output, 480), Int32(ZR_AGC_OK))
    }
    expect(output[0..<480].allSatisfy { $0.isFinite && abs($0) <= 1 }, "输出都是 -1…1 的有限的数")
    // 输入和输出可以是同一块内存
    var inPlace = [Float](repeating: 0.001, count: 480)
    let status = inPlace.withUnsafeMutableBufferPointer { zr_agc_process(agc, $0.baseAddress!, $0.baseAddress!, 480) }
    expectEqual(status, Int32(ZR_AGC_OK))
    expect(inPlace.allSatisfy { $0 >= 0.001 && $0 < 0.0033 }, "调大了，最多一开始的 10 dB：\(inPlace[0])")
    // 从 0 dB 重新开始：小声的一帧原样出来
    expectEqual(zr_agc_reset(agc, 0), Int32(ZR_AGC_OK))
    let quiet = [Float](repeating: 0.001, count: 480)
    expectEqual(zr_agc_process(agc, quiet, &output, 480), Int32(ZR_AGC_OK))
    expect(zip(quiet, output).allSatisfy { abs($0 - $1) <= 1e-9 }, "0 dB 时原样：\(output[0])")
    // Swift 那层也一样：参数不对时建不起来
    var wrong = GainSettings()
    wrong.maxGain = 0
    expect(WebRTCGainController(settings: wrong) == nil)
    wrong = GainSettings()
    wrong.initialGain = 40
    expect(WebRTCGainController(settings: wrong) == nil)
    expect(WebRTCGainController(settings: GainSettings()) != nil)
}

func testGainQuietSpeech() {
    // 40 秒小声说话（-40 dBFS，像 MacBook 自带的麦克风录一两米外的人），底噪 -70 dBFS
    let clean = speech(-40, seconds: 40)
    let input = add(clean, noise(clean.count, dbfs: -70, seed: 5))
    let output = agc2(input)
    let first = activeLevel(output, clean: clean, from: 0, to: 5)
    let steady = activeLevel(output, clean: clean, from: 10, to: 40)
    let speechGain = steady - activeLevel(input, clean: clean, from: 10, to: 40)
    let pauseGain = pauseLevel(output, clean: clean, from: 10, to: 40) - pauseLevel(input, clean: clean, from: 10, to: 40)
    let gains = frameGains(input: input, output: output).compactMap { $0 }
    let peak = peakDecibels(output)
    report("说话 \(format(activeLevel(input, clean: clean, from: 0, to: 40))) dBFS → 开头 5 秒 \(format(first))、10 秒以后 \(format(steady)) dBFS"
        + "（调大 \(format(speechGain)) dB）；停顿时的底噪调大 \(format(pauseGain)) dB；最大增益 \(format(gains.max() ?? 0)) dB；峰值 \(format(peak)) dBFS")
    // 量到的：说话 -39.4 dBFS → 开头 5 秒 -25.6、10 秒以后 -23.5（调大 15.9 dB）；停顿时的底噪 +16.0 dB（和说话一样）；
    // 最大增益 16.6 dB；峰值 -5.9 dBFS。真的说话声（macOS 的 say 合成的中文）同样 -40 dBFS 时调到 -21 左右
    //（AGC2 按峰值留余量，这种声音的峰值和均方根差得少一点）
    expect(steady >= -26 && steady <= -19, "10 秒以后说话的音量是 \(steady) dBFS，不在 -26 到 -19 之间")
    expect(first >= -30, "开头 5 秒说话的音量只有 \(first) dBFS")
    expect(pauseGain <= speechGain + 1, "停顿时的底噪调大了 \(pauseGain) dB，比说话（\(speechGain) dB）还多")
    expect((gains.max() ?? 0) <= 30.01, "增益超过了 30 dB：\(gains.max() ?? 0)")
    expect(peak < 0 && peak > -12, "峰值 \(peak) dBFS")
}

func testGainVeryQuietSpeech() {
    // -55 dBFS（离得更远，或者系统设置里的输入音量很小），底噪 -80 dBFS
    let clean = speech(-55, seconds: 40, seed: 32)
    let input = add(clean, noise(clean.count, dbfs: -80, seed: 6))
    let output = agc2(input)
    let steady = activeLevel(output, clean: clean, from: 30, to: 40)
    let gains = frameGains(input: input, output: output).compactMap { $0 }
    report("-55 dBFS 的说话：30 秒以后 \(format(steady)) dBFS，最大增益 \(format(gains.max() ?? 0)) dB")
    // 量到的：-24.1 dBFS，增益到了上限 30 dB；-50 dBFS 时 -22.3（27 dB），-60 时 -27（30 dB，调到头了）
    expect(steady >= -27, "很小声的说话只调到 \(steady) dBFS")
    expect((gains.max() ?? 0) <= 30.01, "增益超过了 30 dB：\(gains.max() ?? 0)")
}

func testGainNoise() {
    // 只有底噪（没人说话）30 秒：AGC2 只在很有把握是在说话时往上调，增益一直是一开始的 10 dB（白噪声偶尔会被
    // 当成一小段说话，多调大不到 1 dB）；底噪响（-45 dBFS）时调小，让调大以后的底噪不超过 -40 dBFS
    var results: [String] = []
    for (level, seed) in [(-70.0, UInt64(3)), (-55.0, 13)] {
        let input = noise(30 * 48_000, dbfs: level, seed: seed)
        let output = agc2(input)
        let gains = frameGains(input: input, output: output).compactMap { $0 }
        let maxGain = gains.max() ?? 0
        results.append("底噪 \(format(level)) dBFS：最大增益 \(format(maxGain)) dB")
        // 量到的：都一直是 10.0 dB（换一个随机数种子时，-55 dBFS 的白噪声有一次多调大了 0.9 dB）
        expect(maxGain <= GainSettings().initialGain + 2, "只有 \(level) dBFS 的底噪，增益却到了 \(maxGain) dB")
    }
    let loud = noise(30 * 48_000, dbfs: -45, seed: 23)
    let loudOutput = agc2(loud)
    let tail = Array(loudOutput[(20 * 48_000)...])
    let tailLevel = dbfs(energy(tail[...]), count: tail.count)
    results.append("底噪 -45 dBFS：20 秒以后 \(format(tailLevel)) dBFS")
    // 量到的：-39.5 dBFS（AGC2 按最安静的那些帧估计底噪，比平均的小一点）
    expect(tailLevel <= -38.5, "-45 dBFS 的底噪调到了 \(tailLevel) dBFS")

    // 说 10 秒，停 20 秒（只有底噪），再说 10 秒：停的时候增益不变，不会把底噪越调越大
    let clean = speech(-40, seconds: 40, seed: 33, active: [0..<10, 30..<40])
    let input = add(clean, noise(clean.count, dbfs: -65, seed: 4))
    let output = agc2(input)
    let gains = frameGains(input: input, output: output)
    let before = gains[980..<1_000].compactMap { $0 }.last ?? 0
    let pause = gains[1_050..<2_950].compactMap { $0 }
    results.append("说话停下来的 20 秒里增益 \(format(pause.min() ?? 0)) 到 \(format(pause.max() ?? 0)) dB（停之前 \(format(before)) dB）")
    report(results.joined(separator: "；"))
    // 量到的：停之前 15.5 dB，停的 20 秒里一直是 15.5 dB
    expect((pause.max() ?? 99) <= before + 0.1, "停下来以后增益往上调了：\(pause.max() ?? 0) dB，停之前 \(before) dB")
}

func testGainLoudSpeech() {
    // 大声说话：峰值 -1.5 dBFS，均方根 -19 dBFS 左右（离麦克风很近、耳麦）。一开始调大 10 dB 时限幅器压着，不超过满幅
    let raw = speech(-19, seconds: 20, seed: 34)
    let input = scaled(raw, Float(pow(10, -1.5 / 20) / Double(raw.lazy.map { abs($0) }.max() ?? 1)))
    let output = agc2(input)
    let gains = frameGains(input: input, output: output)
    let ceiling = output.filter { abs($0) > 0.9999 }.count
    let late = output[(3 * 48_000)...].filter { abs($0) > 0.9999 }.count
    let lateGains = gains[300...].compactMap { $0 }
    let inputLevel = activeLevel(input, clean: input, from: 10, to: 20)
    let outputLevel = activeLevel(output, clean: input, from: 10, to: 20)
    report("输入峰值 \(format(peakDecibels(input))) dBFS，输出峰值 \(format(peakDecibels(output))) dBFS，碰到满幅的采样 \(ceiling) 个"
        + "（3 秒以后 \(late) 个）；3 秒以后增益 \(format(lateGains.min() ?? 0)) 到 \(format(lateGains.max() ?? 0)) dB；"
        + "10 秒以后说话 \(format(inputLevel)) → \(format(outputLevel)) dBFS")
    // 量到的：碰到满幅的 13 个（都在开头、增益还没降下来的时候：1 秒时还调大 3.3 dB，2 秒 1.0），3 秒起 0 dB，
    // 输出和输入一样（-19.3 dBFS）
    expect(output.allSatisfy { abs($0) <= 1 }, "超过了满幅")
    expect(ceiling <= output.count / 10_000, "碰到满幅的采样有 \(ceiling) 个")
    expectEqual(late, 0, "3 秒以后还有碰到满幅的采样")
    expect(lateGains.allSatisfy { abs($0) <= 0.1 }, "3 秒以后增益应该是 0 dB：\(lateGains.min() ?? 0) 到 \(lateGains.max() ?? 0)")
    expect(abs(outputLevel - inputLevel) <= 0.2, "大声说话被改了音量：\(inputLevel) → \(outputLevel)")
}

func testGainChunkingAndSmoothness() {
    let clean = speech(-40, seconds: 12, seed: 35)
    let input = add(clean, noise(clean.count, dbfs: -70, seed: 7))
    // 一帧一帧地交、两帧一起交、乱七八糟地交（1 个、不满一帧、跨帧……）、一下子全交：出来的一个比特都不差
    let reference = agc2(input, blocks: [480])
    let irregular = [1, 7, 479, 480, 481, 4_096, 13, 960, 2, 1_500]
    for blocks in [[960], irregular, [input.count]] {
        let output = agc2(input, blocks: blocks)
        expect(output == reference, "按 \(blocks.prefix(4)) 这样分批交，出来的不一样")
    }
    // 增益是每个采样上「处理后 / 处理前」（限幅器没在压的时候正好是 AGC2 的增益）：相邻两个采样之间最多变多少。
    // AGC2 每帧里从上一帧的增益平滑地过渡到这一帧的，分批交进来的地方也一样；一下子跳了的话这里会有零点几 dB
    var worst = 0.0
    var previous: Double?
    for index in 0..<input.count {
        guard abs(input[index]) >= 1e-3 else {
            previous = nil
            continue
        }
        let gain = 20 * log10(Double(reference[index]) / Double(input[index]))
        if let previous {
            worst = max(worst, abs(gain - previous))
        }
        previous = gain
    }
    let gains = frameGains(input: input, output: reference).compactMap { $0 }
    let steps = zip(gains, gains.dropFirst()).map { abs($1 - $0) }
    report("相邻两个采样的增益最多差 \(String(format: "%.4f", worst)) dB，相邻两帧最多差 \(String(format: "%.2f", steps.max() ?? 0)) dB")
    // 量到的：采样之间最多 0.0016 dB，相邻两帧最多 0.42 dB（AGC2 每帧最多变 0.06 dB，刚认出有人说话时一次最多 0.72 dB，
    // 都平摊在一帧的 480 个采样上）
    expect(worst <= 0.01, "增益跳了 \(worst) dB")
    expect((steps.max() ?? 0) <= 1, "相邻两帧的增益差了 \(steps.max() ?? 0) dB")
}

func testGainSpeed() {
    guard let controller = WebRTCGainController(settings: GainSettings()) else {
        Harness.fail("建不了 AGC2")
        return
    }
    let input = add(speech(-40, seconds: 10, seed: 36), noise(10 * 48_000, dbfs: -70, seed: 8))
    var output = [Float](repeating: 0, count: 480)
    let batches = 10
    let perBatch = input.count / 480 / batches
    // 和回声消除的测试一样（EchoQualityTests 的 testCancellerSpeed）：量这个线程用掉的 CPU 时间，分 10 段看最快的一段；
    // Rosetta 下量经过的时间
    let rosetta = runningUnderRosetta()
    let clock = rosetta ? CLOCK_UPTIME_RAW : CLOCK_THREAD_CPUTIME_ID
    let previousQoS = qos_class_self()
    pthread_set_qos_class_self_np(QOS_CLASS_USER_INTERACTIVE, 0)
    defer { pthread_set_qos_class_self_np(previousQoS, 0) }
    var fastest = Double.infinity
    input.withUnsafeBufferPointer { samples in
        output.withUnsafeMutableBufferPointer { out in
            for batch in 0..<batches {
                let started = clock_gettime_nsec_np(clock)
                for frame in (batch * perBatch)..<((batch + 1) * perBatch) {
                    _ = controller.process(samples.baseAddress! + frame * 480, into: out.baseAddress!)
                }
                fastest = min(fastest, Double(clock_gettime_nsec_np(clock) - started) / 1_000 / Double(perBatch))
            }
        }
    }
    report("每帧 \(format(fastest)) 微秒（最快的一段，10 毫秒的声音\(rosetta ? "，在 Rosetta 下，量的是经过的时间" : "")），"
        + "约占一个核的 \(String(format: "%.2f", fastest / 100))%")
    // 量到的：约 10 微秒（M4 Pro），一个核的 0.1%，比回声消除少得多。机器很忙时放宽（见 testCancellerSpeed）
    var load = [Double](repeating: 0, count: 1)
    let busy = getloadavg(&load, 1) == 1 && load[0] > Double(ProcessInfo.processInfo.activeProcessorCount) * 0.75
    let limit = busy ? 100.0 : 50.0
    expect(fastest < limit, "每帧要 \(fastest) 微秒（1 分钟平均负载 \(load[0])，上限 \(limit) 微秒）")
}

// MARK: 出错、暂停、换麦克风（假的 AGC）

func testGainFailures() {
    let frame = MicGainStage.frameSize
    let input = [Float](repeating: 0.1, count: 300 * frame)
    // 偶尔出错一次：这一帧从现在的 4 倍平滑地回到原声，从 0 dB 重新开始，接着调
    let flaky = FakeGain(factor: 4)
    flaky.failProcess = { $0 == 100 }
    let (output, stage) = runMicGain(input, controller: flaky)
    expectEqual(output.count, input.count, "进多少出多少")
    expect(output[0..<(100 * frame)].allSatisfy { abs($0 - 0.4) < 1e-6 }, "出错之前调大了 4 倍")
    let fade = Array(output[(100 * frame)..<(101 * frame)])
    expect(zip(fade, fade.dropFirst()).allSatisfy { $1 <= $0 + 1e-7 } && abs(fade[0] - 0.4) < 0.001 && abs(fade[frame - 1] - 0.1) < 1e-6,
           "出错的那一帧从 0.4 平滑地回到 0.1：\(fade.first ?? 0)…\(fade.last ?? 0)")
    expect(output[(101 * frame)...].allSatisfy { abs($0 - 0.4) < 1e-6 }, "之后接着调")
    expectEqual(flaky.resets.map(\.afterFrames), [101])
    expectEqual(flaky.resets.map(\.initialGain), [0], "从 0 dB 重新开始（接着原声往上调，不会一下子变响）")
    expectEqual(stage.counters.failures, 1)
    expect(stage.healthy, "还在调")

    // 连续出错：3 次以后不再调，后面都是原声，一个采样都不少
    let broken = FakeGain(factor: 2)
    broken.failProcess = { $0 >= 50 }
    let (raw, gaveUp) = runMicGain(input, controller: broken)
    expectEqual(raw.count, input.count)
    expect(raw[(51 * frame)...].allSatisfy { $0 == 0.1 }, "不再调以后是原声")
    expectEqual(broken.frames.count, 53, "连续出错 3 次就不再调用")
    expectEqual(broken.resets.map(\.afterFrames), [51, 52], "前两次出错各从头开始一次")
    expect(!gaveUp.healthy)
    expectEqual(gaveUp.counters.bypassed, 300 - 53)

    // 出错以后连从头开始都不行：马上不再调
    let stuck = FakeGain(factor: 2)
    stuck.failProcess = { $0 >= 10 }
    stuck.resetSucceeds = false
    let (rest, hopeless) = runMicGain(input, controller: stuck)
    expectEqual(rest.count, input.count)
    expectEqual(stuck.frames.count, 11)
    expect(!hopeless.healthy)
    expect(rest[(11 * frame)...].allSatisfy { $0 == 0.1 })
}

func testGainPauseResumeFinish() {
    let fake = FakeGain()
    let stage = MicGainStage(controller: fake, settings: GainSettings())
    var lane = SampleFIFO()
    func feed(_ range: Range<Int>) {
        let samples = sequence(range.count, from: range.lowerBound)
        samples.withUnsafeBufferPointer { stage.process($0, end: Int64(range.upperBound), into: &lane) }
    }
    func take() -> [Float] {
        var out: [Float] = []
        lane.withUnsafeSamples { out.append(contentsOf: $0) }
        lane.removeAll()
        return out
    }
    // 点暂停之前采到 1000 个：两帧处理了，剩下 40 个压着
    feed(0..<1_000)
    expectEqual(take(), sequence(960))
    // 点暂停：压着的 40 个补 0 凑成一帧处理掉，交出来（这样它们会被编码进去）
    stage.flush(into: &lane)
    expectEqual(take(), sequence(40, from: 960))
    expectEqual(stage.counters.padded, 1)
    expectEqual(fake.frames.last?.first, 961)
    expectEqual(fake.frames.last?.last, 0, "后面补的是 0")
    // 暂停期间照常处理（交出来的录音那边不要），点继续：压着的（暂停期间采到的）不要
    feed(1_000..<1_700)
    _ = take()
    stage.discardHeld()
    // 继续以后的：从新的一帧开始
    feed(2_000..<2_500)
    expectEqual(take(), sequence(480, from: 2_000), "继续以后从新的一帧开始")
    stage.finish(into: &lane)
    expectEqual(take(), sequence(20, from: 2_480), "结束时压着的都交出去")
    expectEqual(fake.frames.map(\.first), [1, 481, 961, 1_001, 2_001, 2_481])
    expectEqual(fake.resets.count, 0)
    // 什么都没压着的时候暂停、结束不多处理
    let idle = MicGainStage(controller: FakeGain(), settings: GainSettings())
    idle.flush(into: &lane)
    idle.finish(into: &lane)
    expect(lane.isEmpty && idle.counters.processed == 0)
}

func testGainAfterEchoStage() {
    // 回声消除（假的，像 AEC3 一样晚 431 个采样）后面接自动增益（假的，原样），按时间一批一批交：麦克风和电脑声音
    // 每 10 毫秒一批，带时间戳。3 秒多 123 个采样处换了麦克风（不在帧的边上），5 秒时暂停、6 秒时继续，8 秒结束
    let total = 8 * 48_000
    let boundary = 3 * 48_000 + 123
    let mic = sequence(total)
    let reference = sequence(total + 48_000, from: 5_000_000)
    let canceller = FakeCanceller(latency: Int(ZR_AEC_LATENCY))
    let echo = EchoStage(configuration: EchoStage.Configuration(mode: .on), canceller: canceller, route: nil,
                         events: RecordedEvents(), now: 0)
    echo.begin(now: 0)
    let fake = FakeGain()
    let gain = MicGainStage(controller: fake, settings: GainSettings())
    var echoed = SampleFIFO()
    var lane = SampleFIFO()
    var output: [Float] = []
    var paused = false
    var pausedAt = 0
    var resumedAt = 0
    var micSent = 0
    var referenceSent = 0
    var now = 0.0
    /// 回声消除交出来的交给自动增益（和 LiveEngine.fromEcho 一样）
    func toGain() {
        guard !echoed.isEmpty else { return }
        echoed.withUnsafeSamples { gain.process($0, end: echo.deliveredEnd, into: &lane) }
        echoed.removeAll()
    }
    /// 录音那边收到的：暂停期间的不要（TimeMixer 混出来、RecordingPipeline 丢掉）
    func collect() {
        if !paused {
            lane.withUnsafeSamples { output.append(contentsOf: $0) }
        }
        lane.removeAll()
    }
    while micSent < total {
        now += 0.02
        // 麦克风早 10 毫秒、电脑声音早 15 毫秒采到；换麦克风那一刻正好分成两批（LiveEngine 先把旧的都取完再换）
        let micLimit = min(total, Int((now - 0.01) * 48_000))
        while micSent < micLimit {
            var end = min(micSent + 480, micLimit)
            if micSent < boundary && end > boundary {
                end = boundary
            }
            mic[micSent..<end].withUnsafeBufferPointer { echo.appendMic($0, now: now) }
            echo.addStamps(.mic, [(index: Double(micSent), time: Double(micSent) / 48_000)], now: now)
            micSent = end
            if micSent == boundary {
                echo.micDeviceChanged(at: Int64(boundary))
                gain.micDeviceChanged(at: Int64(boundary))
            }
        }
        echo.noteArrival(.mic, now: now)
        let referenceLimit = min(reference.count, Int((now - 0.015) * 48_000))
        if referenceLimit > referenceSent {
            reference[referenceSent..<referenceLimit].withUnsafeBufferPointer { echo.appendReference($0, now: now) }
            echo.addStamps(.system, [(index: Double(referenceSent), time: Double(referenceSent) / 48_000)], now: now)
            referenceSent = referenceLimit
            echo.noteArrival(.system, now: now)
        }
        echo.process(now: now, into: &echoed)
        toGain()
        collect()
        if !paused && now >= 5 && resumedAt == 0 {
            // 点暂停：回声消除那里压着的原样先交出来，自动增益压着的也交出来，都编进去
            echo.previewHeld(into: &echoed)
            toGain()
            gain.flush(into: &lane)
            collect()
            paused = true
            pausedAt = micSent
        } else if paused && now >= 6 {
            // 点继续：两边压着的（暂停期间采到的）都不要
            echo.discardHeld()
            gain.discardHeld()
            paused = false
            resumedAt = micSent
        }
    }
    echo.finish(now: now, into: &echoed)
    toGain()
    gain.finish(into: &lane)
    collect()
    let expected = sequence(pausedAt) + sequence(total - resumedAt, from: resumedAt)
    expect(output == expected, "暂停之前、继续以后采到的一个不少、顺序不变（\(output.count) / \(expected.count) 个）")
    // 自动增益正好在新的麦克风的第一个采样处从头开始：在它之前处理的帧都是旧的麦克风的（补的 0 不算），之后的都是新的
    guard fake.resets.count == 1, let reset = fake.resets.first else {
        Harness.fail("换了麦克风应该从头开始一次：\(fake.resets)")
        return
    }
    expectEqual(reset.initialGain, GainSettings().initialGain)
    let old = fake.frames[..<reset.afterFrames]
    let new = fake.frames[reset.afterFrames...]
    expect(old.allSatisfy { $0.first <= Float(boundary) && ($0.last <= Float(boundary)) }, "从头开始之前处理的都是旧的麦克风的")
    expectEqual(old.last?.last, 0, "旧的麦克风最后不满一帧的补 0 处理")
    expectEqual(new.first?.first, Float(boundary + 1), "从头开始以后的第一帧正好从新的麦克风的第一个采样开始")
    expect(new.allSatisfy { $0.first > Float(boundary) }, "之后都是新的麦克风的")
    expectEqual(canceller.resets, 1, "回声消除也从头开始了一次")
}

/// 现场录音（LiveEngine）加回声消除、自动增益，两路都是假的、带时间戳的来源，时钟手动拨
private final class GainEngineFixture {
    let mic = FakeSource(kind: .mic)
    let system = FakeSource(kind: .system)
    let clock = ManualClock()
    let events = RecordedEvents()
    let pipeline: RecordingPipeline
    let engine: LiveEngine

    init(echo: EchoSetup?, gain: MicGainSetup?) throws {
        pipeline = RecordingPipeline(encoder: try AACEncoder(),
                                     chunks: ChunkWriter(directory: try temporaryDirectory("live-gain"), startSeq: 0, chunkSeconds: 10),
                                     events: events)
        let clock = clock
        engine = try LiveEngine(sources: [mic, system], pipeline: pipeline, events: events, clock: { clock.now }, echo: echo,
                                micGain: gain)
        engine.queue.sync { engine.startClock() }
    }

    /// 走 ticks 次，每次 20 毫秒；两路各交 20 毫秒的声音，带时间戳（麦克风早 5 毫秒采到，电脑声音早 10 毫秒）
    func run(ticks: Int, mic micValue: Float = 0.1, source: FakeSource? = nil) {
        for _ in 0..<ticks {
            clock.advance(milliseconds: 20)
            (source ?? mic).push([Float](repeating: micValue, count: 960), at: clock.now - 25_000_000)
            system.push([Float](repeating: 0.05, count: 960), at: clock.now - 30_000_000)
            engine.queue.sync { engine.tick() }
        }
    }

    func stop() -> Result<RecordingResult, RecorderFailure>? {
        let finished = DispatchSemaphore(value: 0)
        var result: Result<RecordingResult, RecorderFailure>?
        engine.onFinished = {
            result = $0
            finished.signal()
        }
        engine.stop()
        return finished.wait(timeout: .now() + 10) == .success ? result : nil
    }

    /// 音量事件里的麦克风音量（继续以后的）
    func micLevels(afterResume: Bool) -> [Double] {
        let objects = events.all.compactMap { (try? JSONSerialization.jsonObject(with: Data($0.utf8))) as? [String: Any] }
        let start = afterResume ? (objects.firstIndex { $0["event"] as? String == "resumed" } ?? objects.count) : 0
        return objects[start...].filter { $0["event"] as? String == "level" }.compactMap { $0["mic"] as? Double }
    }
}

func testEngineGainWithEcho() throws {
    let canceller = NegatingCanceller()
    let fake = FakeGain(factor: 2)
    let fixture = try GainEngineFixture(echo: EchoSetup(mode: .on, makeCanceller: { canceller }, route: TestRoute(.speakers)),
                                        gain: MicGainSetup(makeController: { fake }))
    expectEqual(fixture.engine.micGainStatus, .auto)
    expect(fixture.engine.micGain != nil && fixture.engine.echoStage != nil, "两步都在")
    fixture.run(ticks: 100, mic: 0.01)
    // 先经过回声消除（假的，出来的是相反数），再调音量（假的，2 倍）
    expect(!fake.frames.isEmpty && fake.frames.allSatisfy { $0.first == -0.01 }, "自动增益收到的是回声消除处理过的")
    // 麦克风那一路的音量是调大了的：0.02 是 -34 dBFS，音量 0.43（不调是 0.01，-40 dBFS，0.33）
    let levels = fixture.micLevels(afterResume: false).filter { $0 > 0 }
    expect(levels.suffix(10).allSatisfy { abs($0 - (20 * log10(0.02) + 60) / 60) < 0.01 }, "麦克风那一路调大了 2 倍：\(levels.suffix(3))")
    fixture.engine.pause()
    fixture.engine.queue.sync {}
    let beforePause = fixture.pipeline.recordedSamples
    // 暂停期间的声音很响：编进去一点就看得出来（和 testEngineWithEcho 一样）
    fixture.run(ticks: 50, mic: 1)
    expectEqual(fixture.pipeline.recordedSamples, beforePause, "暂停期间什么都不编")
    fixture.engine.resume()
    fixture.engine.queue.sync {}
    // 换麦克风：新的接着交（时间戳接着走）
    let next = FakeSource(kind: .mic, name: "AirPods Pro")
    fixture.run(ticks: 50, mic: 0.01)
    expectEqual(fixture.engine.switchMic(to: next), .switched)
    fixture.run(ticks: 50, mic: 0.01, source: next)
    expectEqual(fixture.pipeline.recordedSamples - beforePause, 96_000, "继续以后的 2 秒")
    expect(!fixture.micLevels(afterResume: true).contains { $0 > 0.5 }, "暂停期间采到的声音编进去了")
    guard case .success(let recording)? = fixture.stop() else {
        Harness.fail("没有正常结束")
        return
    }
    expect(recording.seconds > 4 && recording.seconds < 5, "录了 \(recording.seconds) 秒")
    expectEqual(fake.resets.count, 1, "换了麦克风，处理到新的麦克风的声音时从头开始")
}

func testEngineGainOffAndUnavailable() throws {
    // 同样的输入：不调（不传 micGain）、建不起来（makeController 返回 nil）时麦克风那一路收到的完全一样；
    // 在调的时候电脑声音那一路也一个比特都不变
    var random = SplitMix(seed: 12)
    let chunks = (0..<50).map { _ in (0..<960).map { _ in random.uniform() - 0.5 } }
    func lanes(_ gain: MicGainSetup?, expect status: MicGainStatus) throws -> (mic: [Float], system: [Float]) {
        let mic = FakeSource(kind: .mic)
        let system = FakeSource(kind: .system)
        let events = RecordedEvents()
        let pipeline = RecordingPipeline(encoder: try AACEncoder(),
                                         chunks: ChunkWriter(directory: try temporaryDirectory("gain-off"), startSeq: 0, chunkSeconds: 10),
                                         events: events)
        let engine = try LiveEngine(sources: [mic, system], pipeline: pipeline, events: events, clock: { 0 }, micGain: gain)
        expectEqual(engine.micGainStatus, status)
        var micLane: [Float] = []
        var systemLane: [Float] = []
        for (index, chunk) in chunks.enumerated() {
            mic.push(chunk, at: UInt64(index) * 20_000_000)
            system.push(chunk.reversed(), at: UInt64(index) * 20_000_000)
            engine.queue.sync {
                engine.tick()
                // 时钟不走，混音不取，两路收到的都在队列里
                engine.mixer.lanes[0].queue.withUnsafeSamples { micLane.append(contentsOf: $0) }
                engine.mixer.lanes[0].queue.removeAll()
                engine.mixer.lanes[1].queue.withUnsafeSamples { systemLane.append(contentsOf: $0) }
                engine.mixer.lanes[1].queue.removeAll()
            }
        }
        expectEqual(events.all, [], "不发事件")
        return (micLane, systemLane)
    }
    let pushedMic = chunks.flatMap { $0 }
    let pushedSystem = chunks.flatMap { $0.reversed() }
    let off = try lanes(nil, expect: .off)
    expect(off.mic == pushedMic && off.system == pushedSystem, "off：一个比特都不变")
    let unavailable = try lanes(MicGainSetup(makeController: { nil }), expect: .unavailable)
    expect(unavailable.mic == pushedMic && unavailable.system == pushedSystem, "建不起来：原样录")
    let doubled = try lanes(MicGainSetup(makeController: { FakeGain(factor: 2) }), expect: .auto)
    expectEqual(doubled.mic.count, pushedMic.count / 480 * 480, "麦克风那一路调大了（不满一帧的压着）")
    expect(zip(doubled.mic, pushedMic).allSatisfy { $0 == $1 * 2 }, "麦克风那一路调大了 2 倍")
    expect(doubled.system == pushedSystem, "电脑声音那一路一个比特都不变")

    // 只录电脑声音：不建自动增益，started 照样报要的模式
    let system = FakeSource(kind: .system)
    let pipeline = RecordingPipeline(encoder: try AACEncoder(),
                                     chunks: ChunkWriter(directory: try temporaryDirectory("gain-system"), startSeq: 0, chunkSeconds: 10),
                                     events: RecordedEvents())
    var built = false
    let systemOnly = try LiveEngine(sources: [system], pipeline: pipeline, events: RecordedEvents(), clock: { 0 },
                                    micGain: MicGainSetup(makeController: { built = true; return FakeGain() }))
    expect(!built && systemOnly.micGain == nil, "没录麦克风就不建自动增益")
    expectEqual(systemOnly.micGainStatus, .auto)
    // 读文件的测试模式也一样报
    let directory = try temporaryDirectory("gain-file")
    let file = directory.appendingPathComponent("in.wav")
    try writeWAV(file, [Float](repeating: 0.01, count: 4_800))
    let filePipeline = RecordingPipeline(encoder: try AACEncoder(),
                                         chunks: ChunkWriter(directory: directory, startSeq: 0, chunkSeconds: 10), events: RecordedEvents())
    expectEqual(try FileEngine(reader: AudioFileReader(path: file.path), pipeline: filePipeline).micGainStatus, .off)
    expectEqual(try FileEngine(reader: AudioFileReader(path: file.path), pipeline: filePipeline,
                               micGain: MicGainSetup(makeController: { nil })).micGainStatus, .unavailable)
    expectEqual(try FileEngine(reader: AudioFileReader(path: file.path), pipeline: filePipeline,
                               micGain: .webRTC()).micGainStatus, .auto)
}

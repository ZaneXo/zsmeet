import Accelerate
import Foundation
@testable import RecorderKit

/// 回声消除消得好不好：真的 AEC3（WebRTC）加 EchoStage，用合成的像说话的声音（不用真的录音、不放声音、
/// 不要设备），模拟外放时麦克风录到的回声：对方的声音（参考）延迟一段、经过房间的反射，加上这边自己说话。
/// 量回声消掉了多少（ERLE：只有对方在说话的帧，麦克风和处理后的能量比）和这边说话保留得怎么样
/// （处理后的声音投影到这边说的话上：增益、信号失真比）。门槛是第一次量到的（2026-11，M4 Pro，见每个测试里的注释）
/// 减去一些余量；升级 WebRTC、改了 EchoStage 或者 AEC3 的配置以后要重新量一遍再改门槛
let echoQualityTests: [(String, () throws -> Void)] = [
    ("回声消除（AEC3）：外放的回声消掉 20 dB 以上，这边单独说话基本不变，两边同时说话时不被压得太狠", testCancellerQuality),
    ("回声消除（AEC3）：回声比参考的时间早到 60 毫秒也消得掉（靠提前 170 毫秒），不提前就消不掉", testCancellerNeedsLead),
    ("回声消除（AEC3）：回声晚到 400 毫秒也消得掉", testCancellerLongDelay),
    ("回声消除（AEC3）：回声的延迟突然变了 100 毫秒，5 秒内重新消掉", testCancellerDelayJump),
    ("回声消除（AEC3）：两路时钟差 100 ppm 也消得掉", testCancellerDrift),
    ("回声消除（AEC3）：没有回声（戴耳机）、对方不说话时，这边的声音基本不变", testCancellerTransparent),
    ("回声消除（AEC3）：扬声器失真（非线性）的回声也消掉 10 dB 以上", testCancellerNonlinear),
    ("回声消除（AEC3）：每 10 毫秒一帧的处理时间不到一个核的 2.5%（机器很忙时 5%）", testCancellerSpeed),
    ("回声消除（AEC3）：处理过的声音正好晚 431 个采样（EchoStage 靠它无缝接上）", testCancellerLatency),
]

private let rate = 48_000.0

// MARK: 合成的声音

/// 一组二阶滤波器（vDSP_deq22），系数按 RBJ 的公式算
private func biquad(_ input: [Float], _ coefficients: [Double]) -> [Float] {
    // vDSP_deq22 要求输入、输出前面各多两个采样
    let padded = [Float](repeating: 0, count: 2) + input
    var output = [Float](repeating: 0, count: input.count + 2)
    var c = coefficients.map(Float.init)
    vDSP_deq22(padded, 1, &c, &output, 1, vDSP_Length(input.count))
    return Array(output[2...])
}

/// 带通（峰值增益 0 dB）
private func bandPass(_ input: [Float], frequency: Double, q: Double) -> [Float] {
    let w0 = 2 * Double.pi * frequency / rate
    let alpha = sin(w0) / (2 * q)
    let a0 = 1 + alpha
    return biquad(input, [alpha / a0, 0, -alpha / a0, -2 * cos(w0) / a0, (1 - alpha) / a0])
}

/// 低通
func lowPass(_ input: [Float], frequency: Double) -> [Float] {
    let w0 = 2 * Double.pi * frequency / rate
    let alpha = sin(w0) / (2 * 0.707)
    let a0 = 1 + alpha
    let c = cos(w0)
    return biquad(input, [(1 - c) / 2 / a0, (1 - c) / a0, (1 - c) / 2 / a0, -2 * c / a0, (1 - alpha) / a0])
}

/// 高通
private func highPass(_ input: [Float], frequency: Double) -> [Float] {
    let w0 = 2 * Double.pi * frequency / rate
    let alpha = sin(w0) / (2 * 0.707)
    let a0 = 1 + alpha
    let c = cos(w0)
    return biquad(input, [(1 + c) / 2 / a0, -(1 + c) / a0, (1 + c) / 2 / a0, -2 * c / a0, (1 - alpha) / a0])
}

private func whiteNoise(_ count: Int, seed: UInt64) -> [Float] {
    var random = SplitMix(seed: seed)
    var noise = [Float](repeating: 0, count: count)
    noise.withUnsafeMutableBufferPointer { buffer in
        for index in 0..<count {
            buffer[index] = random.uniform() * 2 - 1
        }
    }
    return noise
}

private func add(_ a: [Float], _ b: [Float], scale: Float = 1) -> [Float] {
    var out = a
    var s = scale
    vDSP_vsma(b, 1, &s, a, 1, &out, 1, vDSP_Length(min(a.count, b.count)))
    return out
}

private func multiply(_ a: [Float], _ b: [Float]) -> [Float] {
    var out = [Float](repeating: 0, count: a.count)
    vDSP_vmul(a, 1, b, 1, &out, 1, vDSP_Length(a.count))
    return out
}

private func scaled(_ a: [Float], _ factor: Float) -> [Float] {
    var out = [Float](repeating: 0, count: a.count)
    var f = factor
    vDSP_vsmul(a, 1, &f, &out, 1, vDSP_Length(a.count))
    return out
}

func energy(_ a: ArraySlice<Float>) -> Double {
    a.withUnsafeBufferPointer { buffer in
        var sum: Float = 0
        vDSP_svesq(buffer.baseAddress!, 1, &sum, vDSP_Length(buffer.count))
        return Double(sum)
    }
}

/// 像说话的声音：按音节一开一合（一个音节 0.1 到 0.3 秒，中间停 30 到 120 毫秒），一句话 1.5 到 4 秒，句子之间
/// 停 0.3 到 1 秒，只在 active 的时间段里说。浊音是声带的脉冲（音高 pitch 上下浮动、每个音节里滑一点）经过
/// 两个共振峰，再加一点气声。一直不停的噪声 AEC3 会当成背景噪声；两个人的声音要有不同的音高、共振峰，
/// 像真的两个人，AEC3 才分得出哪边在说话
private func speechLike(seconds: Double, seed: UInt64, rms: Float, active: [Range<Double>], formants: (Double, Double),
                        pitch: Double) -> [Float] {
    let count = Int(seconds * rate)
    var pulses = [Float](repeating: 0, count: count)
    var envelope = [Float](repeating: 0, count: count)
    var random = SplitMix(seed: seed &+ 1)
    pulses.withUnsafeMutableBufferPointer { glottis in
        envelope.withUnsafeMutableBufferPointer { env in
            for range in active {
                var t = range.lowerBound + Double(random.uniform()) * 0.1
                while t < range.upperBound {
                    let phraseEnd = min(range.upperBound, t + 1.5 + Double(random.uniform()) * 2.5)
                    while t < phraseEnd {
                        let length = 0.1 + Double(random.uniform()) * 0.2
                        let amplitude = 0.4 + random.uniform() * 0.6
                        let start = Int(t * rate)
                        let end = min(Int(min(t + length, phraseEnd) * rate), count)
                        if end > start {
                            let span = Float(end - start)
                            // 这个音节的音高：基准上下 15%，从头到尾滑 ±10%
                            let f0 = pitch * (0.85 + Double(random.uniform()) * 0.3)
                            let glide = 1 + (Double(random.uniform()) - 0.5) * 0.2
                            var phase = 1.0
                            for index in start..<end {
                                let x = sin(Float.pi * Float(index - start) / span)
                                env[index] = amplitude * x * x
                                let progress = Double(index - start) / Double(end - start)
                                phase += f0 * (1 + (glide - 1) * progress) / rate
                                if phase >= 1 {
                                    phase -= 1
                                    glottis[index] = 1
                                }
                            }
                        }
                        t += length + 0.03 + Double(random.uniform()) * 0.09
                    }
                    t = phraseEnd + 0.3 + Double(random.uniform()) * 0.7
                }
            }
        }
    }
    // 声带的脉冲本身是往高频越来越弱的（低通），再经过两个共振峰；气声是一点点高通的噪声
    let voiced = lowPass(pulses, frequency: 900)
    var carrier = add(scaled(bandPass(voiced, frequency: formants.0, q: 4), 0.8), bandPass(voiced, frequency: formants.1, q: 5))
    carrier = add(carrier, scaled(voiced, 0.3))
    carrier = add(carrier, highPass(whiteNoise(count, seed: seed), frequency: 2_500), scale: 0.02)
    carrier = highPass(carrier, frequency: 100)
    let signal = multiply(carrier, envelope)
    // 说话的时候均方根是 rms
    let activeSamples = envelope.reduce(0) { $0 + ($1 > 0.05 ? 1 : 0) }
    let current = sqrt(energy(signal[...]) / Double(max(activeSamples, 1)))
    return scaled(signal, current > 0 ? rms / Float(current) : 0)
}

/// 回声：far 经过房间（直达声加上 150 毫秒以内越来越弱的反射，混响时间约 reverb 秒），整体延迟 delay 秒
/// （负数：回声比参考的时间戳早到），直达声的增益是 gain
private func echo(of far: [Float], delay: Double, gain: Float, reverb: Double = 0.2, seed: UInt64 = 99) -> [Float] {
    var random = SplitMix(seed: seed)
    var taps: [(Int, Float)] = [(0, 1)]
    for _ in 0..<60 {
        let tau = 0.001 + Double(random.uniform()) * 0.149
        let amplitude = 0.45 * Float(exp(-6.9 * tau / reverb)) * (random.uniform() < 0.5 ? -1 : 1)
        taps.append((Int(tau * rate), amplitude))
    }
    let count = far.count
    let shift = Int((delay * rate).rounded())
    var out = [Float](repeating: 0, count: count)
    far.withUnsafeBufferPointer { source in
        out.withUnsafeMutableBufferPointer { destination in
            for (tap, amplitude) in taps {
                let offset = shift + tap
                let start = max(0, offset)
                let end = min(count, count + offset)
                guard end > start else { continue }
                var g = amplitude * gain
                vDSP_vsma(source.baseAddress! + (start - offset), 1, &g, destination.baseAddress! + start, 1,
                          destination.baseAddress! + start, 1, vDSP_Length(end - start))
            }
        }
    }
    return out
}

/// 一段模拟：对方的声音（参考）、这边的声音、麦克风录到的（这边的 + 回声 + 底噪）
struct Scene {
    var far: [Float]
    var near: [Float]
    var mic: [Float]
}

func scene(seconds: Double, far farActive: [Range<Double>], near nearActive: [Range<Double>],
                   delay: Double = 0.08, gain: Float = 0.5, nonlinear: Bool = false, seed: UInt64 = 1) -> Scene {
    // 对方是音高高一些的声音，这边是低一些的
    let far = speechLike(seconds: seconds, seed: seed, rms: 0.1, active: farActive, formants: (700, 1_900), pitch: 210)
    let near = speechLike(seconds: seconds, seed: seed &+ 100, rms: 0.08, active: nearActive, formants: (550, 1_500), pitch: 120)
    var played = far
    if nonlinear {
        // 笔记本的小扬声器开得很响：削顶失真
        var boosted = scaled(far, 4)
        var n = Int32(boosted.count)
        vvtanhf(&played, &boosted, &n)
        played = scaled(played, 0.25)
    }
    var mic = add(near, echo(of: played, delay: delay, gain: gain))
    // -65 dBFS 左右的底噪
    mic = add(mic, whiteNoise(mic.count, seed: seed &+ 7), scale: 0.001)
    return Scene(far: far, near: near, mic: mic)
}

// MARK: 跑一遍、量一量

/// 用真的 AEC3 跑一遍：参考提前 lead 毫秒，referenceRate 是电脑声音那一路的时钟（对方的声音按这个时钟重新采样）
private func process(_ scene: Scene, lead: Int = 170, referencePPM: Double = 0) -> (output: [Float], run: StageRun) {
    guard let canceller = WebRTCEchoCanceller(filters: 6) else {
        Harness.fail("建不了 AEC3")
        return ([], StageRun(mic: SimStream([]), reference: nil, canceller: FakeCanceller()))
    }
    var reference = scene.far + [Float](repeating: 0, count: 48_000)
    var segments = [SimStream.Segment(start: 0, time: 0)]
    if referencePPM != 0 {
        // 电脑声音的时钟快 ppm：它的第 j 个采样其实是第 j / (1 + ppm) 个采样的时刻（线性插值）
        let ratio = 1 / (1 + referencePPM)
        let source = reference
        reference = (0..<Int(Double(source.count) * (1 + referencePPM)) - 1).map { j in
            let x = Double(j) * ratio
            let i = Int(x)
            let f = Float(x - Double(i))
            return i + 1 < source.count ? source[i] * (1 - f) + source[i + 1] * f : 0
        }
        segments = [SimStream.Segment(start: 0, time: 0, rate: 48_000 * (1 + referencePPM))]
    }
    let run = StageRun(mic: SimStream(scene.mic), reference: SimStream(reference, segments: segments), canceller: canceller, lead: lead)
    run.run()
    run.finish()
    expectEqual(run.output.count, scene.mic.count, "进多少出多少")
    return (run.output, run)
}

/// 只有对方在说话（这边没说话）的 10 毫秒帧里，回声消掉了多少 dB
private func echoReduction(_ scene: Scene, _ output: [Float], from: Double, to: Double) -> Double {
    var micEnergy = 0.0
    var outEnergy = 0.0
    var frame = Int(from * 100)
    while frame < Int(to * 100) && (frame + 1) * 480 <= output.count {
        let range = (frame * 480)..<((frame + 1) * 480)
        // 这边没说话，回声又够响（-50 dBFS 以上）才算
        if energy(scene.near[range]) < 1e-9 && energy(scene.mic[range]) > 480 * 1e-5 {
            micEnergy += energy(scene.mic[range])
            outEnergy += energy(output[range])
        }
        frame += 1
    }
    return 10 * log10(micEnergy / max(outEnergy, 1e-20))
}

/// 处理后的声音里这边说的话：在 ±10 毫秒内找最对得上的位置，投影的增益（dB）和信号失真比（dB）。
/// 只看 300 Hz 到 6 kHz（8 阶的高通、低通）：服务器转写前降到 16 kHz，8 kHz 以上本来就不要；AudioProcessing 在
/// 48 kHz 时把声音分成三段处理再合起来，分段的滤波器在 8 kHz 以上会留下镜像（不开回声消除、只开高通滤波也一样），
/// 它的高通滤波也会让很低的频率相位变一点，这些都不是回声消除造成的，不算
private func nearEnd(_ scene: Scene, _ output: [Float], from: Double, to: Double, lowCut: Double = 300) -> (gain: Double, sdr: Double) {
    let band = { (signal: [Float]) -> [Float] in
        var filtered = signal
        for _ in 0..<4 {
            filtered = lowPass(highPass(filtered, frequency: lowCut), frequency: 6_000)
        }
        return filtered
    }
    let output = band(output)
    let near = band(scene.near)
    let start = Int(from * rate) + 480
    let end = min(Int(to * rate), output.count) - 480
    let length = vDSP_Length(end - start)
    var best = (lag: 0, dot: -Float.infinity)
    output.withUnsafeBufferPointer { out in
        near.withUnsafeBufferPointer { near in
            for lag in -480...480 {
                var dot: Float = 0
                vDSP_dotpr(out.baseAddress! + start, 1, near.baseAddress! + start - lag, 1, &dot, length)
                if dot > best.dot {
                    best = (lag, dot)
                }
            }
        }
    }
    let reference = Array(near[(start - best.lag)..<(end - best.lag)])
    let g = Double(best.dot) / energy(reference[...])
    return (20 * log10(max(g, 1e-9)), fittedSDR(Array(output[start..<end]), near, start: start - best.lag))
}

/// 信号失真比：用一个 16 阶的 FIR（最小二乘）从 near 拟合 output，拟合出来的和剩下的能量比（dB）。
/// 只是延迟了零点几个采样、频率响应有一点起伏（线性的变化，听不出来）都算在拟合出来的里面；
/// 剩下的是一阵一阵的压低、残余的回声、噪声这些
private func fittedSDR(_ output: [Float], _ near: [Float], start: Int, taps: Int = 16) -> Double {
    let count = output.count
    let half = taps / 2
    guard start - half >= 0, start + count + half <= near.count else { return -.infinity }
    // x_k[t] = near[start + t + k - half]
    var matrix = [[Double]](repeating: [Double](repeating: 0, count: taps), count: taps)
    var vector = [Double](repeating: 0, count: taps)
    near.withUnsafeBufferPointer { x in
        output.withUnsafeBufferPointer { y in
            for i in 0..<taps {
                var dot: Float = 0
                vDSP_dotpr(y.baseAddress!, 1, x.baseAddress! + start + i - half, 1, &dot, vDSP_Length(count))
                vector[i] = Double(dot)
                for j in i..<taps {
                    vDSP_dotpr(x.baseAddress! + start + i - half, 1, x.baseAddress! + start + j - half, 1, &dot, vDSP_Length(count))
                    matrix[i][j] = Double(dot)
                    matrix[j][i] = Double(dot)
                }
            }
        }
    }
    // 高斯消元（选主元）
    var a = matrix
    var b = vector
    for column in 0..<taps {
        let pivot = (column..<taps).max { abs(a[$0][column]) < abs(a[$1][column]) }!
        a.swapAt(column, pivot)
        b.swapAt(column, pivot)
        guard abs(a[column][column]) > 1e-12 else { return -.infinity }
        for row in (column + 1)..<taps {
            let factor = a[row][column] / a[column][column]
            for k in column..<taps {
                a[row][k] -= factor * a[column][k]
            }
            b[row] -= factor * b[column]
        }
    }
    var h = [Double](repeating: 0, count: taps)
    for row in stride(from: taps - 1, through: 0, by: -1) {
        var sum = b[row]
        for k in (row + 1)..<taps {
            sum -= a[row][k] * h[k]
        }
        h[row] = sum / a[row][row]
    }
    var fitted = [Float](repeating: 0, count: count)
    near.withUnsafeBufferPointer { x in
        for k in 0..<taps {
            var weight = Float(h[k])
            vDSP_vsma(x.baseAddress! + start + k - half, 1, &weight, fitted, 1, &fitted, 1, vDSP_Length(count))
        }
    }
    let residual = add(output, fitted, scale: -1)
    return 10 * log10(energy(fitted[...]) / max(energy(residual[...]), 1e-20))
}

private func report(_ text: String) {
    print("    测得：\(text)")
}

private func format(_ value: Double) -> String {
    String(format: "%.1f", value)
}

// MARK: 测试

func testCancellerQuality() {
    // 0 到 12 秒对方说话；13 到 17 秒这边单独说；18 到 26 秒对方说，其中 20 到 24 秒两边同时说；27 到 30 秒这边单独说
    let s = scene(seconds: 30, far: [0..<12, 18..<26], near: [13..<17, 20..<24, 27..<30])
    let (output, run) = process(s)
    let erle = echoReduction(s, output, from: 5, to: 12)
    let erleLater = echoReduction(s, output, from: 18, to: 26)
    let alone = nearEnd(s, output, from: 13.5, to: 17)
    let aloneLater = nearEnd(s, output, from: 27, to: 30)
    let both = nearEnd(s, output, from: 20.5, to: 24)
    report("回声消掉 \(format(erle)) dB（5 到 12 秒）、\(format(erleLater)) dB（18 到 26 秒）；这边单独说话：增益 \(format(alone.gain)) dB、"
        + "SDR \(format(alone.sdr)) dB，后来又 \(format(aloneLater.gain)) dB、\(format(aloneLater.sdr)) dB；两边同时说话：这边增益 "
        + "\(format(both.gain)) dB、SDR \(format(both.sdr)) dB；\(run.stage.canceller.stats()?.summary ?? "")")
    // 量到的：回声 27.7 dB（刚开始，还在收敛）、37.1 dB；这边单独说话 -0.2 dB、SDR 22 dB（300 Hz 以下 AudioProcessing
    // 的高通滤波让相位变了，16 阶的 FIR 拟合不全；600 Hz 以上是 29 dB）；两边同时说话 -6.6 dB（一阵一阵地被压，
    // 刚开始同时说的那一秒压得最多）。两边同时说话时这边会被压低，这是 AEC3 的做法，真机上还要再看（docs/desktop.md）
    expect(erle >= 20, "回声只消掉了 \(erle) dB")
    expect(erleLater >= 25, "后来回声只消掉了 \(erleLater) dB")
    expect(abs(alone.gain) <= 1 && abs(aloneLater.gain) <= 1, "这边单独说话的增益：\(alone.gain) dB、\(aloneLater.gain) dB")
    expect(alone.sdr >= 18 && aloneLater.sdr >= 18, "这边单独说话的 SDR：\(alone.sdr) dB、\(aloneLater.sdr) dB")
    expect(both.gain >= -9, "两边同时说话时这边被压了 \(both.gain) dB")
    // AEC3 估计的延迟：提前的 170 毫秒 + 回声延迟 80 毫秒
    if let delay = run.stage.canceller.stats()?.delayMilliseconds {
        expectClose(delay, 250, within: 20, "AEC3 估计的延迟")
    } else {
        Harness.fail("AEC3 没报延迟")
    }
}

func testCancellerNeedsLead() {
    // 回声比参考的时间戳早到 60 毫秒（比如电脑声音的时间戳比实际放出来的晚）
    let s = scene(seconds: 15, far: [0..<15], near: [], delay: -0.06, seed: 2)
    let led = echoReduction(s, process(s, lead: 170).output, from: 5, to: 15)
    let none = echoReduction(s, process(s, lead: 0).output, from: 5, to: 15)
    report("提前 170 毫秒：回声消掉 \(format(led)) dB；不提前：\(format(none)) dB")
    // 量到的：41.9 dB、2 到 4 dB
    expect(led >= 30, "提前了还只消掉 \(led) dB")
    expect(none < led - 20, "不提前也消掉了 \(none) dB，测不出提前量的作用")
}

func testCancellerLongDelay() {
    // 蓝牙音箱、显示器的扬声器：回声晚到 400 毫秒（AEC3 看到的是 570 毫秒，6 个滤波器能找到 608 毫秒）
    let s = scene(seconds: 15, far: [0..<15], near: [], delay: 0.4, seed: 3)
    let reduction = echoReduction(s, process(s).output, from: 6, to: 15)
    report("回声晚到 400 毫秒：消掉 \(format(reduction)) dB")
    // 量到的：22.7 dB
    expect(reduction >= 15, "只消掉了 \(reduction) dB")
}

func testCancellerDelayJump() {
    // 12 秒时回声的延迟从 80 毫秒变成 180 毫秒（比如换了输出设备）
    let first = scene(seconds: 25, far: [0..<25], near: [], delay: 0.08, seed: 4)
    let later = echo(of: first.far, delay: 0.18, gain: 0.5)
    let split = 12 * 48_000
    var mic = first.mic
    let laterMic = add(later, whiteNoise(later.count, seed: 11), scale: 0.001)
    mic.replaceSubrange(split..., with: laterMic[split...])
    let s = Scene(far: first.far, near: first.near, mic: mic)
    let output = process(s).output
    let before = echoReduction(s, output, from: 5, to: 12)
    // 最后一秒不算：结束时最后那一点的参考还没放出来，喂的是 0
    let after = echoReduction(s, output, from: 17, to: 24)
    report("延迟变之前消掉 \(format(before)) dB，变了 5 秒以后 \(format(after)) dB")
    // 量到的：45.8 dB；变了以后第 2 秒 14 dB，第 3 秒起 45 dB 左右
    expect(before >= 30, "变之前只消掉了 \(before) dB")
    expect(after >= 30, "延迟变了 5 秒以后只消掉了 \(after) dB")
}

func testCancellerDrift() {
    // 电脑声音的时钟比麦克风快 100 ppm（USB 麦克风加电脑自带的扬声器），30 秒
    let s = scene(seconds: 30, far: [0..<30], near: [], seed: 5)
    let (output, run) = process(s, referencePPM: 100e-6)
    let reduction = echoReduction(s, output, from: 5, to: 30)
    report("时钟差 100 ppm：消掉 \(format(reduction)) dB，参考补、丢了 \(run.stage.counters.slips) 个采样")
    // 量到的：33.3 dB
    expect(reduction >= 25, "只消掉了 \(reduction) dB")
    expectClose(Double(run.stage.counters.slips), 30 * 48_000 * 100e-6, within: 30, "补、丢的采样数")
}

func testCancellerTransparent() {
    // 戴耳机：对方在说话，但没有回声；这边断断续续说话
    let quiet = scene(seconds: 15, far: [0..<15], near: [1..<15], gain: 0, seed: 6)
    let quietOutput = process(quiet).output
    // 一直找不到回声，AEC3 几秒以后不再压低这边（HMM 透明模式，见 zr_aec.cc；默认的那种要十几秒）
    let headphones = nearEnd(quiet, quietOutput, from: 5, to: 15)
    // 对方不说话：只有这边
    let alone = scene(seconds: 12, far: [], near: [0..<12], seed: 7)
    let passthrough = nearEnd(alone, process(alone).output, from: 1, to: 12)
    report("戴耳机：增益 \(format(headphones.gain)) dB、SDR \(format(headphones.sdr)) dB；对方不说话：增益 \(format(passthrough.gain)) dB、"
        + "SDR \(format(passthrough.sdr)) dB")
    // 量到的：戴耳机 5 秒以后 -0.3 dB 左右；对方不说话 -0.2 dB、SDR 22 dB
    expect(abs(headphones.gain) <= 1, "戴耳机时这边的增益是 \(headphones.gain) dB")
    expect(headphones.sdr >= 12, "戴耳机时这边的 SDR 是 \(headphones.sdr) dB")
    expect(abs(passthrough.gain) <= 0.5, "对方不说话时这边的增益是 \(passthrough.gain) dB")
    expect(passthrough.sdr >= 18, "对方不说话时这边的 SDR 是 \(passthrough.sdr) dB")
}

func testCancellerNonlinear() {
    let s = scene(seconds: 15, far: [0..<15], near: [], nonlinear: true, seed: 8)
    let reduction = echoReduction(s, process(s).output, from: 5, to: 15)
    report("扬声器削顶失真：消掉 \(format(reduction)) dB")
    // 量到的：37.2 dB
    expect(reduction >= 20, "只消掉了 \(reduction) dB")
}

func testCancellerSpeed() {
    guard let canceller = WebRTCEchoCanceller(filters: 6) else {
        Harness.fail("建不了 AEC3")
        return
    }
    let s = scene(seconds: 10, far: [0..<10], near: [3..<8], seed: 9)
    var output = [Float](repeating: 0, count: 480)
    let batches = 10
    let perBatch = s.mic.count / 480 / batches
    // 量这个线程用掉的 CPU 时间（被换下去的时间不算）。分 10 段量、看最快的一段：机器很忙（别的程序在编译）时
    // 线程会被排到能效核上、和别的程序抢缓存，慢两三倍，最快的那段最接近它本来要花的时间。
    // 量的时候按「用户交互」的优先级跑，尽量排在性能核上。
    // 在 Rosetta 下跑的（test.sh 用 Intel 那一份再跑一遍）不能这样量：那里线程的 CPU 时钟给的是 24 MHz 的计数，
    // 当成纳秒就小了约 41.7 倍，没开优化的也「够快」。改量经过的时间（CLOCK_UPTIME_RAW 在 Rosetta 下是对的），
    // 一样按「用户交互」的优先级跑、看最快的一段，很少被换下去
    let rosetta = runningUnderRosetta()
    let clock = rosetta ? CLOCK_UPTIME_RAW : CLOCK_THREAD_CPUTIME_ID
    let previousQoS = qos_class_self()
    pthread_set_qos_class_self_np(QOS_CLASS_USER_INTERACTIVE, 0)
    defer { pthread_set_qos_class_self_np(previousQoS, 0) }
    var fastest = Double.infinity
    var total: UInt64 = 0
    s.far.withUnsafeBufferPointer { far in
        s.mic.withUnsafeBufferPointer { mic in
            output.withUnsafeMutableBufferPointer { out in
                for batch in 0..<batches {
                    let started = clock_gettime_nsec_np(clock)
                    for frame in (batch * perBatch)..<((batch + 1) * perBatch) {
                        _ = canceller.render(far.baseAddress! + frame * 480)
                        _ = canceller.capture(mic.baseAddress! + frame * 480, into: out.baseAddress!)
                    }
                    let elapsed = clock_gettime_nsec_np(clock) - started
                    total += elapsed
                    fastest = min(fastest, Double(elapsed) / 1_000 / Double(perBatch))
                }
            }
        }
    }
    let average = Double(total) / 1_000 / Double(perBatch * batches)
    let measured = rosetta ? "在 Rosetta 下，量的是经过的时间" : "量的是线程用掉的 CPU 时间"
    report("每帧 \(format(fastest)) 微秒（最快的一段，平均 \(format(average)) 微秒；10 毫秒的声音；\(measured)），"
        + "约占一个核的 \(String(format: "%.2f", fastest / 100))%")
    // 量到的：直接用 C 调用约 55 微秒（M4 Pro，同时在编译别的东西），这里约 130 微秒。CI 的虚拟机慢一些。
    // 最快的一段也超过 250 微秒（一个核的 2.5%）多半是编错了（比如没开优化，要慢 5 到 10 倍）。
    // 机器很忙（最近 1 分钟的平均负载超过核数的 3/4，比如几个工作区同时在编译）时所有的核都降频、抢缓存，
    // 最快的一段也要 300 微秒左右（量到过负载 15 时 290 到 320 微秒），这时放宽到 500 微秒，没开优化的照样超出
    var load = [Double](repeating: 0, count: 1)
    let busy = getloadavg(&load, 1) == 1 && load[0] > Double(ProcessInfo.processInfo.activeProcessorCount) * 0.75
    let limit = busy ? 500.0 : 250.0
    expect(fastest < limit, "每帧要 \(fastest) 微秒（平均 \(average) 微秒，1 分钟平均负载 \(load[0])，上限 \(limit) 微秒）")
}

/// 这个测试程序是不是在 Rosetta 下跑的（Apple 芯片上跑 Intel 那一份）
func runningUnderRosetta() -> Bool {
    var translated: Int32 = 0
    var size = MemoryLayout<Int32>.size
    return sysctlbyname("sysctl.proc_translated", &translated, &size, nil, 0) == 0 && translated == 1
}

func testCancellerLatency() {
    // 一小段 2 kHz 的声音（高通滤波不会削掉它），看它在处理后的声音里出现在哪里：前后试几个位置，都是同一个延迟
    for position in [12_000, 12_097, 12_480, 13_001] {
        guard let canceller = WebRTCEchoCanceller(filters: 6) else {
            Harness.fail("建不了 AEC3")
            return
        }
        var mic = [Float](repeating: 0, count: 48_000)
        for index in 0..<96 {
            let window = sin(Float.pi * Float(index) / 96)
            mic[position + index] = 0.5 * sin(2 * Float.pi * 2_000 * Float(index) / 48_000) * window * window
        }
        let silence = [Float](repeating: 0, count: 480)
        var output = [Float](repeating: 0, count: 48_000)
        mic.withUnsafeBufferPointer { input in
            output.withUnsafeMutableBufferPointer { out in
                for frame in 0..<100 {
                    _ = canceller.render(silence)
                    _ = canceller.capture(input.baseAddress! + frame * 480, into: out.baseAddress! + frame * 480)
                }
            }
        }
        var best = (lag: -1, score: -Float.infinity)
        for lag in 0..<2_000 {
            var score: Float = 0
            for index in 0..<96 {
                score += mic[position + index] * output[position + index + lag]
            }
            if score > best.score {
                best = (lag, score)
            }
        }
        expectEqual(best.lag, canceller.latency, "从第 \(position) 个采样起的声音晚了 \(best.lag) 个采样")
    }
}

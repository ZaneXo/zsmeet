import CoreAudio
import CRecorder
import Foundation
@testable import RecorderKit

/// 声音处理：队列、环形缓冲、混音、音量、采样率转换、编码
let audioTests: [(String, () throws -> Void)] = [
    ("采样队列：先进先出，大量进出顺序不乱", testSampleFIFO),
    ("环形缓冲：绕圈、满了丢新来的并计数", testRingWrapsAndCountsDrops),
    ("环形缓冲：IOProc 把前两个声道取平均成单声道", testRingDownmix),
    ("环形缓冲：IOProc 带着主机时间时记下这一批第一个采样的序号和时间", testRingStamps),
    ("环形缓冲：时间戳攒满了不覆盖旧的、不等，计数；采样照常写", testRingStampOverflow),
    ("环形缓冲：看一眼最早的时间戳，不取出", testRingPeekStamp),
    ("混音：先攒够缓冲再取", testMixerWaitsForCushion),
    ("混音：来源跟不上时补静音，再重新攒", testMixerPadsWhenSourceFallsBehind),
    ("混音：积压超过上限时丢掉最早的", testMixerDropsBacklog),
    ("混音：卡住以后分几次补上，等着补上的声音不算积压", testMixerCatchesUpInSeveralPasses),
    ("混音：两路相加，没超过 -1 dBFS 的原样", testMixerSums),
    ("混音：两路加起来超过 -1 dBFS 时压下来，不削顶", testMixerLimits),
    ("混音的限幅器：一直很响时整体压低、波形不变，响完以后回到原样", testLimiterSteadyAndRelease),
    ("混音的限幅器：峰值在一块的开头也不超过，往下压的速度有上限，NaN 换成 0", testLimiterEdges),
    ("混音：结束时剩下的都混出来", testMixerDrain),
    ("音量：按分贝换算到 0…1", testLevelMeter),
    ("采样率转换：48 kHz 原样通过", testResamplerPassThrough),
    ("采样率转换：44.1 kHz 转成 48 kHz，前后接得上", testResamplerConvertsRate),
    ("编码：整场一个编码器，包数只比采样多出一次预热", testEncoderIsOneContinuousStream),
]

func testSampleFIFO() {
    var fifo = SampleFIFO()
    fifo.append([1, 2, 3, 4, 5])
    var out = [Float](repeating: 0, count: 3)
    out.withUnsafeMutableBufferPointer { fifo.take(3, into: $0.baseAddress!) }
    expectEqual(out, [1, 2, 3])
    expectEqual(fifo.count, 2)

    var mixed: [Float] = [10, 10]
    let energy = mixed.withUnsafeMutableBufferPointer { fifo.takeAdding(2, into: $0.baseAddress!) }
    expectEqual(mixed, [14, 15])
    expectEqual(energy, 16 + 25)
    expect(fifo.isEmpty)

    // 进的比出的多，内部会挪动存储：取出来的必须还是连续的
    var next: Float = 0
    var expected: Float = 0
    var inOrder = true
    for _ in 0..<200 {
        fifo.append((0..<1_000).map { _ in
            next += 1
            return next
        })
        var chunk = [Float](repeating: 0, count: 700)
        chunk.withUnsafeMutableBufferPointer { fifo.take(700, into: $0.baseAddress!) }
        for value in chunk {
            expected += 1
            inOrder = inOrder && value == expected
        }
    }
    expect(inOrder, "取出来的顺序乱了")
    fifo.drop(100)
    var one: [Float] = [0]
    one.withUnsafeMutableBufferPointer { fifo.take(1, into: $0.baseAddress!) }
    expectEqual(one[0], expected + 101)
    expectEqual(fifo.count, 200 * 300 - 101)
}

private func write(_ ring: OpaquePointer, _ samples: [Float]) -> Int {
    samples.withUnsafeBufferPointer { zr_ring_write(ring, $0.baseAddress!, $0.count) }
}

private func read(_ ring: OpaquePointer, _ count: Int) -> [Float] {
    var out = [Float](repeating: 0, count: count)
    let n = out.withUnsafeMutableBufferPointer { zr_ring_read(ring, $0.baseAddress!, count) }
    return Array(out.prefix(n))
}

func testRingWrapsAndCountsDrops() {
    let ring = zr_ring_create(5, 2)!
    defer { zr_ring_destroy(ring) }
    expectEqual(zr_ring_capacity(ring), 8, "向上取到 2 的幂")
    expectEqual(write(ring, [1, 2, 3, 4, 5, 6]), 6)
    expectEqual(read(ring, 4), [1, 2, 3, 4])
    // 还剩 2 个，只放得下 6 个：13 被丢掉
    expectEqual(write(ring, [7, 8, 9, 10, 11, 12, 13]), 6)
    expectEqual(zr_ring_total_dropped(ring), 1)
    expectEqual(zr_ring_available(ring), 8)
    expectEqual(read(ring, 100), [5, 6, 7, 8, 9, 10, 11, 12])
    expectEqual(read(ring, 1), [])
    expectEqual(zr_ring_total_written(ring), 12)
}

/// 造一个 AudioBufferList 交给 zr_ring_write_buffer_list（和 IOProc 走同一段代码），time 是 IOProc 的 input_time
private func writeBufferList(_ ring: OpaquePointer, _ buffers: [(channels: Int, samples: [Float])], time: AudioTimeStamp? = nil) -> Int {
    let list = AudioBufferList.allocate(maximumBuffers: max(buffers.count, 1))
    list.unsafeMutablePointer.pointee.mNumberBuffers = UInt32(buffers.count)
    var storage: [UnsafeMutableBufferPointer<Float>] = []
    defer {
        storage.forEach { $0.deallocate() }
        free(list.unsafeMutablePointer)
    }
    for (index, buffer) in buffers.enumerated() {
        let data = UnsafeMutableBufferPointer<Float>.allocate(capacity: max(buffer.samples.count, 1))
        _ = data.initialize(from: buffer.samples)
        storage.append(data)
        list[index] = AudioBuffer(mNumberChannels: UInt32(buffer.channels),
                                  mDataByteSize: UInt32(buffer.samples.count * MemoryLayout<Float>.size),
                                  mData: UnsafeMutableRawPointer(data.baseAddress))
    }
    guard var time else {
        return zr_ring_write_buffer_list(ring, list.unsafePointer, nil)
    }
    return zr_ring_write_buffer_list(ring, list.unsafePointer, &time)
}

/// 取出全部时间戳
private func stamps(_ ring: OpaquePointer) -> [zr_stamp] {
    var out = [zr_stamp](repeating: zr_stamp(), count: Int(ZR_STAMP_CAPACITY))
    let count = out.withUnsafeMutableBufferPointer { zr_ring_read_stamps(ring, $0.baseAddress!, $0.count) }
    return Array(out[0..<count])
}

func testRingStamps() {
    let ring = zr_ring_create(64, 2)!
    defer { zr_ring_destroy(ring) }
    var time = AudioTimeStamp()
    time.mFlags = [.hostTimeValid, .sampleTimeValid]
    time.mHostTime = 1_000_000
    time.mSampleTime = 512
    expectEqual(writeBufferList(ring, [(2, [1, 1, 1, 1, 1, 1])], time: time), 3)
    // 没有主机时间的不记
    var noHost = AudioTimeStamp()
    noHost.mFlags = .sampleTimeValid
    expectEqual(writeBufferList(ring, [(1, [2, 2])], time: noHost), 2)
    expectEqual(writeBufferList(ring, [(1, [2, 2])]), 2)
    // 只有主机时间：采样计数是 NaN
    var hostOnly = AudioTimeStamp()
    hostOnly.mFlags = .hostTimeValid
    hostOnly.mHostTime = 2_000_000
    expectEqual(writeBufferList(ring, [(1, [3, 3, 3, 3])], time: hostOnly), 4)
    // 读文件、测试用的：直接给纳秒
    expectEqual([Float](repeating: 4, count: 5).withUnsafeBufferPointer { zr_ring_write_stamped(ring, $0.baseAddress!, 5, 123_456_789) }, 5)

    let got = stamps(ring)
    expectEqual(got.map(\.index), [0, 7, 11], "第一个采样的序号")
    expectEqual(got.map(\.frames), [3, 4, 5])
    expectEqual(got.map(\.host_ns), [zr_host_time_to_ns(1_000_000), zr_host_time_to_ns(2_000_000), 123_456_789])
    expectEqual(got.first?.sample_time, 512)
    expect(got[1].sample_time.isNaN && got[2].sample_time.isNaN, "没有采样计数时是 NaN")
    expectEqual(stamps(ring).count, 0, "取过的不会再取到")
    // 主机时间换算成纳秒：和 DispatchTime 是同一个时钟
    let now = DispatchTime.now().uptimeNanoseconds
    let host = zr_host_time_to_ns(mach_absolute_time())
    expect(host >= now && host - now < 50_000_000, "主机时间 \(host) 和 DispatchTime \(now) 对不上")
    expectEqual(zr_ring_read_position(ring), 0)
    _ = read(ring, 4)
    expectEqual(zr_ring_read_position(ring), 4, "读出过几个采样")
}

func testRingStampOverflow() {
    let ring = zr_ring_create(1 << 16, 1)!
    defer { zr_ring_destroy(ring) }
    let one: [Float] = [0.5]
    for index in 0..<(Int(ZR_STAMP_CAPACITY) + 10) {
        _ = one.withUnsafeBufferPointer { zr_ring_write_stamped(ring, $0.baseAddress!, 1, UInt64(index)) }
    }
    expectEqual(zr_ring_stamps_dropped(ring), 10, "满了的 10 个没记")
    expectEqual(zr_ring_total_written(ring), UInt64(ZR_STAMP_CAPACITY) + 10, "采样照常写")
    let got = stamps(ring)
    expectEqual(got.count, Int(ZR_STAMP_CAPACITY))
    expectEqual(got.last?.host_ns, UInt64(ZR_STAMP_CAPACITY) - 1, "留着的是先来的，没被覆盖")
    // 取走以后又能记了
    _ = one.withUnsafeBufferPointer { zr_ring_write_stamped(ring, $0.baseAddress!, 1, 99) }
    expectEqual(stamps(ring).map(\.host_ns), [99])
}

func testRingPeekStamp() {
    let ring = zr_ring_create(64, 1)!
    defer { zr_ring_destroy(ring) }
    var stamp = zr_stamp()
    expect(!zr_ring_peek_stamp(ring, &stamp), "还没有时间戳")
    let samples: [Float] = [1, 2, 3]
    _ = samples.withUnsafeBufferPointer { zr_ring_write_stamped(ring, $0.baseAddress!, 3, 100) }
    _ = samples.withUnsafeBufferPointer { zr_ring_write_stamped(ring, $0.baseAddress!, 3, 200) }
    expect(zr_ring_peek_stamp(ring, &stamp))
    expectEqual(stamp.host_ns, 100, "最早的那个")
    expect(zr_ring_peek_stamp(ring, &stamp))
    expectEqual(stamp.host_ns, 100, "看了不算取走")
    expectEqual(stamps(ring).map(\.index), [0, 3], "照样都取得到")
    expect(!zr_ring_peek_stamp(ring, &stamp), "取完了")
}

func testRingDownmix() {
    let ring = zr_ring_create(64, 2)!
    defer { zr_ring_destroy(ring) }
    // 交错的立体声 L R L R：左右取平均
    expectEqual(writeBufferList(ring, [(2, [1, 3, -1, 1])]), 2)
    expectEqual(read(ring, 10), [2, 0])
    // 不交错的立体声（两个 buffer 各一个声道）
    expectEqual(writeBufferList(ring, [(1, [1, 1]), (1, [0, -1])]), 2)
    expectEqual(read(ring, 10), [0.5, 0])
    // 4 个声道的声卡：只用前两个
    expectEqual(writeBufferList(ring, [(4, [1, 3, 100, 100, 5, 7, 100, 100])]), 2)
    expectEqual(read(ring, 10), [2, 6])
    // 单声道原样写入
    expectEqual(writeBufferList(ring, [(1, [0.25, -0.5])]), 2)
    expectEqual(read(ring, 10), [0.25, -0.5])
    // 空的
    expectEqual(writeBufferList(ring, []), 0)
}

private func mix(_ mixer: TimeMixer, _ count: Int, due: Int? = nil) -> (samples: [Float], energy: [Double]) {
    var out = [Float](repeating: 9, count: count)
    var energy = [Double](repeating: -1, count: mixer.lanes.count)
    out.withUnsafeMutableBufferPointer { mixer.mix(count: count, due: due, into: $0.baseAddress!, energy: &energy) }
    return (out, energy)
}

func testMixerWaitsForCushion() {
    let mixer = TimeMixer(kinds: [.mic], cushion: 100, maxBacklog: 400)
    let lane = mixer.lanes[0]
    lane.queue.append([Float](repeating: 0.5, count: 60))
    let first = mix(mixer, 50)
    expectEqual(first.samples, [Float](repeating: 0, count: 50), "还没攒够：静音")
    expectEqual(first.energy, [0])
    expect(!lane.primed)
    expectEqual(lane.queue.count, 60, "没攒够时不取")

    lane.queue.append([Float](repeating: 0.5, count: 40))
    let second = mix(mixer, 50)
    expectEqual(second.samples, [Float](repeating: 0.5, count: 50))
    expectEqual(second.energy, [12.5])
    expect(lane.primed)
    expectEqual(lane.queue.count, 50)
}

func testMixerPadsWhenSourceFallsBehind() {
    let mixer = TimeMixer(kinds: [.mic], cushion: 100, maxBacklog: 400)
    let lane = mixer.lanes[0]
    lane.queue.append([Float](repeating: 0.25, count: 100))
    _ = mix(mixer, 80)
    let padded = mix(mixer, 50)
    expectEqual(Array(padded.samples.prefix(20)), [Float](repeating: 0.25, count: 20))
    expectEqual(Array(padded.samples.suffix(30)), [Float](repeating: 0, count: 30), "不够的补静音")
    expect(!lane.primed, "断了以后要重新攒")
    expectEqual(lane.paddedSamples, 30)
}

func testMixerDropsBacklog() {
    let mixer = TimeMixer(kinds: [.system], cushion: 100, maxBacklog: 400)
    let lane = mixer.lanes[0]
    // 每个采样的值是它的序号除以 2000（都小于 -1 dBFS，限幅器不动它们），看取出来的是哪几个
    lane.queue.append((0..<1_000).map { Float($0) / 2_000 })
    let out = mix(mixer, 50)
    // 积压 950 超过 400：丢掉最早的 850，取第 850…899 个，剩下的正好是 cushion
    expectEqual(out.samples.first, 0.425)
    expectEqual(out.samples.last, 0.4495)
    expectEqual(lane.droppedSamples, 850)
    expectEqual(lane.queue.count, 100)
    expectEqual(mix(mixer, 1).samples, [0.45])
}

func testMixerCatchesUpInSeveralPasses() {
    let mixer = TimeMixer(kinds: [.mic], cushion: 100, maxBacklog: 400)
    let lane = mixer.lanes[0]
    // 处理线程卡了一阵：这一次该出 2000 个，分两次混；队列里攒着 2100 个，多出来的正好是 cushion
    lane.queue.append((0..<2_100).map { Float($0) / 10_000 })
    let first = mix(mixer, 1_000, due: 2_000)
    let second = mix(mixer, 1_000, due: 1_000)
    expectEqual(lane.droppedSamples, 0, "卡住期间录下的声音不能当成积压丢掉")
    expectEqual(lane.paddedSamples, 0, "也不用补静音")
    expectEqual(first.samples.first, 0)
    expectEqual(second.samples.first, 0.1)
    expectEqual(second.samples.last, 0.1999)
    expectEqual(lane.queue.count, 100)
}

func testMixerSums() {
    let mixer = TimeMixer(kinds: [.mic, .system], cushion: 1, maxBacklog: 400)
    mixer.lanes[0].queue.append([0.25, -0.25, 0.125])
    mixer.lanes[1].queue.append([0.5, 0.25, 0.25])
    let out = mix(mixer, 3)
    expectEqual(out.samples, [0.75, 0, 0.375])
    expectEqual(out.energy, [0.0625 + 0.0625 + 0.015625, 0.25 + 0.0625 + 0.0625])
}

func testMixerLimits() {
    let mixer = TimeMixer(kinds: [.mic, .system], cushion: 1, maxBacklog: 400)
    mixer.lanes[0].queue.append([0.75, 0.25, -0.75])
    mixer.lanes[1].queue.append([0.5, 0.25, -0.5])
    let out = mix(mixer, 3)
    let ceiling: Float = MixLimiter.threshold
    // 两个 1.25 的峰压到正好 -1 dBFS（以前削成 1），中间的跟着压低一些，正负号不变。
    // 分开写、标上类型：CI 上的 Swift（Xcode 16）推不出太长的式子的类型
    let firstOff: Float = abs(out.samples[0] - ceiling)
    let lastOff: Float = abs(out.samples[2] + ceiling)
    expect(firstOff < 1e-6 && lastOff < 1e-6, "峰值应该压到 \(ceiling)：\(out.samples)")
    let middle: Float = out.samples[1]
    expect(middle > 0.3 && middle < 0.5, "中间的跟着压低：\(out.samples)")
    // 音量（能量）按限幅之前的算
    expectEqual(out.energy, [0.5625 + 0.0625 + 0.5625, 0.25 + 0.0625 + 0.25])
}

private func report(_ text: String) {
    print("    测得：\(text)")
}

/// 用限幅器按每块 block 个采样处理 input，返回结果
private func limit(_ input: [Float], block: Int, limiter: inout MixLimiter) -> [Float] {
    var output = input
    output.withUnsafeMutableBufferPointer { buffer in
        var start = 0
        while start < buffer.count {
            let count = min(block, buffer.count - start)
            limiter.process(buffer.baseAddress! + start, count: count)
            start += count
        }
    }
    return output
}

func testLimiterSteadyAndRelease() {
    let rate = 48_000
    let loudAmplitude: Float = 1.6
    // 0.5 秒很响的正弦波（麦克风调大了、电脑声音也很响，加起来 +4 dBFS），接着 3 秒正常的声音
    let loudStep: Double = 2.0 * Double.pi * 440.0 / Double(rate)
    let quietStep: Double = 2.0 * Double.pi * 300.0 / Double(rate)
    var input: [Float] = (0..<(rate / 2)).map { (i: Int) -> Float in loudAmplitude * Float(sin(loudStep * Double(i))) }
    let loudEnd = input.count
    input += (0..<(3 * rate)).map { (i: Int) -> Float in Float(0.1 * sin(quietStep * Double(i))) }
    var limiter = MixLimiter()
    let output = limit(input, block: 480, limiter: &limiter)
    let ceiling = MixLimiter.threshold

    let peak: Float = output.map { (x: Float) -> Float in abs(x) }.max() ?? 0
    let allowed: Float = ceiling * 1.000_001
    expect(peak <= allowed, "超过了 -1 dBFS：\(peak)")
    // 压下来以后是整体变小、波形不变（削顶的话峰值附近差 0.7 左右）：和按比例缩小的原波形相比，差不超过 0.02
    let scale = ceiling / loudAmplitude
    var worst: Float = 0
    for index in (rate / 10)..<loudEnd {
        let off: Float = abs(output[index] - input[index] * scale)
        worst = max(worst, off)
    }
    report("一直很响时和按比例缩小的原波形最多差 \(worst)")
    expect(worst <= 0.02, "压的时候波形变了：最多差 \(worst)")
    // 响完 1.5 秒以后回到原样，一个比特都不变
    let settled = loudEnd + 3 * rate / 2
    expect(output[settled...].elementsEqual(input[settled...]), "响完以后应该回到原样")
    expectEqual(limiter.gain, 1)
}

func testLimiterEdges() {
    let ceiling = MixLimiter.threshold
    // 峰值在一块的第一个采样：来不及提前压，也不超过
    var limiter = MixLimiter()
    var out = limit([2, 0.1, 0.1], block: 3, limiter: &limiter)
    let startOff: Float = abs(out[0] - ceiling)
    expect(startOff < 1e-6, "块开头的峰值应该压到 \(ceiling)：\(out[0])")
    expect(out[1] < 0.1 && out[2] < 0.1, "后面的跟着压低、慢慢回来：\(out)")
    limiter.reset()
    expectEqual(limiter.gain, 1, "reset 以后从 1 重新开始")

    // 峰值在块的中间：提前开始压，往下压每个采样最多 1/96，离峰值远的原样
    let quiet: Float = 0.05
    var input = [Float](repeating: quiet, count: 1_000)
    input[500] = 3
    out = limit(input, block: 1_000, limiter: &limiter)
    let peakOff: Float = abs(out[500] - ceiling)
    expect(peakOff < 1e-6, "峰值应该压到 \(ceiling)：\(out[500])")
    let gains: [Float] = (0..<500).map { (i: Int) -> Float in out[i] / input[i] }
    let steepest: Float = zip(gains, gains.dropFirst()).map { (a: Float, b: Float) -> Float in a - b }.max() ?? 0
    let fastest: Float = 1 / MixLimiter.attackSamples + 1e-6
    expect(steepest <= fastest, "往下压得太快：每个采样 \(steepest)")
    expect(out[0..<400].elementsEqual(input[0..<400]), "离峰值远的地方不该动")

    // 大得离谱、但是有限的数：压得很低时也不超过（增益算成 1 减去压低的量，Float 的精度不够时会超过）
    for huge in [Float(2e7), Float(1e30), Float.greatestFiniteMagnitude] {
        limiter = MixLimiter()
        out = limit([huge, -huge], block: 2, limiter: &limiter)
        let worstHuge: Float = max(abs(out[0]), abs(out[1]))
        let allowedHuge: Float = ceiling * 1.000_01
        expect(worstHuge <= allowedHuge, "\(huge) 压下来以后是 \(out)，超过了 \(ceiling)")
    }

    // 不是有限的数换成 0（以前削成 -1 或 1，是一声「咔」）
    limiter = MixLimiter()
    out = limit([.nan, 0.5, .infinity, -.infinity], block: 4, limiter: &limiter)
    expectEqual(out, [0, 0.5, 0, 0])
}

func testMixerDrain() {
    let mixer = TimeMixer(kinds: [.mic, .system], cushion: 100, maxBacklog: 400)
    mixer.lanes[0].queue.append([Float](repeating: 0.25, count: 30))
    mixer.lanes[1].queue.append([Float](repeating: 0.5, count: 10))
    var out: [Float] = []
    var energy = [0.0, 0.0]
    expectEqual(mixer.drain(into: &out, energy: &energy), 30)
    expectEqual(Array(out.prefix(10)), [Float](repeating: 0.75, count: 10))
    expectEqual(Array(out.suffix(20)), [Float](repeating: 0.25, count: 20))
    expectEqual(energy, [30 * 0.0625, 10 * 0.25])
    expectEqual(mixer.drain(into: &out, energy: &energy), 0)
}

func testLevelMeter() {
    func level(rms: Double) -> Double {
        LevelMeter.level(energy: rms * rms * 4_800, count: 4_800)
    }
    expectEqual(LevelMeter.level(energy: 0, count: 4_800), 0)
    expectEqual(LevelMeter.level(energy: 1, count: 0), 0)
    expectClose(level(rms: 1), 1, within: 1e-9)
    expectClose(level(rms: 2), 1, within: 1e-9)
    expectClose(level(rms: 0.1), 2.0 / 3, within: 1e-9)
    expectClose(level(rms: 0.01), 1.0 / 3, within: 1e-9)
    expectClose(level(rms: 0.001), 0, within: 1e-9)
    expectEqual(level(rms: 0.0001), 0)
}

func testResamplerPassThrough() throws {
    let resampler = try Resampler(inputRate: 48_000)
    var out = SampleFIFO()
    try [Float](arrayLiteral: 1, 2, 3).withUnsafeBufferPointer { try resampler.process($0, into: &out) }
    try resampler.finish(into: &out)
    var samples = [Float](repeating: 0, count: 3)
    samples.withUnsafeMutableBufferPointer { out.take(3, into: $0.baseAddress!) }
    expectEqual(samples, [1, 2, 3])
}

func testResamplerConvertsRate() throws {
    let resampler = try Resampler(inputRate: 44_100)
    let input = (0..<44_100).map { Float(0.5 * sin(2 * Double.pi * 1_000 * Double($0) / 44_100)) }
    var out = SampleFIFO()
    // 像设备一样一批一批地交，批的大小不固定
    var offset = 0
    var size = 1
    while offset < input.count {
        let n = min(size, input.count - offset)
        try input.withUnsafeBufferPointer { try resampler.process(UnsafeBufferPointer(rebasing: $0[offset..<offset + n]), into: &out) }
        offset += n
        size = size * 7 % 1_201 + 1
    }
    try resampler.finish(into: &out)
    expectClose(Double(out.count), 48_000, within: 2, "1 秒的声音转完还是 1 秒")
    var samples = [Float](repeating: 0, count: out.count)
    let count = out.count
    samples.withUnsafeMutableBufferPointer { out.take(count, into: $0.baseAddress!) }
    // 中间一段的均方根和原来一样（0.5 / √2），说明没有断续、没有多出静音
    let middle = samples[4_800..<43_200]
    let rms = (middle.reduce(0.0) { $0 + Double($1 * $1) } / Double(middle.count)).squareRoot()
    expectClose(rms, 0.5 / 2.0.squareRoot(), within: 0.005)
}

func testEncoderIsOneContinuousStream() throws {
    let encoder = try AACEncoder()
    expectEqual(encoder.primingSamples, 2_112, "苹果的 AAC 编码器预热 2112 个采样")
    expectEqual(encoder.droppedPrimingPackets, 2)

    let total = 480_000
    let input = (0..<total).map { Float(0.3 * sin(2 * Double.pi * 440 * Double($0) / 48_000)) }
    var packets = 0
    var largest = 0
    var offset = 0
    var size = 960
    // 像现场录音一样一小段一小段地喂（大小不固定）
    while offset < total {
        let n = min(size, total - offset)
        try input.withUnsafeBufferPointer { buffer in
            try encoder.encode(UnsafeBufferPointer(rebasing: buffer[offset..<offset + n])) { packet in
                packets += 1
                largest = max(largest, packet.count)
            }
        }
        offset += n
        size = size * 5 % 3_001 + 1
    }
    try encoder.finish { packet in
        packets += 1
        largest = max(largest, packet.count)
    }
    // 整场只预热一次：包数 = （采样数 + 2112）向上取整到整包，再减去丢掉的两包
    expectEqual(packets, Int((Double(total + 2_112) / 1_024).rounded(.up)) - 2)
    expect(largest > 0 && largest <= 768, "单声道 AAC 包最大 768 字节，实际 \(largest)")
}

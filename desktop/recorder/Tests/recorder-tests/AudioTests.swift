import CoreAudio
import CRecorder
import Foundation
@testable import RecorderKit

/// 声音处理：队列、环形缓冲、混音、音量、采样率转换、编码
let audioTests: [(String, () throws -> Void)] = [
    ("采样队列：先进先出，大量进出顺序不乱", testSampleFIFO),
    ("环形缓冲：绕圈、满了丢新来的并计数", testRingWrapsAndCountsDrops),
    ("环形缓冲：IOProc 把前两个声道取平均成单声道", testRingDownmix),
    ("混音：先攒够缓冲再取", testMixerWaitsForCushion),
    ("混音：来源跟不上时补静音，再重新攒", testMixerPadsWhenSourceFallsBehind),
    ("混音：积压超过上限时丢掉最早的", testMixerDropsBacklog),
    ("混音：两路相加，超过满幅的削掉", testMixerSumsAndClips),
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

/// 造一个 AudioBufferList 交给 zr_ring_write_buffer_list（和 IOProc 走同一段代码）
private func writeBufferList(_ ring: OpaquePointer, _ buffers: [(channels: Int, samples: [Float])]) -> Int {
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
    return zr_ring_write_buffer_list(ring, list.unsafePointer)
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

private func mix(_ mixer: TimeMixer, _ count: Int) -> (samples: [Float], energy: [Double]) {
    var out = [Float](repeating: 9, count: count)
    var energy = [Double](repeating: -1, count: mixer.lanes.count)
    out.withUnsafeMutableBufferPointer { mixer.mix(count: count, into: $0.baseAddress!, energy: &energy) }
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
    lane.queue.append((0..<1_000).map { Float($0) / 1_000 })
    let out = mix(mixer, 50)
    // 积压 950 超过 400：丢掉最早的 850，取第 850…899 个，剩下的正好是 cushion
    expectEqual(out.samples.first, 0.85)
    expectEqual(out.samples.last, 0.899)
    expectEqual(lane.droppedSamples, 850)
    expectEqual(lane.queue.count, 100)
    expectEqual(mix(mixer, 1).samples, [0.9])
}

func testMixerSumsAndClips() {
    let mixer = TimeMixer(kinds: [.mic, .system], cushion: 1, maxBacklog: 400)
    mixer.lanes[0].queue.append([0.75, 0.25, -0.75])
    mixer.lanes[1].queue.append([0.5, 0.25, -0.5])
    let out = mix(mixer, 3)
    expectEqual(out.samples, [1, 0.5, -1])
    expectEqual(out.energy, [0.5625 + 0.0625 + 0.5625, 0.25 + 0.0625 + 0.25])
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
    [Float](arrayLiteral: 1, 2, 3).withUnsafeBufferPointer { resampler.process($0, into: &out) }
    resampler.finish(into: &out)
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
        input.withUnsafeBufferPointer { resampler.process(UnsafeBufferPointer(rebasing: $0[offset..<offset + n]), into: &out) }
        offset += n
        size = size * 7 % 1_201 + 1
    }
    resampler.finish(into: &out)
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

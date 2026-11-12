import AVFoundation

/// AAC-LC 编码器（48 kHz、单声道、约 64 kbps）。整场录音只建这一个：暂停、切分片都不重建，
/// 所以分片拼起来是一路连续的流，时长和录到的一样。每片重新开一个编码器的话，
/// 每片开头都会多出约 44 毫秒的预热，一小时差出十几秒。
public final class AACEncoder {
    public static let sampleRate = 48_000
    public static let samplesPerPacket = 1_024

    /// 编码器在开头垫的预热采样数（苹果的编码器是 2112 个，约 44 毫秒的静音）
    public let primingSamples: Int
    /// 开头丢掉的整包数：预热里够一整包的部分都丢掉，剩下的不到一包（2112 的话剩 64 个，1.3 毫秒）。
    /// 这样拼起来的声音和录音的时间轴对得上，标记重点的时间点、转写的时间也准。
    /// 丢掉的两包本来就是静音，代价只是解码出来的第一包（约 21 毫秒）有点失真，那时还没开始录到声音。
    public let droppedPrimingPackets: Int

    private let converter: AVAudioConverter
    private let inputBuffer: AVAudioPCMBuffer
    private let outputBuffer: AVAudioCompressedBuffer
    private var pending = SampleFIFO()
    private var ended = false
    private var packetsToDrop: Int

    public init(bitRate: Int = 64_000) throws {
        var description = AudioStreamBasicDescription(
            mSampleRate: Double(Self.sampleRate),
            mFormatID: kAudioFormatMPEG4AAC,
            mFormatFlags: AudioFormatFlags(MPEG4ObjectID.AAC_LC.rawValue),
            mBytesPerPacket: 0,
            mFramesPerPacket: UInt32(Self.samplesPerPacket),
            mBytesPerFrame: 0,
            mChannelsPerFrame: 1,
            mBitsPerChannel: 0,
            mReserved: 0
        )
        guard let pcm = AVAudioFormat(commonFormat: .pcmFormatFloat32, sampleRate: Double(Self.sampleRate), channels: 1, interleaved: false),
              let aac = AVAudioFormat(streamDescription: &description),
              let converter = AVAudioConverter(from: pcm, to: aac),
              let inputBuffer = AVAudioPCMBuffer(pcmFormat: pcm, frameCapacity: 4_096)
        else {
            throw RecorderFailure("encoder", "这台电脑上建不了 AAC 编码器，没法录音")
        }
        converter.bitRate = bitRate
        self.converter = converter
        self.inputBuffer = inputBuffer
        outputBuffer = AVAudioCompressedBuffer(format: aac, packetCapacity: 16, maximumPacketSize: max(converter.maximumOutputPacketSize, 1_024))
        primingSamples = Int(converter.primeInfo.leadingFrames)
        droppedPrimingPackets = primingSamples / Self.samplesPerPacket
        packetsToDrop = droppedPrimingPackets
    }

    /// 喂进一段 48 kHz 单声道的采样。编好的每个 AAC 包（不带 ADTS 头）按顺序交给 emit。
    public func encode(_ samples: UnsafeBufferPointer<Float>, emit: (UnsafeRawBufferPointer) throws -> Void) throws {
        precondition(!ended, "编码器已经结束了")
        pending.append(samples)
        try drain(emit: emit)
    }

    /// 录音结束：把编码器里压着的都编出来（最后一包不满的补静音）。之后不能再用。
    public func finish(emit: (UnsafeRawBufferPointer) throws -> Void) throws {
        guard !ended else { return }
        ended = true
        try drain(emit: emit)
    }

    private func drain(emit: (UnsafeRawBufferPointer) throws -> Void) throws {
        while true {
            outputBuffer.packetCount = 0
            outputBuffer.byteLength = 0
            var error: NSError?
            let status = converter.convert(to: outputBuffer, error: &error) { requested, inputStatus in
                let n = min(Int(requested), self.pending.count, Int(self.inputBuffer.frameCapacity))
                guard n > 0 else {
                    inputStatus.pointee = self.ended ? .endOfStream : .noDataNow
                    return nil
                }
                self.pending.take(n, into: self.inputBuffer.floatChannelData![0])
                self.inputBuffer.frameLength = AVAudioFrameCount(n)
                inputStatus.pointee = .haveData
                return self.inputBuffer
            }
            if status == .error {
                throw RecorderFailure("encoder", "AAC 编码出错（\(error?.localizedDescription ?? "未知原因")），录音停了。已经录好的部分不受影响。")
            }
            try emitPackets(emit)
            if status != .haveData {
                return
            }
        }
    }

    private func emitPackets(_ emit: (UnsafeRawBufferPointer) throws -> Void) throws {
        guard let descriptions = outputBuffer.packetDescriptions else { return }
        for index in 0..<Int(outputBuffer.packetCount) {
            if packetsToDrop > 0 {
                packetsToDrop -= 1
                continue
            }
            let description = descriptions[index]
            let packet = UnsafeRawBufferPointer(start: outputBuffer.data + Int(description.mStartOffset), count: Int(description.mDataByteSize))
            try emit(packet)
        }
    }
}

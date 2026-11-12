import AVFoundation

/// 把一路单声道的声音转成 48 kHz。一路声音从头到尾用同一个转换器，前后的采样接得上，
/// 不会每次重新开始、多出或者少掉一小段。本来就是 48 kHz 的原样通过。
public final class Resampler {
    public static let outputRate = 48_000.0

    public let inputRate: Double
    private let converter: AVAudioConverter?
    private let inputBuffer: AVAudioPCMBuffer?
    private let outputBuffer: AVAudioPCMBuffer?
    private var pending = SampleFIFO()
    private var ended = false

    public init(inputRate: Double) throws {
        self.inputRate = inputRate
        if inputRate == Self.outputRate {
            converter = nil
            inputBuffer = nil
            outputBuffer = nil
            return
        }
        guard inputRate >= 8_000, inputRate <= 384_000,
              let inputFormat = AVAudioFormat(commonFormat: .pcmFormatFloat32, sampleRate: inputRate, channels: 1, interleaved: false),
              let outputFormat = AVAudioFormat(commonFormat: .pcmFormatFloat32, sampleRate: Self.outputRate, channels: 1, interleaved: false),
              let converter = AVAudioConverter(from: inputFormat, to: outputFormat),
              let inputBuffer = AVAudioPCMBuffer(pcmFormat: inputFormat, frameCapacity: 4_096),
              let outputBuffer = AVAudioPCMBuffer(pcmFormat: outputFormat, frameCapacity: 16_384)
        else {
            throw RecorderFailure("unsupported_format", "不支持 \(Int(inputRate)) Hz 的声音")
        }
        converter.sampleRateConverterQuality = AVAudioQuality.high.rawValue
        self.converter = converter
        self.inputBuffer = inputBuffer
        self.outputBuffer = outputBuffer
    }

    /// 转换 samples，转好的追加到 output（转换器会压着一点点，下次或者 finish 时才出来）
    public func process(_ samples: UnsafeBufferPointer<Float>, into output: inout SampleFIFO) {
        guard converter != nil else {
            output.append(samples)
            return
        }
        pending.append(samples)
        drain(into: &output)
    }

    /// 这一路结束了：把转换器里压着的也取出来。之后不能再用。
    public func finish(into output: inout SampleFIFO) {
        guard converter != nil, !ended else { return }
        ended = true
        drain(into: &output)
    }

    private func drain(into output: inout SampleFIFO) {
        guard let converter, let inputBuffer, let outputBuffer else { return }
        while true {
            outputBuffer.frameLength = 0
            var error: NSError?
            let status = converter.convert(to: outputBuffer, error: &error) { requested, inputStatus in
                let n = min(Int(requested), self.pending.count, Int(inputBuffer.frameCapacity))
                guard n > 0 else {
                    inputStatus.pointee = self.ended ? .endOfStream : .noDataNow
                    return nil
                }
                self.pending.take(n, into: inputBuffer.floatChannelData![0])
                inputBuffer.frameLength = AVAudioFrameCount(n)
                inputStatus.pointee = .haveData
                return inputBuffer
            }
            if outputBuffer.frameLength > 0 {
                output.append(UnsafeBufferPointer(start: outputBuffer.floatChannelData![0], count: Int(outputBuffer.frameLength)))
            }
            switch status {
            case .haveData:
                continue
            case .error:
                // 转换出错时这一段丢掉，不让整场录音停下来
                log("采样率转换出错：\(error?.localizedDescription ?? "未知原因")")
                pending.removeAll()
                return
            default:
                return
            }
        }
    }
}

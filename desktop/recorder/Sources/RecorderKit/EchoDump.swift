import AVFoundation

/// --aec-dump <目录>：把回声消除每一帧的麦克风原声、喂给 AEC3 的参考、处理后的麦克风写成三个
/// 48 kHz 单声道的 WAV（mic-in.wav、reference.wav、mic-out.wav），在真机上排查、调参用。
/// 三个文件的第 n 个采样是对应的：mic-in 是送进去的麦克风原声，reference 是和它配对、喂给 AEC3 的参考
/// （提前了 lead 的那一段；没做回声消除的帧参考是 0），mic-out 是 EchoStage 处理出来的（处理过的，或者原样通过的）。
/// 暂停、继续时录音里不要、或者先原样交出去了的那一小段（见 EchoStage 的 previewHeld、discardHeld），mic-out 里
/// 照样是处理出来的那一份，三个文件一直一个采样对一个采样。结束时最后不到 9 毫秒（AEC3 的处理延迟）是原样交出的。
/// 文件只留在这台电脑上（只有自己能读），录音照常上传，和它没有关系。写不进去就不写了，录音照常。
final class EchoDump {
    static let fileNames = ["mic-in.wav", "reference.wav", "mic-out.wav"]
    /// 攒够这么多个采样（0.1 秒）写一次
    private static let flushSamples = 4_800

    private var files: [AVAudioFile]
    private var pending: [[Float]] = [[], [], []]
    private let format: AVAudioFormat
    private var failed = false

    /// 建目录和三个文件，建不了返回 nil（写日志）
    init?(directory: String) {
        let url = URL(fileURLWithPath: directory).standardizedFileURL
        guard let format = AVAudioFormat(commonFormat: .pcmFormatFloat32, sampleRate: Resampler.outputRate, channels: 1, interleaved: false) else {
            return nil
        }
        self.format = format
        do {
            try FileManager.default.createDirectory(at: url, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
            files = try Self.fileNames.map { name in
                let file = url.appendingPathComponent(name)
                let audio = try AVAudioFile(forWriting: file, settings: format.settings, commonFormat: .pcmFormatFloat32, interleaved: false)
                chmod(file.path, 0o600)
                return audio
            }
        } catch {
            log("回声消除：建不了调试用的录音文件（\(url.path)）：\(error.localizedDescription)")
            return nil
        }
        log("回声消除：调试用的录音写到 \(url.path)")
    }

    /// 一帧送进回声消除的：麦克风原声、参考，各 count 个采样
    func writeInput(mic: [Float], reference: [Float], count: Int) {
        guard !failed else { return }
        pending[0].append(contentsOf: mic[0..<count])
        pending[1].append(contentsOf: reference[0..<count])
        if pending[0].count >= Self.flushSamples {
            flush()
        }
    }

    /// 处理出来的麦克风声音（处理过的，或者原样通过的），和送进去的一个对一个
    func writeOutput(_ samples: ArraySlice<Float>) {
        guard !failed else { return }
        pending[2].append(contentsOf: samples)
    }

    /// 结束录音时：剩下的写进去，关掉文件（关的时候才写好 WAV 文件头里的长度；
    /// 录音小程序是直接 exit 的，不关的话文件头里的长度是 0）
    func finish() {
        flush()
        failed = true
        // AVAudioFile 没有了就会关掉文件（macOS 15 才有 close()）
        files.removeAll()
    }

    private func flush() {
        guard !failed, !pending[0].isEmpty || !pending[2].isEmpty else { return }
        do {
            for (index, samples) in pending.enumerated() where !samples.isEmpty {
                guard let buffer = AVAudioPCMBuffer(pcmFormat: format, frameCapacity: AVAudioFrameCount(samples.count)) else {
                    throw RecorderFailure("aec_dump", "内存不够")
                }
                buffer.frameLength = AVAudioFrameCount(samples.count)
                samples.withUnsafeBufferPointer { buffer.floatChannelData![0].update(from: $0.baseAddress!, count: samples.count) }
                try files[index].write(from: buffer)
            }
        } catch {
            failed = true
            log("回声消除：写调试用的录音出错，不再写了：\(error.localizedDescription)")
        }
        for index in pending.indices {
            pending[index].removeAll(keepingCapacity: true)
        }
    }
}

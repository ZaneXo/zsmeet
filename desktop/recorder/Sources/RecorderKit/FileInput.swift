import AVFoundation
import CRecorder

/// 读音频文件（测试用）：各声道取平均混成单声道，按文件原来的采样率给出来。
public final class AudioFileReader {
    public let sampleRate: Double
    /// 文件名，started 事件里当作「麦克风」的名字
    public let name: String

    private let file: AVAudioFile
    private let buffer: AVAudioPCMBuffer

    public init(path: String) throws {
        let url = URL(fileURLWithPath: path)
        do {
            file = try AVAudioFile(forReading: url)
        } catch {
            throw RecorderFailure("input_file", "读不了音频文件 \(path)：\(error.localizedDescription)")
        }
        guard let buffer = AVAudioPCMBuffer(pcmFormat: file.processingFormat, frameCapacity: 48_000) else {
            throw RecorderFailure("input_file", "读不了音频文件 \(path) 的格式")
        }
        self.buffer = buffer
        sampleRate = file.processingFormat.sampleRate
        name = url.lastPathComponent
    }

    /// 读最多 maxFrames 帧，混成单声道。读完了返回空数组
    public func read(maxFrames: Int) throws -> [Float] {
        guard file.framePosition < file.length else { return [] }
        buffer.frameLength = 0
        do {
            try file.read(into: buffer, frameCount: AVAudioFrameCount(min(maxFrames, Int(buffer.frameCapacity))))
        } catch {
            throw RecorderFailure("input_file", "读音频文件出错：\(error.localizedDescription)")
        }
        let frames = Int(buffer.frameLength)
        guard frames > 0, let channels = buffer.floatChannelData else { return [] }
        let channelCount = Int(buffer.format.channelCount)
        var mono = [Float](repeating: 0, count: frames)
        for channel in 0..<channelCount {
            let data = channels[channel]
            for index in 0..<frames {
                mono[index] += data[index]
            }
        }
        if channelCount > 1 {
            let scale = 1 / Float(channelCount)
            for index in 0..<frames {
                mono[index] *= scale
            }
        }
        return mono
    }
}

/// 测试用的「麦克风」：按文件的实际时长一点一点写进环形缓冲，和真的设备一样走现场录音那一套
/// （按时间混音、暂停时丢掉采到的声音）。读完了调用 onEnded。
public final class RealtimeFileSource: CaptureSource {
    public let kind = SourceKind.mic
    public let name: String
    public let ring: OpaquePointer
    public let sampleRate: Double
    public let silenceWarning: String? = nil
    /// 文件读完了（在它自己的线程上调用）
    public var onEnded: (() -> Void)?

    private let reader: AudioFileReader
    private let queue = DispatchQueue(label: "io.github.zanexo.zsmeet.recorder.realtime-file")
    private var timer: DispatchSourceTimer?
    private var startTime: UInt64 = 0
    private var written: Int64 = 0

    public init(reader: AudioFileReader) throws {
        guard let ring = zr_ring_create(1 << 20, 1) else {
            throw RecorderFailure("internal", "内存不够")
        }
        self.reader = reader
        self.ring = ring
        sampleRate = reader.sampleRate
        name = reader.name
    }

    deinit {
        stop()
        zr_ring_destroy(ring)
    }

    public func start() throws {
        queue.sync {
            startTime = DispatchTime.now().uptimeNanoseconds
            let timer = DispatchSource.makeTimerSource(queue: queue)
            timer.schedule(deadline: .now(), repeating: .milliseconds(10), leeway: .milliseconds(2))
            timer.setEventHandler { [weak self] in self?.feed() }
            timer.resume()
            self.timer = timer
        }
    }

    public func stop() {
        // 在同一个队列上取消：正在执行的那次写完以后才返回，之后不会再写
        queue.sync {
            timer?.cancel()
            timer = nil
        }
    }

    private func feed() {
        let elapsed = Double(DispatchTime.now().uptimeNanoseconds - startTime) / 1_000_000_000
        let due = Int(elapsed * sampleRate) - Int(written)
        guard due > 0 else { return }
        do {
            let samples = try reader.read(maxFrames: due)
            if samples.isEmpty {
                timer?.cancel()
                timer = nil
                onEnded?()
                return
            }
            samples.withUnsafeBufferPointer { _ = zr_ring_write(ring, $0.baseAddress!, $0.count) }
            written += Int64(samples.count)
        } catch {
            log("读测试文件出错：\(error)")
            timer?.cancel()
            timer = nil
            onEnded?()
        }
    }
}

import Foundation

/// 录音结束时的结果：下一片的序号（接着录时从这里开始）、一共录了多少秒（不算暂停）
public struct RecordingResult: Equatable {
    public let nextSeq: Int
    public let seconds: Double
}

/// 混好的 48 kHz 单声道声音从这里进去：记录录了多久、算音量、编码、切分片、发事件。
/// 暂停时进来的声音直接丢掉，什么都不编码。只在一个线程上用。
public final class RecordingPipeline {
    /// 每 0.1 秒的声音发一次音量
    public static let levelInterval = 4_800
    /// 分片数到了服务器能收的上限时的提醒（chunk_limit）
    static let chunkLimitMessage = "这场录音的分片数到了服务器能收的上限（\(ChunkWriter.maxChunks) 片），录不了多久了。请结束录音，录好的部分会照常上传。"

    public private(set) var recordedSamples: Int64 = 0
    public private(set) var isPaused = false

    private let encoder: AACEncoder
    private let chunks: ChunkWriter
    private let events: EventSink
    private var micEnergy = 0.0
    private var systemEnergy = 0.0
    private var levelSamples = 0
    private var chunkLimitWarned = false

    public init(encoder: AACEncoder, chunks: ChunkWriter, events: EventSink) {
        self.encoder = encoder
        self.chunks = chunks
        self.events = events
    }

    /// 录了多少秒（不算暂停）
    public var seconds: Double {
        Double(recordedSamples) / Double(AACEncoder.sampleRate)
    }

    /// 服务器能收的最后一片写不下了（见 ChunkWriter.isFull）：后面的声音没地方放，
    /// 引擎看到它就自己结束录音，和收到 stop 一样（最后一片照常写好，发 stopped）
    public var isFull: Bool {
        chunks.isFull
    }

    /// 喂进一段混好的声音。micEnergy、systemEnergy 是两路各自这一段的平方和（算音量）
    public func feed(_ samples: UnsafeBufferPointer<Float>, micEnergy: Double, systemEnergy: Double) throws {
        // 写不下了：不编码，也不算录到的时间（引擎马上就结束录音）
        guard !isPaused, !samples.isEmpty, !chunks.isFull else { return }
        try encoder.encode(samples) { try write($0) }
        recordedSamples += Int64(samples.count)
        self.micEnergy += micEnergy
        self.systemEnergy += systemEnergy
        levelSamples += samples.count
        if levelSamples >= Self.levelInterval {
            events.send(.level(
                seconds: seconds,
                mic: LevelMeter.level(energy: self.micEnergy, count: levelSamples),
                system: LevelMeter.level(energy: self.systemEnergy, count: levelSamples)
            ))
            resetLevel()
        }
        warnAtChunkLimit()
    }

    /// 暂停：正在写的那一片先落盘，这样暂停期间 App 被关掉也不丢，录好的部分也可以先传上去
    public func pause() throws {
        guard !isPaused else { return }
        isPaused = true
        resetLevel()
        try finishChunk()
        events.send(.paused)
        // 写好的是最后一片的话，后面就录不了了（paused 前面还是紧跟着这一片）
        warnAtChunkLimit()
    }

    public func resume() {
        guard isPaused else { return }
        isPaused = false
        events.send(.resumed)
    }

    /// 结束：编码器里剩下的都编出来，写进最后一片（可能不满）
    public func finish() throws -> RecordingResult {
        try encoder.finish { try write($0) }
        try finishChunk()
        return RecordingResult(nextSeq: chunks.nextSeq, seconds: seconds)
    }

    /// 出错退出时调用：不再写了
    public func abandon() {
        chunks.abandon()
    }

    private func write(_ packet: UnsafeRawBufferPointer) throws {
        if let chunk = try chunks.append(packet: packet) {
            send(chunk)
        }
    }

    private func finishChunk() throws {
        if let chunk = try chunks.finishChunk() {
            send(chunk)
        }
    }

    private func send(_ chunk: ChunkInfo) {
        events.send(.chunk(seq: chunk.seq, path: chunk.path, bytes: chunk.bytes, seconds: chunk.seconds))
    }

    /// 到了服务器能收的片数上限（见 ChunkWriter.reachedChunkLimit）时提醒一次，App 收到就结束录音。
    /// 结束时（finish）不提醒：已经在结束了
    private func warnAtChunkLimit() {
        guard !chunkLimitWarned, chunks.reachedChunkLimit else { return }
        chunkLimitWarned = true
        log("到了服务器能收的分片数上限（第 \(ChunkWriter.lastSeq) 片是最后一片），不再切新的分片，等 App 结束录音")
        events.send(.warning(.chunkLimit, Self.chunkLimitMessage))
    }

    private func resetLevel() {
        micEnergy = 0
        systemEnergy = 0
        levelSamples = 0
    }
}

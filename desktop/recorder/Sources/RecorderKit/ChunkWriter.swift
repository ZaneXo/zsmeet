import Foundation

/// 写好的一个分片
public struct ChunkInfo: Equatable {
    public let seq: Int
    public let path: String
    public let bytes: Int
    public let packets: Int

    /// 这一片的声音有多少秒
    public var seconds: Double {
        Double(packets * AACEncoder.samplesPerPacket) / Double(AACEncoder.sampleRate)
    }
}

/// 把编好的 AAC 包加上 ADTS 头分到一个个分片文件里：先写 <seq>.aac.part，
/// 写满（或者暂停、结束时）先落盘再改名成 <seq>.aac，所以看得到 <seq>.aac 就说明它是完整的。
/// 只是按包切，不碰编码器，分片拼起来还是同一路流。
///
/// 服务器一场会议最多收 maxChunks 片，序号不小于它的分片一个都不写：最后一片（lastSeq）够长了也不切，
/// 后面的包接着写进它，等 App 结束录音；它再写就超过服务器能收的大小（maxBytes）时不写了（isFull）。
public final class ChunkWriter {
    /// 服务器一场会议最多收这么多片（序号 0 到 3999，见 internal/service/meetings.go 的 MaxChunks）
    public static let maxChunks = 4_000
    /// 服务器能收的最后一片的序号
    public static let lastSeq = maxChunks - 1
    /// 服务器收的一片最多这么多字节（internal/api/meetings.go 的 maxChunkBytes）。64 kbps 的声音约 16 分钟
    public static let maxChunkBytes = 8 << 20

    public let directory: URL
    /// 每片多少个 AAC 包（每包 1024 个采样，约 21 毫秒）
    public let packetsPerChunk: Int
    /// 最后一片最多多少字节（测试时改小）
    let maxBytes: Int
    /// 下一个要写的分片序号；正在写的那片也是这个号
    public private(set) var nextSeq: Int
    /// 写不下了：最后一片放不下新的包（再写就超过 maxBytes，或者它已经写好了），这个包和之后的都没写。
    /// 录音该结束了
    public private(set) var isFull = false

    private var file: Int32 = -1
    private var buffer: [UInt8] = []
    private var bytes = 0
    private var packets = 0

    public init(directory: URL, startSeq: Int, chunkSeconds: Double, maxBytes: Int = ChunkWriter.maxChunkBytes) {
        self.directory = directory
        nextSeq = startSeq
        let packets = chunkSeconds * Double(AACEncoder.sampleRate) / Double(AACEncoder.samplesPerPacket)
        packetsPerChunk = max(1, Int(packets.rounded(.up)))
        self.maxBytes = maxBytes
    }

    /// 到了服务器能收的片数上限，App 该结束录音了：正在写的最后一片已经够一片长了（不会再切），
    /// 或者最后一片已经写好了
    public var reachedChunkLimit: Bool {
        isFull || nextSeq > Self.lastSeq || (nextSeq == Self.lastSeq && packets >= packetsPerChunk)
    }

    deinit {
        abandon()
    }

    public static func url(directory: URL, seq: Int) -> URL {
        directory.appendingPathComponent("\(seq).aac")
    }

    /// 从文件名认出分片序号："12.aac" 是 12，别的（包括 .part）是 nil
    public static func seq(fromFileName name: String) -> Int? {
        guard name.hasSuffix(".aac") else { return nil }
        let digits = name.dropLast(4)
        guard !digits.isEmpty, digits.allSatisfy({ $0.isASCII && $0.isNumber }) else { return nil }
        return Int(digits)
    }

    /// 写一个 AAC 包。这一片写满了就落盘，返回它。
    /// 最后一片写满了也不切，接着往里写；它放不下这个包时什么都不写，isFull 变成 true
    public func append(packet: UnsafeRawBufferPointer) throws -> ChunkInfo? {
        let frameLength = ADTS.headerLength + packet.count
        if nextSeq > Self.lastSeq || (nextSeq == Self.lastSeq && packets > 0 && bytes + frameLength > maxBytes) {
            if !isFull {
                isFull = true
                log("服务器能收的最后一片（第 \(Self.lastSeq) 片）放不下了，后面的声音不录了，结束录音")
            }
            return nil
        }
        if file < 0 {
            try openPart()
        }
        buffer.append(contentsOf: ADTS.header(payloadLength: packet.count))
        buffer.append(contentsOf: packet)
        bytes += frameLength
        packets += 1
        if buffer.count >= 64 * 1_024 {
            try flush()
        }
        // 最后一片够长了也不切：再切出来的序号服务器不收
        guard packets >= packetsPerChunk, nextSeq < Self.lastSeq else { return nil }
        return try finishChunk()
    }

    /// 把正在写的这一片落盘、改成正式的文件名（暂停、结束时调用）。还没有内容时什么都不做，返回 nil。
    public func finishChunk() throws -> ChunkInfo? {
        guard file >= 0 else { return nil }
        try flush()
        // F_FULLFSYNC 才会让硬盘把它自己的缓存也写下去；不支持的文件系统退回 fsync
        if fcntl(file, F_FULLFSYNC) != 0 && fsync(file) != 0 {
            throw writeFailure()
        }
        close(file)
        file = -1
        let seq = nextSeq
        let finalURL = Self.url(directory: directory, seq: seq)
        guard rename(partURL(seq).path, finalURL.path) == 0 else {
            throw writeFailure()
        }
        syncDirectory()
        let info = ChunkInfo(seq: seq, path: finalURL.path, bytes: bytes, packets: packets)
        nextSeq += 1
        bytes = 0
        packets = 0
        return info
    }

    /// 出错退出时调用：关掉文件，没写完的 .part 留着（下次从这个序号接着录时会覆盖它）
    public func abandon() {
        if file >= 0 {
            close(file)
            file = -1
        }
        buffer.removeAll()
    }

    private func partURL(_ seq: Int) -> URL {
        directory.appendingPathComponent("\(seq).aac.part")
    }

    private func openPart() throws {
        // 分片只有自己能读写（0600）：录的是会议的声音，目录万一别人也进得去，也读不了。
        // 上次没写完、留下来的 .part 会接着用，open 不改它原来的权限，所以再设一次
        let fd = open(partURL(nextSeq).path, O_WRONLY | O_CREAT | O_TRUNC | O_CLOEXEC, 0o600)
        guard fd >= 0 else { throw writeFailure() }
        guard fchmod(fd, 0o600) == 0 else {
            let failure = writeFailure()
            close(fd)
            throw failure
        }
        file = fd
        bytes = 0
        packets = 0
    }

    private func flush() throws {
        var offset = 0
        while offset < buffer.count {
            let written = buffer.withUnsafeBytes { raw in
                write(file, raw.baseAddress! + offset, raw.count - offset)
            }
            if written < 0 {
                if errno == EINTR { continue }
                throw writeFailure()
            }
            offset += written
        }
        buffer.removeAll(keepingCapacity: true)
    }

    /// 改名以后把目录也落盘，断电时新文件名不会丢
    private func syncDirectory() {
        let fd = open(directory.path, O_RDONLY | O_CLOEXEC)
        if fd >= 0 {
            fsync(fd)
            close(fd)
        }
    }

    private func writeFailure() -> RecorderFailure {
        let code = errno
        if code == ENOSPC || code == EDQUOT {
            return RecorderFailure("write_failed", "磁盘满了，录音停了。已经录好的部分还在，腾出空间后会接着上传。")
        }
        return RecorderFailure("write_failed", "写不了录音文件（\(String(cString: strerror(code)))），录音停了。已经录好的部分还在。")
    }
}

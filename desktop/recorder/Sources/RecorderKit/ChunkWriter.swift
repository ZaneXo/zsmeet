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
public final class ChunkWriter {
    public let directory: URL
    /// 每片多少个 AAC 包（每包 1024 个采样，约 21 毫秒）
    public let packetsPerChunk: Int
    /// 下一个要写的分片序号；正在写的那片也是这个号
    public private(set) var nextSeq: Int

    private var file: Int32 = -1
    private var buffer: [UInt8] = []
    private var bytes = 0
    private var packets = 0

    public init(directory: URL, startSeq: Int, chunkSeconds: Double) {
        self.directory = directory
        nextSeq = startSeq
        let packets = chunkSeconds * Double(AACEncoder.sampleRate) / Double(AACEncoder.samplesPerPacket)
        packetsPerChunk = max(1, Int(packets.rounded(.up)))
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
    public func append(packet: UnsafeRawBufferPointer) throws -> ChunkInfo? {
        if file < 0 {
            try openPart()
        }
        buffer.append(contentsOf: ADTS.header(payloadLength: packet.count))
        buffer.append(contentsOf: packet)
        bytes += ADTS.headerLength + packet.count
        packets += 1
        if buffer.count >= 64 * 1_024 {
            try flush()
        }
        return packets >= packetsPerChunk ? try finishChunk() : nil
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
        let fd = open(partURL(nextSeq).path, O_WRONLY | O_CREAT | O_TRUNC | O_CLOEXEC, 0o644)
        guard fd >= 0 else { throw writeFailure() }
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

/// ADTS 帧头：每个 AAC 包前面加 7 个字节，写明这是 AAC-LC、48 kHz、单声道、这一帧有多长。
/// 有了它每一帧都能单独解码，分片按序号首尾相接就是一个完整的 .aac 文件，服务器直接拼字节。
public enum ADTS {
    public static let headerLength = 7
    /// 帧长（含帧头）只有 13 位
    public static let maxFrameLength = (1 << 13) - 1

    /// MPEG-4 的音频类型 2（AAC-LC）减 1
    static let profile: UInt8 = 1
    /// 采样率表里 48000 是第 3 个
    static let sampleRateIndex: UInt8 = 3
    /// 单声道
    static let channelConfiguration: UInt8 = 1
    /// 缓冲区满度填 0x7FF 表示可变码率
    static let bufferFullness = 0x7FF

    /// 给 payloadLength 字节的 AAC 包生成帧头。
    public static func header(payloadLength: Int) -> [UInt8] {
        precondition(payloadLength >= 0 && payloadLength + headerLength <= maxFrameLength, "AAC 包太大：\(payloadLength) 字节")
        let frameLength = payloadLength + headerLength
        return [
            0xFF,
            // 同步字的后 4 位、MPEG-4、layer 0、没有 CRC
            0xF1,
            (profile << 6) | (sampleRateIndex << 2) | (channelConfiguration >> 2),
            ((channelConfiguration & 0x3) << 6) | UInt8((frameLength >> 11) & 0x3),
            UInt8((frameLength >> 3) & 0xFF),
            UInt8((frameLength & 0x7) << 5) | UInt8(bufferFullness >> 6),
            // 最后 2 位是 0：这一帧里只有 1 个 AAC 块
            UInt8((bufferFullness & 0x3F) << 2),
        ]
    }
}

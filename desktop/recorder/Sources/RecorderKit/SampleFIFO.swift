/// 采样的先进先出队列。只在处理线程上用，不跨线程。
public struct SampleFIFO {
    private var storage: [Float] = []
    private var head = 0

    public init() {}

    public var count: Int { storage.count - head }
    public var isEmpty: Bool { count == 0 }

    public mutating func append(_ samples: UnsafeBufferPointer<Float>) {
        storage.append(contentsOf: samples)
    }

    public mutating func append(_ samples: [Float]) {
        storage.append(contentsOf: samples)
    }

    /// 取出最前面的 n 个，复制到 out
    public mutating func take(_ n: Int, into out: UnsafeMutablePointer<Float>) {
        precondition(n >= 0 && n <= count)
        guard n > 0 else { return }
        storage.withUnsafeBufferPointer { buffer in
            out.update(from: buffer.baseAddress! + head, count: n)
        }
        advance(n)
    }

    /// 取出最前面的 n 个，加到 out 上（混音），返回这些采样的平方和（算音量用）
    public mutating func takeAdding(_ n: Int, into out: UnsafeMutablePointer<Float>) -> Double {
        precondition(n >= 0 && n <= count)
        guard n > 0 else { return 0 }
        var energy = 0.0
        storage.withUnsafeBufferPointer { buffer in
            let source = buffer.baseAddress! + head
            for index in 0..<n {
                let sample = source[index]
                out[index] += sample
                energy += Double(sample * sample)
            }
        }
        advance(n)
        return energy
    }

    /// 丢掉最前面的 n 个
    public mutating func drop(_ n: Int) {
        advance(min(max(n, 0), count))
    }

    public mutating func removeAll() {
        storage.removeAll(keepingCapacity: true)
        head = 0
    }

    private mutating func advance(_ n: Int) {
        head += n
        if head == storage.count {
            storage.removeAll(keepingCapacity: true)
            head = 0
        } else if head >= 16_384 && head * 2 >= storage.count {
            // 读过的部分占了一半以上再挪，平均下来每个采样只挪一次
            storage.removeFirst(head)
            head = 0
        }
    }
}

import CoreAudio
import CRecorder
import Foundation

/// 麦克风：直接在设备上挂一个 IOProc（C 写的 zr_capture_ioproc，音频线程上不加锁、不分配内存），
/// 把前两个声道取平均写进环形缓冲。设备拔掉时调用 onLost。
final class MicCapture: CaptureSource {
    let kind = SourceKind.mic
    let name: String
    let ring: OpaquePointer
    /// 设备拔掉了、不能用了（在监听队列上调用）
    var onLost: (() -> Void)?

    private let device: AudioObjectID
    /// 录的是哪个设备（回声消除判断输出是不是同一个设备，也就是耳麦）
    var deviceID: AudioObjectID { device }
    /// 保护 rate（监听线程写、处理线程读）
    private let lock = NSLock()
    private var rate: Double
    /// start、stop 可能在不同的线程上调用
    private let controlLock = NSLock()
    private var procID: AudioDeviceIOProcID?
    private var listeners: [PropertyListener] = []
    private let listenerQueue = DispatchQueue(label: "io.github.zanexo.zsmeet.recorder.mic-listener")

    init(_ microphone: Microphone) throws {
        device = microphone.id
        name = microphone.name
        rate = try Self.currentSampleRate(device)
        guard let ring = zr_ring_create(1 << 20, 2) else {
            throw CoreAudioError(action: "分配录音缓冲", status: kAudioHardwareUnspecifiedError)
        }
        self.ring = ring
    }

    deinit {
        stop()
        zr_ring_destroy(ring)
    }

    var sampleRate: Double {
        lock.lock()
        defer { lock.unlock() }
        return rate
    }

    var silenceWarning: String? {
        "麦克风「\(name)」一直没有声音。确认它没有被静音；还是不行的话，到「系统设置 → 隐私与安全性 → 麦克风」里看看 zsmeet 是不是打开了，或者换一个麦克风。"
    }

    func start() throws {
        controlLock.lock()
        defer { controlLock.unlock() }
        // 先挂上监听，再读一次采样率，最后才开始录（和 SystemAudioCapture 一样）：init 里读过以后变了的这次读得到，
        // 之后再变监听会收到；读和改都在监听的队列上做，旧的值不会盖掉新的
        listeners = [
            PropertyListener(object: device, selector: kAudioDevicePropertyDeviceIsAlive, queue: listenerQueue) { [weak self] in
                self?.checkAlive()
            },
            PropertyListener(object: device, selector: kAudioDevicePropertyNominalSampleRate, queue: listenerQueue) { [weak self] in
                self?.updateSampleRate()
            },
        ]
        listenerQueue.sync { updateSampleRate() }
        var started = false
        defer {
            if !started {
                listeners.forEach { $0.remove() }
                listeners = []
            }
        }
        var procID: AudioDeviceIOProcID?
        try check(AudioDeviceCreateIOProcID(device, zr_capture_ioproc, UnsafeMutableRawPointer(ring), &procID), "打开麦克风")
        guard let procID else {
            throw CoreAudioError(action: "打开麦克风", status: kAudioHardwareUnspecifiedError)
        }
        self.procID = procID
        do {
            try check(AudioDeviceStart(device, procID), "开始录麦克风")
        } catch {
            AudioDeviceDestroyIOProcID(device, procID)
            self.procID = nil
            throw error
        }
        started = true
    }

    func stop() {
        controlLock.lock()
        defer { controlLock.unlock() }
        listeners.forEach { $0.remove() }
        listeners = []
        guard let procID else { return }
        self.procID = nil
        // AudioDeviceStop 返回以后 IOProc 不会再被调用
        AudioDeviceStop(device, procID)
        AudioDeviceDestroyIOProcID(device, procID)
    }

    private func checkAlive() {
        let alive = (try? AudioProperty.value(device, kAudioDevicePropertyDeviceIsAlive, initial: UInt32(1))) ?? 0
        if alive == 0 {
            log("麦克风「\(name)」断开了")
            onLost?()
        }
    }

    private func updateSampleRate() {
        guard let newRate = try? Self.currentSampleRate(device) else { return }
        lock.lock()
        rate = newRate
        lock.unlock()
    }

    /// IOProc 收到的是设备输入流的「虚拟格式」：先确认是 32 位浮点，采样率以它为准
    private static func currentSampleRate(_ device: AudioObjectID) throws -> Double {
        let streams = try AudioProperty.objectIDs(device, kAudioDevicePropertyStreams, scope: kAudioObjectPropertyScopeInput)
        guard let stream = streams.first else {
            throw CoreAudioError(action: "读取麦克风的格式", status: kAudioHardwareBadStreamError)
        }
        let format = try AudioProperty.value(stream, kAudioStreamPropertyVirtualFormat, initial: AudioStreamBasicDescription())
        guard format.mFormatID == kAudioFormatLinearPCM,
              format.mFormatFlags & kAudioFormatFlagIsFloat != 0,
              format.mBitsPerChannel == 32,
              format.mSampleRate > 0
        else {
            throw CoreAudioError(action: "读取麦克风的格式", status: kAudioDeviceUnsupportedFormatError)
        }
        return format.mSampleRate
    }
}

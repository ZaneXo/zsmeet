import CoreAudio
import CRecorder
import Foundation

/// 电脑里的声音：Core Audio 的进程音频捕获（macOS 14.2 起，权限是「录制系统音频」，不用录屏权限）。
/// 建一个抓所有进程声音的 tap（不影响它们正常播放），放进一个只有本进程看得见的聚合设备，
/// 再在聚合设备上挂 IOProc，和麦克风一样写进环形缓冲。
/// 照苹果的文档（Capturing system audio with Core Audio taps）聚合设备里只放这个 tap、不放真的设备：
/// 不会把耳机的麦克风也带进来，也不会让蓝牙耳机切到通话模式。
/// tap 和聚合设备用完一定要拆掉：正常结束、出错、收到信号时都会走到 destroy()（见 AudioCleanup）。
final class SystemAudioCapture: CaptureSource {
    /// 我们自己建的聚合设备的 UID 前缀（它在本进程里看得见，有输入流，列麦克风时要去掉）
    static let aggregateUIDPrefix = "io.github.zanexo.zsmeet.recorder."

    let kind = SourceKind.system
    let name = "电脑声音"
    let ring: OpaquePointer
    /// 电脑没在放声音时本来就全是 0，不检查
    let silenceWarning: String? = nil

    /// 保护 rate（监听线程写、处理线程读）
    private let lock = NSLock()
    private var rate = 48_000.0
    /// start、stop、destroy 可能在不同的线程上调用
    private let controlLock = NSLock()
    private var tapID = AudioObjectID(kAudioObjectUnknown)
    private var aggregateID = AudioObjectID(kAudioObjectUnknown)
    private var procID: AudioDeviceIOProcID?
    private var listener: PropertyListener?
    private var cleanupID: Int?
    private let listenerQueue = DispatchQueue(label: "io.github.zanexo.zsmeet.recorder.system-listener")

    init() throws {
        guard let ring = zr_ring_create(1 << 20, 2) else {
            throw CoreAudioError(action: "分配录音缓冲", status: kAudioHardwareUnspecifiedError)
        }
        self.ring = ring
        cleanupID = AudioCleanup.add { [weak self] in self?.destroy() }
        do {
            try createTap()
            rate = try Self.tapSampleRate(tapID)
            try createAggregateDevice()
        } catch {
            destroy()
            throw error
        }
    }

    deinit {
        destroy()
        zr_ring_destroy(ring)
    }

    var sampleRate: Double {
        lock.lock()
        defer { lock.unlock() }
        return rate
    }

    func start() throws {
        controlLock.lock()
        defer { controlLock.unlock() }
        // 先挂上格式的监听，再读一次现在的格式，最后才开始录：init 里读过以后、监听挂上以前变了的，这次读得到；
        // 挂上以后（包括开始录的时候）再变，监听会收到。反过来先开始录、再挂监听的话，中间变了就收不到通知，
        // 一直按旧的采样率转换，录出来的声音变调、时长也不对。
        // 读和改都在监听的队列上做，先读到的旧值不会盖掉监听后来读到的新值
        let tap = tapID
        listener = PropertyListener(object: tap, selector: kAudioTapPropertyFormat, queue: listenerQueue) { [weak self] in
            self?.updateSampleRate(tap)
        }
        listenerQueue.sync { updateSampleRate(tap) }
        var started = false
        defer {
            if !started {
                listener?.remove()
                listener = nil
            }
        }
        var procID: AudioDeviceIOProcID?
        try check(AudioDeviceCreateIOProcID(aggregateID, zr_capture_ioproc, UnsafeMutableRawPointer(ring), &procID), "打开电脑声音")
        guard let procID else {
            throw CoreAudioError(action: "打开电脑声音", status: kAudioHardwareUnspecifiedError)
        }
        self.procID = procID
        do {
            try check(AudioDeviceStart(aggregateID, procID), "开始录电脑声音")
        } catch {
            AudioDeviceDestroyIOProcID(aggregateID, procID)
            self.procID = nil
            throw error
        }
        started = true
    }

    /// 重新读 tap 的格式（在监听的队列上）。读不出来时还用原来的
    private func updateSampleRate(_ tap: AudioObjectID) {
        guard let newRate = try? Self.tapSampleRate(tap) else { return }
        lock.lock()
        rate = newRate
        lock.unlock()
    }

    func stop() {
        controlLock.lock()
        defer { controlLock.unlock() }
        stopLocked()
    }

    /// 停下来并拆掉聚合设备和 tap。可以重复调用
    func destroy() {
        controlLock.lock()
        defer { controlLock.unlock() }
        stopLocked()
        if aggregateID != kAudioObjectUnknown {
            let status = AudioHardwareDestroyAggregateDevice(aggregateID)
            if status != noErr {
                log("拆掉聚合设备失败（\(CoreAudioError.code(status))）")
            }
            aggregateID = AudioObjectID(kAudioObjectUnknown)
        }
        if tapID != kAudioObjectUnknown {
            let status = AudioHardwareDestroyProcessTap(tapID)
            if status != noErr {
                log("拆掉电脑声音的捕获失败（\(CoreAudioError.code(status))）")
            }
            tapID = AudioObjectID(kAudioObjectUnknown)
        }
        if let cleanupID {
            AudioCleanup.remove(cleanupID)
            self.cleanupID = nil
        }
    }

    private func stopLocked() {
        listener?.remove()
        listener = nil
        guard let procID else { return }
        self.procID = nil
        AudioDeviceStop(aggregateID, procID)
        AudioDeviceDestroyIOProcID(aggregateID, procID)
    }

    private func createTap() throws {
        // 抓所有进程混在一起的声音（立体声，IOProc 里再取平均成单声道），一个都不排除
        let description = CATapDescription(stereoGlobalTapButExcludeProcesses: [])
        description.name = "zsmeet 电脑声音"
        description.isPrivate = true
        description.muteBehavior = .unmuted
        var tap = AudioObjectID(kAudioObjectUnknown)
        try check(AudioHardwareCreateProcessTap(description, &tap), "建立电脑声音的捕获")
        tapID = tap
    }

    private func createAggregateDevice() throws {
        let tapUID = try AudioProperty.string(tapID, kAudioTapPropertyUID)
        // 私有的聚合设备只有本进程看得见，进程退出时系统也会收回
        let composition: [String: Any] = [
            kAudioAggregateDeviceNameKey: "zsmeet 电脑声音",
            kAudioAggregateDeviceUIDKey: "\(Self.aggregateUIDPrefix)\(UUID().uuidString)",
            kAudioAggregateDeviceIsPrivateKey: 1,
        ]
        var aggregate = AudioObjectID(kAudioObjectUnknown)
        try check(AudioHardwareCreateAggregateDevice(composition as CFDictionary, &aggregate), "建立录电脑声音用的聚合设备")
        aggregateID = aggregate

        var address = AudioProperty.address(kAudioAggregateDevicePropertyTapList)
        var taps = [tapUID] as CFArray
        try withUnsafeMutablePointer(to: &taps) { pointer in
            try check(AudioObjectSetPropertyData(aggregate, &address, 0, nil, UInt32(MemoryLayout<CFArray>.size), pointer),
                      "把电脑声音接到聚合设备上")
        }
        waitForInputStream()
    }

    /// 接上 tap 以后，聚合设备可能要过一会儿才出现对应的输入流：等它出现（最多 1 秒）再开始录
    private func waitForInputStream() {
        for _ in 0..<20 {
            let streams = (try? AudioProperty.objectIDs(aggregateID, kAudioDevicePropertyStreams, scope: kAudioObjectPropertyScopeInput)) ?? []
            if !streams.isEmpty {
                return
            }
            usleep(50_000)
        }
        log("录电脑声音的聚合设备还没有输入流，先开始录")
    }

    /// tap 的格式就是聚合设备里拿到的数据的格式
    private static func tapSampleRate(_ tap: AudioObjectID) throws -> Double {
        let format = try AudioProperty.value(tap, kAudioTapPropertyFormat, initial: AudioStreamBasicDescription())
        guard format.mFormatID == kAudioFormatLinearPCM,
              format.mFormatFlags & kAudioFormatFlagIsFloat != 0,
              format.mBitsPerChannel == 32,
              format.mSampleRate > 0
        else {
            throw CoreAudioError(action: "读取电脑声音的格式", status: kAudioDeviceUnsupportedFormatError)
        }
        return format.mSampleRate
    }
}

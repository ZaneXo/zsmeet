import CoreAudio
import Foundation

/// Core Audio 调用失败。status 是 OSStatus，多半是四个字符的代码，写日志时一起写上
struct CoreAudioError: Error, CustomStringConvertible {
    let action: String
    let status: OSStatus

    var description: String {
        "\(action)失败（\(CoreAudioError.code(status))）"
    }

    static func code(_ status: OSStatus) -> String {
        let value = UInt32(bitPattern: status)
        let bytes = [24, 16, 8, 0].map { UInt8((value >> $0) & 0xFF) }
        if bytes.allSatisfy({ $0 >= 0x20 && $0 < 0x7F }) {
            return "'" + String(decoding: bytes, as: UTF8.self) + "'"
        }
        return String(status)
    }
}

func check(_ status: OSStatus, _ action: String) throws {
    if status != noErr {
        throw CoreAudioError(action: action, status: status)
    }
}

/// 读写 Core Audio 对象属性的小工具
enum AudioProperty {
    static let system = AudioObjectID(kAudioObjectSystemObject)

    static func address(_ selector: AudioObjectPropertySelector,
                        _ scope: AudioObjectPropertyScope = kAudioObjectPropertyScopeGlobal) -> AudioObjectPropertyAddress {
        AudioObjectPropertyAddress(mSelector: selector, mScope: scope, mElement: kAudioObjectPropertyElementMain)
    }

    /// 定长的属性（数字、AudioObjectID、AudioStreamBasicDescription 等）
    static func value<T>(_ object: AudioObjectID, _ selector: AudioObjectPropertySelector,
                         scope: AudioObjectPropertyScope = kAudioObjectPropertyScopeGlobal, initial: T) throws -> T {
        var address = address(selector, scope)
        var value = initial
        var size = UInt32(MemoryLayout<T>.size)
        let status = withUnsafeMutablePointer(to: &value) { pointer in
            AudioObjectGetPropertyData(object, &address, 0, nil, &size, pointer)
        }
        try check(status, "读取音频属性")
        return value
    }

    /// CFString 的属性（名字、UID）
    static func string(_ object: AudioObjectID, _ selector: AudioObjectPropertySelector) throws -> String {
        var address = address(selector)
        var value: Unmanaged<CFString>?
        var size = UInt32(MemoryLayout<Unmanaged<CFString>?>.size)
        try check(AudioObjectGetPropertyData(object, &address, 0, nil, &size, &value), "读取音频属性")
        guard let value else {
            throw CoreAudioError(action: "读取音频属性", status: kAudioHardwareUnspecifiedError)
        }
        return value.takeRetainedValue() as String
    }

    /// AudioObjectID 数组的属性（设备列表、流列表）
    static func objectIDs(_ object: AudioObjectID, _ selector: AudioObjectPropertySelector,
                          scope: AudioObjectPropertyScope = kAudioObjectPropertyScopeGlobal) throws -> [AudioObjectID] {
        var address = address(selector, scope)
        var size: UInt32 = 0
        try check(AudioObjectGetPropertyDataSize(object, &address, 0, nil, &size), "读取音频属性")
        var ids = [AudioObjectID](repeating: 0, count: Int(size) / MemoryLayout<AudioObjectID>.size)
        guard !ids.isEmpty else { return [] }
        try check(AudioObjectGetPropertyData(object, &address, 0, nil, &size, &ids), "读取音频属性")
        return Array(ids.prefix(Int(size) / MemoryLayout<AudioObjectID>.size))
    }

    /// 设备有几个输入声道（0 表示不是输入设备）
    static func inputChannelCount(_ device: AudioObjectID) -> Int {
        var address = address(kAudioDevicePropertyStreamConfiguration, kAudioObjectPropertyScopeInput)
        var size: UInt32 = 0
        guard AudioObjectGetPropertyDataSize(device, &address, 0, nil, &size) == noErr, size > 0 else { return 0 }
        let raw = UnsafeMutableRawPointer.allocate(byteCount: Int(size), alignment: MemoryLayout<AudioBufferList>.alignment)
        defer { raw.deallocate() }
        guard AudioObjectGetPropertyData(device, &address, 0, nil, &size, raw) == noErr else { return 0 }
        let list = UnsafeMutableAudioBufferListPointer(raw.assumingMemoryBound(to: AudioBufferList.self))
        return list.reduce(0) { $0 + Int($1.mNumberChannels) }
    }

    /// 系统设置里选的默认输入设备，没有时返回 nil
    static func defaultInputDevice() -> AudioObjectID? {
        let device = try? value(system, kAudioHardwarePropertyDefaultInputDevice, initial: AudioObjectID(kAudioObjectUnknown))
        guard let device, device != kAudioObjectUnknown else { return nil }
        return device
    }
}

/// 监听一个音频属性的变化，不用了要 remove（同一个 block 才能取消监听）
final class PropertyListener {
    private let object: AudioObjectID
    private var address: AudioObjectPropertyAddress
    private let queue: DispatchQueue
    private let block: AudioObjectPropertyListenerBlock
    private var active = false

    init(object: AudioObjectID, selector: AudioObjectPropertySelector, queue: DispatchQueue, handler: @escaping () -> Void) {
        self.object = object
        address = AudioProperty.address(selector)
        self.queue = queue
        block = { _, _ in handler() }
        active = AudioObjectAddPropertyListenerBlock(object, &address, queue, block) == noErr
    }

    func remove() {
        guard active else { return }
        active = false
        // 取消时要给和添加时同一个队列、同一个 block
        AudioObjectRemovePropertyListenerBlock(object, &address, queue, block)
    }

    deinit {
        remove()
    }
}

/// 退出前一定要拆掉的 Core Audio 对象（聚合设备、进程音频捕获）。正常结束、出错、收到信号时都会调用 runAll；
/// 万一走到 exit() 时还有没拆的，atexit 里再拆一次。这些对象都是本进程私有的，进程崩溃时系统也会收回。
public enum AudioCleanup {
    private static let lock = NSLock()
    private static var actions: [Int: () -> Void] = [:]
    private static var nextID = 0
    private static var installed = false

    /// 登记一个拆除动作，返回编号；已经拆掉了就用编号取消登记
    static func add(_ action: @escaping () -> Void) -> Int {
        lock.lock()
        defer { lock.unlock() }
        if !installed {
            installed = true
            atexit { AudioCleanup.runAll() }
        }
        nextID += 1
        actions[nextID] = action
        return nextID
    }

    static func remove(_ id: Int) {
        lock.lock()
        actions[id] = nil
        lock.unlock()
    }

    /// 执行所有还没执行的拆除动作（每个只执行一次），后登记的先拆
    public static func runAll() {
        lock.lock()
        let pending = actions.sorted { $0.key > $1.key }.map(\.value)
        actions.removeAll()
        lock.unlock()
        for action in pending {
            action()
        }
    }
}

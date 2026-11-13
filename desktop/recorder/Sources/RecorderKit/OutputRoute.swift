import CoreAudio
import Foundation

/// 电脑的声音现在从哪儿出来。回声消除的 auto 模式按它决定做不做：
/// 外放（扬声器）时麦克风会录到会议里其他人的声音，要消除；戴耳机时没有回声，不做；
/// AirPlay 的延迟有好几秒，超出了 AEC3 能找的范围（约 0.6 秒），做了也没用，不做。
public enum OutputRouteKind: String, Equatable {
    case speakers
    case headphones
    case airplay
}

/// 回声消除要知道的输出路线（auto 模式用）。可以在任何线程上读
public protocol OutputRouteSource: AnyObject {
    var current: OutputRouteKind { get }
    /// 给日志看的：哪个设备、为什么这么判断
    var currentDescription: String { get }
    /// 录的麦克风换了（跟着系统默认的麦克风换了）：输出和麦克风是不是同一个设备（耳麦）要重新判断
    func updateMicDevice(_ device: AudioObjectID?)
    func stop()
}

extension OutputRouteSource {
    /// 不看麦克风的输出路线（固定的、测试用的）不用管
    public func updateMicDevice(_ device: AudioObjectID?) {}
}

/// 固定的输出路线：读文件录音（测试）时不看真的设备，当成外放
public final class FixedRoute: OutputRouteSource {
    public let current: OutputRouteKind

    public init(_ kind: OutputRouteKind) {
        current = kind
    }

    public var currentDescription: String {
        "固定为 \(current.rawValue)"
    }

    public func stop() {}
}

/// 默认输出设备的情况（判断用，见 OutputRouteClassifier）
public struct OutputDeviceInfo: Equatable {
    public var name: String
    /// kAudioDevicePropertyTransportType：'bltn'、'usb '、'blue'、'airp'……
    public var transport: UInt32
    /// 输出流的类型（kAudioStreamPropertyTerminalType）
    public var terminalTypes: [UInt32]
    /// 输出的数据源（kAudioDevicePropertyDataSource，Intel 的 Mac 插耳机时是 'hdpn'），没有时是 nil
    public var dataSource: UInt32?
    /// 和正在录的麦克风是同一个设备（USB 耳麦这类：麦克风和耳机在一个设备里）
    public var isMicDevice: Bool

    public init(name: String, transport: UInt32, terminalTypes: [UInt32] = [], dataSource: UInt32? = nil, isMicDevice: Bool = false) {
        self.name = name
        self.transport = transport
        self.terminalTypes = terminalTypes
        self.dataSource = dataSource
        self.isMicDevice = isMicDevice
    }
}

/// 按默认输出设备判断是外放、耳机还是 AirPlay。认不出来的都当外放（宁可多做一次回声消除）
public enum OutputRouteClassifier {
    /// USB 音频的终端类型（IOKit 的 IOAudioTypes.h）：耳机、头戴显示器的音频、耳麦
    static let usbHeadphoneTerminals: Set<UInt32> = [0x0302, 0x0303, 0x0402]
    /// 名字里有这些字的当成耳机
    static let headphoneWords = ["headphone", "headset", "earphone", "earbud", "airpods", "buds", "耳机", "耳麦"]
    /// 名字里有这些字的当成扬声器（蓝牙音箱、显示器的扬声器）
    static let speakerWords = ["speaker", "扬声器", "音箱", "音响", "soundbar", "homepod", "display", "显示器"]

    /// 判断结果和原因（原因写日志用）。info 是 nil（没有默认输出设备、读不出来）时当外放
    public static func classify(_ info: OutputDeviceInfo?) -> (kind: OutputRouteKind, reason: String) {
        guard let info else { return (.speakers, "读不到默认的输出设备，当成外放") }
        let name = info.name.lowercased()
        let speakerName = speakerWords.contains { name.contains($0) }
        if info.transport == kAudioDeviceTransportTypeAirPlay {
            return (.airplay, "「\(info.name)」是 AirPlay")
        }
        if info.terminalTypes.contains(where: { $0 == kAudioStreamTerminalTypeHeadphones || usbHeadphoneTerminals.contains($0) }) {
            return (.headphones, "「\(info.name)」的输出是耳机")
        }
        if info.dataSource == fourCharCode("hdpn") {
            return (.headphones, "「\(info.name)」插着耳机")
        }
        if headphoneWords.contains(where: { name.contains($0) }) {
            return (.headphones, "「\(info.name)」看名字是耳机")
        }
        if info.transport == kAudioDeviceTransportTypeBluetooth || info.transport == kAudioDeviceTransportTypeBluetoothLE {
            return speakerName ? (.speakers, "「\(info.name)」是蓝牙音箱") : (.headphones, "「\(info.name)」是蓝牙设备，当成耳机")
        }
        if info.isMicDevice && !speakerName {
            return (.headphones, "「\(info.name)」和麦克风是同一个设备，当成耳麦")
        }
        return (.speakers, "「\(info.name)」是外放")
    }

    static func fourCharCode(_ text: String) -> UInt32 {
        text.utf8.reduce(0) { ($0 << 8) | UInt32($1) }
    }
}

/// 跟着系统的默认输出设备判断输出路线：默认输出设备、它的数据源（插拔耳机）、它的输出流变了都重新判断。
/// 判断在自己的队列上做，结果加锁放着，处理线程每 20 毫秒读一次（EchoStage 再等它稳定 1 秒才跟着变）
final class OutputRoute: OutputRouteSource {
    /// 正在录的麦克风（只在 queue 上读写）
    private var micDevice: AudioObjectID?
    private let queue = DispatchQueue(label: "io.github.zanexo.zsmeet.recorder.output-route")
    private let lock = NSLock()
    private var kind = OutputRouteKind.speakers
    private var reason = "还没判断"
    private var device = AudioObjectID(kAudioObjectUnknown)
    private var systemListener: PropertyListener?
    private var deviceListeners: [PropertyListener] = []
    private var stopped = false

    /// micDevice：正在录的麦克风（判断是不是耳麦），没录麦克风时是 nil
    init(micDevice: AudioObjectID?) {
        self.micDevice = micDevice
        queue.sync {
            systemListener = PropertyListener(object: AudioProperty.system, selector: kAudioHardwarePropertyDefaultOutputDevice,
                                              queue: queue) { [weak self] in self?.refresh() }
            refresh()
        }
    }

    deinit {
        removeListeners()
    }

    var current: OutputRouteKind {
        lock.lock()
        defer { lock.unlock() }
        return kind
    }

    var currentDescription: String {
        lock.lock()
        defer { lock.unlock() }
        return reason
    }

    func updateMicDevice(_ device: AudioObjectID?) {
        queue.async { [weak self] in
            guard let self, !self.stopped, self.micDevice != device else { return }
            self.micDevice = device
            self.refresh()
        }
    }

    func stop() {
        queue.sync {
            stopped = true
            removeListeners()
        }
    }

    private func removeListeners() {
        systemListener?.remove()
        systemListener = nil
        deviceListeners.forEach { $0.remove() }
        deviceListeners = []
    }

    /// 重新判断（在 queue 上）
    private func refresh() {
        guard !stopped else { return }
        let current = (try? AudioProperty.value(AudioProperty.system, kAudioHardwarePropertyDefaultOutputDevice,
                                                 initial: AudioObjectID(kAudioObjectUnknown))) ?? AudioObjectID(kAudioObjectUnknown)
        if current != device {
            deviceListeners.forEach { $0.remove() }
            deviceListeners = []
            device = current
            if current != kAudioObjectUnknown {
                deviceListeners = [kAudioDevicePropertyDataSource, kAudioDevicePropertyStreams].map { selector in
                    PropertyListener(object: current, selector: selector, scope: kAudioObjectPropertyScopeOutput,
                                     queue: queue) { [weak self] in self?.refresh() }
                }
            }
        }
        let info = current == kAudioObjectUnknown ? nil : Self.info(current, micDevice: micDevice)
        let (kind, reason) = OutputRouteClassifier.classify(info)
        lock.lock()
        let changed = kind != self.kind || reason != self.reason
        self.kind = kind
        self.reason = reason
        lock.unlock()
        if changed {
            log("输出路线：\(reason)")
        }
    }

    private static func info(_ device: AudioObjectID, micDevice: AudioObjectID?) -> OutputDeviceInfo {
        let name = (try? AudioProperty.string(device, kAudioObjectPropertyName)) ?? "未知设备"
        let transport = (try? AudioProperty.value(device, kAudioDevicePropertyTransportType, initial: UInt32(0))) ?? 0
        let streams = (try? AudioProperty.objectIDs(device, kAudioDevicePropertyStreams, scope: kAudioObjectPropertyScopeOutput)) ?? []
        let terminals = streams.compactMap { try? AudioProperty.value($0, kAudioStreamPropertyTerminalType, initial: UInt32(0)) }
        let dataSource = try? AudioProperty.value(device, kAudioDevicePropertyDataSource, scope: kAudioObjectPropertyScopeOutput,
                                                  initial: UInt32(0))
        return OutputDeviceInfo(name: name, transport: transport, terminalTypes: terminals, dataSource: dataSource,
                                isMicDevice: micDevice == device)
    }
}

import CoreAudio
import Foundation

/// 一个能录音的输入设备
public struct Microphone: Equatable {
    public let id: AudioObjectID
    public let uid: String
    public let name: String
    public let isDefault: Bool

    var json: JSON {
        .object([("uid", .string(uid)), ("name", .string(name)), ("default", .bool(isDefault))])
    }
}

public enum Microphones {
    /// 所有能录音的输入设备，系统默认的排在最前面。只读设备信息，不会弹权限窗口
    public static func list() -> [Microphone] {
        let defaultID = AudioProperty.defaultInputDevice()
        let ids = (try? AudioProperty.objectIDs(AudioProperty.system, kAudioHardwarePropertyDevices)) ?? []
        let microphones = ids.compactMap { id -> Microphone? in
            guard AudioProperty.inputChannelCount(id) > 0,
                  (try? AudioProperty.value(id, kAudioDevicePropertyIsHidden, initial: UInt32(0))) != 1,
                  let uid = try? AudioProperty.string(id, kAudioDevicePropertyDeviceUID),
                  // 系统给某些 App 临时建的聚合设备、我们自己录电脑声音用的聚合设备，不是真的麦克风
                  !uid.hasPrefix("CADefaultDeviceAggregate"), !uid.hasPrefix(SystemAudioCapture.aggregateUIDPrefix)
            else { return nil }
            let name = (try? AudioProperty.string(id, kAudioObjectPropertyName)) ?? uid
            return Microphone(id: id, uid: uid, name: name, isDefault: id == defaultID)
        }
        return microphones.filter(\.isDefault) + microphones.filter { !$0.isDefault }
    }

    /// devices 子命令的输出：{"microphones":[{"uid":"…","name":"…","default":true}]}
    public static func json(_ microphones: [Microphone]) -> JSON {
        .object([("microphones", .array(microphones.map(\.json)))])
    }

    /// 按 UID 找麦克风；"default" 用系统默认的
    static func find(_ choice: MicChoice) -> Microphone? {
        let all = list()
        switch choice {
        case .none:
            return nil
        case .systemDefault:
            return all.first(where: \.isDefault) ?? all.first
        case .device(let uid):
            return all.first { $0.uid == uid }
        }
    }
}

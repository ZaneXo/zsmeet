import AVFoundation
import Foundation

enum PermissionResult {
    case granted
    case denied
    /// 查不到（系统没有给查询的办法），照常开始，系统会在第一次录的时候自己弹窗
    case unknown
    /// 等用户点弹窗的时候，App 让停止录音了
    case cancelled
}

/// 麦克风权限。由 App 启动的录音小程序，系统算到 App 头上：弹窗写的是 zsmeet，设置里也是开 zsmeet。
enum MicPermission {
    /// 有权限就返回 granted；还没问过就弹窗问，等用户点（cancelled 返回 true 时不等了）
    static func ensure(cancelled: () -> Bool) -> PermissionResult {
        switch AVCaptureDevice.authorizationStatus(for: .audio) {
        case .authorized:
            return .granted
        case .notDetermined:
            let granted = waitForAnswer(cancelled: cancelled) { answer in
                AVCaptureDevice.requestAccess(for: .audio, completionHandler: answer)
            }
            guard let granted else { return .cancelled }
            return granted ? .granted : .denied
        default:
            return .denied
        }
    }
}

/// 录电脑声音的权限（「隐私与安全性 → 屏幕与系统录音」里的「仅系统录音」）。
/// 苹果没有公开查询和申请这个权限的接口，这里和其他录系统声音的软件一样，用系统 TCC 框架里的
/// TCCAccessPreflight、TCCAccessRequest（只查询、弹窗，不改设置）。将来系统里找不到这两个函数时
/// 返回 unknown：照常开始录，系统在第一次录的时候自己弹窗，只是拒绝了的话我们发现不了。
enum SystemAudioPermission {
    private typealias Preflight = @convention(c) (CFString, CFDictionary?) -> Int
    private typealias Request = @convention(c) (CFString, CFDictionary?, @escaping @convention(block) (Bool) -> Void) -> Void

    private static let service = "kTCCServiceAudioCapture" as CFString
    private static let framework = dlopen("/System/Library/PrivateFrameworks/TCC.framework/Versions/A/TCC", RTLD_NOW)

    static func ensure(cancelled: () -> Bool) -> PermissionResult {
        guard let framework,
              let preflightSymbol = dlsym(framework, "TCCAccessPreflight"),
              let requestSymbol = dlsym(framework, "TCCAccessRequest")
        else { return .unknown }
        let preflight = unsafeBitCast(preflightSymbol, to: Preflight.self)
        let request = unsafeBitCast(requestSymbol, to: Request.self)

        switch preflight(service, nil) {
        case 0:
            return .granted
        case 1:
            return .denied
        default:
            let granted = waitForAnswer(cancelled: cancelled) { answer in
                request(service, nil, answer)
            }
            guard let granted else { return .cancelled }
            return granted ? .granted : .denied
        }
    }
}

/// 发起一个带回调的请求（比如权限弹窗），等回调的结果。每 0.1 秒看一次要不要放弃，放弃时返回 nil
private func waitForAnswer(cancelled: () -> Bool, _ ask: (@escaping (Bool) -> Void) -> Void) -> Bool? {
    let semaphore = DispatchSemaphore(value: 0)
    let lock = NSLock()
    var answer: Bool?
    ask { value in
        lock.lock()
        answer = value
        lock.unlock()
        semaphore.signal()
    }
    while semaphore.wait(timeout: .now() + 0.1) == .timedOut {
        if cancelled() {
            return nil
        }
    }
    lock.lock()
    defer { lock.unlock() }
    return answer
}

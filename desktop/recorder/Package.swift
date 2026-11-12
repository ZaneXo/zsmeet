// swift-tools-version: 5.9
// 录音小程序 zsmeet-recorder：录麦克风和电脑里的声音，编码成 AAC 分片，由桌面版 App 启动、上传。
// 打包用的通用版本由 desktop/scripts/build-recorder.sh 直接用 swiftc 构建（只装了 Command Line Tools
// 也能编两种架构），测试用 desktop/recorder/test.sh。这个文件给编辑器和 swift build / swift run 用，
// 目录结构和脚本里的一致。
import Foundation
import PackageDescription

let packageDirectory = URL(fileURLWithPath: #filePath).deletingLastPathComponent().path

let package = Package(
    name: "zsmeet-recorder",
    platforms: [.macOS("14.4")],
    products: [
        .executable(name: "zsmeet-recorder", targets: ["zsmeet-recorder"]),
    ],
    targets: [
        // 音频线程上跑的部分：环形缓冲和 IOProc（C，不加锁、不分配内存）
        .target(name: "CRecorder"),
        .target(name: "RecorderKit", dependencies: ["CRecorder"]),
        .executableTarget(
            name: "zsmeet-recorder",
            dependencies: ["RecorderKit"],
            linkerSettings: [
                // 把 Info.plist 嵌进程序里，签名时用里面的标识
                .unsafeFlags(["-Xlinker", "-sectcreate", "-Xlinker", "__TEXT", "-Xlinker", "__info_plist",
                              "-Xlinker", "\(packageDirectory)/Info.plist"]),
            ]
        ),
        // 测试是一个普通的程序（没有 XCTest 也能跑）：swift run recorder-tests；
        // 端到端测试要先设 ZSMEET_RECORDER 为要测的程序
        .executableTarget(
            name: "recorder-tests",
            dependencies: ["RecorderKit", "CRecorder"],
            path: "Tests/recorder-tests"
        ),
    ]
)

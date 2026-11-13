// swift-tools-version: 5.9
// 录音小程序 zsmeet-recorder：录麦克风和电脑里的声音，编码成 AAC 分片，由桌面版 App 启动、上传。
// 打包用的通用版本由 desktop/scripts/build-recorder.sh 直接用 swiftc 构建（只装了 Command Line Tools
// 也能编两种架构），测试用 desktop/recorder/test.sh。这个文件给编辑器用（代码补全、跳转），目录结构和脚本里的一致。
// 回声消除、自动增益用的 WebRTC（ThirdParty/，几百个文件，有的文件只在一种架构上编、要单独的编译参数）SwiftPM 编不了，
// 所以 swift build 链接不起来：构建、测试一律用上面两个脚本，编译参数以它们（ThirdParty/webrtc-flags.sh）为准。
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
        // 音频线程上跑的部分：环形缓冲和 IOProc（C，不加锁、不分配内存），
        // 以及回声消除（WebRTC 的 AEC3）、麦克风自动增益（AGC2）的 C 接口 zr_aec.cc、zr_agc.cc
        //（C++，宏和 webrtc-flags.sh 里的一样）
        .target(
            name: "CRecorder",
            cxxSettings: [
                .headerSearchPath("../../ThirdParty/webrtc-audio-processing/webrtc"),
                .headerSearchPath("../../ThirdParty/abseil-cpp"),
                .define("NDEBUG"),
                .define("WEBRTC_MAC"),
                .define("WEBRTC_POSIX"),
                .define("WEBRTC_LIBRARY_IMPL"),
                .define("WEBRTC_ENABLE_SYMBOL_EXPORT"),
                .define("_WINSOCKAPI_"),
                .define("WEBRTC_APM_DEBUG_DUMP", to: "0"),
            ]
        ),
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
        // 测试是一个普通的程序（没有 XCTest 也能跑），用 desktop/recorder/test.sh 编译、运行；
        // 端到端测试要先设 ZSMEET_RECORDER 为要测的程序
        .executableTarget(
            name: "recorder-tests",
            dependencies: ["RecorderKit", "CRecorder"],
            path: "Tests/recorder-tests"
        ),
    ],
    cxxLanguageStandard: .cxx17
)

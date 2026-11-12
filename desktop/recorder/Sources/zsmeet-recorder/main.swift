// zsmeet-recorder：桌面版的录音小程序，由 App 启动，录麦克风和电脑里的声音，编码成 AAC 分片。
// 命令行用法见 CommandLineParser.usage，和 App 之间的约定见 docs/desktop.md 的「录音（第三阶段）」。
import RecorderKit

RecorderMain.run(Array(CommandLine.arguments.dropFirst()))

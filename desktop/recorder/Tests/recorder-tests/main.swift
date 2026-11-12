// 录音小程序的测试。desktop/recorder/test.sh 会编译它、设好 ZSMEET_RECORDER（要测的程序）后运行
import Foundation

// 往已经退出的录音小程序写命令时不要被 SIGPIPE 杀掉
signal(SIGPIPE, SIG_IGN)
setvbuf(stdout, nil, _IOLBF, 0)

// 测试用的临时目录都在这下面：开始前清掉上次的；全部通过再删掉，没通过的留着排查
let scratch = FileManager.default.temporaryDirectory.appendingPathComponent("zsmeet-recorder-tests", isDirectory: true)
try? FileManager.default.removeItem(at: scratch)

let passed = Harness.run(formatTests + audioTests + recordingTests + endToEndTests)
if passed {
    try? FileManager.default.removeItem(at: scratch)
    print("全部通过")
} else {
    print("有测试没通过，测试用的文件留在 \(scratch.path)")
}
exit(passed ? 0 : 1)

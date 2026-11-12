// 录音小程序的测试。desktop/recorder/test.sh 会编译它、设好 ZSMEET_RECORDER（要测的程序）后运行
import Foundation

// 往已经退出的录音小程序写命令时不要被 SIGPIPE 杀掉
signal(SIGPIPE, SIG_IGN)
setvbuf(stdout, nil, _IOLBF, 0)

// 这次运行的临时目录，测试用的文件都在这下面。每次运行单独一个：同时跑好几次（比如几个工作区一起测）
// 也不会删掉别的运行正在用的。全部通过再删掉，没通过的留着排查
let scratch = FileManager.default.temporaryDirectory
    .appendingPathComponent("zsmeet-recorder-tests", isDirectory: true)
    .appendingPathComponent(UUID().uuidString, isDirectory: true)

switch Harness.run(harnessTests + formatTests + audioTests + recordingTests + endToEndTests) {
case .passed:
    try? FileManager.default.removeItem(at: scratch)
    print("全部通过")
    exit(0)
case .failed:
    print("有测试没通过，测试用的文件留在 \(scratch.path)")
    exit(1)
case .nothingSelected:
    print("一个测试都没跑：没有名字里有「\(Harness.filter)」的测试，看看 \(Harness.filterVariable) 是不是写错了")
    exit(1)
}

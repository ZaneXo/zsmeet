import Foundation
@testable import RecorderKit

/// 极简的测试框架：只装了 Command Line Tools 的机器上没有 XCTest，所以测试是一个普通的程序，
/// 用 desktop/recorder/test.sh 编译、运行（Package.swift 里是 swift run recorder-tests）。
enum Harness {
    private static var failures = 0
    private static var currentTest = ""
    /// 在测断言本身（countFailures）：没通过的地方不打印
    private static var muted = false

    /// 只跑名字里有这几个字的测试，不设就全跑：ZSMEET_RECORDER_TEST_FILTER=暂停 desktop/recorder/test.sh
    static let filterVariable = "ZSMEET_RECORDER_TEST_FILTER"
    private static let filter = ProcessInfo.processInfo.environment[filterVariable] ?? ""

    static func run(_ tests: [(String, () throws -> Void)]) -> Bool {
        for (name, body) in tests where filter.isEmpty || name.contains(filter) {
            currentTest = name
            let before = failures
            do {
                try body()
            } catch {
                fail("抛出了错误：\(error)")
            }
            print(failures == before ? "  通过  \(name)" : "  失败  \(name)")
        }
        return failures == 0
    }

    static func fail(_ message: String, file: StaticString = #fileID, line: UInt = #line) {
        failures += 1
        if !muted {
            print("    \(file):\(line) \(message)")
        }
    }

    /// 测断言本身用：body 里有几处没通过。不算进这次运行，也不打印
    static func countFailures(in body: () -> Void) -> Int {
        let before = failures
        let wasMuted = muted
        muted = true
        body()
        muted = wasMuted
        let count = failures - before
        failures = before
        return count
    }
}

/// 测试框架自己
let harnessTests: [(String, () throws -> Void)] = [
    ("测试框架：差得太多、有一边是 NaN 时 expectClose 都算没通过", testExpectClose),
    ("测试框架：同时跑两次测试，不会删掉对方正在用的临时目录", testScratchIsPerRun),
]

func testExpectClose() {
    expectEqual(Harness.countFailures { expectClose(1, 1.05, within: 0.1) }, 0, "差得不多")
    expectEqual(Harness.countFailures { expectClose(1, 1.2, within: 0.1) }, 1, "差得太多")
    expectEqual(Harness.countFailures { expectClose(.nan, 1, within: 0.1) }, 1, "实际是 NaN")
    expectEqual(Harness.countFailures { expectClose(1, .nan, within: 0.1) }, 1, "期望是 NaN")
}

func testScratchIsPerRun() throws {
    let mine = try temporaryDirectory("parallel")
    // 这时候又开始了一次测试：一个测试都不跑，只有开始、结束时建、删临时目录的那部分
    let other = Process()
    other.executableURL = Bundle.main.executableURL
    var environment = ProcessInfo.processInfo.environment
    environment[Harness.filterVariable] = "（一个测试都不跑）"
    other.environment = environment
    other.standardOutput = FileHandle.nullDevice
    other.standardError = FileHandle.nullDevice
    try other.run()
    other.waitUntilExit()
    expectEqual(other.terminationStatus, 0, "另一次测试")
    expect(FileManager.default.fileExists(atPath: mine.path), "另一次测试删掉了这次正在用的临时目录")
}

func expect(_ condition: @autoclosure () -> Bool, _ message: @autoclosure () -> String = "条件不成立",
            file: StaticString = #fileID, line: UInt = #line) {
    if !condition() {
        Harness.fail(message(), file: file, line: line)
    }
}

func expectEqual<T: Equatable>(_ actual: T, _ expected: T, _ message: @autoclosure () -> String = "",
                               file: StaticString = #fileID, line: UInt = #line) {
    if actual != expected {
        let note = message()
        Harness.fail("期望 \(expected)，实际是 \(actual)\(note.isEmpty ? "" : "（\(note)）")", file: file, line: line)
    }
}

func expectClose(_ actual: Double, _ expected: Double, within tolerance: Double, _ message: @autoclosure () -> String = "",
                 file: StaticString = #fileID, line: UInt = #line) {
    // 写成「不在范围内」：有一边是 NaN 时比较的结果总是 false，写成「差得比 tolerance 多」就会让它通过
    if !(abs(actual - expected) <= tolerance) {
        let note = message()
        Harness.fail("期望 \(expected) ± \(tolerance)，实际是 \(actual)\(note.isEmpty ? "" : "（\(note)）")", file: file, line: line)
    }
}

func expectThrows<E: Error & Equatable>(_ expected: E, file: StaticString = #fileID, line: UInt = #line, _ body: () throws -> Void) {
    do {
        try body()
        Harness.fail("期望抛出 \(expected)，结果没有抛出", file: file, line: line)
    } catch let error as E {
        expectEqual(error, expected, file: file, line: line)
    } catch {
        Harness.fail("期望抛出 \(expected)，实际是 \(error)", file: file, line: line)
    }
}

/// 每个测试自己的临时目录，都在这次运行的临时目录（main.swift 的 scratch）下面
func temporaryDirectory(_ name: String) throws -> URL {
    let url = scratch.appendingPathComponent("\(name)-\(UUID().uuidString)", isDirectory: true)
    try FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
    return url
}

/// 把事件记在内存里，测试时代替标准输出
final class RecordedEvents: EventSink {
    private let lock = NSLock()
    private var lines: [String] = []

    func send(_ event: RecorderEvent) {
        lock.lock()
        lines.append(event.json.text)
        lock.unlock()
    }

    var all: [String] {
        lock.lock()
        defer { lock.unlock() }
        return lines
    }

    /// 每个事件的 "event" 字段
    var names: [String] {
        all.compactMap { line in
            guard let data = line.data(using: .utf8),
                  let object = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any]
            else { return nil }
            return object["event"] as? String
        }
    }
}

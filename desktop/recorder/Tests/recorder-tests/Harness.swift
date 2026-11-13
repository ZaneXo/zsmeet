import Foundation
@testable import RecorderKit

/// 极简的测试框架：只装了 Command Line Tools 的机器上没有 XCTest，所以测试是一个普通的程序，
/// 用 desktop/recorder/test.sh 编译、运行（Package.swift 里是 swift run recorder-tests）。
enum Harness {
    /// 一次运行的结果
    enum Outcome {
        case passed
        case failed
        /// 设了 ZSMEET_RECORDER_TEST_FILTER，却一个测试都没选中（多半是写错了）：什么都没测，不能算通过
        case nothingSelected
    }

    private static var failures = 0
    private static var currentTest = ""
    /// 在测断言本身（countFailures）：没通过的地方不打印
    private static var muted = false

    /// 只跑名字里有这几个字的测试，不设就全跑：ZSMEET_RECORDER_TEST_FILTER=暂停 desktop/recorder/test.sh。
    /// 用「|」隔开几个，名字里有其中一个就跑
    static let filterVariable = "ZSMEET_RECORDER_TEST_FILTER"
    static let filter = ProcessInfo.processInfo.environment[filterVariable] ?? ""
    /// 设成 1 时，一个测试都没选中也算通过。只给故意一个都不跑的时候用（测试框架自己的测试会这样再启动一次）
    static let allowEmptyVariable = "ZSMEET_RECORDER_TEST_ALLOW_EMPTY"
    private static let allowEmpty = ProcessInfo.processInfo.environment[allowEmptyVariable] == "1"

    static func run(_ tests: [(String, () throws -> Void)]) -> Outcome {
        let words = filter.split(separator: "|").map(String.init).filter { !$0.isEmpty }
        let selected = tests.filter { test in words.isEmpty || words.contains { test.0.contains($0) } }
        if selected.isEmpty && !allowEmpty {
            return .nothingSelected
        }
        for (name, body) in selected {
            currentTest = name
            let before = failures
            do {
                try body()
            } catch {
                fail("抛出了错误：\(error)")
            }
            print(failures == before ? "  通过  \(name)" : "  失败  \(name)")
        }
        return failures == 0 ? .passed : .failed
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
    ("测试框架：ZSMEET_RECORDER_TEST_FILTER 一个测试都没选中时算没通过", testFilterSelectingNothingFails),
]

func testExpectClose() {
    expectEqual(Harness.countFailures { expectClose(1, 1.05, within: 0.1) }, 0, "差得不多")
    expectEqual(Harness.countFailures { expectClose(1, 1.2, within: 0.1) }, 1, "差得太多")
    expectEqual(Harness.countFailures { expectClose(.nan, 1, within: 0.1) }, 1, "实际是 NaN")
    expectEqual(Harness.countFailures { expectClose(1, .nan, within: 0.1) }, 1, "期望是 NaN")
}

func testScratchIsPerRun() throws {
    let mine = try temporaryDirectory("parallel")
    // 这时候又开始了一次测试：一个测试都不跑（明确允许），只有开始、结束时建、删临时目录的那部分
    let other = try runSelectingNothing(allowEmpty: true)
    expectEqual(other.status, 0, "另一次测试")
    expect(FileManager.default.fileExists(atPath: mine.path), "另一次测试删掉了这次正在用的临时目录")
}

func testFilterSelectingNothingFails() throws {
    // 名字写错了，一个测试都没跑：不能说「全部通过」
    let other = try runSelectingNothing(allowEmpty: false)
    expectEqual(other.status, 1, "一个测试都没跑")
    expect(!other.output.contains("全部通过"), other.output)
    expect(other.output.contains("一个测试都没跑"), other.output)
}

/// 再启动一次这个测试程序，ZSMEET_RECORDER_TEST_FILTER 谁都不匹配，一个测试都不选中。
/// allowEmpty：明确允许这样（ZSMEET_RECORDER_TEST_ALLOW_EMPTY=1）。返回它的退出码和标准输出
private func runSelectingNothing(allowEmpty: Bool) throws -> (status: Int32, output: String) {
    let other = Process()
    other.executableURL = Bundle.main.executableURL
    var environment = ProcessInfo.processInfo.environment
    environment[Harness.filterVariable] = "（一个测试都不跑）"
    environment[Harness.allowEmptyVariable] = allowEmpty ? "1" : nil
    other.environment = environment
    let output = Pipe()
    other.standardOutput = output
    other.standardError = FileHandle.nullDevice
    try other.run()
    let data = output.fileHandleForReading.readDataToEndOfFile()
    other.waitUntilExit()
    return (other.terminationStatus, String(decoding: data, as: UTF8.self))
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

import Foundation
@testable import RecorderKit

/// 极简的测试框架：只装了 Command Line Tools 的机器上没有 XCTest，所以测试是一个普通的程序，
/// 用 desktop/recorder/test.sh 编译、运行（Package.swift 里是 swift run recorder-tests）。
enum Harness {
    private static var failures = 0
    private static var currentTest = ""

    static func run(_ tests: [(String, () throws -> Void)]) -> Bool {
        for (name, body) in tests {
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
        print("    \(file):\(line) \(message)")
    }
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
    if abs(actual - expected) > tolerance {
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

/// 每个测试自己的临时目录
func temporaryDirectory(_ name: String) throws -> URL {
    let url = FileManager.default.temporaryDirectory
        .appendingPathComponent("zsmeet-recorder-tests", isDirectory: true)
        .appendingPathComponent("\(name)-\(UUID().uuidString)", isDirectory: true)
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

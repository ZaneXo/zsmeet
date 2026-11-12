import Foundation

/// 事件和设备列表输出用的 JSON。自己拼而不用 JSONEncoder：键按写的顺序输出（"event" 总在最前面），
/// 小数最多 3 位，App 和测试读起来都简单。
public enum JSON {
    case string(String)
    case int(Int)
    /// 小数：最多保留 3 位，去掉末尾的 0
    case number(Double)
    case bool(Bool)
    case null
    case array([JSON])
    case object([(String, JSON)])

    /// 有值时是字符串，没有时是 null
    public static func optional(_ value: String?) -> JSON {
        value.map(JSON.string) ?? .null
    }

    /// 一行 JSON 文本（不带换行）
    public var text: String {
        var out = ""
        write(to: &out)
        return out
    }

    private func write(to out: inout String) {
        switch self {
        case .string(let value):
            JSON.writeString(value, to: &out)
        case .int(let value):
            out += String(value)
        case .number(let value):
            out += JSON.format(value)
        case .bool(let value):
            out += value ? "true" : "false"
        case .null:
            out += "null"
        case .array(let items):
            out += "["
            for (index, item) in items.enumerated() {
                if index > 0 { out += "," }
                item.write(to: &out)
            }
            out += "]"
        case .object(let fields):
            out += "{"
            for (index, field) in fields.enumerated() {
                if index > 0 { out += "," }
                JSON.writeString(field.0, to: &out)
                out += ":"
                field.1.write(to: &out)
            }
            out += "}"
        }
    }

    static func format(_ value: Double) -> String {
        guard value.isFinite else { return "0" }
        // String(format:) 不按系统的地区设置，小数点总是「.」
        var text = String(format: "%.3f", value)
        while text.hasSuffix("0") { text.removeLast() }
        if text.hasSuffix(".") { text.removeLast() }
        return text == "-0" ? "0" : text
    }

    static func writeString(_ value: String, to out: inout String) {
        out += "\""
        for scalar in value.unicodeScalars {
            switch scalar {
            case "\"": out += "\\\""
            case "\\": out += "\\\\"
            case "\n": out += "\\n"
            case "\r": out += "\\r"
            case "\t": out += "\\t"
            default:
                if scalar.value < 0x20 {
                    out += String(format: "\\u%04x", scalar.value)
                } else {
                    out.unicodeScalars.append(scalar)
                }
            }
        }
        out += "\""
    }
}

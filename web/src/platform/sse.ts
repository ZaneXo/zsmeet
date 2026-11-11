/**
 * 服务器推送（text/event-stream）的解析。桌面版的 EventSource 加不了 X-Zsmeet-App 请求头，
 * 只能自己用 fetch 读流，再用它切成一条条事件。不依赖 React 和浏览器，可以直接测。
 */

export type SSEEvent = { event: string; data: string };

export class SSEParser {
  private buffer = "";
  private event = "";
  private data: string[] = [];

  /** 喂进收到的一段文本，返回这段里完整的事件。没收完的部分留着，和下一段拼起来。 */
  push(text: string): SSEEvent[] {
    this.buffer += text;
    const out: SSEEvent[] = [];
    for (;;) {
      const match = /\r\n|\r|\n/.exec(this.buffer);
      if (!match) break;
      // 「\r」在末尾时可能是「\r\n」的前一半，等下一段再说
      if (match[0] === "\r" && match.index === this.buffer.length - 1) break;
      const line = this.buffer.slice(0, match.index);
      this.buffer = this.buffer.slice(match.index + match[0].length);
      const event = this.line(line);
      if (event) out.push(event);
    }
    return out;
  }

  private line(line: string): SSEEvent | undefined {
    if (line === "") {
      // 空行：一条事件结束。没有 data 的不算
      const event = this.data.length > 0 ? { event: this.event || "message", data: this.data.join("\n") } : undefined;
      this.event = "";
      this.data = [];
      return event;
    }
    if (line.startsWith(":")) return undefined; // 注释（心跳）
    const colon = line.indexOf(":");
    const field = colon === -1 ? line : line.slice(0, colon);
    let value = colon === -1 ? "" : line.slice(colon + 1);
    if (value.startsWith(" ")) value = value.slice(1);
    if (field === "event") this.event = value;
    else if (field === "data") this.data.push(value);
    return undefined;
  }
}

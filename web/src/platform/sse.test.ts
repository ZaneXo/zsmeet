import assert from "node:assert/strict";
import { test } from "node:test";
import { SSEParser } from "./sse.ts";

test("切成一条条事件，跨段的也能拼起来", () => {
  const p = new SSEParser();
  assert.deepEqual(p.push('event: meeting\ndata: {"id":'), []);
  assert.deepEqual(p.push('"m1"}\n\n: 心跳\n\nevent: meeting\r\ndata: 2\r'), [{ event: "meeting", data: '{"id":"m1"}' }]);
  assert.deepEqual(p.push("\n\r\n"), [{ event: "meeting", data: "2" }]);
});

test("流在最后一个空行的「\\r」之后结束：最后一条事件也要交出来", () => {
  const p = new SSEParser();
  // 只用「\r」换行：最后那个「\r」可能是「\r\n」的前一半，push 时还不能确定
  assert.deepEqual(p.push("event: meeting\rdata: 1\r\r"), []);
  assert.deepEqual(p.end(), [{ event: "meeting", data: "1" }]);
});

test("流结束时没写完的行、没用空行结束的事件不算", () => {
  const p = new SSEParser();
  assert.deepEqual(p.push("event: meeting\ndata: 1\ndata: 2"), []);
  assert.deepEqual(p.end(), []);
  const q = new SSEParser();
  assert.deepEqual(q.push("data: 1\r"), []);
  assert.deepEqual(q.end(), []);
});

test("多行 data 用换行连起来，没写 event 的是 message", () => {
  const p = new SSEParser();
  assert.deepEqual(p.push("data: a\ndata:b\n\nevent: x\n\n"), [{ event: "message", data: "a\nb" }]);
  // 只有 event 没有 data 的不算一条，也不影响下一条
  assert.deepEqual(p.push("data: c\n\n"), [{ event: "message", data: "c" }]);
});

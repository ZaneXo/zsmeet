// 用 Node 自带的测试运行：pnpm -C web test
import assert from "node:assert/strict";
import { test } from "node:test";
import { localOnly } from "./site.ts";

test("只有本机能打开的地址", () => {
  for (const url of [
    "http://localhost:18090",
    "http://LOCALHOST",
    "http://meet.localhost",
    "http://localhost.:8080",
    "http://127.0.0.1:18090",
    "http://127.0.1.1",
    "http://127.1",
    "http://0.0.0.0:18090",
    "http://[::1]:18090",
    "http://[0:0:0:0:0:0:0:1]",
    "http://[::]",
    "http://[::ffff:127.0.0.1]",
  ]) {
    assert.equal(localOnly(url), true, url);
  }
});

test("同事能打开的地址、看不懂的地址都不算", () => {
  for (const url of ["https://meet.example.com", "http://192.168.1.10:18090", "http://10.0.0.5", "http://128.0.0.1", "http://[2001:db8::1]", "http://localhost-meet.example.com", "", "不是地址"]) {
    assert.equal(localOnly(url), false, url);
  }
});

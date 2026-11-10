// 用 Node 自带的测试运行：pnpm -C web test
import assert from "node:assert/strict";
import { test } from "node:test";
import { newestFirst } from "./time.ts";

test("按时间排，新的在前；小数秒位数不同、时区写法不同也排得对", () => {
  const times = [
    "2026-11-10T10:00:00+08:00", // 整秒，没有小数点
    "2026-11-10T10:00:00.5+08:00",
    "2026-11-10T10:00:00.123456+08:00",
    "2026-11-10T02:00:01Z", // 同一天 10:00:01（东八区）
  ];
  assert.deepEqual([...times].sort(newestFirst), [
    "2026-11-10T02:00:01Z",
    "2026-11-10T10:00:00.5+08:00",
    "2026-11-10T10:00:00.123456+08:00",
    "2026-11-10T10:00:00+08:00",
  ]);
});

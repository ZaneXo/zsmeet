// 用 Node 自带的测试运行：pnpm -C web test
import assert from "node:assert/strict";
import { test } from "node:test";
import { hms, SOURCE_LABEL } from "./format.ts";

test("录音计时写成 时:分:秒，不足一小时也带小时，小数秒舍去", () => {
  assert.equal(hms(0), "00:00:00");
  assert.equal(hms(59.9), "00:00:59");
  assert.equal(hms(61), "00:01:01");
  assert.equal(hms(3600), "01:00:00");
  assert.equal(hms(36000 + 59 * 60 + 59), "10:59:59");
});

test("计时不会出现负数", () => {
  assert.equal(hms(-3), "00:00:00");
});

test("会议来源：桌面版录的显示「桌面版录音」", () => {
  assert.equal(SOURCE_LABEL.desktop, "桌面版录音");
  assert.equal(SOURCE_LABEL.record, "网页录音");
});

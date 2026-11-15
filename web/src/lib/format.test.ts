// 用 Node 自带的测试运行：pnpm -C web test
import assert from "node:assert/strict";
import { test } from "node:test";
import { asrLanguageLabel, highlightPieces, hms, includesFold, needsSpace, SOURCE_LABEL } from "./format.ts";

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

test("页内搜索：忽略大小写匹配并保留原文大小写，正则特殊字符不报错", () => {
  assert.equal(includesFold("讨论一下 API latency 和 Q3 计划", "api"), true);
  assert.equal(includesFold("讨论一下 API latency", "SDK"), false);
  assert.deepEqual(highlightPieces("Check API and api+(v2)!", "api"), [
    { text: "Check ", mark: false },
    { text: "API", mark: true },
    { text: " and ", mark: false },
    { text: "api", mark: true },
    { text: "+(v2)!", mark: false },
  ]);
  assert.deepEqual(highlightPieces("C++ build", "c++"), [
    { text: "C++", mark: true },
    { text: " build", mark: false },
  ]);
});

test("多语种断句空格与语言名称：英韩留空格，中日不留空格", () => {
  assert.equal(needsSpace("Hello.", "How are you?"), true);
  assert.equal(needsSpace("안녕하세요.", "오늘 회의"), true);
  assert.equal(needsSpace("你好。", "OK"), false);
  assert.equal(needsSpace("こんにちは。", "会議を始めます。"), false);
  assert.equal(asrLanguageLabel("yue", true), "粤语");
  assert.equal(asrLanguageLabel(""), "跟随团队默认");
});

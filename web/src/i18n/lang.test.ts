import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { parseAppAuthorize } from "../lib/appAuthorize.ts";
import { feishuErrorText } from "../lib/feishuErrors.ts";
import { duration, SOURCE_LABEL, speakerName, STAGE_LABEL } from "../lib/format.ts";
import { lastReceived } from "../lib/unfinishedRecording.ts";
import { BUSY_LABEL, recordedLength, sentence } from "../platform/recorderView.ts";
import { updateHeadline, upToDateText } from "../platform/updateView.ts";
import {
  acceptLanguageHeader,
  detectBrowserLang,
  getLang,
  getLangSetting,
  setLangSetting,
  subscribeLang,
  tr,
} from "./lang.ts";

afterEach(() => {
  setLangSetting("auto");
});

test("detectBrowserLang 按浏览器首选语言顺序识别中英文", () => {
  assert.equal(detectBrowserLang(["en-US", "zh-CN"]), "en");
  assert.equal(detectBrowserLang(["zh-Hans-CN", "en-US"]), "zh");
  assert.equal(detectBrowserLang(["fr-FR", "en-GB"]), "en");
  assert.equal(detectBrowserLang(["ja-JP"]), "zh");
  assert.equal(detectBrowserLang([]), "zh");
});

test("setLangSetting 切换语言、通知订阅者并更新 Accept-Language", () => {
  assert.equal(getLangSetting(), "auto");
  assert.equal(getLang(), "zh");
  assert.equal(tr("你好", "Hello"), "你好");
  assert.equal(acceptLanguageHeader(), "zh-CN,zh;q=0.9");

  let notified = 0;
  const unsub = subscribeLang(() => {
    notified += 1;
  });
  try {
    setLangSetting("en");
    assert.equal(notified, 1);
    assert.equal(getLangSetting(), "en");
    assert.equal(getLang(), "en");
    assert.equal(tr("你好", "Hello"), "Hello");
    assert.equal(acceptLanguageHeader(), "en-US,en;q=0.9");
  } finally {
    unsub();
  }
});

test("英文模式下格式化与桌面端提示函数输出英文", () => {
  setLangSetting("en");
  assert.equal(SOURCE_LABEL.desktop, "Desktop recording");
  assert.equal(STAGE_LABEL.minutes, "Generating minutes");
  assert.equal(duration(45), "45s");
  assert.equal(duration(125), "2 min");
  assert.equal(speakerName(0), "Speaker 1");
  assert.match(feishuErrorText("denied", "login"), /Sign in with Feishu/);
  const badAuth = parseAppAuthorize(new URLSearchParams("client=unknown"));
  assert.ok("error" in badAuth && badAuth.error.includes("unknown app"));
  assert.equal(lastReceived({ created_at: "2026-01-01T00:00:00Z" }, Date.now()), "No audio received by the server yet");
  assert.equal(BUSY_LABEL.recording, "Recording");
  assert.equal(recordedLength(12), "about 2 min");
  assert.equal(sentence("Network offline"), "Network offline.");
  assert.equal(upToDateText("0.1.0"), "zsmeet is up to date (0.1.0).");
  assert.equal(updateHeadline({ version: "0.2.0", notes: "", date: null }, "0.1.0").title, "zsmeet 0.2.0 is available");
});

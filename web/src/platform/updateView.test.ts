// 用 Node 自带的测试运行：pnpm -C web test
import assert from "node:assert/strict";
import { test } from "node:test";
import type { RecorderState, RecorderUpload, UploadStatus } from "./recorder";
import type { UpdateInfo } from "./updater";
import {
  checkFailedText,
  installFailedText,
  installView,
  normalizeCheck,
  normalizeInfo,
  normalizeProgress,
  noticeVisible,
  notesText,
  parseDismissed,
  recordUpdateNote,
  releaseDate,
  REMIND_AFTER_MS,
  size,
  updateBlocker,
  updateHeadline,
  upToDateText,
} from "./updateView.ts";

function state(patch: Partial<RecorderState> = {}): RecorderState {
  return {
    status: "idle",
    meetingId: null,
    title: null,
    seconds: 0,
    level: null,
    mic: null,
    system: false,
    marks: [],
    warnings: [],
    error: null,
    uploads: [],
    echo: null,
    ...patch,
  };
}

function upload(status: UploadStatus, meetingId = "m1"): RecorderUpload {
  return { meetingId, title: "周会", recorded: 10, uploaded: 4, status, error: null };
}

const info: UpdateInfo = { version: "0.2.0", notes: "录音更稳了。", date: "2026-11-10T12:00:00" };

// ---- App 报过来的东西 ----

test("新版本：有版本号才算，说明、发布时间缺了补成空的", () => {
  assert.deepEqual(normalizeInfo({ version: "0.2.0", notes: "录音更稳了。", date: "2026-11-10T08:00:00Z" }), {
    version: "0.2.0",
    notes: "录音更稳了。",
    date: "2026-11-10T08:00:00Z",
  });
  assert.deepEqual(normalizeInfo({ version: " 0.2.0 ", notes: null, date: null }), { version: "0.2.0", notes: "", date: null });
  assert.deepEqual(normalizeInfo({ version: "0.2.0", date: "  " }), { version: "0.2.0", notes: "", date: null });
  for (const bad of [null, undefined, "0.2.0", 2, [], {}, { version: "" }, { version: "  " }, { version: 2 }]) {
    assert.equal(normalizeInfo(bad), null, JSON.stringify(bad));
  }
});

test("检查的结果：现在的版本，有没有新版本", () => {
  assert.deepEqual(normalizeCheck({ current: "0.1.0", available: info }), { current: "0.1.0", available: info });
  assert.deepEqual(normalizeCheck({ current: "0.1.0", available: null }), { current: "0.1.0", available: null });
  assert.deepEqual(normalizeCheck(null), { current: "", available: null });
  assert.deepEqual(normalizeCheck({ available: { version: "" } }), { current: "", available: null });
});

test("下载进度：一共多大不知道（或者报的不对）时是 null", () => {
  assert.deepEqual(normalizeProgress({ downloaded: 1024, total: 4096 }), { downloaded: 1024, total: 4096 });
  assert.deepEqual(normalizeProgress({ downloaded: 1024, total: null }), { downloaded: 1024, total: null });
  assert.deepEqual(normalizeProgress({ downloaded: 1024 }), { downloaded: 1024, total: null });
  assert.deepEqual(normalizeProgress({ downloaded: 1024, total: 0 }), { downloaded: 1024, total: null });
  assert.deepEqual(normalizeProgress({ downloaded: -5, total: 10 }), { downloaded: 0, total: 10 });
  for (const bad of [null, {}, { downloaded: "1" }, { downloaded: Number.NaN }, { downloaded: Number.POSITIVE_INFINITY }]) {
    assert.equal(normalizeProgress(bad), null);
  }
});

// ---- 新版本是什么 ----

test("发布日期只写到天，看不懂的不写", () => {
  // 不带时区的按这台电脑的时间算，测试在哪个时区跑都一样
  assert.equal(releaseDate("2026-11-10T12:00:00"), "2026年11月10日");
  const utc = new Date("2026-03-05T08:00:00Z");
  assert.equal(releaseDate("2026-03-05T08:00:00Z"), `${utc.getFullYear()}年${utc.getMonth() + 1}月${utc.getDate()}日`);
  assert.equal(releaseDate(null), "");
  assert.equal(releaseDate("2026-11-10 8:00:00.0 +00:00:00"), "");
  assert.equal(releaseDate("昨天"), "");
});

test("新版本提示的标题：版本号；下面一行写现在的版本和发布日期，不知道的不写", () => {
  assert.deepEqual(updateHeadline(info, "0.1.0"), { title: "zsmeet 有新版本 0.2.0", detail: "现在用的是 0.1.0 · 2026年11月10日发布" });
  assert.deepEqual(updateHeadline(info, ""), { title: "zsmeet 有新版本 0.2.0", detail: "2026年11月10日发布" });
  assert.deepEqual(updateHeadline({ ...info, date: null }, "0.1.0"), { title: "zsmeet 有新版本 0.2.0", detail: "现在用的是 0.1.0" });
  assert.equal(updateHeadline({ ...info, date: null }, "").detail, "");
});

test("更新说明：换行留着，去掉首尾空白，连着的空行并成一个", () => {
  assert.equal(notesText("\n  - 录音更稳了\r\n- 修了上传的问题\n\n\n\n- 别的  \n"), "- 录音更稳了\n- 修了上传的问题\n\n- 别的");
  assert.equal(notesText("一行\n \n\t\n两行"), "一行\n\n两行");
  assert.equal(notesText("   "), "");
});

test("检查的结果、没装上：App 给的原因照着说", () => {
  assert.equal(upToDateText("0.1.0"), "已经是最新版本（0.1.0）。");
  assert.equal(upToDateText(""), "已经是最新版本。");
  assert.equal(checkFailedText("连不上 GitHub，请检查网络"), "没能检查更新：连不上 GitHub，请检查网络。");
  assert.equal(checkFailedText(" "), "没能检查更新，稍后再试一次。");
  assert.equal(installFailedText("新版本的签名对不上，没有安装。"), "没能更新：新版本的签名对不上，没有安装。");
  assert.equal(installFailedText(""), "没能更新，稍后再试一次。");
});

// ---- 能不能现在更新 ----

test("能更新：没在录音，录音都传完了（或者传不上去、中断了等人处理）", () => {
  assert.equal(updateBlocker(state()), "");
  assert.equal(updateBlocker(state({ uploads: [upload("completed"), upload("failed", "m2"), upload("interrupted", "m3")] })), "");
});

test("正在录音（包括正在开始、暂停着、正在结束）时不能更新，说为什么", () => {
  assert.equal(updateBlocker(state({ status: "recording", meetingId: "m1" })), "正在录音。更新要重启 zsmeet，会打断录音：结束录音、等录好的都传完以后再更新。");
  assert.match(updateBlocker(state({ status: "starting" })), /^正在开始录音。更新要重启 zsmeet，会打断录音/);
  assert.match(updateBlocker(state({ status: "paused", meetingId: "m1" })), /^录音已暂停。更新要重启 zsmeet，会打断录音/);
  assert.equal(updateBlocker(state({ status: "stopping", meetingId: "m1" })), "录音正在结束。更新要重启 zsmeet，等录好的都传完以后再更新。");
  // 在录音的时候，没传完的录音不用再说一遍
  assert.match(updateBlocker(state({ status: "recording", meetingId: "m1", uploads: [upload("recording"), upload("waiting", "m2")] })), /^正在录音。/);
});

test("还有录音没传完（边录边传、在传、正在交给服务器）时不能更新，说有几段", () => {
  for (const status of ["recording", "uploading", "completing"] as const) {
    assert.equal(updateBlocker(state({ uploads: [upload(status)] })), "还有 1 段录音没传完。更新要重启 zsmeet，传完以后再更新。", status);
  }
  const two = state({ uploads: [upload("uploading"), upload("completing", "m2"), upload("completed", "m3"), upload("interrupted", "m4")] });
  assert.equal(updateBlocker(two), "还有 2 段录音没传完。更新要重启 zsmeet，传完以后再更新。");
});

test("有录音暂时传不上去（断网、连不上服务器）时，说去看看网络和服务器", () => {
  assert.equal(updateBlocker(state({ uploads: [upload("waiting")] })), "还有 1 段录音暂时传不上去，在等着自动再试。请检查网络和服务器，传完以后再更新。");
  const mixed = state({ uploads: [upload("uploading"), upload("waiting", "m2"), upload("waiting", "m3"), upload("failed", "m4")] });
  assert.equal(updateBlocker(mixed), "还有 3 段录音没传完，其中 2 段暂时传不上去，在等着自动再试。请检查网络和服务器，传完以后再更新。");
});

// ---- 下载、安装 ----

test("下载进度：已下载多少、一共多大；下载完了说正在安装", () => {
  assert.equal(installView("idle", null), null);
  assert.equal(installView("failed", { downloaded: 10, total: 100 }), null);
  assert.deepEqual(installView("installing", null), { text: "正在准备下载新版本…", value: null });
  assert.deepEqual(installView("installing", { downloaded: 3 * 1024 * 1024, total: null }), { text: "正在下载新版本：已下载 3.0 MB", value: null });
  assert.deepEqual(installView("installing", { downloaded: 5 * 1024 * 1024, total: 20 * 1024 * 1024 }), {
    text: "正在下载新版本：5.0 MB / 20.0 MB",
    value: 0.25,
  });
  assert.deepEqual(installView("installing", { downloaded: 20 * 1024 * 1024, total: 20 * 1024 * 1024 }), {
    text: "下载好了，正在核对和安装，装好以后 zsmeet 会自动重新打开…",
    value: 1,
  });
  const installed = installView("installed", null);
  assert.equal(installed?.value, 1);
  assert.match(installed?.text ?? "", /^新版本装好了，zsmeet 正在重新打开。没有自己打开的话，退出 zsmeet/);
});

test("字节数写成 KB、MB", () => {
  assert.equal(size(0), "0 KB");
  assert.equal(size(512 * 1024), "512 KB");
  assert.equal(size(1024 * 1024), "1.0 MB");
  assert.equal(size(1.25 * 1024 * 1024 + 1), "1.3 MB");
});

test("正在更新时的录音页：还在下载时照样能开始录音（这次先不装），下载完在安装、装好了时不让开始", () => {
  assert.equal(recordUpdateNote("idle", null), null);
  assert.equal(recordUpdateNote("failed", { downloaded: 10, total: 10 }), null);
  // 还在下载（包括还没开始、不知道一共多大）：能开始录音，说清楚开始了的话这次不装
  for (const progress of [null, { downloaded: 5, total: 10 }, { downloaded: 5, total: null }]) {
    const note = recordUpdateNote("installing", progress);
    assert.equal(note?.blocks, false, JSON.stringify(progress));
    assert.match(note?.text ?? "", /^正在下载 zsmeet 的新版本。现在开始录音的话，这次先不装/);
  }
  // 下载完了：App 不让开始录音（会被重启打断），说法和 App 拒绝开始录音时一样
  assert.deepEqual(recordUpdateNote("installing", { downloaded: 10, total: 10 }), {
    text: "正在安装 zsmeet 的新版本，装好以后会自动重新打开，请稍等一下再开始录音。",
    blocks: true,
  });
  const installed = recordUpdateNote("installed", null);
  assert.equal(installed?.blocks, true);
  assert.match(installed?.text ?? "", /正在重新打开/);
});

// ---- 「以后再说」 ----

test("点过的「以后再说」：读得懂就用，读不懂的当成没点过", () => {
  assert.deepEqual(parseDismissed('{"version":"0.2.0","at":1700000000000}'), { version: "0.2.0", at: 1700000000000 });
  for (const bad of [null, "", "{", "[]", '"0.2.0"', '{"version":"","at":1}', '{"version":"0.2.0"}', '{"version":"0.2.0","at":"1"}']) {
    assert.equal(parseDismissed(bad), null, String(bad));
  }
});

test("页面上的新版本提示：有新版本就提示；点了「以后再说」，同一个版本一天以后再提示", () => {
  const now = 1_800_000_000_000;
  const seen = (dismissed: { version: string; at: number } | null, at = now, phase: "idle" | "installing" | "installed" | "failed" = "idle") =>
    noticeVisible({ available: info, dismissed, phase, now: at });
  assert.equal(noticeVisible({ available: null, dismissed: null, phase: "idle", now }), false);
  assert.equal(seen(null), true);
  const later = { version: "0.2.0", at: now };
  assert.equal(seen(later), false);
  assert.equal(seen(later, now + REMIND_AFTER_MS - 1), false);
  assert.equal(seen(later, now + REMIND_AFTER_MS), true);
  // 又出了更新的版本：马上提示
  assert.equal(seen({ version: "0.1.9", at: now }), true);
  // 正在更新、出了错：一直显示，要看进度、原因
  for (const phase of ["installing", "installed", "failed"] as const) assert.equal(seen(later, now, phase), true, phase);
  // 电脑的时间往回调了：照常提示，不会一直不提示
  assert.equal(seen(later, now - 60_000), true);
});

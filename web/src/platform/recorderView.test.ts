// 用 Node 自带的测试运行：pnpm -C web test
import assert from "node:assert/strict";
import { test } from "node:test";
import type { RecorderState, RecorderUpload } from "./recorder";
import {
  bannerUploads,
  DEFAULT_MIC,
  DEFAULT_SOURCES,
  discardConfirm,
  effectiveMic,
  endedUpload,
  IDLE_STATE,
  isBusy,
  isLive,
  liveMeetingText,
  liveUpload,
  localMeeting,
  micArg,
  micOptions,
  nextEnded,
  NO_MIC,
  normalizeState,
  parseSources,
  recordedLength,
  sentence,
  settledMeetings,
  signOutNote,
  sourceLabels,
  sourcesProblem,
  uploadView,
  warningText,
} from "./recorderView.ts";

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
    ...patch,
  };
}

function upload(patch: Partial<RecorderUpload> = {}): RecorderUpload {
  return { meetingId: "m1", title: "周会", recorded: 10, uploaded: 4, status: "uploading", error: null, ...patch };
}

const mics = [
  { uid: "built-in", name: "MacBook Pro 麦克风", default: true },
  { uid: "usb-1", name: "USB 麦克风", default: false },
];

test("Rust 推过来的状态缺了字段时用默认值补上", () => {
  assert.deepEqual(IDLE_STATE, state());
  assert.deepEqual(normalizeState(null), state());
  assert.deepEqual(normalizeState({ status: "recording", meetingId: "m1", seconds: 3, mic: "USB 麦克风" }), state({ status: "recording", meetingId: "m1", seconds: 3, mic: "USB 麦克风" }));
  const full = state({ status: "paused", meetingId: "m1", title: "周会", seconds: 9, level: { mic: 0.5, system: 0.1 }, system: true, marks: [1], uploads: [upload()] });
  assert.deepEqual(normalizeState(full), full);
});

// ---- 开始前 ----

test("在录：录音中和暂停着；没结束：除了 idle 都算", () => {
  assert.deepEqual(
    (["idle", "starting", "recording", "paused", "stopping"] as const).map((s) => [isLive(s), isBusy(s)]),
    [
      [false, false],
      [false, true],
      [true, true],
      [true, true],
      [false, true],
    ],
  );
});

test("上次选的声音来源：读得懂就用，读不懂、两样都不录的用默认的", () => {
  assert.deepEqual(parseSources(null), DEFAULT_SOURCES);
  assert.deepEqual(DEFAULT_SOURCES, { mic: DEFAULT_MIC, system: true });
  assert.deepEqual(parseSources('{"mic":"usb-1","system":false}'), { mic: "usb-1", system: false });
  assert.deepEqual(parseSources('{"mic":"none","system":true}'), { mic: NO_MIC, system: true });
  for (const bad of ["{", "[]", '"x"', '{"mic":"","system":true}', '{"mic":"usb-1","system":"yes"}', '{"mic":"none","system":false}']) {
    assert.deepEqual(parseSources(bad), DEFAULT_SOURCES, bad);
  }
});

test("麦克风的选项：系统默认的（写出现在是哪个）、每一个麦克风、不录麦克风", () => {
  assert.deepEqual(micOptions(mics), [
    { value: DEFAULT_MIC, label: "系统默认的麦克风（现在是 MacBook Pro 麦克风）" },
    { value: "built-in", label: "MacBook Pro 麦克风" },
    { value: "usb-1", label: "USB 麦克风" },
    { value: NO_MIC, label: "不录麦克风" },
  ]);
  assert.deepEqual(micOptions([]), [
    { value: DEFAULT_MIC, label: "系统默认的麦克风" },
    { value: NO_MIC, label: "不录麦克风" },
  ]);
});

test("选的麦克风拔掉了、还没列出来时用系统默认的；交给 App 时不录麦克风是 null", () => {
  assert.equal(effectiveMic("usb-1", mics), "usb-1");
  assert.equal(effectiveMic("usb-2", mics), DEFAULT_MIC);
  assert.equal(effectiveMic("usb-1", undefined), DEFAULT_MIC);
  assert.equal(effectiveMic(NO_MIC, undefined), NO_MIC);
  assert.equal(effectiveMic(DEFAULT_MIC, []), DEFAULT_MIC);
  assert.equal(micArg(NO_MIC), null);
  assert.equal(micArg(DEFAULT_MIC), "default");
  assert.equal(micArg("usb-1"), "usb-1");
});

test("麦克风和电脑里的声音至少要录一种", () => {
  assert.equal(sourcesProblem({ mic: NO_MIC, system: true }), "");
  assert.equal(sourcesProblem({ mic: DEFAULT_MIC, system: false }), "");
  assert.match(sourcesProblem({ mic: NO_MIC, system: false }), /至少要录一种/);
});

// ---- 录音中 ----

// 录音程序（desktop/recorder 的 RecordSession.swift）给的说明，原样拿来测
const MIC_DENIED = "没有麦克风权限，录不到你这边的声音。到「系统设置 → 隐私与安全性 → 麦克风」里打开 zsmeet，再重新开始录音。";
const SYSTEM_DENIED =
  "没有录电脑声音的权限，录不到会议软件里其他人的声音。到「系统设置 → 隐私与安全性 → 屏幕与系统录音」，在「仅系统录音」里打开 zsmeet，再重新开始录音。";
const MIC_GONE = "麦克风「USB 麦克风」断开了，后面录不到你这边的声音。接好以后结束这次录音，再重新开始。";

test("没有权限的提示说清楚去哪里打开", () => {
  // 录音程序的说明已经说了去哪里打开：照着显示，不重复
  assert.equal(warningText({ code: "mic_denied", message: MIC_DENIED }), MIC_DENIED);
  assert.equal(warningText({ code: "system_denied", message: SYSTEM_DENIED }), SYSTEM_DENIED);
  // 说明里没说去哪里打开的，补上
  const mic = warningText({ code: "mic_denied", message: "没有麦克风权限" });
  assert.match(mic, /^没有麦克风权限。打开方法：「系统设置 → 隐私与安全性 → 麦克风」，把 zsmeet 打开。/);
  const system = warningText({ code: "system_denied", message: "" });
  assert.match(system, /^zsmeet 没有录制电脑声音的权限/);
  assert.match(system, /系统设置 → 隐私与安全性 → 屏幕与系统录音/);
  assert.match(system, /「仅系统录音」里把 zsmeet 打开/);
});

test("设备的提示：录音程序说了怎么办就照着显示，没说时用自己的", () => {
  assert.equal(warningText({ code: "mic_unavailable", message: MIC_GONE }), MIC_GONE);
  assert.match(warningText({ code: "mic_unavailable", message: "" }), /^麦克风用不了.*换一个麦克风/);
  assert.match(warningText({ code: "system_unavailable", message: "" }), /^录不到电脑里的声音。退出 zsmeet 再打开试试/);
  // 不认识的提示原样显示
  assert.equal(warningText({ code: "disk_full", message: "磁盘快满了" }), "磁盘快满了");
  assert.equal(warningText({ code: "disk_full", message: " " }), "录音出了点问题。");
});

test("录音中：麦克风写名字，想录没录到的说「录不到」", () => {
  assert.deepEqual(sourceLabels(state({ status: "recording", mic: "USB 麦克风", system: true })), { mic: "USB 麦克风", system: "正在录" });
  assert.deepEqual(sourceLabels(state({ status: "recording", mic: null, system: true })), { mic: "不录", system: "正在录" });
  assert.deepEqual(
    sourceLabels(state({ status: "recording", mic: "USB 麦克风", warnings: [{ code: "system_denied", message: "" }] })),
    { mic: "USB 麦克风", system: "录不到（见下面的提示）" },
  );
  assert.equal(sourceLabels(state({ warnings: [{ code: "mic_unavailable", message: "" }] })).mic, "录不到（见下面的提示）");
});

// App（desktop/src-tauri 的 recorder）给的几种原因，原样拿来测
const OFFLINE = "连不上服务器，网络恢复后会自动接着传";
const RELOGIN = "登录已失效，重新登录这个账号后会自动接着传";
const CRASHED = "录音程序意外退出了。已经录好的部分都在，可以接着录，也可以直接结束、把录好的传上去";
const MEETING_GONE = "服务器上找不到这场会议了（可能已经被删除），录音传不上去了";

test("录音中的上传情况：边录边传；传不上去时说录音没停，什么时候接着传照 App 说的", () => {
  assert.deepEqual(liveUpload(undefined), { text: "已上传 0 片 · 网络正常，边录边传", trouble: "", tone: "warn" });
  assert.deepEqual(liveUpload(upload({ status: "recording", recorded: 6, uploaded: 6 })), {
    text: "已上传 6 片 · 网络正常，边录边传",
    trouble: "",
    tone: "warn",
  });
  const offline = liveUpload(upload({ status: "recording", recorded: 9, uploaded: 6, error: OFFLINE }));
  assert.equal(offline.text, "已上传 6 片 · 待上传 3 片 · 暂时传不上去");
  assert.equal(offline.trouble, "暂时传不上去：连不上服务器，网络恢复后会自动接着传。录音没有停，分片先存在这台电脑上。");
  // 要重新登录时不能说「不用做任何操作」
  assert.doesNotMatch(liveUpload(upload({ status: "waiting", error: RELOGIN })).trouble, /不用做任何操作/);
  assert.match(liveUpload(upload({ status: "waiting", error: null })).trouble, /^暂时传不上去。录音没有停.*恢复后自动补传，不用做任何操作。$/);
});

test("录音中就传不上去了（会议被删了）：不说「暂时」，建议结束", () => {
  const gone = liveUpload(upload({ status: "failed", recorded: 9, uploaded: 6, error: MEETING_GONE }));
  assert.equal(gone.text, "已上传 6 片 · 待上传 3 片 · 传不上去了");
  assert.equal(gone.tone, "bad");
  assert.equal(gone.trouble, `${MEETING_GONE}。现在录的也传不上去，建议结束这次录音，新建一场会议重新录。`);
  assert.doesNotMatch(gone.trouble, /暂时/);
});

test("会议页面上：这场会议正在这台电脑上录", () => {
  assert.match(liveMeetingText("recording", "00:12:34"), /正在这台电脑上录音，已经录了 00:12:34/);
  assert.match(liveMeetingText("paused", "00:12:34"), /暂停着（已经录了 00:12:34）/);
  assert.match(liveMeetingText("stopping", "00:12:34"), /正在结束/);
});

// ---- 结束以后 ----

test("录音页记下刚结束的那一场；又开始录就不再显示；开始失败不算", () => {
  const live = state({ status: "recording", meetingId: "m1", title: "周会", seconds: 125.4, marks: [3, 60] });
  const idle = state();
  const ended = { meetingId: "m1", title: "周会", seconds: 125.4, marks: 2 };
  assert.deepEqual(nextEnded(live, idle, null), ended);
  assert.deepEqual(nextEnded({ ...live, status: "paused" }, idle, null), ended);
  assert.deepEqual(nextEnded({ ...live, status: "stopping" }, idle, null), ended);
  // 结束中、空闲时的其他变化（上传进度）不影响
  assert.equal(nextEnded(live, { ...live, status: "stopping" }, null), null);
  assert.deepEqual(nextEnded(idle, state({ uploads: [upload()] }), ended), ended);
  // 又开始了
  assert.equal(nextEnded(idle, state({ status: "starting", meetingId: "m2" }), ended), null);
  assert.equal(nextEnded(idle, state({ status: "recording", meetingId: "m2" }), ended), null);
  // 没能开始：starting 直接回到 idle
  assert.equal(nextEnded(state({ status: "starting", meetingId: "m2" }), idle, null), null);
});

test("刚结束的那一场不在上传列表里了，就是已经传完了", () => {
  const ended = { meetingId: "m1", title: "周会", seconds: 60, marks: 0 };
  assert.deepEqual(endedUpload(state({ uploads: [upload()] }), ended), upload());
  assert.deepEqual(endedUpload(state({ uploads: [upload({ meetingId: "m2" })] }), ended), {
    meetingId: "m1",
    title: "周会",
    recorded: 0,
    uploaded: 0,
    status: "completed",
    error: null,
  });
});

test("传完了、被删掉了的录音要重新取会议", () => {
  const prev = state({
    uploads: [
      upload({ meetingId: "a", status: "uploading" }),
      upload({ meetingId: "b", status: "completing" }),
      upload({ meetingId: "c", status: "completed" }),
      upload({ meetingId: "d", status: "interrupted" }),
      upload({ meetingId: "e", status: "waiting" }),
    ],
  });
  const next = state({
    uploads: [upload({ meetingId: "a", status: "uploading", uploaded: 5 }), upload({ meetingId: "b", status: "completed" }), upload({ meetingId: "e", status: "waiting" })],
  });
  // b 刚传完；c 本来就传完了，不再算；d 被删掉了
  assert.deepEqual(settledMeetings(prev, next), ["b", "d"]);
  assert.deepEqual(settledMeetings(next, next), []);
});

test("上传中、等网络：进度和还剩几片", () => {
  assert.deepEqual(uploadView(upload(), { where: "banner", canStart: true }), {
    tone: "info",
    text: "「周会」的录音正在上传：已传 4 / 10 片。",
    progress: 0.4,
    spinning: false,
    actions: [],
  });
  const waiting = uploadView(upload({ status: "waiting", error: OFFLINE }), { where: "meeting", canStart: true });
  assert.equal(waiting.tone, "warn");
  assert.equal(waiting.progress, 0.4);
  assert.equal(waiting.text, "这台电脑上的录音还有 6 片没传上去：连不上服务器，网络恢复后会自动接着传。");
  assert.equal(
    uploadView(upload({ status: "waiting", error: RELOGIN }), { where: "banner", canStart: true }).text,
    "「周会」的录音还有 6 片没传上去：登录已失效，重新登录这个账号后会自动接着传。",
  );
  assert.equal(uploadView(upload({ status: "waiting", error: null }), { where: "record", canStart: true }).text, "录音还有 6 片没传上去，连上服务器以后自动接着传。");
  // 都传完了，只差告诉服务器开始处理
  assert.equal(
    uploadView(upload({ status: "waiting", uploaded: 10 }), { where: "record", canStart: true }).text,
    "录音已经传完，连上服务器以后自动开始处理。",
  );
  assert.equal(
    uploadView(upload({ status: "waiting", uploaded: 10, error: OFFLINE }), { where: "record", canStart: true }).text,
    "录音已经传完，还没能交给服务器处理：连不上服务器，网络恢复后会自动接着传。",
  );
  const completing = uploadView(upload({ status: "completing", uploaded: 10 }), { where: "record", canStart: true });
  assert.equal(completing.spinning, true);
  assert.equal(completing.progress, null);
});

test("传完了：只有页面顶部的提示条里有「查看」「知道了」", () => {
  const done = upload({ status: "completed", uploaded: 10 });
  assert.deepEqual(uploadView(done, { where: "banner", canStart: true }).actions, ["view", "dismiss"]);
  assert.equal(uploadView(done, { where: "banner", canStart: true }).tone, "ok");
  assert.deepEqual(uploadView(done, { where: "record", canStart: true }).actions, []);
  assert.deepEqual(uploadView(done, { where: "meeting", canStart: true }).actions, []);
});

test("别处给的说明补成完整的一句，好接着往下说", () => {
  assert.equal(sentence("连不上服务器 https://meet.example.com，请检查网络"), "连不上服务器 https://meet.example.com，请检查网络。");
  assert.equal(sentence(" 已经在录音了。 "), "已经在录音了。");
  assert.equal(sentence("录音程序出错了."), "录音程序出错了。");
  assert.equal(sentence("真的要删吗？"), "真的要删吗？");
});

test("录下的片数说成大概多长时间（每片约 10 秒）", () => {
  assert.equal(recordedLength(0), "不到 1 分钟");
  assert.equal(recordedLength(2), "不到 1 分钟");
  assert.equal(recordedLength(3), "大约 1 分钟");
  assert.equal(recordedLength(12), "大约 2 分钟");
  assert.equal(recordedLength(360), "大约 1 小时");
  assert.equal(recordedLength(369), "大约 1 小时 2 分钟");
});

test("中断了：可以接着录（没在录别的时候）、把录下的部分传上去，或者删掉", () => {
  const broken = upload({ status: "interrupted", recorded: 12, uploaded: 3, error: CRASHED });
  const view = uploadView(broken, { where: "banner", canStart: true });
  assert.equal(view.tone, "warn");
  assert.deepEqual(view.actions, ["continue", "finish", "discard"]);
  assert.equal(
    view.text,
    "「周会」的录音录到一半中断了，已经录下大约 2 分钟。录音程序意外退出了。已经录好的部分都在，可以接着录，也可以直接结束、把录好的传上去。",
  );
  // 正在录别的：先不能接着录
  const busy = uploadView(broken, { where: "banner", canStart: false });
  assert.deepEqual(busy.actions, ["finish", "discard"]);
  assert.match(busy.text, /要接着录的话，等现在这一场录完。$/);
  // 不知道原因
  assert.equal(
    uploadView({ ...broken, error: null }, { where: "meeting", canStart: true }).text,
    "这台电脑上的录音录到一半中断了（zsmeet 退出了，或者录音出了错），已经录下大约 2 分钟。可以接着录，也可以把录下的部分传上去生成纪要。",
  );
  // 什么也没录下来：没有可以传的，原因也不要紧
  const empty = uploadView(upload({ status: "interrupted", recorded: 0, uploaded: 0, error: "什么也没有录下来。" }), { where: "banner", canStart: true });
  assert.deepEqual(empty.actions, ["continue", "discard"]);
  assert.equal(empty.text, "「周会」的录音刚开始就中断了，什么也没有录下来。");
});

test("传不上去了：说原因，只能删掉", () => {
  const failed = uploadView(upload({ status: "failed", error: MEETING_GONE }), { where: "banner", canStart: true });
  assert.deepEqual(failed, {
    tone: "bad",
    text: "「周会」的录音：服务器上找不到这场会议了（可能已经被删除），录音传不上去了。",
    progress: null,
    spinning: false,
    actions: ["discard"],
  });
  assert.match(uploadView(upload({ status: "failed" }), { where: "meeting", canStart: true }).text, /^这台电脑上的录音传不上去了：.*没有权限。$/);
});

test("删除前的确认：中断了的录音说清楚不会生成纪要", () => {
  assert.match(discardConfirm(upload({ status: "interrupted", recorded: 3 })), /不会生成纪要/);
  assert.match(discardConfirm(upload({ status: "failed" })), /不能恢复/);
});

test("页面顶部的提示条：不含正在录的那一场、当前页面自己显示的、点过「知道了」的", () => {
  const s = state({
    status: "recording",
    meetingId: "live",
    uploads: [
      upload({ meetingId: "live", status: "recording" }),
      upload({ meetingId: "a" }),
      upload({ meetingId: "b", status: "interrupted" }),
      upload({ meetingId: "c", status: "completed" }),
    ],
  });
  const ids = (list: RecorderUpload[]) => list.map((u) => u.meetingId);
  assert.deepEqual(ids(bannerUploads(s, new Set(), new Set())), ["a", "b", "c"]);
  assert.deepEqual(ids(bannerUploads(s, new Set(["b"]), new Set(["c"]))), ["a"]);
  // 刚结束、还在传的那一场要显示出来
  const after = state({ uploads: [upload({ meetingId: "live", status: "uploading" })] });
  assert.deepEqual(ids(bannerUploads(after, new Set(), new Set())), ["live"]);
});

test("退出登录、切换服务器前：正在录的会先结束，没传完的留着等这个账号回来", () => {
  assert.equal(signOutNote(state(), "退出登录"), "");
  // 传完了、传不上去的不用提
  assert.equal(signOutNote(state({ uploads: [upload({ status: "completed" }), upload({ meetingId: "x", status: "failed" })] }), "退出登录"), "");
  assert.equal(
    signOutNote(state({ status: "recording", meetingId: "m1", uploads: [upload({ status: "recording" })] }), "退出登录"),
    "正在录音，退出登录会先结束这场录音。录好的部分留在这台电脑上，以后用这个账号登录这台服务器时接着上传。",
  );
  const busy = state({
    status: "paused",
    meetingId: "m1",
    uploads: [upload({ status: "recording" }), upload({ meetingId: "a", status: "waiting" }), upload({ meetingId: "b", status: "interrupted" })],
  });
  assert.match(signOutNote(busy, "切换服务器"), /^正在录音，切换服务器会先结束这场录音。这一场和另外 2 段没传完的录音留在这台电脑上/);
  assert.equal(
    signOutNote(state({ uploads: [upload({ meetingId: "a", status: "uploading" })] }), "退出登录"),
    "这台电脑上还有 1 段录音没传完，退出登录以后先不传，以后用这个账号登录这台服务器时接着上传。",
  );
});

test("会议页面：这台电脑正在录它、在传它，或者跟它没关系", () => {
  const s = state({ status: "paused", meetingId: "m1", seconds: 42, uploads: [upload({ meetingId: "m1", status: "recording" }), upload({ meetingId: "m2" })] });
  assert.deepEqual(localMeeting(s, "m1"), { kind: "live", status: "paused", seconds: 42 });
  assert.deepEqual(localMeeting(s, "m2"), { kind: "upload", upload: upload({ meetingId: "m2" }) });
  assert.equal(localMeeting(s, "m3"), null);
  // 已经结束了：看上传情况
  const after = state({ meetingId: "m1", uploads: [upload({ meetingId: "m1", status: "completing" })] });
  assert.deepEqual(localMeeting(after, "m1"), { kind: "upload", upload: upload({ meetingId: "m1", status: "completing" }) });
});

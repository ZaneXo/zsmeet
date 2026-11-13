// 用 Node 自带的测试运行：pnpm -C web test
import assert from "node:assert/strict";
import { test } from "node:test";
import type { RecorderState, RecorderUpload } from "./recorder";
import {
  bannerUploads,
  DEFAULT_MIC,
  DEFAULT_SOURCES,
  discardConfirm,
  ECHO_CANCEL_DEFAULT,
  ECHO_CANCEL_OFF,
  ECHO_CANCEL_ON,
  echoCancelArg,
  echoCancelUsable,
  echoLabel,
  effectiveMic,
  endedGone,
  endedUpload,
  IDLE_STATE,
  isBusy,
  isLive,
  levelToDb,
  liveMeetingText,
  liveUpload,
  localMeeting,
  micArg,
  micGainLabel,
  micOptions,
  micTooQuiet,
  nextEnded,
  NO_MIC,
  NO_MIC_LEVELS,
  normalizeState,
  parseEchoCancel,
  parseSources,
  QUIET_MIC_WINDOW,
  quietMicHint,
  recordedLength,
  sentence,
  settledMeetings,
  signOutNote,
  sourceLabels,
  sourcesProblem,
  stuckHere,
  trackMicLevels,
  uploadView,
  warningText,
} from "./recorderView.ts";
import type { MicLevels } from "./recorderView.ts";

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
    micGain: null,
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
  const full = state({
    status: "paused",
    meetingId: "m1",
    title: "周会",
    seconds: 9,
    level: { mic: 0.5, system: 0.1 },
    system: true,
    marks: [1],
    uploads: [upload()],
    echo: { mode: "auto", active: true, reason: "speakers" },
    micGain: "auto",
  });
  assert.deepEqual(normalizeState(full), full);
});

test("麦克风自动增益的模式：没报、认不出来的当成没报", () => {
  assert.equal(normalizeState({ status: "recording" }).micGain, null);
  for (const mode of ["off", "auto", "unavailable"] as const) {
    assert.equal(normalizeState({ micGain: mode }).micGain, mode);
  }
  for (const raw of ["loud", "", 3, { mode: "auto" }]) {
    assert.equal(normalizeState({ micGain: raw as never }).micGain, null, String(raw));
  }
});

test("回声消除的情况：没报、报得不全时不出错", () => {
  // 旧版本的 App 没有这个字段
  assert.equal(normalizeState({ status: "recording" }).echo, null);
  assert.equal(normalizeState({ status: "recording", echo: null }).echo, null);
  // 没有模式、模式不认识的当成没报（不显示，免得说错）；缺了 active、reason 的当成没开、不知道为什么
  assert.equal(normalizeState({ echo: { active: true, reason: "speakers" } as never }).echo, null);
  assert.equal(normalizeState({ echo: { mode: "speakers_only", active: true, reason: "speakers" } as never }).echo, null);
  assert.equal(normalizeState({ echo: { mode: "" } as never }).echo, null);
  assert.deepEqual(normalizeState({ echo: { mode: "auto" } as never }).echo, { mode: "auto", active: false, reason: null });
  assert.deepEqual(normalizeState({ echo: { mode: "on", active: true, reason: "" } }).echo, { mode: "on", active: true, reason: null });
  for (const mode of ["off", "auto", "on", "unavailable"] as const) {
    assert.deepEqual(normalizeState({ echo: { mode, active: false, reason: "off" } }).echo, { mode, active: false, reason: "off" }, mode);
  }
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

test("外放时去掉回声（实验）：默认关；点过的照点的来，读不懂的用默认的", () => {
  assert.equal(ECHO_CANCEL_DEFAULT, false);
  assert.equal(parseEchoCancel(null), false);
  assert.equal(parseEchoCancel(ECHO_CANCEL_ON), true);
  assert.equal(parseEchoCancel(ECHO_CANCEL_OFF), false);
  for (const bad of ["", "true", "1", "ON", '{"on":true}']) assert.equal(parseEchoCancel(bad), ECHO_CANCEL_DEFAULT, bad);
});

test("外放时去掉回声：麦克风和电脑里的声音都录才用得上，交给 App 的也照这个来", () => {
  const both = { mic: DEFAULT_MIC, system: true };
  assert.equal(echoCancelUsable(both), true);
  assert.equal(echoCancelUsable({ mic: "usb-1", system: true }), true);
  assert.equal(echoCancelUsable({ mic: NO_MIC, system: true }), false);
  assert.equal(echoCancelUsable({ mic: DEFAULT_MIC, system: false }), false);
  assert.equal(echoCancelArg(both, true), true);
  assert.equal(echoCancelArg(both, false), false);
  // 打开了，但只录一样：不处理
  assert.equal(echoCancelArg({ mic: NO_MIC, system: true }, true), false);
  assert.equal(echoCancelArg({ mic: DEFAULT_MIC, system: false }, true), false);
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

test("录音中在录的这一路有提示（断开了、一直没有声音、改用了别的麦克风）：照旧写名字，指向下面的提示", () => {
  // 录着录着麦克风断开了：Rust 那边还报着它的名字，不能看起来一切正常
  const lost = state({ status: "recording", mic: "USB 麦克风", system: true, warnings: [{ code: "mic_unavailable", message: MIC_GONE }] });
  assert.deepEqual(sourceLabels(lost), { mic: "USB 麦克风（见下面的提示）", system: "正在录" });
  // 没找到选的麦克风、改用了别的：也是 mic_unavailable，但照常在录，不能说「录不到」
  const fallback = "没找到选的麦克风（可能已经拔掉了），改用「MacBook Pro 麦克风」。";
  const switched = state({ status: "recording", mic: "MacBook Pro 麦克风", warnings: [{ code: "mic_unavailable", message: fallback }] });
  assert.equal(sourceLabels(switched).mic, "MacBook Pro 麦克风（见下面的提示）");
  // 电脑里的声音也一样
  const system = state({ status: "recording", system: true, warnings: [{ code: "system_unavailable", message: "" }] });
  assert.equal(sourceLabels(system).system, "正在录（见下面的提示）");
});

test("系统默认的麦克风换了（mic_changed）：麦克风的名字跟着 App 报的换", () => {
  const before = state({ status: "recording", meetingId: "m1", mic: "MacBook Pro 麦克风", system: true });
  // App 收到 mic_changed 以后推过来的状态里 mic 换成了新的名字
  const after = normalizeState({ ...before, mic: "AirPods Pro" });
  assert.equal(sourceLabels(before).mic, "MacBook Pro 麦克风");
  assert.equal(sourceLabels(after).mic, "AirPods Pro");
});

// 录音程序给的说明，原样拿来测
const MIC_SILENT = "麦克风已经 30 秒没有声音了，可能被会议软件占用了。看看会议软件里选的麦克风，或者戴上耳机再说话试试。";
const ECHO_FAILED = "回声消除没能打开，这次录音里外放的会议声音可能会录两遍。戴耳机可以避免。";

test("麦克风一直没有声音（mic_silent）：照录音程序说的显示，麦克风的名字后面指向提示", () => {
  assert.equal(warningText({ code: "mic_silent", message: MIC_SILENT }), MIC_SILENT);
  // 没给说明时用自己的：说清楚可能的原因和怎么办
  const own = warningText({ code: "mic_silent", message: "" });
  assert.match(own, /^麦克风很长时间都没有声音/);
  assert.match(own, /会议软件/);
  assert.match(own, /换一个麦克风/);
  const silent = state({ status: "recording", mic: "MacBook Pro 麦克风", system: true, warnings: [{ code: "mic_silent", message: MIC_SILENT }] });
  assert.deepEqual(sourceLabels(silent), { mic: "MacBook Pro 麦克风（见下面的提示）", system: "正在录" });
});

test("回声消除没能打开（echo_unavailable）：说外放的声音可能录两遍、戴耳机可以避免，不算哪一路录不到", () => {
  assert.equal(warningText({ code: "echo_unavailable", message: ECHO_FAILED }), ECHO_FAILED);
  assert.equal(warningText({ code: "echo_unavailable", message: "" }), ECHO_FAILED);
  const s = state({ status: "recording", mic: "MacBook Pro 麦克风", system: true, warnings: [{ code: "echo_unavailable", message: ECHO_FAILED }] });
  assert.deepEqual(sourceLabels(s), { mic: "MacBook Pro 麦克风", system: "正在录" });
});

test("录音中的「回声消除：…」：开着、没开（为什么）、不可用", () => {
  const live = (echo: RecorderState["echo"], patch: Partial<RecorderState> = {}) =>
    echoLabel(state({ status: "recording", meetingId: "m1", mic: "MacBook Pro 麦克风", system: true, echo, ...patch }));
  // 外放，开着
  assert.deepEqual(live({ mode: "auto", active: true, reason: "speakers" }), { text: "回声消除：开", tone: "ok" });
  assert.deepEqual(live({ mode: "on", active: true, reason: "forced" }), { text: "回声消除：开", tone: "ok" });
  assert.deepEqual(live({ mode: "auto", active: true, reason: null }), { text: "回声消除：开", tone: "ok" });
  // 自动不处理的几种
  assert.deepEqual(live({ mode: "auto", active: false, reason: "headphones" }), { text: "回声消除：没开（戴着耳机，没有回声）", tone: "mute" });
  assert.deepEqual(live({ mode: "auto", active: false, reason: "airplay" }), { text: "回声消除：没开（用 AirPlay 外放时处理不了）", tone: "mute" });
  // 电脑里的声音录着录着断了：等它恢复；开始时就录不到：说录不到
  assert.deepEqual(live({ mode: "auto", active: false, reason: "no_reference" }), { text: "回声消除：暂时没开（没收到电脑里的声音）", tone: "mute" });
  assert.deepEqual(live({ mode: "auto", active: false, reason: "no_reference" }, { system: false }), { text: "回声消除：没开（录不到电脑里的声音）", tone: "mute" });
  // 不认识的原因：只说没开
  assert.deepEqual(live({ mode: "auto", active: false, reason: "bluetooth_speaker" }), { text: "回声消除：没开", tone: "mute" });
  // 录音程序还没说现在开没开（App 这时报的 active 是 false）：说选的是什么，不说「没开」
  assert.deepEqual(live({ mode: "auto", active: false, reason: null }), { text: "回声消除：外放时自动处理", tone: "mute" });
  // 一会儿外放、一会儿戴耳机：跟着录音程序报的变
  assert.equal(live({ mode: "auto", active: false, reason: "headphones" })?.text, "回声消除：没开（戴着耳机，没有回声）");
  assert.equal(live({ mode: "auto", active: true, reason: "speakers" })?.text, "回声消除：开");
  // 要开但没能打开：录的是原来的声音，提醒外放的声音可能录两遍
  const unavailable = live({ mode: "unavailable", active: false, reason: null });
  assert.equal(unavailable?.tone, "warn");
  assert.match(unavailable?.text ?? "", /^回声消除：不可用（.*录两遍.*戴耳机可以避免）$/);
});

test("没打开这个选项、不录麦克风、App 没报时，不显示回声消除", () => {
  assert.equal(echoLabel(state({ status: "recording", mic: "MacBook Pro 麦克风", system: true, echo: null })), null);
  assert.equal(echoLabel(state({ status: "recording", mic: "MacBook Pro 麦克风", system: true, echo: { mode: "off", active: false, reason: "off" } })), null);
  // 麦克风开始时就录不到：没有要去掉回声的声音
  assert.equal(echoLabel(state({ status: "recording", mic: null, system: true, echo: { mode: "auto", active: false, reason: "no_reference" } })), null);
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
  const live = state({ status: "recording", meetingId: "m1", title: "周会", seconds: 125.4, marks: [3, 60], uploads: [upload({ status: "recording" })] });
  const idle = state({ uploads: [upload()] });
  const ended = { meetingId: "m1", title: "周会", seconds: 125.4, marks: 2, empty: false };
  assert.deepEqual(nextEnded(live, idle, null), ended);
  assert.deepEqual(nextEnded({ ...live, status: "paused" }, idle, null), ended);
  assert.deepEqual(nextEnded({ ...live, status: "stopping" }, idle, null), ended);
  // 结束中、空闲时的其他变化（上传进度）不影响
  assert.equal(nextEnded(live, { ...live, status: "stopping" }, null), null);
  assert.deepEqual(nextEnded(idle, state({ uploads: [upload({ uploaded: 5 })] }), ended), ended);
  // 又开始了
  assert.equal(nextEnded(idle, state({ status: "starting", meetingId: "m2" }), ended), null);
  assert.equal(nextEnded(idle, state({ status: "recording", meetingId: "m2" }), ended), null);
  // 没能开始：starting 直接回到 idle
  assert.equal(nextEnded(state({ status: "starting", meetingId: "m2" }), state(), null), null);
});

test("别的账号的录音结束了，不记成自己刚结束的那一场", () => {
  // 上一个人的登录失效以后录音照常，换了人登录：它不在这个人的上传列表里
  const theirs = state({ status: "stopping", meetingId: "m1", title: "别人的会", seconds: 600, marks: [5] });
  assert.equal(nextEnded(theirs, state(), null), null);
  // 结束之前就不给看是哪场会议了
  assert.equal(nextEnded(state({ status: "stopping" }), state(), null), null);
});

test("结束时 App 把它删了、说了原因：一片都没录下来", () => {
  const live = state({ status: "recording", meetingId: "m1", title: "周会", seconds: 3, uploads: [upload({ status: "recording", recorded: 0 })] });
  const nothing = state({ error: "什么也没有录下来。" });
  assert.deepEqual(nextEnded(live, nothing, null), { meetingId: "m1", title: "周会", seconds: 3, marks: 0, empty: true });
  // 不在列表里了，但 App 没说为什么（同时换了账号）：不当成空的，不删服务器上的会议
  assert.equal(nextEnded(live, state(), null)?.empty, false);
});

test("刚结束的那一场：传完了也还在列表里；不在了就说清楚为什么，不能说成已经传完", () => {
  const ended = { meetingId: "m1", title: "周会", seconds: 60, marks: 0, empty: false };
  assert.deepEqual(endedUpload(state({ uploads: [upload()] }), ended), upload());
  assert.deepEqual(endedUpload(state({ uploads: [upload({ status: "completed", uploaded: 10 })] }), ended), upload({ status: "completed", uploaded: 10 }));
  assert.equal(endedUpload(state({ uploads: [upload({ meetingId: "m2" })] }), ended), null);
  // 被删掉了（中断了、传不上去的删了）
  const deleted = endedGone(ended);
  assert.equal(deleted.title, "录音已删除");
  assert.match(deleted.text, /已经删掉了，不会再上传，这场会议也不会生成纪要/);
  // 什么也没录下来
  const empty = endedGone({ ...ended, empty: true });
  assert.equal(empty.title, "什么也没有录下来");
  assert.match(empty.text, /什么也没有录下来，不会生成纪要。可以再录一场。$/);
  for (const gone of [deleted, empty]) assert.doesNotMatch(gone.text, /传上去|转写/);
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

// App 给的原因（desktop/src-tauri/src/recorder/mod.rs 的 taken_on_server）
const TAKEN =
  "服务器上已经有这场会议的第 3 片录音，这台电脑上却没有记录；断网时接着录的部分也排到了这里，传上去会把服务器上的录音覆盖掉，所以没有传，录下的都还留在这台电脑上。可以在会议页面上点「用已经收到的部分生成纪要」，先用服务器上的录音生成纪要（服务器 90 秒没再收到录音以后才有这个按钮）";

test("会议页面：这台电脑上的录音传不上去了，还能用服务器已经收到的部分生成纪要，会议不会一直结束不了", () => {
  const here = (s: RecorderState) => {
    const local = localMeeting(s, "m1");
    assert.ok(local);
    return stuckHere(local);
  };
  // 断网时接着录的分片和服务器上已有的重了：这台电脑上的传不上去，删掉是唯一的按钮，会议页面上要能用服务器上的部分结束
  assert.equal(here(state({ uploads: [upload({ status: "failed", error: TAKEN })] })), true);
  assert.deepEqual(uploadView(upload({ status: "failed", error: TAKEN }), { where: "meeting", canStart: true }).actions, ["discard"]);
  // 在传、等网络、中断了等着选、传完了：这台电脑自己会传完（或者能直接结束），不给
  for (const status of ["recording", "uploading", "waiting", "completing", "completed", "interrupted"] as const) {
    assert.equal(here(state({ uploads: [upload({ status })] })), false, status);
  }
  // 还在录（录音时就传不上去了的也一样）：先结束录音
  assert.equal(here(state({ status: "recording", meetingId: "m1", uploads: [upload({ status: "failed" })] })), false);
});

// ---- 麦克风自动增益、麦克风的声音一直很小 ----

test("录音中：自动调音量没能打开时在麦克风后面说一声，在调、不调、不录麦克风时不说", () => {
  const live = { status: "recording" as const, mic: "MacBook Pro 麦克风" };
  assert.equal(micGainLabel(state({ ...live, micGain: "unavailable" })), "自动调音量没能打开，录的是原来的音量");
  for (const micGain of ["auto", "off", null] as const) {
    assert.equal(micGainLabel(state({ ...live, micGain })), null, String(micGain));
  }
  assert.equal(micGainLabel(state({ ...live, mic: null, micGain: "unavailable" })), null);
});

test("音量（0 到 1）换成 dBFS：0 是 -60 及以下，1 是 0 dBFS", () => {
  assert.equal(levelToDb(0), -60);
  assert.equal(levelToDb(1), 0);
  assert.equal(levelToDb(0.5), -30);
  assert.equal(levelToDb(-1), -60);
  assert.equal(levelToDb(2), 0);
});

/** 在录的状态：录到第 seconds 秒，麦克风的音量是 db dBFS */
function recording(seconds: number, db: number, patch: Partial<RecorderState> = {}): RecorderState {
  return state({ status: "recording", meetingId: "m1", mic: "MacBook Pro 麦克风", seconds, level: { mic: (db + 60) / 60, system: 0 }, ...patch });
}

/** 从 from 秒到 to 秒，每 0.1 秒一个音量（level(第几秒) 给 dBFS） */
function feed(levels: MicLevels, from: number, to: number, level: (t: number) => number, patch: Partial<RecorderState> = {}): MicLevels {
  let current = levels;
  for (let i = Math.round(from * 10); i <= Math.round(to * 10); i++) {
    const t = i / 10;
    current = trackMicLevels(current, recording(t, level(t), patch));
  }
  return current;
}

/** 小声说话：说的时候 -45 dBFS，每秒停 0.4 秒（只有底噪，-62 dBFS） */
const quietSpeech = (t: number) => (t % 1 < 0.6 ? -45 : -62);

test("麦克风的声音一直很小：有人在说话（时大时小）、20 秒里最响的也不到 -35 dBFS 时提示", () => {
  const quiet = feed(NO_MIC_LEVELS, 0, 25, quietSpeech);
  assert.equal(micTooQuiet(quiet), true);
  // 只留最近 20 秒
  assert.ok(quiet.samples.length <= QUIET_MIC_WINDOW * 10 + 1, String(quiet.samples.length));
  assert.ok(quiet.samples[0]!.at > 25 - QUIET_MIC_WINDOW);
  // 还没记满 20 秒：先不说
  assert.equal(micTooQuiet(feed(NO_MIC_LEVELS, 0, 15, quietSpeech)), false);
  // 声音正常（调大以后说话在 -20 dBFS 左右）：不提示
  assert.equal(micTooQuiet(feed(NO_MIC_LEVELS, 0, 25, (t) => (t % 1 < 0.6 ? -20 : -50))), false);
  // 一直差不多大（只有底噪、没人说话，或者戴着耳机在听）：不提示
  assert.equal(micTooQuiet(feed(NO_MIC_LEVELS, 0, 25, (t) => -50 + (t % 1) * 3)), false);
  // 一点声音都没有（音量一直是 0）：也不提示（全是 0 的另有 mic_silent 提醒）
  assert.equal(micTooQuiet(feed(NO_MIC_LEVELS, 0, 25, () => -70)), false);
  // 说话声一变大马上不提示；之后又小了 20 秒才再提示
  const louder = trackMicLevels(quiet, recording(25.1, -22));
  assert.equal(micTooQuiet(louder), false);
  assert.equal(micTooQuiet(feed(louder, 25.2, 44, quietSpeech)), false);
  assert.equal(micTooQuiet(feed(louder, 25.2, 46, quietSpeech)), true);
});

test("麦克风的音量：暂停时留着，秒数没变（只是上传情况变了）不重复记；换了录音、换了麦克风、结束了从头记", () => {
  const quiet = feed(NO_MIC_LEVELS, 0, 25, quietSpeech);
  assert.equal(trackMicLevels(quiet, recording(25, -10)), quiet, "秒数没往前走：不记");
  assert.equal(trackMicLevels(quiet, recording(25, -45, { status: "paused" })), quiet, "暂停着：原样留着");
  assert.equal(micTooQuiet(trackMicLevels(quiet, recording(25, -45, { status: "paused" }))), true);
  // 换了麦克风（mic_changed）、换了一场录音、秒数往回走了：从头记
  for (const changed of [{ mic: "AirPods Pro" }, { meetingId: "m2" }]) {
    const next = trackMicLevels(quiet, recording(25.1, -45, changed));
    assert.equal(next.samples.length, 1, JSON.stringify(changed));
    assert.equal(micTooQuiet(next), false);
  }
  assert.equal(trackMicLevels(quiet, recording(3, -45)).samples.length, 1, "秒数往回走了");
  // 没在录、不录麦克风、还没有音量：清掉
  for (const s of [state(), recording(25.1, -45, { status: "stopping" }), recording(25.1, -45, { mic: null }), recording(25.1, -45, { level: null })]) {
    assert.equal(trackMicLevels(quiet, s), NO_MIC_LEVELS);
  }
  // 暂停以后继续：接着记，暂停的那段时间不算（秒数是录到的秒数，暂停时不走）
  const resumed = feed(quiet, 25.1, 26, quietSpeech);
  assert.equal(micTooQuiet(resumed), true);
});

test("麦克风的声音一直很小时的提示：说怎么办，自动调大过了也说一声", () => {
  const plain = quietMicHint(null);
  assert.match(plain, /把电脑挪到离说话的人近一些的地方/);
  assert.match(plain, /「系统设置 → 声音 → 输入」里把输入音量调大/);
  assert.doesNotMatch(plain, /自动调大/);
  assert.equal(quietMicHint("off"), plain);
  assert.equal(quietMicHint("unavailable"), plain);
  assert.match(quietMicHint("auto"), /自动调大了也还不够/);
});

import { invoke } from "@tauri-apps/api/core";
import { listen } from "@tauri-apps/api/event";
import { useSyncExternalStore } from "react";
import { api, errorMessage, queryClient, unwrap } from "../api/client";
import { onIdentityChange } from "./desktop";
import {
  ECHO_CANCEL_OFF,
  ECHO_CANCEL_ON,
  IDLE_STATE,
  micTooQuiet,
  nextEnded,
  NO_MIC_LEVELS,
  normalizeState,
  parseEchoCancel,
  parseSources,
  sentence,
  settledMeetings,
  trackMicLevels,
} from "./recorderView";
import type { Ended, MicLevels, Sources } from "./recorderView";

/**
 * 桌面版的原生录音（docs/desktop.md 的「录音（第三阶段）」）。录音、上传都在 Rust 那边
 * （desktop/src-tauri，它再管着 Swift 写的录音程序 zsmeet-recorder），界面只发命令、显示状态：
 * 关掉窗口录音不停，菜单栏上也能暂停、标记、结束，所以状态以 Rust 推过来的为准。
 *
 * 这里只在桌面版里用到；网页版的构建里没有这些代码（模块顶层没有副作用，没用到就整个去掉）。
 */

/** 录音的状态。idle：没在录；starting：正在打开麦克风和电脑声音；stopping：正在结束，等最后一片写好。 */
export type RecorderStatus = "idle" | "starting" | "recording" | "paused" | "stopping";

/** 录音时的提示，比如没有麦克风权限（code 见 recorderView.ts 的 warningText）。 */
export type RecorderWarning = { code: string; message: string };

/** 上传情况。recording：还在录、边录边传；waiting：暂时传不上去（断网），恢复后接着传；
 *  completing：分片都传完了，在告诉服务器开始处理；interrupted：录到一半 App 退出了或者出错了；
 *  failed：服务器不收了（会议被删了、没有权限了）。 */
export type UploadStatus = "recording" | "uploading" | "waiting" | "completing" | "completed" | "interrupted" | "failed";

/** 这台电脑上一场录音的上传情况。recorded、uploaded 是片数（每片约 10 秒）。 */
export type RecorderUpload = {
  meetingId: string;
  title: string;
  recorded: number;
  uploaded: number;
  status: UploadStatus;
  error: string | null;
};

/**
 * 回声消除（「外放时去掉回声」，实验功能）的模式，录音程序在 started 事件里报的 echo_cancellation：
 * off：不处理；auto：外放时才处理（戴耳机、用 AirPlay 时不处理）；on：一直处理；unavailable：要处理但没能打开，录的是原来的声音。
 */
export type EchoMode = "off" | "auto" | "on" | "unavailable";

/**
 * 回声消除现在开没开的原因（录音程序的 echo 事件）：speakers 在用扬声器外放；headphones 戴着耳机；airplay 用 AirPlay 外放；
 * no_reference 没收到电脑里的声音（要拿它去掉回声）；forced 不管外放还是戴耳机都处理（on）；off 不处理。
 */
export type EchoReason = "speakers" | "headphones" | "airplay" | "no_reference" | "forced" | "off";

/** 回声消除的情况。reason 一般是 EchoReason 里的一个；录音程序以后多了别的原因也照样传过来，所以类型是 string。 */
export type RecorderEcho = { mode: EchoMode; active: boolean; reason: string | null };

/**
 * 麦克风自动增益（docs/desktop.md 的「麦克风自动增益」）的模式，录音程序在 started 事件里报的 mic_gain：
 * auto：在回声消除之后把麦克风的声音调到正常的说话音量（App 总是要它调，界面上没有开关）；off：不调；
 * unavailable：要调却没能打开，录的是原来的音量。
 */
export type MicGainMode = "off" | "auto" | "unavailable";

/** Rust 那边的录音状态（事件 recorder://state 和各个命令的返回值）。 */
export type RecorderState = {
  status: RecorderStatus;
  meetingId: string | null;
  title: string | null;
  /** 录了多少秒，不算暂停的时间 */
  seconds: number;
  /** 现在的音量，0 到 1。录音时大约每 100 毫秒更新一次 */
  level: { mic: number; system: number } | null;
  /**
   * 在录的麦克风的名字，不录麦克风（或者开始时就录不到）时为 null；录到一半断开了还是它的名字，warnings 里有提醒。
   * 选的是「系统默认的麦克风」、录音中系统默认的麦克风换了（比如连上了 AirPods）时，录音程序跟着换（mic_changed），这里也跟着变
   */
  mic: string | null;
  /** 在录电脑里的声音 */
  system: boolean;
  /** 标记的重点：录音开始后的秒数 */
  marks: number[];
  warnings: RecorderWarning[];
  error: string | null;
  /** 这台电脑上没传完（和刚传完）的录音，包括正在录的这一场 */
  uploads: RecorderUpload[];
  /** 回声消除：模式、现在开没开、为什么。没在录、录音程序没报（旧版本）时是 null */
  echo: RecorderEcho | null;
  /** 麦克风自动增益的模式。没在录、录音程序没报（旧版本）时是 null */
  micGain: MicGainMode | null;
};

export type Microphone = { uid: string; name: string; default: boolean };

export type RecorderSnapshot = Readonly<{
  state: RecorderState;
  /** 刚结束的那一场：录音页上接着显示它的上传进度和会议链接，点「再录一场」以后清掉 */
  ended: Ended | null;
  /** 已经传完、点过「知道了」的录音（会议 ID） */
  dismissed: ReadonlySet<string>;
  /** 录音功能没能启动的原因 */
  failure: string;
  /** 麦克风的声音一直很小（见 recorderView.ts 的 micTooQuiet）：录音页上提示把电脑挪近一些、调大输入音量 */
  micQuiet: boolean;
}>;

const EVENT_STATE = "recorder://state";

let snap: RecorderSnapshot = { state: IDLE_STATE, ended: null, dismissed: new Set(), failure: "", micQuiet: false };
/** 最近的麦克风音量：在这里记（不在录音页上），离开录音页再回来也接着算 */
let micLevels: MicLevels = NO_MIC_LEVELS;
const listeners = new Set<() => void>();

function update(patch: Partial<RecorderSnapshot>) {
  snap = { ...snap, ...patch };
  for (const l of listeners) l();
}

/** 收到新的状态。刚结束录音、刚传完的会议，服务器那边的状态也变了，重新取一次。 */
function apply(raw: RecorderState) {
  const next = normalizeState(raw);
  const prev = snap.state;
  const settled = settledMeetings(prev, next);
  for (const id of settled) void queryClient.invalidateQueries({ queryKey: ["meeting", id] });
  if (settled.length > 0) void queryClient.invalidateQueries({ queryKey: ["meetings"] });
  const ended = nextEnded(prev, next, snap.ended);
  // 刚结束的这一场什么也没录下来：服务器上为它建的会议是空的，删掉
  if (ended && ended !== snap.ended && ended.empty) void deleteEmptyMeeting(ended.meetingId);
  micLevels = trackMicLevels(micLevels, next);
  update({ state: next, ended, micQuiet: micTooQuiet(micLevels) });
}

/**
 * 删掉服务器上为录音刚建的一场空会议（连回收站里的一起）：没能开始录音，或者什么也没录下来，
 * 免得留下一场一直等着上传的空会议。
 */
export async function deleteEmptyMeeting(meetingId: string) {
  try {
    await unwrap(api.DELETE("/api/meetings/{id}", { params: { path: { id: meetingId } } }));
    await unwrap(api.DELETE("/api/trash/{id}", { params: { path: { id: meetingId } } }));
  } catch {
    // 删不掉也不要紧：会议列表里会多一场空会议，可以自己删
  }
  void queryClient.invalidateQueries({ queryKey: ["meetings"] });
}

let started = false;
let sawEvent = false;

/** 第一次用到时：订阅 Rust 推过来的状态，再取一次当前状态。 */
function start() {
  if (started) return;
  started = true;
  // 换了人、换了服务器：上一个人刚录完的那一场、点过的「知道了」都不留
  onIdentityChange(() => update({ ended: null, dismissed: new Set() }));
  void (async () => {
    try {
      await listen<RecorderState>(EVENT_STATE, (e) => {
        sawEvent = true;
        apply(e.payload);
      });
      const current = await invoke<RecorderState>("recorder_state");
      // 取的时候已经收到了推送：推送的比取到的新，不用取到的
      if (!sawEvent) apply(current);
    } catch (e) {
      update({ failure: `录音功能没能启动：${sentence(errorMessage(e))}请退出 zsmeet 再打开试试。` });
    }
  })();
}

function subscribe(listener: () => void) {
  start();
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

/** 录音的整体状态。整个 App 只有一场录音，切到别的页面、关掉窗口都不影响它。 */
export function useRecorder(): RecorderSnapshot {
  return useSyncExternalStore(subscribe, () => snap);
}

/** 现在的状态（音量条每 100 毫秒读一次）。 */
export function recorderState(): RecorderState {
  return snap.state;
}

// ---- 命令（desktop/src-tauri 里的 recorder_*）。出错时 Rust 返回给人看的中文 ----

async function command(name: string, args?: Record<string, unknown>): Promise<RecorderState> {
  apply(await invoke<RecorderState>(name, args));
  return snap.state;
}

/** 电脑上的麦克风。 */
export async function recorderDevices(): Promise<Microphone[]> {
  const devices = await invoke<{ microphones?: Microphone[] } | null>("recorder_devices");
  return devices?.microphones ?? [];
}

/**
 * 开始录音：服务器上的会议要先建好。mic 是麦克风的 uid 或者 "default"（系统默认的），null 是不录麦克风。
 * echoCancel：外放时去掉回声（实验功能）。true 时 App 让录音程序按 auto 处理（外放时处理，戴耳机、用 AirPlay 时不处理），
 * false 时不处理。麦克风和电脑里的声音都录时才有用（见 recorderView.ts 的 echoCancelArg）。
 * 这台电脑上有这场会议中断了的录音时，接着它录。
 */
export function recorderStart(opts: { meetingId: string; mic: string | null; system: boolean; echoCancel: boolean }): Promise<RecorderState> {
  return command("recorder_start", opts);
}

export function recorderPause(): Promise<RecorderState> {
  return command("recorder_pause");
}

export function recorderResume(): Promise<RecorderState> {
  return command("recorder_resume");
}

/** 标记重点：记下现在录到了第几秒。 */
export function recorderMark(): Promise<RecorderState> {
  return command("recorder_mark");
}

/** 结束录音。剩下的分片传完以后，服务器自动开始处理。 */
export function recorderStop(): Promise<RecorderState> {
  return command("recorder_stop");
}

/** 中断了的录音不接着录了：把已经录下的部分传上去，开始处理。 */
export function recorderFinish(meetingId: string): Promise<RecorderState> {
  return command("recorder_finish", { meetingId });
}

/** 删掉这台电脑上一段中断了、或者传不上去的录音。 */
export function recorderDiscard(meetingId: string): Promise<RecorderState> {
  return command("recorder_discard", { meetingId });
}

// ---- 界面自己记的东西 ----

/** 录音页上点了「再录一场」：不再显示刚结束的那一场。 */
export function clearEnded() {
  update({ ended: null });
}

/** 已经传完的录音，点了「知道了」或者「查看」：页面顶部不再提示。 */
export function dismissUpload(meetingId: string) {
  update({ dismissed: new Set([...snap.dismissed, meetingId]) });
}

const SOURCES_KEY = "zsmeet.recorder.sources";

/** 上次录音选的声音来源（只是方便，读不到就用默认的）。 */
export function loadSources(): Sources {
  try {
    return parseSources(localStorage.getItem(SOURCES_KEY));
  } catch {
    return parseSources(null);
  }
}

export function saveSources(sources: Sources) {
  try {
    localStorage.setItem(SOURCES_KEY, JSON.stringify(sources));
  } catch {
    // 存不下也没关系，下次用默认的
  }
}

const ECHO_KEY = "zsmeet.recorder.echoCancel";

/** 「外放时去掉回声（实验）」开没开：每台电脑记自己的，没点过时用默认的（见 recorderView.ts 的 parseEchoCancel）。 */
export function loadEchoCancel(): boolean {
  try {
    return parseEchoCancel(localStorage.getItem(ECHO_KEY));
  } catch {
    return parseEchoCancel(null);
  }
}

/** 点了这个开关就记下来，不用等开始录音。只记点过的：以后默认值变了，没点过的人跟着新的默认值走。 */
export function saveEchoCancel(on: boolean) {
  try {
    localStorage.setItem(ECHO_KEY, on ? ECHO_CANCEL_ON : ECHO_CANCEL_OFF);
  } catch {
    // 存不下也没关系，这次照样按点的来，下次用默认的
  }
}

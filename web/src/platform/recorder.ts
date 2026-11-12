import { invoke } from "@tauri-apps/api/core";
import { listen } from "@tauri-apps/api/event";
import { useSyncExternalStore } from "react";
import { errorMessage, queryClient } from "../api/client";
import { onIdentityChange } from "./desktop";
import { IDLE_STATE, nextEnded, normalizeState, parseSources, sentence, settledMeetings } from "./recorderView";
import type { Ended, Sources } from "./recorderView";

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

/** Rust 那边的录音状态（事件 recorder://state 和各个命令的返回值）。 */
export type RecorderState = {
  status: RecorderStatus;
  meetingId: string | null;
  title: string | null;
  /** 录了多少秒，不算暂停的时间 */
  seconds: number;
  /** 现在的音量，0 到 1。录音时大约每 100 毫秒更新一次 */
  level: { mic: number; system: number } | null;
  /** 在录的麦克风的名字，不录麦克风（或者录不到）时为 null */
  mic: string | null;
  /** 在录电脑里的声音 */
  system: boolean;
  /** 标记的重点：录音开始后的秒数 */
  marks: number[];
  warnings: RecorderWarning[];
  error: string | null;
  /** 这台电脑上没传完（和刚传完）的录音，包括正在录的这一场 */
  uploads: RecorderUpload[];
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
}>;

const EVENT_STATE = "recorder://state";

let snap: RecorderSnapshot = { state: IDLE_STATE, ended: null, dismissed: new Set(), failure: "" };
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
  update({ state: next, ended: nextEnded(prev, next, snap.ended) });
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
 * 这台电脑上有这场会议中断了的录音时，接着它录。
 */
export function recorderStart(opts: { meetingId: string; mic: string | null; system: boolean }): Promise<RecorderState> {
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

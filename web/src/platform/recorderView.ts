// 桌面版录音的界面逻辑：把 Rust 推过来的录音状态（recorder.ts 的 RecorderState）换成界面上的话和按钮。
// 不依赖 React 和 Tauri，方便单独测试（recorderView.test.ts）。
// 引用别的文件里的值要带 .ts 后缀：测试用 Node 直接跑，它不会自己补后缀
import { DESKTOP_CHUNK_MS } from "../lib/unfinishedRecording.ts";
import type { Microphone, RecorderState, RecorderStatus, RecorderUpload, RecorderWarning } from "./recorder";

/**
 * 整理 Rust 推过来的状态：缺了的字段用默认值补上。Rust 那边和界面的版本对不上时（比如省掉了空的列表），
 * 界面也不至于出错。
 */
export function normalizeState(raw: Partial<RecorderState> | null | undefined): RecorderState {
  const s = raw ?? {};
  return {
    status: s.status ?? "idle",
    meetingId: s.meetingId ?? null,
    title: s.title ?? null,
    seconds: s.seconds ?? 0,
    level: s.level ?? null,
    mic: s.mic ?? null,
    system: s.system ?? false,
    marks: s.marks ?? [],
    warnings: s.warnings ?? [],
    error: s.error ?? null,
    uploads: s.uploads ?? [],
  };
}

/** 没在录、也没有要传的录音。 */
export const IDLE_STATE: RecorderState = normalizeState(null);

/** 正在录（含暂停）。 */
export function isLive(status: RecorderStatus): boolean {
  return status === "recording" || status === "paused";
}

/** 有一场录音还没结束（正在开始、在录、暂停着、正在结束）：这时不能开始新的录音。 */
export function isBusy(status: RecorderStatus): boolean {
  return status !== "idle";
}

/** 页面顶部「正在录音」提示条、会议页面上的说法。 */
export const BUSY_LABEL: Record<Exclude<RecorderStatus, "idle">, string> = {
  starting: "正在开始录音",
  recording: "正在录音",
  paused: "录音已暂停",
  stopping: "正在结束录音",
};

/** 去掉句末的标点，好接在别的话里（Rust 那边给的说明可能带着句号）。 */
function clause(text: string): string {
  return text.trim().replace(/[。．.！!；;，,]+$/u, "");
}

/** 写成完整的一句：没有句末标点的补上句号，好在后面接着说别的。 */
export function sentence(text: string): string {
  const t = text.trim();
  return /[。！？!?]$/u.test(t) ? t : `${clause(t)}。`;
}

/** 录下了多少片，说成大概多长时间（每片约 10 秒，和会议页面上估算服务器收到了多长一样）。 */
export function recordedLength(chunks: number): string {
  const minutes = Math.round((chunks * DESKTOP_CHUNK_MS) / 60_000);
  if (minutes < 1) return "不到 1 分钟";
  if (minutes < 60) return `大约 ${minutes} 分钟`;
  const rest = minutes % 60;
  return `大约 ${Math.floor(minutes / 60)} 小时${rest ? ` ${rest} 分钟` : ""}`;
}

// ---- 开始前：选声音来源 ----

/** 麦克风选「系统默认的」时的值；选「不录麦克风」时的值。其他值是麦克风的 uid。 */
export const DEFAULT_MIC = "default";
export const NO_MIC = "none";

export type Sources = { mic: string; system: boolean };

/** 默认：系统默认的麦克风，加上电脑里的声音（线上会议里其他人的声音）。 */
export const DEFAULT_SOURCES: Sources = { mic: DEFAULT_MIC, system: true };

/** 读出上次选的声音来源（存在本机的 JSON），读不懂、两样都不录的话用默认的。 */
export function parseSources(raw: string | null): Sources {
  if (!raw) return DEFAULT_SOURCES;
  try {
    const value: unknown = JSON.parse(raw);
    if (value && typeof value === "object") {
      const { mic, system } = value as Record<string, unknown>;
      if (typeof mic === "string" && mic && typeof system === "boolean" && !(mic === NO_MIC && !system)) return { mic, system };
    }
  } catch {
    // 存坏了：用默认的
  }
  return DEFAULT_SOURCES;
}

/** 选麦克风的下拉框：系统默认的、电脑上的每一个、不录麦克风。 */
export function micOptions(devices: readonly Microphone[]): { value: string; label: string }[] {
  const current = devices.find((d) => d.default);
  return [
    { value: DEFAULT_MIC, label: current ? `系统默认的麦克风（现在是 ${current.name}）` : "系统默认的麦克风" },
    ...devices.map((d) => ({ value: d.uid, label: d.name })),
    { value: NO_MIC, label: "不录麦克风" },
  ];
}

/** 实际用哪个麦克风：上次选的那个现在不在了（拔掉了），或者还没列出麦克风时，用系统默认的。 */
export function effectiveMic(mic: string, devices: readonly Microphone[] | undefined): string {
  if (mic === DEFAULT_MIC || mic === NO_MIC) return mic;
  return devices?.some((d) => d.uid === mic) ? mic : DEFAULT_MIC;
}

/** 交给 recorder_start 的 mic：麦克风的 uid 或者 "default"；不录麦克风是 null。 */
export function micArg(mic: string): string | null {
  return mic === NO_MIC ? null : mic;
}

/** 开始前的检查：麦克风和电脑里的声音至少要录一种。没问题时是空字符串。 */
export function sourcesProblem(sources: Sources): string {
  return sources.mic === NO_MIC && !sources.system ? "麦克风和电脑里的声音至少要录一种。" : "";
}

// ---- 录音中 ----

/** 录音时的提示（没有权限、找不到设备）：录音程序没给说明时用的话，和怎么解决。 */
const WARNINGS: Record<string, { message: string; help: string }> = {
  mic_denied: {
    message: "zsmeet 没有使用麦克风的权限，录不到麦克风的声音。",
    help: "打开方法：「系统设置 → 隐私与安全性 → 麦克风」，把 zsmeet 打开。打开以后，再开始录音就能录到麦克风。",
  },
  system_denied: {
    message: "zsmeet 没有录制电脑声音的权限，录不到会议软件里其他人的声音。",
    help: "打开方法：「系统设置 → 隐私与安全性 → 屏幕与系统录音」，在「仅系统录音」里把 zsmeet 打开。打开以后，再开始录音就能录到电脑里的声音。",
  },
  mic_unavailable: {
    message: "麦克风用不了，录不到麦克风的声音。",
    help: "看看麦克风是不是插好了、有没有被别的程序占着，也可以在开始录音前换一个麦克风。",
  },
  system_unavailable: {
    message: "录不到电脑里的声音。",
    help: "退出 zsmeet 再打开试试，还不行的话重启一下电脑。",
  },
};

/**
 * 一条录音提示写成给人看的话：出了什么事，怎么解决。录音程序给的说明本身就说了怎么办（见
 * desktop/recorder 的 RecordSession.swift），照着显示，不再重复；没给说明时用这里的。
 * 没有权限时一定要说清楚去哪里打开：说明里没提「系统设置」的，补上打开方法。
 */
export function warningText(w: RecorderWarning): string {
  const known = WARNINGS[w.code];
  const message = w.message.trim();
  if (!message) return known ? `${known.message}${known.help}` : "录音出了点问题。";
  if (known && w.code.endsWith("_denied") && !message.includes("系统设置")) return `${clause(message)}。${known.help}`;
  return message;
}

/**
 * 录音中显示的「麦克风：…」「电脑里的声音：…」。想录但录不到的（没有权限、找不到设备）说「录不到」。
 * 在录的这一路有提示时，名字后面也指向提示，不能看起来一切正常：录着录着麦克风断开了、一直没有声音，
 * Rust 那边照样报着麦克风的名字。这些提示和「没找到选的麦克风，改用了别的」（照常在录）是同一个 code
 * （mic_unavailable），分不出现在还录不录得到，所以不说「录不到」。
 */
export function sourceLabels(state: RecorderState): { mic: string; system: string } {
  const label = (recording: string | null, prefix: string) => {
    const warned = state.warnings.some((w) => w.code.startsWith(prefix));
    if (recording === null) return warned ? "录不到（见下面的提示）" : "不录";
    return warned ? `${recording}（见下面的提示）` : recording;
  };
  return { mic: label(state.mic, "mic_"), system: label(state.system ? "正在录" : null, "system_") };
}

/**
 * 录音中的上传情况：一行进度；传不上去时再加一句说明（没有问题时 trouble 是空字符串）。
 * 暂时传不上去（断网、要重新登录）：App 给的原因里已经说了什么时候接着传，有原因时不再自己补一句。
 * 传不上去了（failed：会议被删了、没有权限了）：录音还在继续，但录下的也传不上去，建议结束。
 */
export function liveUpload(u: RecorderUpload | undefined): { text: string; trouble: string; tone: "warn" | "bad" } {
  const uploaded = u?.uploaded ?? 0;
  const pending = Math.max(0, (u?.recorded ?? 0) - uploaded);
  const error = u?.error ?? "";
  const progress = `已上传 ${uploaded} 片${pending > 0 ? ` · 待上传 ${pending} 片` : ""}`;
  if (u?.status === "failed") {
    return {
      text: `${progress} · 传不上去了`,
      trouble: `${sentence(error || "这场会议的录音传不上去了")}现在录的也传不上去，建议结束这次录音，新建一场会议重新录。`,
      tone: "bad",
    };
  }
  const stuck = u?.status === "waiting" || !!error;
  const text = `${progress} · ${stuck ? "暂时传不上去" : "网络正常，边录边传"}`;
  if (!stuck) return { text, trouble: "", tone: "warn" };
  const trouble = error
    ? `暂时传不上去：${sentence(error)}录音没有停，分片先存在这台电脑上。`
    : "暂时传不上去。录音没有停，分片先存在这台电脑上，恢复后自动补传，不用做任何操作。";
  return { text, trouble, tone: "warn" };
}

/** 会议页面上：这场会议正在这台电脑上录音时的说明。elapsed 是写好的时长（00:12:34）。 */
export function liveMeetingText(status: Exclude<RecorderStatus, "idle">, elapsed: string): string {
  switch (status) {
    case "starting":
      return "正在这台电脑上开始录这场会议…";
    case "recording":
      return `这场会议正在这台电脑上录音，已经录了 ${elapsed}。结束录音、录音传完以后自动开始处理。`;
    case "paused":
      return `这场会议正在这台电脑上录音，现在暂停着（已经录了 ${elapsed}）。`;
    case "stopping":
      return "这场会议的录音正在结束，录音传完以后自动开始处理。";
  }
}

// ---- 结束以后：上传 ----

/**
 * 刚结束的那一场。marks 是标记了几个重点。empty：一片都没录下来，App 已经把它删了，
 * 服务器上为它建的会议是空的（见 recorder.ts 的 apply）。
 */
export type Ended = { meetingId: string; title: string; seconds: number; marks: number; empty: boolean };

/**
 * 录音页上「录音已结束」显示哪一场：从录音中（含暂停、正在结束）回到 idle 时记下刚才那一场；
 * 又开始录了就不再显示。开始失败（starting 直接回到 idle）不算录过。
 * 只记当前账号的录音（在它的上传列表里的）：别的账号的录音不给看。
 * 结束时它从上传列表里没了、App 说了原因（error），就是一片都没录下来，App 把它删了。
 */
export function nextEnded(prev: RecorderState, next: RecorderState, ended: Ended | null): Ended | null {
  if (next.status === "starting" || isLive(next.status)) return null;
  const id = prev.meetingId;
  const listed = (s: RecorderState) => s.uploads.some((u) => u.meetingId === id);
  if (next.status === "idle" && id && (isLive(prev.status) || prev.status === "stopping") && listed(prev)) {
    const empty = !listed(next) && next.error !== null;
    return { meetingId: id, title: prev.title ?? "", seconds: prev.seconds, marks: prev.marks.length, empty };
  }
  return ended;
}

/**
 * 刚结束的那一场的上传情况（传完了的也在，App 重启以前一直留着）。这台电脑上已经没有它了
 * （什么也没录下来，或者被删掉了）时是 null。
 */
export function endedUpload(state: RecorderState, ended: Ended): RecorderUpload | null {
  return state.uploads.find((u) => u.meetingId === ended.meetingId) ?? null;
}

/** 刚结束的那一场在这台电脑上已经没有了：标题和说明。 */
export function endedGone(ended: Ended): { title: string; text: string } {
  return ended.empty
    ? { title: "什么也没有录下来", text: "录音刚开始就结束了，什么也没有录下来，不会生成纪要。可以再录一场。" }
    : { title: "录音已删除", text: "这台电脑上的这段录音已经删掉了，不会再上传，这场会议也不会生成纪要。不需要这场会议的话，可以在会议页面上删掉它。" };
}

/** 传完了、或者被删掉了的录音：服务器上这场会议的状态变了，要重新取。 */
export function settledMeetings(prev: RecorderState, next: RecorderState): string[] {
  const now = new Map(next.uploads.map((u) => [u.meetingId, u.status]));
  return prev.uploads.filter((u) => u.status !== "completed" && (!now.has(u.meetingId) || now.get(u.meetingId) === "completed")).map((u) => u.meetingId);
}

export type UploadAction = "view" | "dismiss" | "continue" | "finish" | "discard";

export type UploadView = {
  tone: "ok" | "info" | "warn" | "bad";
  text: string;
  /** 上传进度（0 到 1）；不显示进度条时是 null */
  progress: number | null;
  /** 正在做的事没有进度可显示（交给服务器处理），显示转圈 */
  spinning: boolean;
  actions: UploadAction[];
};

/** 上传情况显示在哪里：页面顶部的提示条、会议页面、录音页（刚结束的那一场）。 */
export type UploadWhere = "banner" | "meeting" | "record";

/**
 * 一场录音的上传情况写成给人看的话，加上能做的事。
 * 页面顶部的提示条里话要带上会议标题，「查看」「知道了」也只在那里有；会议页面、录音页上看得出是哪一场。
 * canStart：现在没在录音，可以接着录中断了的那一场。
 * App 给的原因（error）是完整的话，已经说了接下来会怎样（联网后接着传、重新登录后接着传……），
 * 有原因时照着它说，不再自己补一句，免得说法打架。
 */
export function uploadView(u: RecorderUpload, { where, canStart }: { where: UploadWhere; canStart: boolean }): UploadView {
  const subject = where === "banner" ? `「${u.title}」的录音` : where === "meeting" ? "这台电脑上的录音" : "录音";
  const progress = u.recorded > 0 ? Math.min(1, u.uploaded / u.recorded) : 0;
  const left = Math.max(0, u.recorded - u.uploaded);
  const base = { progress: null, spinning: false, actions: [] as UploadAction[] };
  switch (u.status) {
    case "recording":
    case "uploading":
      return { ...base, tone: "info", text: `${subject}正在上传：已传 ${u.uploaded} / ${u.recorded} 片。`, progress };
    case "waiting":
      if (left === 0) {
        const text = u.error ? `${subject}已经传完，还没能交给服务器处理：${sentence(u.error)}` : `${subject}已经传完，连上服务器以后自动开始处理。`;
        return { ...base, tone: "warn", text };
      }
      return {
        ...base,
        tone: "warn",
        text: u.error ? `${subject}还有 ${left} 片没传上去：${sentence(u.error)}` : `${subject}还有 ${left} 片没传上去，连上服务器以后自动接着传。`,
        progress,
      };
    case "completing":
      return { ...base, tone: "info", text: `${subject}已经传完，正在交给服务器处理…`, spinning: true };
    case "completed":
      return { ...base, tone: "ok", text: `${subject}已经全部传上去，正在转写和生成纪要。`, actions: where === "banner" ? ["view", "dismiss"] : [] };
    case "interrupted": {
      const actions: UploadAction[] = [];
      if (canStart) actions.push("continue");
      if (u.recorded > 0) actions.push("finish");
      actions.push("discard");
      // 什么也没录下来时，中断的原因不要紧：只能重新录，或者删掉
      if (u.recorded === 0) return { ...base, tone: "warn", text: `${subject}刚开始就中断了，什么也没有录下来。`, actions };
      const length = recordedLength(u.recorded);
      const text = u.error
        ? `${subject}录到一半中断了，已经录下${length}。${sentence(u.error)}`
        : `${subject}录到一半中断了（zsmeet 退出了，或者录音出了错），已经录下${length}。可以接着录，也可以把录下的部分传上去生成纪要。`;
      return { ...base, tone: "warn", text: canStart ? text : `${text}要接着录的话，等现在这一场录完。`, actions };
    }
    case "failed":
      return {
        ...base,
        tone: "bad",
        text: u.error ? `${subject}：${sentence(u.error)}` : `${subject}传不上去了：服务器不再接收这场会议的录音，会议可能已经被删除，或者你已经没有权限。`,
        actions: ["discard"],
      };
  }
}

/** 删除一段录音前的确认。 */
export function discardConfirm(u: RecorderUpload): string {
  return u.status === "interrupted" && u.recorded > 0
    ? "确定删除吗？这台电脑上录下的部分会被删掉，不能恢复，这场会议也不会生成纪要（不需要的话可以在会议页面上删掉它）。"
    : "确定删除这台电脑上的这段录音吗？删掉以后不能恢复。";
}

/**
 * 页面顶部提示条里的上传：正在录的那一场不算（另外有「正在录音」的提示条）；
 * hidden 里的（当前页面自己会显示）、点过「知道了」的也不算。
 */
export function bannerUploads(state: RecorderState, hidden: ReadonlySet<string>, dismissed: ReadonlySet<string>): RecorderUpload[] {
  return state.uploads.filter(
    (u) =>
      u.status !== "recording" &&
      !(isBusy(state.status) && u.meetingId === state.meetingId) &&
      !hidden.has(u.meetingId) &&
      !dismissed.has(u.meetingId),
  );
}

/**
 * 退出登录、切换服务器（action）之前的提醒：正在录的会先结束（App 里的 logout 会先结束录音），
 * 录好的、没传完的都留在这台电脑上，只等这个账号再登录这台服务器时接着传（别的账号看不到）。
 * 没有要提醒的时是空字符串。
 */
export function signOutNote(state: RecorderState, action: string): string {
  const live = isBusy(state.status);
  const unsent = state.uploads.filter(
    (u) => ["uploading", "waiting", "completing", "interrupted"].includes(u.status) && !(live && u.meetingId === state.meetingId),
  ).length;
  const later = "以后用这个账号登录这台服务器时接着上传";
  if (live) {
    const kept = unsent > 0 ? `这一场和另外 ${unsent} 段没传完的录音` : "录好的部分";
    return `正在录音，${action}会先结束这场录音。${kept}留在这台电脑上，${later}。`;
  }
  return unsent > 0 ? `这台电脑上还有 ${unsent} 段录音没传完，${action}以后先不传，${later}。` : "";
}

/** 这场会议在这台电脑上：正在录，或者有录音在传（传不上去、中断了也算）。都没有时是 null。 */
export type LocalMeeting = { kind: "live"; status: Exclude<RecorderStatus, "idle">; seconds: number } | { kind: "upload"; upload: RecorderUpload };

export function localMeeting(state: RecorderState, meetingId: string): LocalMeeting | null {
  if (state.status !== "idle" && state.meetingId === meetingId) return { kind: "live", status: state.status, seconds: state.seconds };
  const upload = state.uploads.find((u) => u.meetingId === meetingId);
  return upload ? { kind: "upload", upload } : null;
}

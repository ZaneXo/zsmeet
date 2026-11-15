// 桌面版自动更新的界面逻辑：把 App（desktop/src-tauri）报的新版本、下载进度，加上录音的情况，换成界面上的话。
// 不依赖 React 和 Tauri，方便单独测试（updateView.test.ts）。
// 引用别的文件里的值要带 .ts 后缀：测试用 Node 直接跑，它不会自己补后缀
import { isEn, tr } from "../i18n/lang.ts";
import type { RecorderState, UploadStatus } from "./recorder";
import { BUSY_LABEL, sentence } from "./recorderView.ts";
import type { UpdateCheck, UpdateInfo, UpdateProgress } from "./updater";

const MONTHS_SHORT = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];

// ---- 整理 App 报过来的东西：Rust 那边和界面的版本对不上时，界面也不至于出错 ----

const record = (raw: unknown): Record<string, unknown> | null => (raw && typeof raw === "object" ? (raw as Record<string, unknown>) : null);

/** 新版本（update_check 的 available、update://available 事件）：没有版本号的当成没有新版本，说明、发布时间缺了的补成空的。 */
export function normalizeInfo(raw: unknown): UpdateInfo | null {
  const r = record(raw);
  const version = typeof r?.version === "string" ? r.version.trim() : "";
  if (!r || !version) return null;
  return {
    version,
    notes: typeof r.notes === "string" ? r.notes : "",
    date: typeof r.date === "string" && r.date.trim() ? r.date.trim() : null,
  };
}

/** update_check 的结果。 */
export function normalizeCheck(raw: unknown): UpdateCheck {
  const r = record(raw);
  return { current: typeof r?.current === "string" ? r.current.trim() : "", available: normalizeInfo(r?.available) };
}

/** 下载进度（update://progress）：downloaded 是一共下载了多少字节；total 不知道（或者报的不对）时是 null。 */
export function normalizeProgress(raw: unknown): UpdateProgress | null {
  const r = record(raw);
  if (!r || typeof r.downloaded !== "number" || !Number.isFinite(r.downloaded)) return null;
  const total = typeof r.total === "number" && Number.isFinite(r.total) && r.total > 0 ? r.total : null;
  return { downloaded: Math.max(0, r.downloaded), total };
}

// ---- 新版本是什么 ----

/** 发布时间写成「2026年11月10日」（只要日期，按这台电脑的时区）。没有、看不懂时是空字符串，界面上就不写。 */
export function releaseDate(date: string | null): string {
  if (!date) return "";
  const d = new Date(date);
  if (Number.isNaN(d.getTime())) return "";
  if (isEn()) return `${MONTHS_SHORT[d.getMonth()]} ${d.getDate()}, ${d.getFullYear()}`;
  return `${d.getFullYear()}年${d.getMonth() + 1}月${d.getDate()}日`;
}

/** 新版本提示的标题，和下面一行小字（现在用的版本、发布日期，不知道的不写）。 */
export function updateHeadline(info: UpdateInfo, current: string): { title: string; detail: string } {
  const date = releaseDate(info.date);
  if (isEn()) {
    const detail = [current ? `Current: ${current}` : "", date ? `Released ${date}` : ""].filter(Boolean).join(" · ");
    return { title: `zsmeet ${info.version} is available`, detail };
  }
  const detail = [current ? `现在用的是 ${current}` : "", date ? `${date}发布` : ""].filter(Boolean).join(" · ");
  return { title: `zsmeet 有新版本 ${info.version}`, detail };
}

/** 更新说明照原样显示（换行留着），只去掉首尾的空白、把连着的几个空行并成一个。没写说明时是空字符串，界面上就不显示。 */
export function notesText(notes: string): string {
  return notes
    .replace(/\r\n?/g, "\n")
    .replace(/\n[ \t]*(\n[ \t]*)+\n/g, "\n\n")
    .trim();
}

/** 手动检查以后没有新版本。 */
export function upToDateText(current: string): string {
  if (isEn()) return current ? `zsmeet is up to date (${current}).` : "zsmeet is up to date.";
  return current ? `已经是最新版本（${current}）。` : "已经是最新版本。";
}

/** 没能检查更新。App 给的原因是完整的一句说明，照着说，不再自己补。 */
export function checkFailedText(error: string): string {
  if (isEn()) return error.trim() ? `Could not check for updates: ${sentence(error)}` : "Could not check for updates. Please try again later.";
  return error.trim() ? `没能检查更新：${sentence(error)}` : "没能检查更新，稍后再试一次。";
}

// ---- 能不能现在更新 ----

/** 还没传完的录音：更新要重启 zsmeet，传到一半会断开。 */
const UNSENT: readonly UploadStatus[] = ["recording", "uploading", "waiting", "completing"];

export function updateBlocker(state: RecorderState): string {
  if (isEn()) {
    if (state.status === "stopping") return "Recording is stopping. Updating restarts zsmeet — wait until all chunks finish uploading.";
    if (state.status !== "idle") return `${BUSY_LABEL[state.status]}. Updating restarts zsmeet and would interrupt recording — stop recording and wait for upload to finish first.`;
    const unsent = state.uploads.filter((u) => UNSENT.includes(u.status)).length;
    if (unsent === 0) return "";
    const waiting = state.uploads.filter((u) => u.status === "waiting").length;
    if (waiting === 0) return `${unsent} recording${unsent === 1 ? " is" : "s are"} still uploading. Wait until upload finishes before updating.`;
    const stuck = waiting === unsent ? `${unsent} recording${unsent === 1 ? " is" : "s are"} waiting to upload` : `${unsent} recordings pending (${waiting} paused)`;
    return `${stuck} and retrying automatically. Check your network and update once uploads finish.`;
  }
  if (state.status === "stopping") return "录音正在结束。更新要重启 zsmeet，等录好的都传完以后再更新。";
  if (state.status !== "idle") return `${BUSY_LABEL[state.status]}。更新要重启 zsmeet，会打断录音：结束录音、等录好的都传完以后再更新。`;
  const unsent = state.uploads.filter((u) => UNSENT.includes(u.status)).length;
  if (unsent === 0) return "";
  // 暂时传不上去的（断网、连不上服务器）要人去看一眼网络，不然一直等不到传完
  const waiting = state.uploads.filter((u) => u.status === "waiting").length;
  if (waiting === 0) return `还有 ${unsent} 段录音没传完。更新要重启 zsmeet，传完以后再更新。`;
  const stuck = waiting === unsent ? `还有 ${unsent} 段录音暂时传不上去` : `还有 ${unsent} 段录音没传完，其中 ${waiting} 段暂时传不上去`;
  return `${stuck}，在等着自动再试。请检查网络和服务器，传完以后再更新。`;
}

// ---- 下载、安装 ----

/** 更新做到哪一步：idle 没在更新；installing 正在下载、核对、安装；installed 装好了，等 App 重新打开；failed 没装上。 */
export type InstallPhase = "idle" | "installing" | "installed" | "failed";

/** 字节数写成 KB、MB。 */
export function size(bytes: number): string {
  if (bytes < 1024 * 1024) return `${Math.round(bytes / 1024)} KB`;
  return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
}

export function installView(phase: InstallPhase, progress: UpdateProgress | null): { text: string; value: number | null } | null {
  if (phase === "installed") {
    return {
      text: tr(
        "新版本装好了，zsmeet 正在重新打开。没有自己打开的话，退出 zsmeet（⌘Q）再打开。",
        "Update installed — zsmeet is relaunching. If it does not reopen automatically, quit (⌘Q) and open zsmeet again.",
      ),
      value: 1,
    };
  }
  if (phase !== "installing") return null;
  if (!progress) return { text: tr("正在准备下载新版本…", "Preparing update download…"), value: null };
  if (progress.total === null) {
    return { text: isEn() ? `Downloading update: ${size(progress.downloaded)}` : `正在下载新版本：已下载 ${size(progress.downloaded)}`, value: null };
  }
  if (progress.downloaded >= progress.total) {
    return {
      text: tr(
        "下载好了，正在核对和安装，装好以后 zsmeet 会自动重新打开…",
        "Download complete — verifying and installing. zsmeet will relaunch automatically…",
      ),
      value: 1,
    };
  }
  return {
    text: isEn()
      ? `Downloading update: ${size(progress.downloaded)} / ${size(progress.total)}`
      : `正在下载新版本：${size(progress.downloaded)} / ${size(progress.total)}`,
    value: progress.downloaded / progress.total,
  };
}

export function recordUpdateNote(phase: InstallPhase, progress: UpdateProgress | null): { text: string; blocks: boolean } | null {
  if (phase === "installed") {
    return {
      text: tr("zsmeet 的新版本装好了，正在重新打开，打开以后再开始录音。", "Update installed — zsmeet is relaunching. Start recording after it reopens."),
      blocks: true,
    };
  }
  if (phase !== "installing") return null;
  if (progress?.total != null && progress.downloaded >= progress.total) {
    return {
      text: tr(
        "正在安装 zsmeet 的新版本，装好以后会自动重新打开，请稍等一下再开始录音。",
        "Installing a new version of zsmeet — it will relaunch automatically. Please wait before starting a recording.",
      ),
      blocks: true,
    };
  }
  return {
    text: tr(
      "正在下载 zsmeet 的新版本。现在开始录音的话，这次先不装，录完、传完以后再点「更新并重启」。",
      "Downloading a zsmeet update. If you start recording now, installation will wait until you finish and click \"Update & Relaunch\".",
    ),
    blocks: false,
  };
}

export function installFailedText(error: string): string {
  if (isEn()) return error.trim() ? `Update failed: ${sentence(error)}` : "Update failed. Please try again later.";
  return error.trim() ? `没能更新：${sentence(error)}` : "没能更新，稍后再试一次。";
}

// ---- 「以后再说」 ----

/** 点了「以后再说」以后，同一个版本隔多久再在页面顶上提示（设置里随时能看到、能更新）。 */
export const REMIND_AFTER_MS = 24 * 60 * 60 * 1000;

/** 对哪个版本、什么时候（毫秒时间戳）点的「以后再说」。 */
export type Dismissed = { version: string; at: number };

/** 读出这台电脑上记下的「以后再说」（localStorage 里的 JSON），读不懂的当成没点过。 */
export function parseDismissed(raw: string | null): Dismissed | null {
  if (!raw) return null;
  try {
    const value: unknown = JSON.parse(raw);
    const r = record(value);
    if (r && typeof r.version === "string" && r.version && typeof r.at === "number" && Number.isFinite(r.at)) return { version: r.version, at: r.at };
  } catch {
    // 存坏了：当成没点过
  }
  return null;
}

/**
 * 页面上的新版本提示显示不显示：有新版本，而且这个版本一天之内没点过「以后再说」（更新的版本出来了马上提示）。
 * 正在更新、更新出了错时一直显示：要看进度、原因。电脑的时间往回调了（now 比点的时候还早）时照常提示。
 */
export function noticeVisible({
  available,
  dismissed,
  phase,
  now,
}: {
  available: UpdateInfo | null;
  dismissed: Dismissed | null;
  phase: InstallPhase;
  now: number;
}): boolean {
  if (!available) return false;
  if (phase !== "idle") return true;
  if (!dismissed || dismissed.version !== available.version) return true;
  return now < dismissed.at || now - dismissed.at >= REMIND_AFTER_MS;
}

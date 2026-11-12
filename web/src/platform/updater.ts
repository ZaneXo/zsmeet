import { invoke } from "@tauri-apps/api/core";
import { listen } from "@tauri-apps/api/event";
import { useEffect, useSyncExternalStore } from "react";
import { errorMessage } from "../api/client";
import { normalizeCheck, normalizeInfo, normalizeProgress, parseDismissed } from "./updateView";
import type { Dismissed, InstallPhase } from "./updateView";

/**
 * 桌面版的自动更新。检查、下载、核对签名、安装、重新打开都在 Rust 那边（desktop/src-tauri，用 Tauri 的 updater
 * 读 GitHub Releases 上 desktop-latest 里的 latest.json），界面只显示、发命令：
 * - App 打开 10 秒左右、之后每 6 小时自己检查一次，有新版本时推 update://available；设置里也能点「检查更新」（update_check）。
 * - 用户点了「更新并重启」才下载安装（update_install），下载时推 update://progress。正在录音、正在结束录音、
 *   还有录音没传完时 App 不装，返回原因。
 *
 * 这里只在桌面版里用到；网页版的构建里没有这些代码（模块顶层没有副作用，没用到就整个去掉）。
 */

/** 新版本：版本号、更新说明（没写时是空字符串）、发布时间（RFC 3339，没有时是 null）。 */
export type UpdateInfo = { version: string; notes: string; date: string | null };

/** update_check 的结果：现在用的版本，有新版本时是它，没有时是 null。 */
export type UpdateCheck = { current: string; available: UpdateInfo | null };

/** 下载进度（update://progress）：一共下载了多少字节，一共多大（不知道时是 null）。 */
export type UpdateProgress = { downloaded: number; total: number | null };

export type UpdaterSnapshot = Readonly<{
  /** 现在用的版本：检查过以后是 App 报的，之前是空字符串（界面上用 DesktopState.version） */
  current: string;
  /** 有新版本时是它 */
  available: UpdateInfo | null;
  phase: InstallPhase;
  /** 下载进度。只在 phase 是 installing 时有意义 */
  progress: UpdateProgress | null;
  /** 没装上的原因（phase 是 failed 时） */
  error: string;
  /** 这台电脑上点过的「以后再说」 */
  dismissed: Dismissed | null;
  /** 设置里的「版本」（DesktopUpdate.tsx 的 UpdateSection）正显示着：页面顶上的提示先不显示，同样的内容不出现两遍 */
  inSettings: boolean;
}>;

const EVENT_AVAILABLE = "update://available";
const EVENT_PROGRESS = "update://progress";
const DISMISSED_KEY = "zsmeet.update.dismissed";

let snap: UpdaterSnapshot = { current: "", available: null, phase: "idle", progress: null, error: "", dismissed: null, inSettings: false };
const listeners = new Set<() => void>();

function update(patch: Partial<UpdaterSnapshot>) {
  snap = { ...snap, ...patch };
  for (const l of listeners) l();
}

/** 正在装、装好了：这时不换新版本、不让再点（装的是点「更新并重启」时的那个版本）。 */
const locked = () => snap.phase === "installing" || snap.phase === "installed";

/** 检查出来的新版本（自动检查推过来的，或者手动检查的结果）。换了版本的话，上一个版本没装上的原因不再显示。 */
function setAvailable(available: UpdateInfo | null) {
  if (locked()) return;
  const changed = available?.version !== snap.available?.version;
  update(changed ? { available, phase: "idle", error: "", progress: null } : { available });
}

let started = false;

/** 第一次用到时：读出点过的「以后再说」，订阅 App 推过来的新版本和下载进度。 */
function start() {
  if (started) return;
  started = true;
  let dismissed: Dismissed | null = null;
  try {
    dismissed = parseDismissed(localStorage.getItem(DISMISSED_KEY));
  } catch {
    // 读不到：当成没点过
  }
  update({ dismissed });
  void (async () => {
    try {
      await listen<unknown>(EVENT_AVAILABLE, (e) => setAvailable(normalizeInfo(e.payload)));
      await listen<unknown>(EVENT_PROGRESS, (e) => update({ progress: normalizeProgress(e.payload) }));
    } catch {
      // 订阅不上（不太可能）：只是不会自己提示新版本，设置里照样能检查、能更新
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

/** 自动更新的整体状态。整个 App 只有一份：在哪一页点的「更新并重启」，别的页面上也看得到进度。 */
export function useUpdater(): UpdaterSnapshot {
  return useSyncExternalStore(subscribe, () => snap);
}

// ---- 命令（desktop/src-tauri 里的 update_*）。出错时 Rust 返回给人看的中文 ----

/** 马上检查一次有没有新版本（设置里的「检查更新」）。 */
export async function checkForUpdate(): Promise<UpdateCheck> {
  const result = normalizeCheck(await invoke<unknown>("update_check"));
  if (result.current) update({ current: result.current });
  setAvailable(result.available);
  return result;
}

/**
 * 下载、核对签名、安装新版本，装好以后 App 自己重新打开（这个界面跟着关掉，一般等不到返回）。
 * 不能装（正在录音、有录音没传完）、下载或者核对没通过时，phase 变成 failed，error 是 App 给的原因。
 */
export async function installUpdate(): Promise<void> {
  if (locked()) return;
  update({ phase: "installing", progress: null, error: "" });
  try {
    await invoke("update_install");
    update({ phase: "installed" });
  } catch (e) {
    update({ phase: "failed", progress: null, error: errorMessage(e) });
  }
}

/** 「以后再说」：这个版本先不在页面顶上提示（隔一天再提示，见 updateView.ts 的 noticeVisible），每台电脑记自己的。 */
export function dismissUpdate() {
  const version = snap.available?.version;
  if (!version || locked()) return;
  const dismissed: Dismissed = { version, at: Date.now() };
  try {
    localStorage.setItem(DISMISSED_KEY, JSON.stringify(dismissed));
  } catch {
    // 存不下也没关系：这次照样不显示，App 重新打开以后再提示
  }
  update({ dismissed, phase: "idle", error: "" });
}

let settingsCount = 0;

/** 设置里的「版本」显示着的时候调用：页面顶上的新版本提示先收起来（设置里有同样的内容和按钮）。 */
export function useUpdateInSettings() {
  useEffect(() => {
    settingsCount += 1;
    update({ inSettings: true });
    return () => {
      settingsCount -= 1;
      update({ inSettings: settingsCount > 0 });
    };
  }, []);
}

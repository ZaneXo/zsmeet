// 用 Node 自带的测试运行：pnpm -C web test
// 桌面版一启动就订阅自动更新的消息（startDesktop → startUpdater）。用 Tauri 自己的假 IPC：和真的一样，
// App 推的消息只交给推的时候已经订阅了的，不替还没订阅的留着
import assert from "node:assert/strict";
import { test } from "node:test";
import { emit } from "@tauri-apps/api/event";
import { mockIPC } from "@tauri-apps/api/mocks";
import { startDesktop } from "./desktop.ts";
import type { DesktopState } from "./desktop.ts";
import { getUpdaterSnapshot } from "./updater.ts";

// Tauri 的假 IPC 挂在 window 上；startDesktop 接管站外链接时要 document（这里用不到，给个空的）
const global = globalThis as unknown as { window?: unknown; document?: unknown };
global.window ??= globalThis;
global.document ??= { addEventListener() {} };

const appState: DesktopState = {
  proxy: "http://127.0.0.1:1",
  secret: "s",
  server: "https://meet.example.com",
  user: null,
  signed_in: false,
  login_pending: false,
  expired: false,
  version: "0.1.0",
};

test("一启动就订阅自动更新的消息：还没有显示更新的界面时 App 推过来的新版本也不丢", async () => {
  mockIPC((cmd) => (cmd === "app_state" ? appState : null), { shouldMockEvents: true });
  // 订阅之前推的收不到（Tauri 不替界面留着），所以要一启动就订阅
  await emit("update://available", { version: "0.1.5", notes: "", date: null });
  assert.equal(getUpdaterSnapshot().available, null);

  // DesktopGate 一挂上就 startDesktop。这时还没有哪个界面用到更新状态（比如服务器回得慢，还停在「正在启动」上）
  await startDesktop();
  // App 打开 10 秒左右查到了新版本，只推这一次
  await emit("update://available", { version: "0.2.0", notes: "录音更稳了", date: "2026-11-12T08:00:00Z" });
  assert.deepEqual(getUpdaterSnapshot().available, { version: "0.2.0", notes: "录音更稳了", date: "2026-11-12T08:00:00Z" });
  // 下载进度也一样
  await emit("update://progress", { downloaded: 1024, total: 4096 });
  assert.deepEqual(getUpdaterSnapshot().progress, { downloaded: 1024, total: 4096 });
});

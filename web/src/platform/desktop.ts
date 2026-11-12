import { invoke } from "@tauri-apps/api/core";
import { listen } from "@tauri-apps/api/event";
import type { Middleware } from "openapi-fetch";
import { useSyncExternalStore } from "react";
import { noteResponse } from "./offline";
import { SSEParser } from "./sse";
import type { SSEEvent } from "./sse";

/**
 * 桌面版和 Rust 那边（desktop/src-tauri）打交道的地方。
 *
 * 界面的接口请求都发给 App 里的本地代理（只听 127.0.0.1），由它加上登录令牌转发给服务器。
 * 本地代理要求每个请求带这次启动随机生成的 X-Zsmeet-App；令牌本身界面拿不到。
 */

export type DesktopUser = { id: string; name: string; email: string; role: string };

/** Rust 那边的状态（core.rs 的 AppState）。 */
export type DesktopState = {
  proxy: string;
  secret: string;
  server: string | null;
  user: DesktopUser | null;
  signed_in: boolean;
  login_pending: boolean;
  /** 登录失效了（令牌被吊销、账号被停用），要重新登录 */
  expired: boolean;
  version: string;
};

// ---- 状态：启动时取一次，之后 Rust 每次变化都推过来 ----

let state: DesktopState | null = null;
let loginError = "";
const listeners = new Set<() => void>();
const identityListeners = new Set<() => void>();

const identity = (s: DesktopState | null) => (s?.signed_in ? `${s.server}\n${s.user?.id}` : "");

function notify() {
  for (const l of listeners) l();
}

function setState(next: DesktopState) {
  const changed = identity(next) !== identity(state);
  state = next;
  // 换了人、换了服务器、退出了：先清掉上一个人的数据，再让界面切换
  if (changed) for (const l of identityListeners) l();
  notify();
}

function setLoginError(message: string) {
  loginError = message;
  notify();
}

function subscribe(listener: () => void) {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

/** 当前登录的是谁（服务器加用户），换了就变。没登录时是空字符串。 */
export function currentIdentity(): string {
  return identity(state);
}

/** 换了用户、换了服务器或者退出登录时调用（用来清掉缓存的数据）。 */
export function onIdentityChange(listener: () => void) {
  identityListeners.add(listener);
}

let started: Promise<void> | undefined;

/** 启动：订阅 Rust 推过来的状态，取一次当前状态，接管站外链接。 */
export function startDesktop(): Promise<void> {
  started ??= (async () => {
    await listen<DesktopState>("zsmeet://state", (e) => setState(e.payload));
    await listen<string>("zsmeet://login-error", (e) => setLoginError(e.payload));
    setState(await invoke<DesktopState>("app_state"));
    installLinkHandler();
  })();
  return started;
}

export function useDesktopState(): DesktopState | null {
  return useSyncExternalStore(subscribe, () => state);
}

/** 浏览器登录回来以后没能完成登录的原因。 */
export function useLoginError(): string {
  return useSyncExternalStore(subscribe, () => loginError);
}

function current(): DesktopState {
  if (!state) throw new Error("zsmeet 还在启动，请稍后再试");
  return state;
}

// ---- 命令 ----

export async function setServer(url: string) {
  setState(await invoke<DesktopState>("set_server", { url }));
}

/**
 * 正在退出登录或者切换服务器。两个都会先结束录音、再退出这台服务器上的登录，改的是同一份登录状态，
 * 一起做的话谁后做完界面就停在谁那里。所以一次只做一个：正在做的时候再要做直接报错（SIGNING_OUT）。
 * 这时也不能「在浏览器里登录」：退出时 Rust 会丢掉正在等的登录，刚登录上的也会被一起退出。
 * 界面上这时把这些按钮都变灰：导航栏、账号页、登录页上的都看 useSigningOut。
 */
let signingOut = false;

/** 正在退出登录、切换服务器时，再点退出登录、切换服务器或者登录得到的原因。 */
export const SIGNING_OUT = "正在退出登录，请稍等";

async function signOutWith(command: "logout" | "switch_server") {
  if (signingOut) throw new Error(SIGNING_OUT);
  signingOut = true;
  try {
    setLoginError(""); // 顺带通知了界面：按钮变灰
    setState(await invoke<DesktopState>(command));
  } finally {
    signingOut = false;
    notify();
  }
}

/** 有没有正在退出登录、切换服务器（不管是在哪里点的）。 */
export function useSigningOut(): boolean {
  return useSyncExternalStore(subscribe, () => signingOut);
}

/** 换服务器：退出登录，再回到「填服务器地址」。 */
export function switchServer(): Promise<void> {
  return signOutWith("switch_server");
}

export async function loginStart() {
  if (signingOut) throw new Error(SIGNING_OUT);
  setLoginError("");
  setState(await invoke<DesktopState>("login_start"));
}

export async function loginCancel() {
  setState(await invoke<DesktopState>("login_cancel"));
}

export function logout(): Promise<void> {
  return signOutWith("logout");
}

/** 服务器已经不认这次登录了（/api/status 里没有用户）：在 App 这边也退出，回到登录页。 */
export async function sessionExpired() {
  setState(await invoke<DesktopState>("session_expired"));
}

/** 在系统浏览器里打开站外链接。打不开时 Rust 那边会弹提示框说明原因（commands.rs 的 open_external）。 */
export function openExternal(url: string): Promise<void> {
  return invoke("open_external", { url });
}

/** 在系统浏览器里打开服务器上的页面，比如 /settings。 */
export function openServerPage(path: string): Promise<void> {
  return invoke("open_server_page", { path });
}

/** 导出：Rust 那边取回文件、弹保存对话框。出错时 Rust 会弹提示框，这里不用再管。 */
export function saveExport(path: string): Promise<string | null> {
  return invoke<string | null>("save_export", { path });
}

// ---- 接口请求 ----

export function proxyUrl(path: string): string {
  return current().proxy + path;
}

export function proxyHeaders(): Record<string, string> {
  return { "X-Zsmeet-App": current().secret };
}

/** <audio> 加不了请求头，录音的地址把 secret 放在查询参数 _s 里（本地代理只对录音接口认这个）。 */
export function mediaUrl(path: string): string {
  const s = current();
  return `${s.proxy}${path}${path.includes("?") ? "&" : "?"}_s=${encodeURIComponent(s.secret)}`;
}

/** openapi-fetch 的中间件：请求改发给本地代理，带上 X-Zsmeet-App；看响应是不是离线结果。 */
export const apiMiddleware: Middleware = {
  async onRequest({ request }) {
    const s = current();
    const url = new URL(request.url);
    const headers = new Headers(request.headers);
    headers.set("X-Zsmeet-App", s.secret);
    const hasBody = request.method !== "GET" && request.method !== "HEAD";
    return new Request(s.proxy + url.pathname + url.search, {
      method: request.method,
      headers,
      body: hasBody ? await request.arrayBuffer() : undefined,
      signal: request.signal,
      credentials: "omit",
    });
  },
  onResponse({ response }) {
    noteResponse(response);
    return undefined;
  },
};

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

/**
 * 订阅服务器推送。和 EventSource 一样：断了隔几秒自动重连；服务器说不行（4xx）就不再连。
 */
export function subscribeEvents(path: string, onEvent: (e: SSEEvent) => void): () => void {
  const abort = new AbortController();
  let closed = false;
  const run = async () => {
    while (!closed) {
      try {
        const resp = await fetch(proxyUrl(path), {
          headers: { ...proxyHeaders(), Accept: "text/event-stream" },
          signal: abort.signal,
        });
        noteResponse(resp);
        if (!resp.ok && resp.status < 500) return;
        if (resp.ok && resp.body) {
          const reader = resp.body.pipeThrough(new TextDecoderStream()).getReader();
          const parser = new SSEParser();
          for (;;) {
            const { value, done } = await reader.read();
            if (done) {
              // 最后一条事件可能还在 parser 里（只用「\r」换行时）
              for (const event of parser.end()) onEvent(event);
              break;
            }
            for (const event of parser.push(value)) onEvent(event);
          }
        }
      } catch {
        if (closed) return;
      }
      await sleep(3000);
    }
  };
  void run();
  return () => {
    closed = true;
    abort.abort();
  };
}

/**
 * 站外链接（纪要里的链接、飞书文档）在系统浏览器里打开。App 的窗口里打开新标签页什么也不会发生，
 * 直接跳转又会把整个 App 换成别的网页。
 */
function installLinkHandler() {
  document.addEventListener("click", (e) => {
    if (e.defaultPrevented || e.button !== 0) return;
    const target = e.target instanceof Element ? e.target.closest("a[href]") : null;
    if (!(target instanceof HTMLAnchorElement)) return;
    let url: URL;
    try {
      url = new URL(target.href, window.location.href);
    } catch {
      return;
    }
    if (url.origin === window.location.origin || !["http:", "https:", "mailto:"].includes(url.protocol)) return;
    e.preventDefault();
    // 打不开时 App 已经弹了提示框说明原因，这里只是接住出错，不留没处理的 Promise
    openExternal(url.href).catch(() => {});
  });
}

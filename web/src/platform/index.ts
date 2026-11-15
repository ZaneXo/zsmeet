import type { NetworkMode } from "@tanstack/react-query";
import type { Client } from "openapi-fetch";
import { BrowserRouter, HashRouter } from "react-router";
import type { paths } from "../api/schema";
import { acceptLanguageHeader } from "../i18n/lang";
import * as desktop from "./desktop";
import { isDesktop } from "./env";

/**
 * 网页版和桌面版不一样的地方（docs/desktop.md 的「平台差异」）。界面的其他代码只通过这里区分。
 *
 * |            | 网页版                     | 桌面版                                   |
 * |------------|----------------------------|------------------------------------------|
 * | 接口地址   | 同源 /api，带 Cookie       | App 里的本地代理，带 X-Zsmeet-App         |
 * | 路由       | BrowserRouter              | HashRouter（界面从 tauri://localhost 加载）|
 * | 录音的地址 | /api/meetings/{id}/audio   | 本地代理的地址加 _s                       |
 * | 进度推送   | EventSource                | fetch 读流（EventSource 加不了请求头）    |
 * | 下载       | 浏览器下载                 | 保存对话框（DownloadLink）                |
 * | 站外链接   | 新标签页                   | 系统浏览器                                |
 */

export { isDesktop };

export const Router = isDesktop ? HashRouter : BrowserRouter;

/** 给接口客户端装上桌面版的中间件（请求改发给本地代理）。网页版什么也不做。 */
export function setupApiClient(client: Client<paths>) {
  if (isDesktop) client.use(desktop.apiMiddleware);
}

/**
 * 系统说断网了的时候，取数据、点按钮（TanStack Query 的 networkMode）要不要先停着。
 * 网页版：停着，连上了再接着做（默认的 online）。
 * 桌面版：照常做（always）。请求和命令都先到 App 里（本地代理、Rust），断网时由 App 来答：取过的内容从离线缓存里拿，
 * 录音的暂停、标记、结束和退出登录照常能用。停着的话，断网时连「结束录音」都点不动。
 * 连上以后重新取数据靠「离线」提示条（OfflineBanner），不靠系统的联网通知。
 */
export const networkMode: NetworkMode = isDesktop ? "always" : "online";

/**
 * 桌面版有没有正在退出登录、切换服务器（不管是在导航栏还是账号页上点的，一次只做一个，见 desktop.ts）。
 * 网页版没有这回事，总是 false。
 */
export const useSigningOut: () => boolean = isDesktop ? desktop.useSigningOut : () => false;

/** 直接用 fetch 调接口时（上传分片）的完整地址。 */
export function apiUrl(path: string): string {
  return isDesktop ? desktop.proxyUrl(path) : path;
}

/** 直接用 fetch 调接口时要加的请求头。 */
export function apiHeaders(): Record<string, string> {
  return {
    ...(isDesktop ? desktop.proxyHeaders() : {}),
    "Accept-Language": acceptLanguageHeader(),
  };
}

/** 直接用 fetch 调接口时带不带 Cookie：网页版靠 Cookie 登录，桌面版靠本地代理加的令牌。 */
export function apiCredentials(): RequestCredentials {
  return isDesktop ? "omit" : "same-origin";
}

/** <audio> 这类加不了请求头的地址。 */
export function mediaUrl(path: string): string {
  return isDesktop ? desktop.mediaUrl(path) : path;
}

type SharedStream = {
  refs: number;
  add: (event: string, fn: (data: string) => void) => () => void;
  close: () => void;
};

const streams = new Map<string, SharedStream>();

/** 订阅服务器推送的某一种事件，同一路径自动复用同一条连接，返回取消订阅的函数。 */
export function subscribe(path: string, event: string, onData: (data: string) => void): () => void {
  let shared = streams.get(path);
  if (!shared) {
    if (isDesktop) {
      const listeners = new Set<{ event: string; fn: (data: string) => void }>();
      const stop = desktop.subscribeEvents(path, (e) => {
        for (const l of listeners) {
          if (l.event === e.event) l.fn(e.data);
        }
      });
      shared = {
        refs: 0,
        add(ev, fn) {
          const entry = { event: ev, fn };
          listeners.add(entry);
          return () => listeners.delete(entry);
        },
        close: stop,
      };
    } else {
      const source = new EventSource(path);
      shared = {
        refs: 0,
        add(ev, fn) {
          const handler = (e: Event) => fn((e as MessageEvent<string>).data);
          source.addEventListener(ev, handler);
          return () => source.removeEventListener(ev, handler);
        },
        close: () => source.close(),
      };
    }
    streams.set(path, shared);
  }
  shared.refs++;
  const remove = shared.add(event, onData);
  let done = false;
  return () => {
    if (done) return;
    done = true;
    remove();
    shared.refs--;
    if (shared.refs === 0) {
      streams.delete(path);
      shared.close();
    }
  };
}

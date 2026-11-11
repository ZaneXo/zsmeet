import type { Client } from "openapi-fetch";
import { BrowserRouter, HashRouter } from "react-router";
import type { paths } from "../api/schema";
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

/** 直接用 fetch 调接口时（上传分片）的完整地址。 */
export function apiUrl(path: string): string {
  return isDesktop ? desktop.proxyUrl(path) : path;
}

/** 直接用 fetch 调接口时要加的请求头。 */
export function apiHeaders(): Record<string, string> {
  return isDesktop ? desktop.proxyHeaders() : {};
}

/** 直接用 fetch 调接口时带不带 Cookie：网页版靠 Cookie 登录，桌面版靠本地代理加的令牌。 */
export function apiCredentials(): RequestCredentials {
  return isDesktop ? "omit" : "same-origin";
}

/** <audio> 这类加不了请求头的地址。 */
export function mediaUrl(path: string): string {
  return isDesktop ? desktop.mediaUrl(path) : path;
}

/** 订阅服务器推送的某一种事件，返回取消订阅的函数。 */
export function subscribe(path: string, event: string, onData: (data: string) => void): () => void {
  if (isDesktop) {
    return desktop.subscribeEvents(path, (e) => {
      if (e.event === event) onData(e.data);
    });
  }
  const source = new EventSource(path);
  source.addEventListener(event, (e) => onData((e as MessageEvent<string>).data));
  return () => source.close();
}

import { useSyncExternalStore } from "react";

/**
 * 离线状态（只有桌面版会变）：本地代理连不上服务器时，返回上次同步的内容并带上 `X-Zsmeet-Offline: 1`。
 * 每收到一个本地代理的响应就更新一次。
 */
let offline = false;
const listeners = new Set<() => void>();

export function noteResponse(response: Response) {
  const now = response.headers.get("X-Zsmeet-Offline") === "1";
  if (now === offline) return;
  offline = now;
  for (const l of listeners) l();
}

function subscribe(listener: () => void) {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

export function useOffline(): boolean {
  return useSyncExternalStore(subscribe, () => offline);
}

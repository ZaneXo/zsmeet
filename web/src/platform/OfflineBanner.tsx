import { useEffect, useRef } from "react";
import { queryClient } from "../api/client";
import { Button } from "../components/ui";
import { useOffline } from "./offline";

/** 连不上服务器时每隔多久试一次。 */
const PROBE_MS = 15_000;

/**
 * 「离线」提示条（只有桌面版会出现）：连不上服务器时，界面上显示的是上次同步的内容。
 * 离线期间定时重新取一次状态，连上了就把所有数据重新取一遍，提示条自己消失。
 */
export function OfflineBanner() {
  const offline = useOffline();
  const was = useRef(offline);

  useEffect(() => {
    if (was.current && !offline) void queryClient.invalidateQueries();
    was.current = offline;
    if (!offline) return;
    const timer = setInterval(() => void queryClient.invalidateQueries({ queryKey: ["status"] }), PROBE_MS);
    return () => clearInterval(timer);
  }, [offline]);

  if (!offline) return null;
  return (
    <div role="status" className="flex flex-wrap items-center justify-between gap-3 rounded-xl bg-warn-tint px-4 py-3 text-sm leading-relaxed text-warn">
      <span>离线，显示的是上次同步的内容。连上服务器以后会自动更新。</span>
      <Button size="sm" onClick={() => void queryClient.invalidateQueries()}>
        重新连接
      </Button>
    </div>
  );
}

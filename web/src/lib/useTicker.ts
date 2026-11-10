import { useEffect, useState } from "react";

/** 每隔 interval 毫秒重新渲染一次，给计时、「多久以前」这类跟着时间变的显示用。 */
export function useTicker(interval: number, enabled = true) {
  const [, setTick] = useState(0);
  useEffect(() => {
    if (!enabled) return;
    const timer = setInterval(() => setTick((n) => n + 1), interval);
    return () => clearInterval(timer);
  }, [interval, enabled]);
}

import { useEffect, useRef, useState } from "react";
import { hms } from "../lib/format";
import { Card } from "./ui";

// 网页录音和桌面版录音共用的几块界面：音量条、标记的重点。

/** 音量条显示最近多少次的音量（每 100 毫秒一次，约 4 秒）。 */
const BARS = 40;

const silence = () => new Array<number>(BARS).fill(0);

/** 音量条：最近几秒的音量，从左往右滚动。read 读出现在的音量（0 到 1），每 100 毫秒读一次。 */
export function LevelMeter({ read, active, paused = false }: { read: () => number; active: boolean; paused?: boolean }) {
  const [levels, setLevels] = useState<number[]>(silence);
  // 每次渲染传进来的 read 都是新的函数，记在 ref 里，免得计时器跟着重启
  const reader = useRef(read);
  useEffect(() => {
    reader.current = read;
  });
  useEffect(() => {
    if (!active) {
      setLevels(silence());
      return;
    }
    const timer = setInterval(() => {
      const level = paused ? 0 : reader.current();
      setLevels((prev) => [...prev.slice(1), level]);
    }, 100);
    return () => clearInterval(timer);
  }, [active, paused]);
  return (
    <div className="flex h-8 flex-1 items-center gap-[3px]" aria-hidden="true">
      {levels.map((level, i) => (
        <span
          key={i}
          className={`w-1 shrink-0 rounded-full ${paused || !active ? "bg-line-strong" : "bg-brand"}`}
          style={{ height: `${Math.max(8, Math.min(1, level) * 100)}%` }}
        />
      ))}
    </div>
  );
}

/** 录音时标记的重点：第几个、在录音的第几秒。 */
export function RecordingMarks({ marks }: { marks: readonly number[] }) {
  return (
    <Card className="flex flex-col gap-2.5 p-6">
      <b className="text-[15px] font-semibold">标记的重点（会写进纪要）</b>
      {marks.length === 0 ? (
        <span className="text-[13px] text-muted">有人说到关键内容时点「标记重点」。</span>
      ) : (
        <ol className="flex flex-wrap gap-2">
          {marks.map((t, i) => (
            // 标记只会往后加，用下标当 key 没问题（两次标记可能落在同一秒）
            <li key={i} className="rounded-lg bg-subtle px-2.5 py-1 text-[13px] tabular-nums">
              重点 {i + 1} · {hms(t)}
            </li>
          ))}
        </ol>
      )}
    </Card>
  );
}

import { useState } from "react";
import type { Segment } from "../api/client";
import { Input, Spinner } from "../components/ui";
import { clock, speakerColor, speakerName } from "../lib/format";

/** 转写：一段一段列出来，点任意一段从那里开始播放，正在播放的那段会高亮。 */
export function TranscriptPane({
  segments,
  loading,
  now,
  canSeek,
  onSeek,
}: {
  segments: Segment[];
  loading: boolean;
  now: number;
  canSeek: boolean;
  onSeek: (sec: number) => void;
}) {
  const [q, setQ] = useState("");
  const keyword = q.trim();

  if (loading) return <Spinner label="正在加载转写…" />;
  if (segments.length === 0) {
    return <p className="px-6 py-10 text-center text-sm text-muted">还没有转写。识别完成后会出现在这里。</p>;
  }

  const shown = keyword ? segments.filter((s) => s.text.includes(keyword)) : segments;
  const speakers = new Set(segments.map((s) => s.speaker).filter((s) => s >= 0)).size;

  return (
    <div className="flex flex-col gap-2 px-4 pb-6 pt-4">
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2 px-2">
        <Input type="search" className="max-w-xs" placeholder="在转写里搜索" aria-label="在转写里搜索" value={q} onChange={(e) => setQ(e.target.value)} />
        <span className="text-xs text-muted">
          {speakers > 0 ? `识别出 ${speakers} 位说话人。` : ""}
          {canSeek ? "点任意一段从那里开始播放。" : ""}
        </span>
      </div>

      {shown.length === 0 && <p className="px-2 py-6 text-sm text-muted">转写里没有「{keyword}」。</p>}

      {shown.map((s) => {
        const active = canSeek && now >= s.start && now < s.end;
        const body = (
          <>
            <span className="w-12 shrink-0 pt-0.5 text-xs tabular-nums text-muted">{clock(s.start)}</span>
            <span className="flex min-w-0 flex-col gap-1">
              <span className="text-[13px] font-semibold" style={{ color: speakerColor(s.speaker) }}>
                {speakerName(s.speaker)}
              </span>
              <span className="text-[15px] leading-relaxed text-ink">
                <Highlight text={s.text} keyword={keyword} />
              </span>
            </span>
          </>
        );
        const rowClass = `flex w-full gap-3 rounded-xl px-3 py-2.5 text-left ${active ? "bg-brand-tint" : ""}`;
        return canSeek ? (
          <button key={s.start} type="button" onClick={() => onSeek(s.start)} className={`${rowClass} cursor-pointer hover:bg-canvas`}>
            {body}
          </button>
        ) : (
          <div key={s.start} className={rowClass}>
            {body}
          </div>
        );
      })}
    </div>
  );
}

function Highlight({ text, keyword }: { text: string; keyword: string }) {
  if (!keyword) return <>{text}</>;
  const parts = text.split(keyword);
  return (
    <>
      {parts.map((part, i) => (
        // 片段的顺序固定，用下标当 key 没问题
        <span key={i}>
          {i > 0 && <mark className="rounded-sm bg-[#ffe58a] px-px text-ink">{keyword}</mark>}
          {part}
        </span>
      ))}
    </>
  );
}

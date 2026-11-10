import { Fragment, useEffect, useRef, useState } from "react";
import type { KeyboardEvent, ReactNode } from "react";
import type { Segment } from "../api/client";
import { Input, Spinner } from "../components/ui";
import { clock, needsSpace, speakerColor, speakerName } from "../lib/format";

/** 段落里相邻两句隔得超过这么多秒，就另起一段 */
const PARAGRAPH_GAP = 2.5;
/** 一段最多这么多字，再多就另起一段，太长不好读 */
const PARAGRAPH_CHARS = 280;

type Paragraph = { speaker: number; start: number; sentences: { seg: Segment; index: number }[] };

/** 把逐句的转写按「同一个人、中间没停太久」合成段落，读起来和平常的文字记录一样。 */
function paragraphs(segments: Segment[]): Paragraph[] {
  const out: Paragraph[] = [];
  let chars = 0;
  segments.forEach((seg, index) => {
    const last = out.at(-1);
    const prev = last?.sentences.at(-1)?.seg;
    if (last && prev && last.speaker === seg.speaker && seg.start - prev.end <= PARAGRAPH_GAP && chars + seg.text.length <= PARAGRAPH_CHARS) {
      last.sentences.push({ seg, index });
      chars += seg.text.length;
      return;
    }
    out.push({ speaker: seg.speaker, start: seg.start, sentences: [{ seg, index }] });
    chars = seg.text.length;
  });
  return out;
}

/** 某个时间点正在说的那一句（最后一句开始得不比它晚的），没有返回 -1。 */
export function sentenceAt(segments: Segment[], sec: number): number {
  let found = -1;
  segments.forEach((s, i) => {
    if (s.start <= sec) found = i;
  });
  return found;
}

/**
 * 转写：按段落显示，每一句都能点，点了从那句开始播放；正在播放的那句高亮。
 * focus 是从搜索结果跳过来时要定位的时间点：滚到那时正在说的那一句并标出来。
 */
export function TranscriptPane({
  segments,
  loading,
  now,
  canSeek,
  onSeek,
  focus,
  names,
}: {
  segments: Segment[];
  loading: boolean;
  now: number;
  canSeek: boolean;
  onSeek: (sec: number) => void;
  focus?: number;
  /** 给说话人起的名字 */
  names?: Record<string, string>;
}) {
  const [q, setQ] = useState("");
  const keyword = q.trim();
  const refs = useRef(new Map<number, HTMLElement>());
  const focused = focus === undefined ? -1 : sentenceAt(segments, focus);

  // 跳过来的那一句滚到中间
  useEffect(() => {
    if (focused >= 0) refs.current.get(focused)?.scrollIntoView({ block: "center", behavior: "smooth" });
  }, [focused, segments.length]);

  if (loading) return <Spinner label="正在加载转写…" />;
  if (segments.length === 0) {
    return <p className="px-6 py-10 text-center text-sm text-muted">还没有转写。识别完成后会出现在这里。</p>;
  }

  const speakers = new Set(segments.map((s) => s.speaker).filter((s) => s >= 0)).size;
  const sentence = (seg: Segment, index: number) => {
    const active = canSeek && now >= seg.start && now < seg.end;
    const className = `rounded-md px-0.5 transition-colors ${active ? "bg-brand-tint" : index === focused ? "bg-[#fff3c4]" : ""} ${
      canSeek ? "cursor-pointer hover:bg-canvas" : ""
    }`;
    const seek = () => onSeek(seg.start);
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Enter" || e.key === " ") {
        e.preventDefault();
        seek();
      }
    };
    return (
      <span
        key={index}
        ref={(el) => {
          if (el) refs.current.set(index, el);
          else refs.current.delete(index);
        }}
        className={className}
        {...(canSeek ? { role: "button", tabIndex: 0, onClick: seek, onKeyDown: onKey, title: `从 ${clock(seg.start)} 开始播放` } : {})}
      >
        <Highlight text={seg.text} keyword={keyword} />
      </span>
    );
  };

  const shown = keyword ? segments.map((seg, index) => ({ seg, index })).filter(({ seg }) => seg.text.includes(keyword)) : [];

  return (
    <div className="flex flex-col gap-2 px-4 pb-6 pt-4">
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2 px-2">
        <Input type="search" className="max-w-xs" placeholder="在转写里搜索" aria-label="在转写里搜索" value={q} onChange={(e) => setQ(e.target.value)} />
        <span className="text-xs text-muted">
          {speakers > 0 ? `识别出 ${speakers} 位说话人。` : ""}
          {canSeek ? "点任意一句从那里开始播放。" : ""}
        </span>
      </div>

      {keyword ? (
        shown.length === 0 ? (
          <p className="px-2 py-6 text-sm text-muted">转写里没有「{keyword}」。</p>
        ) : (
          shown.map(({ seg, index }) => (
            <Row key={index} speaker={seg.speaker} start={seg.start} names={names}>
              {sentence(seg, index)}
            </Row>
          ))
        )
      ) : (
        paragraphs(segments).map((p) => (
          <Row key={p.sentences[0]?.index ?? p.start} speaker={p.speaker} start={p.start} names={names}>
            {p.sentences.map(({ seg, index }, i) => {
              // 英文的两句之间留一个空格（复制、页面内查找时也是分开的），中文直接接上
              const prev = p.sentences[i - 1]?.seg.text;
              return (
                <Fragment key={index}>
                  {prev !== undefined && needsSpace(prev, seg.text) && " "}
                  {sentence(seg, index)}
                </Fragment>
              );
            })}
          </Row>
        ))
      )}
    </div>
  );
}

function Row({ speaker, start, names, children }: { speaker: number; start: number; names?: Record<string, string>; children: ReactNode }) {
  return (
    <div className="flex gap-3 px-3 py-2.5">
      <span className="w-12 shrink-0 pt-0.5 text-xs tabular-nums text-muted">{clock(start)}</span>
      <div className="flex min-w-0 flex-col gap-1">
        <span className="text-[13px] font-semibold" style={{ color: speakerColor(speaker) }}>
          {speakerName(speaker, names)}
        </span>
        <p className="text-[15px] leading-relaxed text-ink">{children}</p>
      </div>
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

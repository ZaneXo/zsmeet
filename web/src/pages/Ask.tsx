import { useMutation } from "@tanstack/react-query";
import { useState } from "react";
import type { FormEvent, ReactNode } from "react";
import { AppLink } from "../components/AppLink";
import { api, errorMessage, unwrap } from "../api/client";
import type { AskAnswer, Citation } from "../api/client";
import { Button, Icon, Input, Notice } from "../components/ui";
import { clock, dateTime } from "../lib/format";
import { jumpParam } from "../lib/jump";

/** 问题最长多少个字，和后端一致。 */
const MAX_QUESTION = 300;

/** 出处在会议页面上的 ?t= 秒数，没有时间点时是 undefined。写法和搜索结果的跳转一致（见 jumpParam）。 */
function citationTime(c: Citation): string | undefined {
  return c.at < 0 ? undefined : jumpParam(c.at, c.source === "transcript");
}

/** 「2026-11-02」写成「11月2日」，不是今年的带上年份。 */
function day(date: string): string {
  const [y, m, d] = date.split("-").map(Number);
  const year = y === new Date().getFullYear() ? "" : `${y}年`;
  return `${year}${m}月${d}日`;
}

type Props = {
  /** 只问这一场会议；不传时在所有能看的会议里找 */
  meetingId?: string;
  /** 打开时输入框里预先填好的问题（比如搜索框里已经输入的词） */
  initial?: string;
  /** 在会议页面上：点出处时在原地跳到那个时间点。不传时出处是链接，打开那场会议并从那句话开始播放 */
  onCite?: (c: Citation) => void;
};

/**
 * 问会议：用自然语言提问，管理员配置的大模型根据会议记录回答，回答里的 [1] [2] 是出处，点了听原话。
 */
export function AskPanel({ meetingId, initial = "", onCite }: Props) {
  const [question, setQuestion] = useState(initial);
  const ask = useMutation({
    mutationFn: (q: string) => unwrap(api.POST("/api/ask", { body: { question: q, meeting_id: meetingId } })),
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    const q = question.trim();
    if (q && !ask.isPending) ask.mutate(q);
  };

  return (
    <div className="flex flex-col gap-3">
      <form onSubmit={submit} className="flex flex-wrap gap-2.5">
        <Input
          className="min-w-0 flex-[1_1_280px]"
          placeholder={meetingId ? "问这场会议，比如：最后定了谁负责？" : "问一个关于会议的问题，比如：上周关于报价定了什么？"}
          aria-label={meetingId ? "问这场会议" : "问会议"}
          value={question}
          onChange={(e) => setQuestion(e.target.value)}
          maxLength={MAX_QUESTION}
          autoFocus
        />
        <Button type="submit" variant="primary" disabled={ask.isPending || !question.trim()}>
          <Icon name="ask" />
          提问
        </Button>
      </form>

      {ask.isPending && (
        <div className="flex items-center gap-3 text-sm text-muted" role="status">
          <span className="size-4 animate-spin rounded-full border-2 border-line-strong border-t-brand" aria-hidden="true" />
          {meetingId ? "正在读这场会议的纪要和转写，等大模型回答…" : "正在翻会议记录，等大模型回答…"}一般要十几秒到一分钟。
        </div>
      )}
      {ask.isError && <Notice tone="bad">{errorMessage(ask.error)}</Notice>}
      {ask.isSuccess && <AnswerView answer={ask.data} single={!!meetingId} onCite={onCite} />}

      <p className="text-xs leading-relaxed text-muted">
        回答由管理员配置的大模型根据{meetingId ? "这场会议的" : "你能看到的会议的"}纪要和转写写成，可能有错，重要的事请点出处核对原话。
      </p>
    </div>
  );
}

function AnswerView({ answer, single, onCite }: { answer: AskAnswer; single: boolean; onCite?: (c: Citation) => void }) {
  const byN = new Map(answer.citations.map((c) => [c.n, c]));
  const ref = (c: Citation, children: ReactNode, className: string) => {
    const label = `${single ? "" : `${c.title} `}${c.at >= 0 ? clock(c.at) : "纪要"}`;
    if (onCite) {
      return (
        <button type="button" className={`cursor-pointer ${className}`} title={c.at >= 0 ? `从 ${label} 开始播放` : "看纪要"} onClick={() => onCite(c)}>
          {children}
        </button>
      );
    }
    const t = citationTime(c);
    return (
      <AppLink to={t === undefined ? `/meetings/${c.meeting_id}` : `/meetings/${c.meeting_id}?t=${t}`} className={className} title={`打开 ${label}`}>
        {children}
      </AppLink>
    );
  };

  const searched =
    answer.keywords.length > 0 || answer.from || answer.to ? (
      <>
        {answer.keywords.length > 0 && <>搜了{answer.keywords.map((k) => `「${k}」`).join("")}。</>}
        {(answer.from || answer.to) && (
          <>
            只看{answer.from ? day(answer.from) : "以前"}到{answer.to ? day(answer.to) : "今天"}的会议。
          </>
        )}
      </>
    ) : null;

  return (
    <div className="flex flex-col gap-3 rounded-xl border border-line bg-canvas px-4 py-3.5">
      <div className="flex flex-col gap-2 text-[15px] leading-relaxed text-ink">
        {answer.answer
          .split("\n")
          .filter((line) => line.trim() !== "")
          .map((line, i) => (
            // 回答的段落顺序固定，用下标当 key 没问题
            <p key={i}>
              {line.split(/(\[\d+\])/).map((part, j) => {
                const c = /^\[(\d+)\]$/.test(part) ? byN.get(Number(part.slice(1, -1))) : undefined;
                if (!c) return <span key={j}>{part}</span>;
                return (
                  <sup key={j} className="mx-px">
                    {ref(c, c.n, "inline-flex min-w-[18px] justify-center rounded bg-brand-tint px-1 text-[11px] font-semibold tabular-nums text-brand-ink hover:bg-[#d7e3fa]")}
                  </sup>
                );
              })}
            </p>
          ))}
      </div>

      {answer.citations.length > 0 && (
        <ol className="-mx-2 flex flex-col gap-0.5 border-t border-line pt-2.5">
          {answer.citations.map((c) => (
            <li key={c.n}>
              {ref(
                c,
                <>
                  <span className="w-6 shrink-0 tabular-nums font-semibold text-brand-ink">[{c.n}]</span>
                  <span className="min-w-0 flex-1">
                    <span className="text-muted">
                      {single ? "" : `${c.title} · ${dateTime(c.date)} · `}
                      {c.at >= 0 ? clock(c.at) : "纪要"}
                      {c.source === "minutes" && c.at >= 0 ? "（纪要）" : ""}
                    </span>
                    <span className="block text-ink-2">{c.quote}</span>
                  </span>
                </>,
                "flex w-full gap-2 rounded-lg px-2 py-1.5 text-left text-[13px] leading-relaxed hover:bg-white",
              )}
            </li>
          ))}
        </ol>
      )}

      {(searched || answer.meetings_searched > 0) && (
        <p className="text-xs leading-relaxed text-muted">
          {searched}
          {answer.meetings_searched > 0 && !single && <>参考了最相关的 {answer.meetings_searched} 场会议。</>}
        </p>
      )}
    </div>
  );
}

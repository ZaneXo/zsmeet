import { useQuery } from "@tanstack/react-query";
import { useEffect, useMemo, useRef, useState } from "react";
import { api, queryClient, unwrap } from "../api/client";
import type { Captions } from "../api/client";
import { tr, useLang } from "../i18n/react";
import { clock } from "../lib/format";
import { groupCaptionLines, isNearBottom } from "../lib/liveCaptions";
import { subscribe } from "../platform";
import { Button, Card, Chip } from "./ui";

/**
 * 录音过程中的实时字幕卡片：
 * - streaming 为 true（还在录音 / 等分片上传）时订阅 SSE captions 事件，并每 4 秒轮询兜底；
 * - streaming 为 false（刚结束录音、后台正在做完整转写和区分说话人）时保留已识别出的草稿供浏览，不显示空白等待框。
 */
export function LiveCaptions({
  meetingId,
  streaming,
  paused = false,
}: {
  meetingId: string;
  streaming: boolean;
  paused?: boolean;
}) {
  useLang();
  const [collapsed, setCollapsed] = useState(false);
  const [stickToBottom, setStickToBottom] = useState(true);
  const listRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!meetingId || !streaming) return;
    return subscribe(`/api/meetings/${meetingId}/events`, "captions", (data) => {
      queryClient.setQueryData(["captions", meetingId], JSON.parse(data) as Captions);
    });
  }, [meetingId, streaming]);

  const captions = useQuery({
    queryKey: ["captions", meetingId],
    queryFn: () => unwrap(api.GET("/api/meetings/{id}/captions", { params: { path: { id: meetingId } } })),
    enabled: Boolean(meetingId),
    refetchInterval: streaming ? 4_000 : false,
  });

  const lines = useMemo(
    () => groupCaptionLines(captions.data?.segments ?? [], captions.data?.finalized_count ?? 0),
    [captions.data?.segments, captions.data?.finalized_count],
  );

  const scrollKey = lines.map((l) => `${l.start}:${l.text}`).join("|");
  useEffect(() => {
    const el = listRef.current;
    if (!el || !stickToBottom || collapsed) return;
    el.scrollTop = el.scrollHeight;
  }, [scrollKey, stickToBottom, collapsed]);

  // 录音已停止且一个字都没录出来时，不占地方显示空框
  if (!streaming && lines.length === 0) return null;

  const jumpToBottom = () => {
    const el = listRef.current;
    setStickToBottom(true);
    if (el) el.scrollTop = el.scrollHeight;
  };

  return (
    <Card className="flex flex-col gap-3 p-6">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex items-center gap-2.5">
          <b className="text-[15px] font-semibold">{tr("实时字幕", "Live Captions")}</b>
          {streaming ? (
            <Chip tone={paused ? "mute" : "info"}>
              {paused ? tr("已暂停", "Paused") : tr("边录边识", "Streaming")}
            </Chip>
          ) : (
            <Chip tone="mute">{tr("录音期草稿", "Draft")}</Chip>
          )}
        </div>
        <div className="flex items-center gap-1.5">
          {!collapsed && !stickToBottom && lines.length > 0 && (
            <Button size="sm" variant="ghost" onClick={jumpToBottom}>
              {tr("回到最新 ↓", "Latest ↓")}
            </Button>
          )}
          <Button size="sm" variant="ghost" aria-expanded={!collapsed} onClick={() => setCollapsed((v) => !v)}>
            {collapsed ? tr("展开", "Show") : tr("收起", "Hide")}
          </Button>
        </div>
      </div>

      {!collapsed && (
        <>
          {lines.length === 0 ? (
            <p className="text-[13px] leading-relaxed text-muted">
              {paused
                ? tr("录音已暂停，继续说话后几秒内会在这里出字。", "Recording is paused. Words will appear within seconds once you resume speaking.")
                : tr(
                    "正在听…说话后几秒内会在这里出字。录音结束后还会自动区分说话人、校对错字并生成纪要。",
                    "Listening… words appear within seconds of speaking. Full speaker separation, proofreading, and minutes run automatically after recording ends.",
                  )}
            </p>
          ) : (
            <>
              <div
                ref={listRef}
                role="log"
                aria-live="polite"
                aria-relevant="additions text"
                onScroll={(e) => {
                  const el = e.currentTarget;
                  setStickToBottom(isNearBottom(el.scrollTop, el.clientHeight, el.scrollHeight));
                }}
                className="flex max-h-72 flex-col gap-2.5 overflow-y-auto pr-1 text-sm leading-relaxed"
              >
                {lines.map((line, idx) => (
                  <div key={`${idx}-${line.start}`} className="flex items-baseline gap-3">
                    <span className="shrink-0 font-mono text-xs tabular-nums text-muted">{clock(line.start)}</span>
                    <p
                      className={line.provisional ? "text-ink-2" : "text-ink"}
                      title={
                        line.provisional
                          ? tr("下一片录音传上来后会结合上下文补全定稿", "Provisional — refines once the next audio chunk arrives")
                          : undefined
                      }
                    >
                      {line.text}
                      {line.provisional && streaming && (
                        <span className="ml-1 inline-block text-muted" aria-hidden="true">
                          …
                        </span>
                      )}
                    </p>
                  </div>
                ))}
              </div>
              <p className="border-t border-line pt-2.5 text-xs leading-relaxed text-muted">
                {streaming
                  ? tr(
                      "边录边出字供参考；录音结束后会自动重新区分说话人、校对错字并生成纪要。",
                      "Live draft for reference — full speaker separation, proofreading, and minutes run automatically after recording ends.",
                    )
                  : tr(
                      "以上是录音期间识别出的草稿；上方处理完成后会自动换成区分好说话人的正式转写。",
                      "Draft captured while recording — will automatically be replaced by the speaker-separated transcript once processing finishes.",
                    )}
              </p>
            </>
          )}
        </>
      )}
    </Card>
  );
}

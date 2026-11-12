import { useMutation, useQuery } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { AppLink } from "../components/AppLink";
import { api, errorMessage, queryClient, unwrap } from "../api/client";
import type { Meeting, MeetingListItem, SearchHit } from "../api/client";
import { Button, buttonClass, Chip, Icon, Input, Notice, Spinner } from "../components/ui";
import type { Tone } from "../components/ui";
import { clock, dateTime, dayGroup, duration, snippet, SOURCE_LABEL, speakerName, STAGE_LABEL } from "../lib/format";
import { jumpParam } from "../lib/jump";
import { waitingForRecording } from "../lib/meetingStatus";
import { useTicker } from "../lib/useTicker";
import { isDesktop } from "../platform";
import { TrashList } from "./Trash";
import { UploadDialog } from "./UploadDialog";
import { VISIBILITY_LABEL } from "./AccessDialog";
import { AskPanel } from "./Ask";

/** 网页录音的会议多久没收到新的分片，就不再显示「录音中」（录音的网页可能关掉了、断网了，或者暂停着）。 */
const RECORDING_STALE_MS = 90_000;

/** 最近一直有新的录音分片传上来。 */
export function chunksFlowing(m: Meeting): boolean {
  const last = Date.parse(m.last_chunk_at ?? m.created_at);
  return Date.now() - last < RECORDING_STALE_MS;
}

/** 网页录音还在进行：最近一直有新的分片传上来。 */
export function recordingLive(m: Meeting): boolean {
  return m.source === "record" && m.status === "uploading" && chunksFlowing(m);
}

/** 会议在列表里显示的状态标签。 */
export function meetingChip(m: Meeting): { tone: Tone; text: string } {
  switch (m.status) {
    case "uploading":
      if (m.source === "record") return recordingLive(m) ? { tone: "info", text: "录音中" } : { tone: "mute", text: "录音未传完" };
      // 桌面版录的：录音从那台电脑传上来，断网时联网后接着传
      if (m.source === "desktop") return chunksFlowing(m) ? { tone: "info", text: "电脑录音中" } : { tone: "mute", text: "等电脑传完录音" };
      return { tone: "mute", text: "上传未完成" };
    case "queued":
      return { tone: "mute", text: "排队中" };
    case "processing": {
      const pct = m.progress > 0 && m.progress < 1 ? ` ${Math.round(m.progress * 100)}%` : "";
      return { tone: "info", text: `${STAGE_LABEL[m.stage]}${pct}` };
    }
    case "failed":
      return { tone: "bad", text: "处理失败" };
    case "done":
      return m.minutes_version > 0 ? { tone: "ok", text: "已完成" } : { tone: "warn", text: "已转写，还没有纪要" };
  }
}

const isActive = (m: Meeting) => m.status === "queued" || m.status === "processing";

export function MeetingsPage({ llmReady }: { llmReady: boolean }) {
  const [input, setInput] = useState("");
  // 「问会议」的面板开着没有
  const [asking, setAsking] = useState(false);
  const [q, setQ] = useState("");
  const [uploading, setUploading] = useState(false);
  const [mine, setMine] = useState(false);
  // 看的是回收站，不是会议列表
  const [trash, setTrash] = useState(false);

  // 停止输入 300 毫秒后再搜，不用每敲一个字就请求一次
  useEffect(() => {
    const timer = setTimeout(() => setQ(input.trim()), 300);
    return () => clearTimeout(timer);
  }, [input]);

  const meetings = useQuery({
    queryKey: ["meetings", q, mine],
    enabled: !trash,
    queryFn: () => unwrap(api.GET("/api/meetings", { params: { query: { q: q || undefined, mine: mine || undefined } } })),
    // 有会议在处理时每 3 秒刷新一次，让状态和进度跟上；有人在录音（网页录音、桌面版录音）时每 15 秒刷新一次
    refetchInterval: (query) => {
      const items = query.state.data?.items ?? [];
      return items.some(isActive) ? 3000 : items.some(waitingForRecording) ? 15_000 : false;
    },
    placeholderData: (previous) => previous,
  });

  // 有人在录音时定时重画：录音中断以后，标签要从「录音中」变成「录音未传完」（桌面版录的从「电脑录音中」变成
  // 「等电脑传完录音」），即使服务器返回的数据没变
  useTicker(15_000, (meetings.data?.items ?? []).some(waitingForRecording));

  const retry = useMutation({
    mutationFn: (id: string) => unwrap(api.POST("/api/meetings/{id}/retry", { params: { path: { id } } })),
    onSettled: () => queryClient.invalidateQueries({ queryKey: ["meetings"] }),
  });

  const items = meetings.data?.items ?? [];
  // 除了输入的词，还搜了哪些写法（第一个就是输入的词）
  const expanded = (meetings.data?.expanded ?? []).slice(1);
  const groups: { label: string; items: MeetingListItem[] }[] = [];
  for (const m of items) {
    const label = dayGroup(m.created_at);
    const last = groups[groups.length - 1];
    if (last?.label === label) last.items.push(m);
    else groups.push({ label, items: [m] });
  }

  return (
    <div className="mx-auto flex max-w-5xl flex-col gap-5">
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div className="flex flex-col gap-1.5">
          <h1 className="text-[26px] font-bold">会议</h1>
          <p className="text-sm text-muted">
            {isDesktop ? "录下电脑里会议软件的声音和麦克风" : "在网页上录音"}
            ，或者上传录音文件，自动转写、区分说话人并生成纪要。会议默认团队里的人都能看到，创建者可以改成只给指定的人看。
          </p>
        </div>
        <div className="flex flex-wrap items-center gap-2.5">
          <label className="relative block w-72 max-w-full">
            <span className="pointer-events-none absolute left-3 top-[11px] text-muted">
              <Icon name="search" />
            </span>
            <Input type="search" className="pl-10" placeholder="搜索标题、纪要、转写原文" aria-label="搜索会议" value={input} onChange={(e) => setInput(e.target.value)} maxLength={100} />
          </label>
          {llmReady && (
            <Button aria-expanded={asking} onClick={() => setAsking(!asking)} title="用一句话提问，大模型根据会议记录回答并给出出处">
              <Icon name="ask" />
              问会议
            </Button>
          )}
          <AppLink to="/record" className={buttonClass("primary")}>
            <Icon name="mic" />
            开始录音
          </AppLink>
          <Button onClick={() => setUploading(true)}>
            <Icon name="upload" />
            上传录音
          </Button>
        </div>
      </div>

      {llmReady && asking && (
        <section aria-label="问会议" className="flex flex-col gap-3 rounded-2xl border border-line bg-white px-5 py-4">
          <div className="flex items-center justify-between gap-3">
            <h2 className="text-base font-semibold">问会议</h2>
            <Button variant="ghost" size="sm" aria-label="收起" onClick={() => setAsking(false)}>
              <Icon name="close" />
            </Button>
          </div>
          <AskPanel initial={input.trim()} />
        </section>
      )}

      <div role="tablist" className="flex border-b border-line">
        {(
          [
            [false, "全部"],
            [true, "我创建的"],
          ] as const
        ).map(([value, label]) => (
          <button
            key={label}
            type="button"
            role="tab"
            aria-selected={!trash && mine === value}
            onClick={() => {
              setMine(value);
              setTrash(false);
            }}
            className={`-mb-px mr-5 min-h-10 cursor-pointer border-b-2 px-1 text-sm ${
              !trash && mine === value ? "border-ink font-semibold text-ink" : "border-transparent text-muted hover:text-ink"
            }`}
          >
            {label}
          </button>
        ))}
        <button
          type="button"
          role="tab"
          aria-selected={trash}
          onClick={() => setTrash(true)}
          className={`-mb-px ml-auto min-h-10 cursor-pointer border-b-2 px-1 text-sm ${
            trash ? "border-ink font-semibold text-ink" : "border-transparent text-muted hover:text-ink"
          }`}
        >
          回收站
        </button>
      </div>

      {trash ? (
        <TrashList />
      ) : (
        <>

      {retry.isError && <Notice tone="bad">{errorMessage(retry.error)}</Notice>}
      {meetings.isError && <Notice tone="bad">{errorMessage(meetings.error)}</Notice>}
      {meetings.isPending && <Spinner label="正在加载会议…" />}

      {meetings.isSuccess && q && (
        <p className="text-[13px] leading-relaxed text-muted">
          在{mine ? "你创建的会议的" : ""}标题、纪要和转写原文里找到 {items.length} 场会议。
          {expanded.length > 0 && <>同时搜了{expanded.map((e) => `「${e}」`).join("")}：会议的术语表里记着它们是同一个词的不同写法（转写有时会听错）。</>}
          {items.some((m) => m.hits.length > 0) && "点下面的句子，从会上说这句话的地方开始听。"}
          {items.some((m) => m.hits.some((h) => h.fuzzy)) && "标着「近似」的是错了一个字的写法，可能是转写听错了，也可能是别的词。"}
          {meetings.data?.partial && "「错一个字」的近似匹配只查了最近的一部分会议。"}
        </p>
      )}

      {meetings.isSuccess && items.length === 0 && (
        <div className="rounded-2xl border border-dashed border-line-strong bg-white px-5 py-14 text-center text-sm leading-relaxed text-muted">
          {q && mine ? (
            <div className="flex flex-col items-center gap-4">
              <span>你创建的会议里没有找到包含「{q}」的。别人创建的会议在「全部」里。</span>
              <Button onClick={() => setMine(false)}>在全部会议里找</Button>
            </div>
          ) : q ? (
            <>没有找到包含「{q}」的会议。试试换个关键词，或者搜会上有人说过的原话。</>
          ) : mine ? (
            <>你还没有创建过会议。别人创建的会议在「全部」里。</>
          ) : (
            <div className="flex flex-col items-center gap-4">
              <span>还没有会议。开一场会录下来，或者上传一段会议录音，几分钟后就能看到转写和纪要。</span>
              <span className="flex flex-wrap justify-center gap-2.5">
                <AppLink to="/record" className={buttonClass("primary")}>
                  <Icon name="mic" />
                  开始录音
                </AppLink>
                <Button onClick={() => setUploading(true)}>
                  <Icon name="upload" />
                  上传录音
                </Button>
              </span>
            </div>
          )}
        </div>
      )}

      {groups.map((group) => (
        <section key={group.label} className="flex flex-col gap-2.5">
          <h2 className="text-[13px] font-semibold text-muted">{group.label}</h2>
          {group.items.map((m) => {
            const chip = meetingChip(m);
            return (
              <div key={m.id} className="flex flex-wrap items-start gap-x-5 gap-y-3 rounded-xl border border-line bg-white px-5 py-4 hover:border-line-strong">
                <AppLink to={`/meetings/${m.id}`} className="group min-w-0 flex-[999_1_360px]">
                  <div className="truncate text-base font-semibold group-hover:text-brand">{m.title}</div>
                  <div className="mt-1.5 flex flex-wrap gap-x-3.5 gap-y-1 text-[13px] text-muted">
                    <span>{dateTime(m.created_at)}</span>
                    {m.duration_sec > 0 && <span>{duration(m.duration_sec)}</span>}
                    <span>{SOURCE_LABEL[m.source]}</span>
                    <span>{m.owner_name}</span>
                    {m.visibility !== "team" && <span>{VISIBILITY_LABEL[m.visibility]}</span>}
                  </div>
                  {m.status === "failed" && <div className="mt-2 text-sm leading-relaxed text-bad">{m.error}</div>}
                </AppLink>
                <div className="flex flex-[1_1_160px] flex-col items-end gap-2">
                  <Chip tone={chip.tone}>{chip.text}</Chip>
                  {m.status === "failed" && m.can_edit && (
                    <Button size="sm" disabled={retry.isPending} onClick={() => retry.mutate(m.id)}>
                      <Icon name="retry" size={15} />
                      重试
                    </Button>
                  )}
                </div>
                {m.hits.length > 0 && <Hits meetingId={m.id} hits={m.hits} names={m.speakers} />}
              </div>
            );
          })}
        </section>
      ))}

        </>
      )}

      {uploading && <UploadDialog onClose={() => setUploading(false)} />}
    </div>
  );
}

/** 搜索时一场会议里命中的句子：时间、谁说的、那句话（命中的字高亮）。点了从那附近开始播放。 */
function Hits({ meetingId, hits, names }: { meetingId: string; hits: SearchHit[]; names: Record<string, string> }) {
  return (
    <ul className="-mx-2 flex basis-full flex-col gap-0.5 border-t border-line pt-2.5">
      {hits.map((h, i) => {
        const parts = snippet(h.text, h.matches);
        return (
          // 同一场会议的命中顺序固定，用下标当 key 没问题
          <li key={i}>
            <AppLink
              // 转写里的句子时间带一位小数（取整的话可能落在上一句里）；纪要里的时间点只到秒
              to={h.start < 0 ? `/meetings/${meetingId}` : `/meetings/${meetingId}?t=${jumpParam(h.start, h.source === "transcript")}`}
              className="flex gap-3 rounded-lg px-2 py-1.5 text-[13px] leading-relaxed hover:bg-canvas"
            >
              <span className="w-11 shrink-0 tabular-nums text-brand">{h.start >= 0 ? clock(h.start) : ""}</span>
              <span className="w-14 shrink-0 truncate text-muted">{h.source === "minutes" ? "纪要" : speakerName(h.speaker, names)}</span>
              <span className="min-w-0 text-ink-2">
                {parts.map((p, j) =>
                  // 片段的顺序固定，用下标当 key 没问题
                  p.mark ? (
                    <mark key={j} className="rounded-sm bg-[#ffe58a] px-px text-ink">
                      {p.text}
                    </mark>
                  ) : (
                    <span key={j}>{p.text}</span>
                  ),
                )}
                {h.fuzzy && <span className="ml-1.5 rounded bg-canvas px-1 py-px text-[11px] text-muted">近似</span>}
              </span>
            </AppLink>
          </li>
        );
      })}
    </ul>
  );
}

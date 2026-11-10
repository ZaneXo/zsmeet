import { useMutation, useQuery } from "@tanstack/react-query";
import { useCallback, useEffect, useRef, useState } from "react";
import type { FormEvent } from "react";
import { useParams } from "react-router";
import { AppLink } from "../components/AppLink";
import { api, errorMessage, queryClient, unwrap } from "../api/client";
import type { Meeting, Segment, User } from "../api/client";
import { LocalRecordingCard, useLocalItems } from "../components/LocalRecordings";
import { Button, buttonClass, Card, Chip, Icon, Input, Notice, ProgressBar, Spinner } from "../components/ui";
import { clock, dateTime, duration, SOURCE_LABEL, STAGE_LABEL } from "../lib/format";
import { session, SLICE_MS, useRecording } from "../lib/recording/session";
import { useTicker } from "../lib/useTicker";
import { meetingChip, recordingLive } from "./Meetings";
import { MinutesPane } from "./MinutesPane";
import { TranscriptPane } from "./TranscriptPane";

const isActive = (m: Meeting | undefined) => m?.status === "queued" || m?.status === "processing";
const isRecording = (m: Meeting | undefined) => m?.source === "record" && m.status === "uploading";

/**
 * 订阅服务器推送的处理进度（SSE），收到就更新缓存里的会议。
 * 推送被中间的代理拦掉也没关系：会议查询本身每 5 秒还会轮询一次。
 */
function useMeetingEvents(id: string, active: boolean) {
  useEffect(() => {
    if (!active) return;
    const source = new EventSource(`/api/meetings/${id}/events`);
    source.addEventListener("meeting", (event) => {
      queryClient.setQueryData(["meeting", id], JSON.parse((event as MessageEvent<string>).data) as Meeting);
    });
    return () => source.close();
  }, [id, active]);
}

export function MeetingDetailPage({ user, llmReady, isAdmin }: { user: User; llmReady: boolean; isAdmin: boolean }) {
  const { id = "" } = useParams();
  const meeting = useQuery({
    queryKey: ["meeting", id],
    queryFn: () => unwrap(api.GET("/api/meetings/{id}", { params: { path: { id } } })),
    // 在处理时每 5 秒、在录音时每 10 秒刷新一次
    refetchInterval: (query) => (isActive(query.state.data) ? 5000 : isRecording(query.state.data) ? 10_000 : false),
  });
  const m = meeting.data;
  useMeetingEvents(id, isActive(m));

  const transcript = useQuery({
    queryKey: ["transcript", id],
    queryFn: () => unwrap(api.GET("/api/meetings/{id}/transcript", { params: { path: { id } } })),
    enabled: !!m,
  });
  const segments = transcript.data?.segments ?? [];

  // 处理每前进一步，转写和纪要都可能有了新内容，重新取一次
  const step = m ? `${m.status}:${m.stage}:${m.minutes_version}` : "";
  useEffect(() => {
    if (!step) return;
    void queryClient.invalidateQueries({ queryKey: ["transcript", id] });
    void queryClient.invalidateQueries({ queryKey: ["minutes", id] });
    void queryClient.invalidateQueries({ queryKey: ["meetings"] });
  }, [id, step]);

  const [tab, setTab] = useState<"minutes" | "transcript">("minutes");
  const audio = useRef<HTMLAudioElement>(null);
  const [now, setNow] = useState(0);
  const seek = useCallback((sec: number) => {
    const el = audio.current;
    if (!el) return;
    el.currentTime = sec;
    void el.play();
  }, []);

  if (meeting.isPending) return <Spinner label="正在加载会议…" />;
  if (meeting.isError || !m) {
    return (
      <div className="mx-auto flex max-w-3xl flex-col gap-4">
        <BackLink />
        <Notice tone="bad">{errorMessage(meeting.error)}</Notice>
      </div>
    );
  }

  const chip = meetingChip(m);
  const hasTranscript = segments.length > 0;

  return (
    <div className="mx-auto flex max-w-6xl flex-col gap-4">
      <BackLink />

      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex min-w-0 flex-col gap-2">
          <Title meeting={m} />
          <div className="flex flex-wrap items-center gap-x-3.5 gap-y-1.5 text-[13px] text-muted">
            <span>{dateTime(m.created_at)}</span>
            {m.duration_sec > 0 && <span>{duration(m.duration_sec)}</span>}
            <span>{SOURCE_LABEL[m.source]}</span>
            <span>创建者 {m.owner_name}</span>
          </div>
        </div>
        <Chip tone={chip.tone}>{chip.text}</Chip>
      </div>

      {m.status !== "done" && <ProcessingPanel meeting={m} user={user} minutesOnly={hasTranscript} />}

      {m.has_audio && (
        <Card className="px-4 py-3">
          <audio
            ref={audio}
            controls
            preload="metadata"
            src={`/api/meetings/${m.id}/audio`}
            aria-label="会议录音"
            className="w-full"
            onTimeUpdate={(e) => setNow(e.currentTarget.currentTime)}
          />
        </Card>
      )}

      {(m.marks ?? []).length > 0 && <MarksCard marks={m.marks ?? []} segments={segments} canSeek={m.has_audio} onSeek={seek} />}

      {(m.status === "done" || hasTranscript) && (
        <Card>
          <div role="tablist" className="flex border-b border-line px-6">
            {(
              [
                ["minutes", "纪要"],
                ["transcript", hasTranscript ? `转写 · ${segments.length} 段` : "转写"],
              ] as const
            ).map(([key, label]) => (
              <button
                key={key}
                type="button"
                role="tab"
                aria-selected={tab === key}
                onClick={() => setTab(key)}
                className={`-mb-px mr-6 min-h-11 cursor-pointer border-b-2 px-1 text-[15px] ${
                  tab === key ? "border-ink font-semibold text-ink" : "border-transparent text-muted hover:text-ink"
                }`}
              >
                {label}
              </button>
            ))}
          </div>
          {tab === "minutes" ? (
            <MinutesPane meeting={m} hasTranscript={hasTranscript} llmReady={llmReady} isAdmin={isAdmin} canSeek={m.has_audio} onSeek={seek} />
          ) : (
            <TranscriptPane segments={segments} loading={transcript.isPending} now={now} canSeek={m.has_audio} onSeek={seek} />
          )}
        </Card>
      )}
    </div>
  );
}

function BackLink() {
  return (
    <AppLink to="/" className="inline-flex min-h-8 items-center gap-1 self-start text-sm text-muted hover:text-ink">
      <Icon name="back" size={16} />
      会议
    </AppLink>
  );
}

/** 标题，点铅笔可以改名。 */
function Title({ meeting }: { meeting: Meeting }) {
  const [editing, setEditing] = useState(false);
  const [title, setTitle] = useState(meeting.title);
  const rename = useMutation({
    mutationFn: () => unwrap(api.PATCH("/api/meetings/{id}", { params: { path: { id: meeting.id } }, body: { title } })),
    onSuccess: (updated) => {
      queryClient.setQueryData(["meeting", meeting.id], updated);
      void queryClient.invalidateQueries({ queryKey: ["meetings"] });
      setEditing(false);
    },
  });

  if (!editing) {
    return (
      <div className="flex items-center gap-2">
        <h1 className="min-w-0 break-words text-[26px] font-bold leading-tight">{meeting.title}</h1>
        {/* 只有创建者和管理员能改标题 */}
        {meeting.can_edit && (
          <Button
            variant="ghost"
            size="sm"
            aria-label="修改标题"
            title="修改标题"
            onClick={() => {
              setTitle(meeting.title);
              setEditing(true);
            }}
          >
            <Icon name="edit" size={16} />
          </Button>
        )}
      </div>
    );
  }
  const submit = (e: FormEvent) => {
    e.preventDefault();
    rename.mutate();
  };
  return (
    <form onSubmit={submit} className="flex flex-col gap-2">
      <div className="flex flex-wrap items-center gap-2">
        <Input className="w-[420px] max-w-full" aria-label="会议标题" value={title} onChange={(e) => setTitle(e.target.value)} maxLength={200} required autoFocus />
        <Button type="submit" variant="primary" disabled={rename.isPending}>
          保存
        </Button>
        <Button onClick={() => setEditing(false)}>取消</Button>
      </div>
      {rename.isError && <Notice tone="bad">{errorMessage(rename.error)}</Notice>}
    </form>
  );
}

const STEPS = ["assemble", "transcribe", "diarize", "minutes"] as const;

const STEP_HINT: Record<(typeof STEPS)[number], string> = {
  assemble: "把上传的分片拼起来，转成统一的格式",
  transcribe: "语音识别，自动加标点",
  diarize: "按声音区分是谁在说话。这一步不显示进度，长会议要等几分钟",
  minutes: "把转写交给大模型写成纪要",
};

/** 会议还没处理完时显示：排队、各步骤的进度，或者失败原因和重试按钮。 */
function ProcessingPanel({ meeting: m, user, minutesOnly }: { meeting: Meeting; user: User; minutesOnly: boolean }) {
  const retry = useMutation({
    mutationFn: () => unwrap(api.POST("/api/meetings/{id}/retry", { params: { path: { id: m.id } } })),
    onSuccess: (updated) => queryClient.setQueryData(["meeting", m.id], updated),
  });

  if (m.status === "uploading" && m.source === "record") return <UnfinishedRecording meeting={m} user={user} />;

  if (m.status === "uploading") {
    return (
      <Notice tone="warn">
        {m.can_edit
          ? "这场会议的录音还没有传完。上传的那个页面如果已经关掉了，需要重新上传一次。"
          : `${m.owner_name} 正在上传这场会议的录音，传完以后才会开始处理。`}
      </Notice>
    );
  }

  if (m.status === "failed") {
    return (
      <Card className="flex flex-col gap-3 border-bad/30 bg-bad-tint/40 p-5">
        <div className="flex flex-col gap-1">
          <b className="text-[15px] font-semibold text-bad">
            处理失败{m.stage ? `：卡在「${STAGE_LABEL[m.stage]}」这一步` : ""}
          </b>
          <span className="text-sm leading-relaxed text-ink-2">{m.error}</span>
        </div>
        {retry.isError && <Notice tone="bad">{errorMessage(retry.error)}</Notice>}
        {m.can_edit ? (
          <div>
            <Button variant="primary" size="sm" disabled={retry.isPending} onClick={() => retry.mutate()}>
              <Icon name="retry" size={15} />
              {minutesOnly ? "重新生成纪要" : "重试"}
            </Button>
          </div>
        ) : (
          <span className="text-[13px] text-muted">可以请 {m.owner_name} 或管理员重试。</span>
        )}
      </Card>
    );
  }

  if (m.status === "queued") {
    return (
      <Card className="flex items-center gap-3 p-5 text-sm text-ink-2">
        <span className="size-4 shrink-0 animate-spin rounded-full border-2 border-line-strong border-t-brand" aria-hidden="true" />
        排队中，前面的会议处理完就轮到这一场。可以先去做别的，不用开着这个页面。
      </Card>
    );
  }

  // 已经有转写时（重新生成纪要）只有写纪要这一步
  if (minutesOnly || m.stage === "minutes") {
    return (
      <Card className="flex flex-col gap-2.5 p-5">
        <div className="flex justify-between text-sm">
          <b className="font-semibold">正在生成纪要</b>
          <span className="text-muted">长会议会分几段处理，可能要几分钟</span>
        </div>
        <ProgressBar value={Math.max(0.05, m.progress)} />
      </Card>
    );
  }

  const current = STEPS.indexOf(m.stage as (typeof STEPS)[number]);
  return (
    <Card className="px-6 py-2">
      {m.stage === "models" && (
        <div className="flex flex-col gap-2 border-b border-line py-4">
          <div className="flex justify-between text-[15px]">
            <b className="font-semibold">下载识别模型</b>
            <span className="text-[13px] tabular-nums text-muted">{Math.round(m.progress * 100)}%</span>
          </div>
          <span className="text-[13px] text-muted">只在第一次处理会议时下载，约 560 MB，之后不用再等</span>
          <ProgressBar value={m.progress} />
        </div>
      )}
      {STEPS.map((stepName, i) => {
        const done = i < current;
        const running = i === current;
        return (
          <div key={stepName} className="flex gap-3.5 border-b border-line py-4 last:border-b-0">
            <span
              className={`flex size-7 shrink-0 items-center justify-center rounded-full text-[13px] font-semibold ${
                done ? "bg-ok-tint text-ok" : running ? "bg-brand text-white" : "bg-subtle text-muted"
              }`}
            >
              {done ? <Icon name="check" size={15} /> : i + 1}
            </span>
            <div className="flex min-w-0 flex-1 flex-col gap-1.5">
              <div className="flex justify-between gap-3 text-[15px]">
                <b className={`font-semibold ${done || running ? "text-ink" : "text-muted"}`}>{STAGE_LABEL[stepName]}</b>
                <span className="text-[13px] tabular-nums text-muted">
                  {done ? "已完成" : running && stepName !== "diarize" && m.progress > 0 ? `${Math.round(m.progress * 100)}%` : running ? "进行中" : ""}
                </span>
              </div>
              <span className="text-[13px] leading-relaxed text-muted">{STEP_HINT[stepName]}</span>
              {running && stepName !== "diarize" && <ProgressBar value={m.progress} />}
            </div>
          </div>
        );
      })}
    </Card>
  );
}

/** 多久以前，给「最后一次收到录音」用。 */
function ago(iso: string): string {
  const sec = Math.max(0, (Date.now() - Date.parse(iso)) / 1000);
  if (sec < 60) return "刚刚";
  if (sec < 3600) return `${Math.floor(sec / 60)} 分钟前`;
  if (sec < 86400) return `${Math.floor(sec / 3600)} 小时前`;
  return dateTime(iso);
}

/** 服务器收到的分片里，从第 0 片开始连续的有几片。 */
function contiguous(received: number[]): number {
  const have = new Set(received);
  let n = 0;
  while (have.has(n)) n++;
  return n;
}

/**
 * 网页录音还没传完的会议：可能正在录，也可能录音的网页关掉了、断网了、暂停着。
 * 这台电脑上有没传完的录音时接着传；找不回来的话，可以先用服务器已经收到的部分生成纪要。
 */
function UnfinishedRecording({ meeting: m, user }: { meeting: Meeting; user: User }) {
  const rec = useRecording();
  const local = useLocalItems(user).find((item) => item.rec.meetingId === m.id);
  // 定时重画：录音中断以后，即使服务器返回的数据没变，「录音中」也要变成「还没有传完」
  useTicker(10_000);
  const live = recordingLive(m);
  const last = m.last_chunk_at ? `最后一次收到录音：${ago(m.last_chunk_at)}` : "服务器还没有收到录音";

  const chunks = useQuery({
    queryKey: ["chunks", m.id],
    queryFn: () => unwrap(api.GET("/api/meetings/{id}/chunks", { params: { path: { id: m.id } } })),
    enabled: m.can_edit && !live,
  });
  const received = contiguous(chunks.data?.received ?? []);
  const [confirming, setConfirming] = useState(false);
  const finish = useMutation({
    mutationFn: () => unwrap(api.POST("/api/meetings/{id}/complete", { params: { path: { id: m.id } }, body: { chunks: received } })),
    onSuccess: (updated) => {
      queryClient.setQueryData(["meeting", m.id], updated);
      void queryClient.invalidateQueries({ queryKey: ["meetings"] });
    },
  });

  // 就是这个网页在录（或者在传最后几片）
  if (rec.meetingId === m.id && session.busy) {
    return (
      <Notice tone="info">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <span>这场会议正在这个网页里录音，录完会自动开始处理。</span>
          <AppLink to="/record" className={buttonClass("default", "sm")}>
            回到录音
          </AppLink>
        </div>
      </Notice>
    );
  }
  if (local) return <LocalRecordingCard item={local} />;
  if (!m.can_edit) {
    return (
      <Notice tone="info">
        {m.owner_name}
        {live ? "正在录这场会议" : "录的这场会议还没有传完"}（{last}）。录完、传完以后才会开始处理。
      </Notice>
    );
  }
  if (live) {
    return <Notice tone="info">这场会议正在录音（{last}）。录音的网页点「结束录音」以后会自动开始处理。</Notice>;
  }

  return (
    <Card className="flex flex-col gap-3 p-5">
      <div className="flex flex-col gap-1">
        <b className="text-[15px] font-semibold">录音还没有传完</b>
        <span className="text-sm leading-relaxed text-ink-2">
          {last}。录音的网页可能关掉了、断网了，或者正暂停着；它恢复以后会自动接着传。
          {received > 0
            ? `服务器已经收到 ${received} 片（大约 ${duration((received * SLICE_MS) / 1000)}）。如果录音的那台电脑找不回来了，可以先用这一部分生成纪要。`
            : ""}
        </span>
      </div>
      {chunks.isError && <Notice tone="bad">{errorMessage(chunks.error)}</Notice>}
      {finish.isError && <Notice tone="bad">{errorMessage(finish.error)}</Notice>}
      {received > 0 &&
        (confirming ? (
          <div className="flex flex-col gap-2.5 rounded-xl bg-warn-tint/60 px-4 py-3 text-sm">
            <span className="text-ink-2">确定吗？如果录音的网页其实还开着（比如暂停着、断网了），开始处理以后它后面录的部分就传不上来了。</span>
            <span className="flex gap-2">
              <Button size="sm" variant="primary" disabled={finish.isPending} onClick={() => finish.mutate()}>
                确定，开始处理
              </Button>
              <Button size="sm" onClick={() => setConfirming(false)}>
                取消
              </Button>
            </span>
          </div>
        ) : (
          <div>
            <Button size="sm" onClick={() => setConfirming(true)}>
              用已经收到的部分生成纪要
            </Button>
          </div>
        ))}
    </Card>
  );
}

/** 这个时间点正在说的那一句（最后一句开始得比它早的）。 */
function segmentAt(segments: Segment[], sec: number): Segment | undefined {
  let found: Segment | undefined;
  for (const s of segments) {
    if (s.start > sec) break;
    found = s;
  }
  return found;
}

/** 录音时标记的重点：时间点和那时正在说的话，点时间从那一句开始播放。 */
function MarksCard({ marks, segments, canSeek, onSeek }: { marks: number[]; segments: Segment[]; canSeek: boolean; onSeek: (sec: number) => void }) {
  return (
    <Card className="flex flex-col gap-2.5 px-5 py-4">
      <b className="text-[15px] font-semibold">录音时标记的重点</b>
      <ol className="flex flex-col gap-2">
        {marks.map((t) => {
          const seg = segmentAt(segments, t);
          const at = seg?.start ?? t;
          return (
            <li key={t} className="flex gap-3 text-sm leading-relaxed">
              {canSeek ? (
                <button type="button" onClick={() => onSeek(at)} className="shrink-0 cursor-pointer font-mono text-brand hover:underline" title="从这里开始播放">
                  {clock(t)}
                </button>
              ) : (
                <span className="shrink-0 font-mono text-muted">{clock(t)}</span>
              )}
              <span className="line-clamp-2 text-ink-2">{seg ? seg.text : "转写完成后这里会显示那时在说什么"}</span>
            </li>
          );
        })}
      </ol>
    </Card>
  );
}

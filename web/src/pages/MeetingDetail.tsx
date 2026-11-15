import { useMutation, useQuery } from "@tanstack/react-query";
import { useCallback, useEffect, useRef, useState } from "react";
import type { FormEvent } from "react";
import { useNavigate, useParams, useSearchParams } from "react-router";
import { AppLink } from "../components/AppLink";
import { api, errorMessage, queryClient, unwrap } from "../api/client";
import type { Citation, Meeting, Segment, User } from "../api/client";
import { LocalRecordingCard, useLocalItems } from "../components/LocalRecordings";
import { Button, buttonClass, Card, Chip, Field, Icon, Input, Notice, ProgressBar, Spinner } from "../components/ui";
import { tr, useLang } from "../i18n/react";
import { asrLanguageLabel, clock, dateTime, duration, SOURCE_LABEL, speakerColor, speakerName, STAGE_LABEL } from "../lib/format";
import type { MeetingASRLanguage } from "../lib/format";
import { citeJump, LEAD_IN, parseJump, withoutJump } from "../lib/jump";
import { waitingForRecording, writingMinutesOnly } from "../lib/meetingStatus";
import { session, SLICE_MS, useRecording } from "../lib/recording/session";
import { chunksFlowing, contiguous, lastReceived, receivedSeconds } from "../lib/unfinishedRecording";
import type { RecordingSource } from "../lib/unfinishedRecording";
import { useTicker } from "../lib/useTicker";
import { isDesktop, mediaUrl, subscribe } from "../platform";
import { ThisComputerRecording } from "../platform/DesktopRecordings";
import { DownloadLink } from "../platform/DownloadLink";
import { AccessDialog, VISIBILITY_LABEL } from "./AccessDialog";
import { AskPanel } from "./Ask";
import { FeishuShareDialog, LatestFeishuShare } from "./FeishuShare";
import { LatestWebhookSends, useMeetingWebhooks, WebhookShareDialog } from "./WebhookShare";
import { meetingChip } from "./Meetings";
import { MinutesPane } from "./MinutesPane";
import { TranscriptPane } from "./TranscriptPane";
import { ASRLanguageSelect, SpeakerCountSelect } from "./UploadDialog";

const isActive = (m: Meeting | undefined) => m?.status === "queued" || m?.status === "processing";

/**
 * 订阅服务器推送的处理进度（SSE），收到就更新缓存里的会议。
 * 推送被中间的代理拦掉也没关系：会议查询本身每 5 秒还会轮询一次。
 */
function useMeetingEvents(id: string, active: boolean) {
  useEffect(() => {
    if (!active) return;
    return subscribe(`/api/meetings/${id}/events`, "meeting", (data) => {
      queryClient.setQueryData(["meeting", id], JSON.parse(data) as Meeting);
    });
  }, [id, active]);
}

export function MeetingDetailPage({
  user,
  llmReady,
  feishuReady,
  voiceprints,
  isAdmin,
}: {
  user: User;
  llmReady: boolean;
  feishuReady: boolean;
  /** 管理员打开了声纹：改过名的人之后的会议按声音认出 */
  voiceprints: boolean;
  isAdmin: boolean;
}) {
  useLang();
  const { id = "" } = useParams();
  const meeting = useQuery({
    queryKey: ["meeting", id],
    queryFn: () => unwrap(api.GET("/api/meetings/{id}", { params: { path: { id } } })),
    // 在处理时每 5 秒、在等录音传完时每 10 秒刷新一次（传完、开始处理时页面跟着变）
    refetchInterval: (query) => (isActive(query.state.data) ? 5000 : waitingForRecording(query.state.data) ? 10_000 : false),
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

  // 从搜索结果点过来时带着 ?t=秒数：打开转写、定位到那一句，从那附近开始播放
  const [params, setParams] = useSearchParams();
  const jump = parseJump(params.get("t"));
  const jumpSec = jump?.sec;
  const [tab, setTab] = useState<"minutes" | "transcript">(jump === undefined ? "minutes" : "transcript");
  useEffect(() => {
    if (jumpSec !== undefined) setTab("transcript");
  }, [jumpSec]);
  const audio = useRef<HTMLAudioElement>(null);
  // 点出处时已经直接播放了的时间点：随后网址里的 ?t= 跟着变，下面跟着网址跳转的效果就不用再跳一次
  const cited = useRef<number | undefined>(undefined);
  // 每点一次出处加一：点的还是同一句时，转写也重新滚到那一句
  const [focusTick, setFocusTick] = useState(0);
  const [now, setNow] = useState(0);
  // 「发到飞书」「谁能看」的弹窗开着没有
  const [sharing, setSharing] = useState(false);
  const [access, setAccess] = useState(false);
  // 「问这场会议」的框开着没有
  const [asking, setAsking] = useState(false);
  // 「发到群里」（钉钉、企业微信、Slack 的群机器人）的弹窗开着没有
  const [posting, setPosting] = useState(false);
  const webhooks = useMeetingWebhooks(id, m?.minutes_version);
  const hooks = webhooks.data?.items ?? [];
  const seek = useCallback((sec: number) => {
    const el = audio.current;
    if (!el) return;
    el.currentTime = sec;
    void el.play();
  }, []);

  // 跳到搜索命中的地方：从那句前 3 秒开始播放，免得开头半句听不到。浏览器不让自动播放时就停在那里，等用户点播放
  const hasAudio = meeting.data?.has_audio;
  useEffect(() => {
    const el = audio.current;
    if (jumpSec !== undefined && cited.current === jumpSec) {
      cited.current = undefined;
      return;
    }
    if (jumpSec === undefined || !el) return;
    const go = () => {
      el.currentTime = Math.max(0, jumpSec - LEAD_IN);
      void el.play().catch(() => {});
    };
    if (el.readyState >= 1) {
      go();
      return;
    }
    el.addEventListener("loadedmetadata", go, { once: true });
    return () => el.removeEventListener("loadedmetadata", go);
  }, [jumpSec, hasAudio]);

  // 点了回答里的出处：在原地跳到那句话（和从搜索结果点过来一样，打开转写、定位、从前 3 秒开始播放）。
  // 总是在这里直接播放，不靠网址变化触发：网址里的时间点数值没变时（哪怕写法不同，「70」和「70.0」）上面的效果不会再跳。
  // 网址统一改成出处时间点的写法，转写按它定位，刷新页面也还在这里。
  // 纪要里没标时间的行就打开纪要，并去掉网址里的 ?t=，刷新页面也停在纪要
  const cite = (c: Citation) => {
    if (c.at < 0) {
      setTab("minutes");
      const next = withoutJump(params);
      if (next) setParams(next, { replace: true });
      return;
    }
    const j = citeJump(params.get("t"), c.at, c.source === "transcript");
    setTab("transcript");
    setFocusTick((n) => n + 1);
    seek(j.seek);
    if (j.param !== undefined) {
      if (j.moved) cited.current = j.focus.sec;
      setParams({ t: j.param }, { replace: true });
    }
  };

  if (meeting.isPending) return <Spinner label={tr("正在加载会议…", "Loading meeting…")} />;
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
  // 管理员配置了飞书、已经有纪要时，能编辑的人可以把纪要发到飞书
  const canShare = feishuReady && m.can_edit && m.minutes_version > 0;
  // 配置了大模型、有转写或纪要时可以问这场会议
  const canAsk = llmReady && (hasTranscript || m.minutes_version > 0);
  // 管理员加了群机器人、已经有纪要时，能编辑的人可以把纪要发到群里
  const canPost = hooks.length > 0 && m.can_edit && m.minutes_version > 0;

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
            <span>{tr(`创建者 ${m.owner_name}`, `Created by ${m.owner_name}`)}</span>
            {m.can_manage ? (
              <button type="button" className="cursor-pointer font-medium text-brand hover:text-brand-hover" onClick={() => setAccess(true)}>
                {VISIBILITY_LABEL[m.visibility]} · {tr("设置谁能看", "Manage access")}
              </button>
            ) : (
              m.visibility !== "team" && <span>{VISIBILITY_LABEL[m.visibility]}</span>
            )}
          </div>
          {feishuReady && <LatestFeishuShare meeting={m} onOpen={canShare ? () => setSharing(true) : undefined} />}
          <LatestWebhookSends hooks={hooks} meeting={m} onOpen={canPost ? () => setPosting(true) : undefined} />
        </div>
        <span className="flex items-center gap-2">
          <Chip tone={chip.tone}>{chip.text}</Chip>
          {canAsk && (
            <Button size="sm" variant="ghost" aria-expanded={asking} onClick={() => setAsking(!asking)}>
              <Icon name="ask" size={15} />
              {tr("问这场会议", "Ask this meeting")}
            </Button>
          )}
          {canShare && (
            <Button size="sm" variant="ghost" onClick={() => setSharing(true)}>
              <Icon name="upload" size={15} />
              {tr("发到飞书", "Share to Feishu")}
            </Button>
          )}
          {canPost && (
            <Button size="sm" variant="ghost" onClick={() => setPosting(true)}>
              <Icon name="upload" size={15} />
              {tr("发到群里", "Send to group")}
            </Button>
          )}
          <ExportMenu meeting={m} hasTranscript={hasTranscript} />
          {m.can_manage && <DeleteMeeting meeting={m} />}
        </span>
      </div>
      {sharing && <FeishuShareDialog meeting={m} onClose={() => setSharing(false)} />}
      {posting && <WebhookShareDialog meeting={m} hooks={hooks} onClose={() => setPosting(false)} />}
      {access && <AccessDialog meeting={m} isOwner={m.owner_id === user.id} feishu={feishuReady} groups={hooks.length > 0} onClose={() => setAccess(false)} />}

      {m.status !== "done" && <ProcessingPanel meeting={m} user={user} hasTranscript={hasTranscript} />}
      {m.asr_warning && <Notice tone="warn">{m.asr_warning}</Notice>}

      {m.has_audio && (
        <Card className="px-4 py-3">
          <audio
            ref={audio}
            controls
            preload="metadata"
            src={mediaUrl(`/api/meetings/${m.id}/audio`)}
            aria-label={tr("会议录音", "Meeting recording")}
            className="w-full"
            onTimeUpdate={(e) => setNow(e.currentTarget.currentTime)}
          />
        </Card>
      )}

      {m.audio_deleted_at && (
        <p className="rounded-xl border border-line bg-white px-4 py-3 text-[13px] leading-relaxed text-muted">
          {tr(
            `录音已经过了保留期，在 ${dateTime(m.audio_deleted_at)} 删除了。转写和纪要都还在，只是不能再回放、重新转写。`,
            `Audio passed the retention period and was deleted on ${dateTime(m.audio_deleted_at)}. The transcript and minutes are preserved, though playback and re-transcription are no longer available.`,
          )}
        </p>
      )}

      {canAsk && asking && (
        <Card className="flex flex-col gap-3 px-5 py-4">
          <div className="flex items-center justify-between gap-3">
            <h2 className="text-base font-semibold">{tr("问这场会议", "Ask this meeting")}</h2>
            <Button variant="ghost" size="sm" aria-label={tr("收起", "Collapse")} onClick={() => setAsking(false)}>
              <Icon name="close" />
            </Button>
          </div>
          <AskPanel meetingId={m.id} onCite={cite} />
        </Card>
      )}

      {(m.marks ?? []).length > 0 && <MarksCard marks={m.marks ?? []} segments={segments} canSeek={m.has_audio} onSeek={seek} />}

      {/* 重新转写、重新生成期间（或者失败了）已有的纪要和转写照样能看 */}
      {(m.status === "done" || hasTranscript || m.minutes_version > 0) && (
        <Card>
          <div className="flex flex-wrap items-center justify-between gap-x-4 border-b border-line px-6">
            <div role="tablist" className="flex">
              {(
                [
                  ["minutes", tr("纪要", "Minutes")],
                  ["transcript", tr("转写", "Transcript")],
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
            {tab === "transcript" && (m.asr_engine || m.language || (m.can_edit && m.has_audio)) && (
              <div className="flex items-center gap-3 py-1.5">
                {(m.asr_engine || m.language) && (
                  <span className="text-xs text-muted">
                    {[
                      m.asr_engine
                        ? m.asr_engine === "deepseek"
                          ? tr("DeepSeek 在线识别", "DeepSeek Cloud ASR")
                          : tr("本机识别", "Local ASR")
                        : "",
                      m.language ? asrLanguageLabel(m.language, true) : "",
                    ]
                      .filter(Boolean)
                      .join(" · ")}
                  </span>
                )}
                {m.can_edit && m.has_audio && <Retranscribe meeting={m} />}
              </div>
            )}
          </div>
          {tab === "minutes" ? (
            <MinutesPane meeting={m} hasTranscript={hasTranscript} llmReady={llmReady} isAdmin={isAdmin} canSeek={m.has_audio} onSeek={seek} />
          ) : (
            <>
              {hasTranscript && <Speakers meeting={m} segments={segments} voiceprints={voiceprints} isAdmin={isAdmin} />}
              <TranscriptPane
                segments={segments}
                loading={transcript.isPending}
                now={now}
                canSeek={m.has_audio}
                onSeek={seek}
                focus={jump && jump.sec + jump.slack}
                focusTick={focusTick}
                names={m.speakers}
              />
            </>
          )}
        </Card>
      )}
    </div>
  );
}

function BackLink() {
  useLang();
  return (
    <AppLink to="/" className="inline-flex min-h-8 items-center gap-1 self-start text-sm text-muted hover:text-ink">
      <Icon name="back" size={16} />
      {tr("会议", "Meetings")}
    </AppLink>
  );
}

/** 标题，点铅笔可以改名。 */
function Title({ meeting }: { meeting: Meeting }) {
  useLang();
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
            aria-label={tr("修改标题", "Edit title")}
            title={tr("修改标题", "Edit title")}
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
        <Input
          className="w-[420px] max-w-full"
          aria-label={tr("会议标题", "Meeting title")}
          value={title}
          onChange={(e) => setTitle(e.target.value)}
          maxLength={200}
          required
          autoFocus
        />
        <Button type="submit" variant="primary" disabled={rename.isPending}>
          {tr("保存", "Save")}
        </Button>
        <Button onClick={() => setEditing(false)}>{tr("取消", "Cancel")}</Button>
      </div>
      {rename.isError && <Notice tone="bad">{errorMessage(rename.error)}</Notice>}
    </form>
  );
}

const STEPS = ["assemble", "transcribe", "diarize", "correct", "minutes"] as const;

function stepHint(step: (typeof STEPS)[number]): string {
  switch (step) {
    case "assemble":
      return tr("把上传的分片拼起来，转成统一的格式", "Assemble uploaded chunks and transcode into a uniform audio format");
    case "transcribe":
      return tr("语音识别，自动加标点", "Speech-to-text recognition with automatic punctuation");
    case "diarize":
      return tr("按声音区分是谁在说话。这一步不显示进度，长会议要等几分钟", "Separate speakers by voice. Progress is not shown for this step; long meetings may take a few minutes.");
    case "correct":
      return tr("用大模型改正同音字、人名和专有名词；没有配置大模型时跳过", "Use LLM to fix homophones, names, and proper nouns; skipped if LLM is not configured.");
    case "minutes":
      return tr("把转写交给大模型写成纪要", "Generate structured minutes from the transcript using LLM");
  }
}

/** 会议还没处理完时显示：排队、各步骤的进度，或者失败原因和重试按钮。 */
function ProcessingPanel({ meeting: m, user, hasTranscript }: { meeting: Meeting; user: User; hasTranscript: boolean }) {
  useLang();
  const retry = useMutation({
    mutationFn: () => unwrap(api.POST("/api/meetings/{id}/retry", { params: { path: { id: m.id } } })),
    onSuccess: (updated) => queryClient.setQueryData(["meeting", m.id], updated),
  });

  if (m.status === "uploading" && m.source === "record") return <UnfinishedRecording meeting={m} user={user} />;

  if (m.status === "uploading" && m.source === "desktop") {
    // 录音在别的电脑上（网页版里总是这样）：服务器收到了多少，录音的电脑找不回来时可以先用收到的部分生成纪要
    const elsewhere = <ReceivedSoFar meeting={m} />;
    // 桌面版：这场会议正在这台电脑上录、录音正从这台电脑传的话，照旧显示这台电脑上录音、上传的情况
    // （那里能接着录、把录下的传上去），不在这里收尾。这台电脑上的录音传不上去了的话（比如断网时接着录的分片
    // 和服务器上已有的序号重了），那一行只能删掉录音：在它下面也能用服务器已经收到的部分生成纪要，不然这场会议一直结束不了
    return isDesktop ? <ThisComputerRecording meetingId={m.id} elsewhere={elsewhere} stuck={<ReceivedSoFar meeting={m} here />} /> : elsewhere;
  }

  if (m.status === "uploading") {
    return (
      <Notice tone="warn">
        {m.can_manage
          ? tr(
              "这场会议的录音还没有传完。上传的那个页面如果已经关掉了，需要重新上传一次。",
              "Audio upload for this meeting is incomplete. If the upload page was closed, please upload the file again.",
            )
          : tr(
              `${m.owner_name} 正在上传这场会议的录音，传完以后才会开始处理。`,
              `${m.owner_name} is uploading the recording for this meeting; processing will begin once upload completes.`,
            )}
      </Notice>
    );
  }

  if (m.status === "failed") {
    return (
      <Card className="flex flex-col gap-3 border-bad/30 bg-bad-tint/40 p-5">
        <div className="flex flex-col gap-1">
          <b className="text-[15px] font-semibold text-bad">
            {m.stage
              ? tr(`处理失败：卡在「${STAGE_LABEL[m.stage]}」这一步`, `Processing failed at “${STAGE_LABEL[m.stage]}”`)
              : tr("处理失败", "Processing failed")}
          </b>
          <span className="text-sm leading-relaxed text-ink-2">{m.error}</span>
        </div>
        {retry.isError && <Notice tone="bad">{errorMessage(retry.error)}</Notice>}
        {m.can_edit ? (
          <div>
            <Button variant="primary" size="sm" disabled={retry.isPending} onClick={() => retry.mutate()}>
              <Icon name="retry" size={15} />
              {/* 有转写时重试只重新写纪要（重新转写失败了也是，见 writingMinutesOnly 的说明） */}
              {hasTranscript ? tr("重新生成纪要", "Regenerate minutes") : tr("重试", "Retry")}
            </Button>
          </div>
        ) : (
          <span className="text-[13px] text-muted">
            {tr(`可以请 ${m.owner_name} 或管理员重试。`, `Ask ${m.owner_name} or an admin to retry.`)}
          </span>
        )}
      </Card>
    );
  }

  if (m.status === "queued") {
    return (
      <Card className="flex items-center gap-3 p-5 text-sm text-ink-2">
        <span className="size-4 shrink-0 animate-spin rounded-full border-2 border-line-strong border-t-brand" aria-hidden="true" />
        {tr(
          "排队中，前面的会议处理完就轮到这一场。可以先去做别的，不用开着这个页面。",
          "Queued — processing will start as soon as earlier meetings finish. You don’t need to keep this page open.",
        )}
      </Card>
    );
  }

  // 重新生成纪要时只有写纪要这一步；重新转写时虽然有旧的转写，也要从接收转码开始一步步显示。
  // 第一次识别完、转写已经出来的时候，可能还在校对
  if (writingMinutesOnly(m, hasTranscript)) {
    const correcting = m.stage === "correct";
    return (
      <Card className="flex flex-col gap-2.5 p-5">
        <div className="flex justify-between text-sm">
          <b className="font-semibold">{correcting ? tr("正在校对转写", "Proofreading transcript") : tr("正在生成纪要", "Generating minutes")}</b>
          <span className="text-muted">
            {correcting
              ? tr("改正认错的字，转写稍后会更新，接着生成纪要", "Fixing recognition errors — transcript will update shortly before minutes are generated")
              : tr("长会议会分几段处理，可能要几分钟", "Long meetings are processed in segments and may take a few minutes")}
          </span>
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
            <b className="font-semibold">{tr("下载识别模型", "Downloading recognition models")}</b>
            <span className="text-[13px] tabular-nums text-muted">{Math.round(m.progress * 100)}%</span>
          </div>
          <span className="text-[13px] text-muted">
            {tr("只在第一次处理会议时下载，约 300 MB，之后不用再等", "Downloaded once on first meeting (~300 MB); subsequent meetings won’t wait")}
          </span>
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
                  {done
                    ? tr("已完成", "Done")
                    : running && stepName !== "diarize" && m.progress > 0
                      ? `${Math.round(m.progress * 100)}%`
                      : running
                        ? tr("进行中", "In progress")
                        : ""}
                </span>
              </div>
              <span className="text-[13px] leading-relaxed text-muted">{stepHint(stepName)}</span>
              {running && stepName !== "diarize" && <ProgressBar value={m.progress} />}
            </div>
          </div>
        );
      })}
    </Card>
  );
}

/**
 * 网页录音还没传完的会议：可能正在录，也可能录音的网页关掉了、断网了、暂停着。
 * 这台电脑上有没传完的录音时接着传；找不回来的话，可以先用服务器已经收到的部分生成纪要。
 */
function UnfinishedRecording({ meeting: m, user }: { meeting: Meeting; user: User }) {
  useLang();
  const rec = useRecording();
  const local = useLocalItems(user).find((item) => item.rec.meetingId === m.id);

  // 就是这个网页在录（或者在传最后几片）
  if (rec.meetingId === m.id && session.busy) {
    return (
      <Notice tone="info">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <span>{tr("这场会议正在这个网页里录音，录完会自动开始处理。", "This meeting is currently recording in this browser tab; processing will start automatically when finished.")}</span>
          <AppLink to="/record" className={buttonClass("default", "sm")}>
            {tr("回到录音", "Back to recording")}
          </AppLink>
        </div>
      </Notice>
    );
  }
  if (local) return <LocalRecordingCard item={local} />;
  return <ReceivedSoFar meeting={m} />;
}

/** 录音还没传完时，网页录音和桌面版录音不一样的说法。 */
type UnfinishedText = {
  /** 别人看到的，接在创建者的名字后面：还在录，或者录的还没传完 */
  others: (ownerName: string, live: boolean, last: string) => string;
  /** 创建者、管理员看到的：录音还在传。last 是「最后一次收到录音：…」 */
  live: (last: string) => string;
  /** 录音不传了：可能是怎么了，恢复以后会怎样 */
  stalled: string;
  /** 跟在「服务器已经收到几片」后面：什么时候可以先用这一部分生成纪要 */
  offer: string;
  /** 用已经收到的部分生成纪要之前的提醒 */
  confirm: string;
};

function unfinishedText(source: RecordingSource): UnfinishedText {
  if (source === "record") {
    return {
      others: (owner, live, last) =>
        live
          ? tr(`${owner}正在录这场会议（${last}）。录完、传完以后才会开始处理。`, `${owner} is recording this meeting (${last}). Processing will start once recording and upload finish.`)
          : tr(`${owner}录的这场会议还没有传完（${last}）。录完、传完以后才会开始处理。`, `${owner}’s recording for this meeting hasn’t finished uploading (${last}). Processing will start once finished.`),
      live: (last) =>
        tr(
          `这场会议正在录音（${last}）。录音的网页点「结束录音」以后会自动开始处理。`,
          `This meeting is currently recording (${last}). Click “Finish recording” on the recording tab to start processing automatically.`,
        ),
      stalled: tr(
        "录音的网页可能关掉了、断网了，或者正暂停着；它恢复以后会自动接着传。",
        "The recording tab may have been closed, lost connection, or paused; upload will resume automatically once restored.",
      ),
      offer: tr(
        "如果录音的那台电脑找不回来了，可以先用这一部分生成纪要。",
        " If the recording device is no longer accessible, you can generate minutes from the received audio.",
      ),
      confirm: tr(
        "确定吗？如果录音的网页其实还开着（比如暂停着、断网了），开始处理以后它后面录的部分就传不上来了。",
        "Are you sure? If the recording tab is still open (e.g. paused or offline), any audio recorded later can no longer be uploaded once processing starts.",
      ),
    };
  }
  // 桌面版录的：录音存在录音的那台电脑上，zsmeet 桌面版开着、连上网就自动接着传（docs/desktop.md 的「上传」）。
  return {
    others: (owner, live, last) =>
      live
        ? tr(
            `${owner}正在用 zsmeet 桌面版录这场会议（${last}）。录完、传完以后才会开始处理。`,
            `${owner} is recording this meeting in zsmeet Desktop (${last}). Processing will start once recording and upload finish.`,
          )
        : tr(
            `${owner}用 zsmeet 桌面版录的这场会议还没有传完（${last}）。录完、传完以后才会开始处理。`,
            `${owner}’s zsmeet Desktop recording hasn’t finished uploading (${last}). Processing will start once finished.`,
          ),
    live: (last) =>
      tr(
        `这场会议正在用 zsmeet 桌面版录音（${last}）。在录音的电脑上点「结束录音」，录音传完以后会自动开始处理。`,
        `This meeting is recording in zsmeet Desktop (${last}). Click “Finish recording” on that computer to start processing automatically after upload.`,
      ),
    stalled: tr(
      "录音的电脑可能断网了、关机了、退出了 zsmeet，或者录音正暂停着。那台电脑上的 zsmeet 桌面版再打开、连上网以后，会自动把剩下的录音传上来。",
      "The recording computer may be offline, powered off, quit zsmeet, or paused. Once zsmeet Desktop on that computer reconnects, remaining audio will upload automatically.",
    ),
    offer: tr(
      "如果录音的那台电脑找不回来了，或者那台电脑上的录音传不上来了，可以先用这一部分生成纪要。",
      " If that computer is unavailable or its upload cannot continue, you can generate minutes from the received audio.",
    ),
    confirm: tr(
      "确定吗？开始处理以后，那台电脑上还没传上来的录音就用不上了，zsmeet 桌面版会显示这段录音传不上去，可以在那里删掉。如果那台电脑其实还在录（比如暂停着、断网了），后面录的也传不上来。",
      "Are you sure? Once processing starts, any remaining audio on that computer cannot be uploaded (zsmeet Desktop will mark it as unuploadable so you can delete it). If that computer is still recording, later audio cannot be uploaded either.",
    ),
  };
}

/**
 * 桌面版里，这场会议的录音就在这台电脑上，却传不上去了（platform/recorderView.ts 的 stuckHere）。
 * 为什么传不上去，上面那一行已经说了，这里只说服务器收到了多少。
 */
function stuckHereText(): Pick<UnfinishedText, "stalled" | "offer" | "confirm"> {
  return {
    stalled: "",
    offer: tr("可以先用这一部分生成纪要。", " You can generate minutes from the received audio."),
    confirm: tr(
      "确定吗？开始处理以后，这台电脑上没传上去的部分就传不上去了（还留在这台电脑上，不需要的话可以删掉）。",
      "Are you sure? Once processing starts, the remaining audio on this computer cannot be uploaded (it stays locally until you delete it).",
    ),
  };
}

/**
 * 网页录音、桌面版录音还没传完，录音又不在这个网页（这台电脑）上：服务器已经收到了多少。
 * 录音还在传时只说正在录；不传了的话，创建者和管理员可以先用服务器从第 0 片起连续收到的部分生成纪要
 * （录音的电脑找不回来时用）。
 * here：桌面版里录音就在这台电脑上，却传不上去了，显示在这台电脑的那一行下面（ThisComputerRecording 的 stuck）。
 * 这时只有收尾这一件事要说：别人看不到；服务器最近还收到过分片时（比如这台电脑刚把前面的分片传完）先不显示，
 * 和别的情况一样，不传了才给。
 */
function ReceivedSoFar({ meeting: m, here = false }: { meeting: Meeting; here?: boolean }) {
  useLang();
  const source: RecordingSource = m.source === "desktop" ? "desktop" : "record";
  const text = unfinishedText(source);
  const card = here ? stuckHereText() : text;
  // 录音还在不在传、多久以前收到的，按服务器算好的 upload_idle_sec（会议每 10 秒重新取一次）。
  // 定时重画是给还没有这一项的旧版本服务器的：那时按这台电脑的时钟算，录音中断以后，
  // 即使服务器返回的数据没变，「录音中」也要变成「还没有传完」
  useTicker(10_000);
  const live = chunksFlowing(m, Date.now());
  const last = lastReceived(m, Date.now());

  const chunks = useQuery({
    queryKey: ["chunks", m.id],
    queryFn: () => unwrap(api.GET("/api/meetings/{id}/chunks", { params: { path: { id: m.id } } })),
    enabled: m.can_manage && !live,
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

  if (!m.can_manage) {
    if (here) return null;
    return <Notice tone="info">{text.others(m.owner_name, live, last)}</Notice>;
  }
  if (live) return here ? null : <Notice tone="info">{text.live(last)}</Notice>;

  const approxDur = duration(receivedSeconds(source, received, SLICE_MS));
  return (
    <Card className="flex flex-col gap-3 p-5">
      <div className="flex flex-col gap-1">
        <b className="text-[15px] font-semibold">{tr("录音还没有传完", "Recording upload incomplete")}</b>
        <span className="text-sm leading-relaxed text-ink-2">
          {tr(`${last}。`, `${last}. `)}
          {card.stalled}
          {received > 0
            ? tr(
                `服务器已经收到 ${received} 片（大约 ${approxDur}）。${card.offer}`,
                ` The server has received ${received} chunk${received === 1 ? "" : "s"} (~${approxDur}).${card.offer}`,
              )
            : ""}
        </span>
      </div>
      {chunks.isError && <Notice tone="bad">{errorMessage(chunks.error)}</Notice>}
      {finish.isError && <Notice tone="bad">{errorMessage(finish.error)}</Notice>}
      {received > 0 &&
        (confirming ? (
          <div className="flex flex-col gap-2.5 rounded-xl bg-warn-tint/60 px-4 py-3 text-sm">
            <span className="text-ink-2">{card.confirm}</span>
            <span className="flex gap-2">
              <Button size="sm" variant="primary" disabled={finish.isPending} onClick={() => finish.mutate()}>
                {tr("确定，开始处理", "Confirm & start processing")}
              </Button>
              <Button size="sm" onClick={() => setConfirming(false)}>
                {tr("取消", "Cancel")}
              </Button>
            </span>
          </div>
        ) : (
          <div>
            <Button size="sm" onClick={() => setConfirming(true)}>
              {tr("用已经收到的部分生成纪要", "Generate minutes from received audio")}
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
  useLang();
  return (
    <Card className="flex flex-col gap-2.5 px-5 py-4">
      <b className="text-[15px] font-semibold">{tr("录音时标记的重点", "Key moments marked during recording")}</b>
      <ol className="flex flex-col gap-2">
        {marks.map((t) => {
          const seg = segmentAt(segments, t);
          const at = seg?.start ?? t;
          return (
            <li key={t} className="flex gap-3 text-sm leading-relaxed">
              {canSeek ? (
                <button
                  type="button"
                  onClick={() => onSeek(at)}
                  className="shrink-0 cursor-pointer font-mono text-brand hover:underline"
                  title={tr("从这里开始播放", "Play from here")}
                >
                  {clock(t)}
                </button>
              ) : (
                <span className="shrink-0 font-mono text-muted">{clock(t)}</span>
              )}
              <span className="line-clamp-2 text-ink-2">
                {seg ? seg.text : tr("转写完成后这里会显示那时在说什么", "What was said at this moment will appear once transcription completes")}
              </span>
            </li>
          );
        })}
      </ol>
    </Card>
  );
}

/**
 * 用现在的识别模型重新转写：说话人分错了时选上有几个人说话再转；换模型以前转写的会议也用它得到逐句的时间。
 * 先确认一次，确认时可以改说话人数。
 */
function Retranscribe({ meeting: m }: { meeting: Meeting }) {
  useLang();
  const [confirming, setConfirming] = useState(false);
  const [speakerCount, setSpeakerCount] = useState(m.speaker_count);
  const [language, setLanguage] = useState<MeetingASRLanguage>(m.language);
  const run = useMutation({
    mutationFn: () =>
      unwrap(
        api.POST("/api/meetings/{id}/retranscribe", {
          params: { path: { id: m.id } },
          body: { speaker_count: speakerCount, language },
        }),
      ),
    onSuccess: (updated) => {
      setConfirming(false);
      queryClient.setQueryData(["meeting", m.id], updated);
      void queryClient.invalidateQueries({ queryKey: ["meetings"] });
    },
  });
  const busy = m.status !== "done" && m.status !== "failed";
  if (!confirming) {
    return (
      <Button
        size="sm"
        variant="ghost"
        disabled={busy}
        onClick={() => {
          setSpeakerCount(m.speaker_count);
          setLanguage(m.language);
          setConfirming(true);
        }}
      >
        <Icon name="retry" size={15} />
        {tr("重新转写", "Re-transcribe")}
      </Button>
    );
  }
  return (
    <div className="flex basis-full flex-col gap-2.5 py-3 text-sm">
      <span className="leading-relaxed text-ink-2">
        {tr(
          "用现在的识别模型从录音重新转写，转完会重新生成一版纪要（之前的版本都保留）。长会议要几分钟，转完之前看到的还是原来的转写；重新转写失败时原来的转写和纪要也都还在。",
          "Re-transcribes from the audio using the current recognition model and generates a new minutes version (previous versions are kept). Long meetings may take a few minutes; existing transcript and minutes remain available while processing or if it fails.",
        )}
      </span>
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <Field
          label={tr("识别语言", "Recognition language")}
          htmlFor="retranscribe-language"
          hint={tr(
            "多语种或粤/日/韩/英会议选「自动识别」或对应语言；纯普通话会议固定选「中文」",
            "Pick \"Auto-detect\" or the target language for multilingual / Cantonese / Japanese / Korean / English meetings",
          )}
        >
          <ASRLanguageSelect id="retranscribe-language" className="sm:w-64" value={language} onChange={setLanguage} disabled={run.isPending} />
        </Field>
        <Field
          label={tr("说话人数", "Speaker count")}
          htmlFor="retranscribe-speakers"
          hint={tr(
            "说话人分得不对（分多了或者几个人混在一起）时，选上有几个人说话再转写",
            "If speaker separation was inaccurate (too many or merged speakers), specify the number of speakers before re-transcribing",
          )}
        >
          <SpeakerCountSelect id="retranscribe-speakers" className="sm:w-48" value={speakerCount} onChange={setSpeakerCount} disabled={run.isPending} />
        </Field>
      </div>
      {run.isError && <Notice tone="bad">{errorMessage(run.error)}</Notice>}
      <span className="flex gap-2">
        <Button size="sm" variant="primary" disabled={run.isPending} onClick={() => run.mutate()}>
          {tr("确定重新转写", "Confirm re-transcribe")}
        </Button>
        <Button size="sm" onClick={() => setConfirming(false)}>
          {tr("取消", "Cancel")}
        </Button>
      </span>
    </div>
  );
}

/** 删除会议：放进回收站，30 天内可以恢复。先确认一次。 */
function DeleteMeeting({ meeting: m }: { meeting: Meeting }) {
  useLang();
  const navigate = useNavigate();
  const [confirming, setConfirming] = useState(false);
  const remove = useMutation({
    mutationFn: () => unwrap(api.DELETE("/api/meetings/{id}", { params: { path: { id: m.id } } })),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["meetings"] });
      void queryClient.invalidateQueries({ queryKey: ["trash"] });
      queryClient.removeQueries({ queryKey: ["meeting", m.id] });
      navigate("/");
    },
  });
  if (!confirming) {
    return (
      <Button size="sm" variant="ghost" onClick={() => setConfirming(true)}>
        {tr("删除", "Delete")}
      </Button>
    );
  }
  return (
    <span className="flex flex-wrap items-center gap-2 rounded-lg bg-bad-tint/60 px-3 py-1.5 text-[13px]">
      <span className="text-ink-2">{tr("放进回收站？30 天内可以恢复。", "Move to Trash? Can be restored within 30 days.")}</span>
      <Button size="sm" variant="danger" disabled={remove.isPending} onClick={() => remove.mutate()}>
        {tr("确定删除", "Confirm delete")}
      </Button>
      <Button size="sm" onClick={() => setConfirming(false)}>
        {tr("取消", "Cancel")}
      </Button>
      {remove.isError && <span className="basis-full text-bad">{errorMessage(remove.error)}</span>}
    </span>
  );
}

/**
 * 说话人：每人一个色块、名字和说话时长的占比。创建者和管理员可以改名，
 * 改完转写和搜索结果马上用新名字，纪要里的「说话人 N」也换成名字（作为新版本）。
 * 按声音认出的名字标着「按声音认出」，认对了点改名再保存一次就算确认。
 */
function Speakers({ meeting: m, segments, voiceprints, isAdmin }: { meeting: Meeting; segments: Segment[]; voiceprints: boolean; isAdmin: boolean }) {
  useLang();
  const [editing, setEditing] = useState<number | null>(null);
  const [draft, setDraft] = useState("");
  const rename = useMutation({
    mutationFn: (names: Record<string, string>) =>
      unwrap(api.PUT("/api/meetings/{id}/speakers", { params: { path: { id: m.id } }, body: { names } })),
    onSuccess: (updated) => {
      setEditing(null);
      queryClient.setQueryData(["meeting", m.id], updated);
      void queryClient.invalidateQueries({ queryKey: ["minutes", m.id] });
      void queryClient.invalidateQueries({ queryKey: ["meetings"] });
    },
  });

  const talk = new Map<number, number>();
  let total = 0;
  for (const s of segments) {
    if (s.speaker < 0) continue;
    const len = Math.max(0, s.end - s.start);
    talk.set(s.speaker, (talk.get(s.speaker) ?? 0) + len);
    total += len;
  }
  const speakers = [...talk.keys()].sort((a, b) => a - b);
  if (speakers.length === 0) return null;

  return (
    <div className="flex flex-col gap-2 border-b border-line px-6 py-3.5">
      <div className="flex flex-wrap items-center gap-2">
        <span className="mr-1 text-[13px] font-semibold text-ink-2">{tr("说话人", "Speakers")}</span>
        {speakers.map((sp) =>
          editing === sp ? (
            <form
              key={sp}
              className="flex items-center gap-1.5"
              onSubmit={(e) => {
                e.preventDefault();
                rename.mutate({ [String(sp)]: draft });
              }}
            >
              <Input
                autoFocus
                className="w-32"
                maxLength={20}
                value={draft}
                placeholder={speakerName(sp)}
                aria-label={tr(`给${speakerName(sp)}起名字`, `Name ${speakerName(sp)}`)}
                onChange={(e) => setDraft(e.target.value)}
              />
              <Button size="sm" type="submit" variant="primary" disabled={rename.isPending}>
                {tr("保存", "Save")}
              </Button>
              <Button size="sm" onClick={() => setEditing(null)}>
                {tr("取消", "Cancel")}
              </Button>
            </form>
          ) : (
            <span key={sp} className="flex items-center gap-1.5 rounded-full bg-canvas px-3 py-1 text-[13px]">
              <span className="size-2 rounded-full" style={{ background: speakerColor(sp) }} aria-hidden="true" />
              <span className="font-medium">{speakerName(sp, m.speakers)}</span>
              {m.auto_speakers.includes(sp) && (
                <span
                  className="rounded-full bg-brand-tint px-1.5 text-[11px] font-medium text-brand-ink"
                  title={tr(
                    "这个名字是按声音和以前会议里的名字比对出来的，可能认错。认错了点「改名」改过来",
                    "Matched by voice against speakers from past meetings — may be inaccurate. Click “Rename” to fix.",
                  )}
                >
                  {tr("按声音认出", "Voice match")}
                </span>
              )}
              <span className="text-muted">{total > 0 ? Math.round(((talk.get(sp) ?? 0) / total) * 100) : 0}%</span>
              {m.can_edit && (
                <button
                  type="button"
                  className="cursor-pointer text-brand hover:text-brand-hover"
                  onClick={() => {
                    setDraft(m.speakers[String(sp)] ?? "");
                    setEditing(sp);
                    rename.reset();
                  }}
                >
                  {tr("改名", "Rename")}
                </button>
              )}
            </span>
          ),
        )}
      </div>
      {rename.isError && <Notice tone="bad">{errorMessage(rename.error)}</Notice>}
      {m.can_edit && editing !== null && (
        <span className="text-xs text-muted">
          {m.auto_speakers.includes(editing) &&
            tr(
              "这个名字是按声音认出的：认对了直接保存就算确认，认错了改成对的名字。",
              "Matched by voice: click Save to confirm if accurate, or enter the right name. ",
            )}
          {tr(
            `改完以后，转写和搜索结果马上用新名字；当前纪要里的「${speakerName(editing)}」也会换成新名字，作为一个新版本保存。留空表示去掉名字。`,
            `Once saved, the transcript and search results update immediately, and “${speakerName(editing)}” in the current minutes is replaced as a new version. Leave blank to clear the name.`,
          )}
          {voiceprints && tr("之后的会议会按声音认出这个人。", " Future meetings will recognize this speaker by voice.")}
        </span>
      )}
      {m.can_edit && editing !== null && !voiceprints && (
        <span className="text-xs text-muted">
          {isAdmin ? (
            <>
              {tr("在", "Enable voiceprints in ")}
              <AppLink to="/settings/voiceprints" className="text-brand hover:text-brand-hover">
                {tr("「设置 → 声纹」", "Settings → Voiceprints")}
              </AppLink>
              {tr("里打开以后，改过名的人在之后的会议里能按声音自动认出。", " to automatically recognize named speakers by voice in future meetings.")}
            </>
          ) : (
            tr(
              "管理员在「设置 → 声纹」里打开以后，改过名的人在之后的会议里能按声音自动认出。",
              "Once an admin enables Settings → Voiceprints, named speakers can be recognized automatically by voice in future meetings.",
            )
          )}
        </span>
      )}
    </div>
  );
}

/** 导出：纪要导出成 Word 或 Markdown，转写导出成 Word 或纯文本。直接下载，不离开这个页面。 */
function ExportMenu({ meeting: m, hasTranscript }: { meeting: Meeting; hasTranscript: boolean }) {
  const lang = useLang();
  const options: [string, string, string][] = [];
  if (m.minutes_version > 0) {
    options.push(
      ["minutes", "docx", tr("纪要 · Word", "Minutes · Word")],
      ["minutes", "md", tr("纪要 · Markdown", "Minutes · Markdown")],
    );
  }
  if (hasTranscript) {
    options.push(
      ["transcript", "docx", tr("转写 · Word", "Transcript · Word")],
      ["transcript", "txt", tr("转写 · 纯文本", "Transcript · Plain text")],
    );
  }
  if (options.length === 0) return null;
  return (
    <details className="relative">
      <summary className={`${buttonClass("ghost", "sm")} list-none [&::-webkit-details-marker]:hidden`}>
        <Icon name="download" size={15} />
        {tr("导出", "Export")}
      </summary>
      <div className="absolute right-0 z-10 mt-1 flex min-w-36 flex-col rounded-xl border border-line bg-white p-1 shadow-lg">
        {options.map(([what, format, label]) => (
          <DownloadLink
            key={`${what}-${format}`}
            href={`/api/meetings/${m.id}/export?what=${what}&format=${format}&lang=${lang}`}
            className="rounded-lg px-3 py-2 text-[13px] text-ink-2 hover:bg-canvas hover:text-ink"
          >
            {label}
          </DownloadLink>
        ))}
      </div>
    </details>
  );
}

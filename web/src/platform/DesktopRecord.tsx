import { useMutation, useQuery } from "@tanstack/react-query";
import { useState } from "react";
import type { FormEvent } from "react";
import { useNavigate, useSearchParams } from "react-router";
import { api, ApiError, errorMessage, queryClient, unwrap } from "../api/client";
import { AppLink } from "../components/AppLink";
import { LevelMeter, RecordingMarks } from "../components/RecordingParts";
import { Button, buttonClass, Card, Field, Icon, Input, Notice, Select } from "../components/ui";
import { hms } from "../lib/format";
import { SpeakerCountField, useTemplates, VisibilityField } from "../pages/UploadDialog";
import { UploadRow } from "./DesktopRecordings";
import { useOffline } from "./offline";
import {
  clearEnded,
  deleteEmptyMeeting,
  dismissUpload,
  loadEchoCancel,
  loadSources,
  recorderDevices,
  recorderMark,
  recorderPause,
  recorderResume,
  recorderStart,
  recorderState,
  recorderStop,
  saveEchoCancel,
  saveSources,
  useRecorder,
} from "./recorder";
import type { RecorderState, RecorderUpload, RecorderWarning } from "./recorder";
import {
  echoCancelArg,
  echoCancelUsable,
  echoLabel,
  effectiveMic,
  endedGone,
  endedUpload,
  isLive,
  liveUpload,
  localMeeting,
  micArg,
  micGainLabel,
  micOptions,
  NO_MIC,
  quietMicHint,
  recordedLength,
  sentence,
  sourceLabels,
  sourcesProblem,
  warningText,
} from "./recorderView";
import type { EchoLabel, Ended, Sources } from "./recorderView";
import { useUpdater } from "./updater";
import { recordUpdateNote } from "./updateView";

/**
 * 桌面版的录音页：录电脑里会议软件的声音和麦克风（docs/desktop.md 的「录音（第三阶段）」）。
 * 录音和上传都在 App 里（Rust 管着录音程序），这个页面只是控制和显示：离开这个页面、关掉窗口，录音都不停。
 */
export function DesktopRecordPage() {
  const { state, ended, failure, micQuiet } = useRecorder();
  const [params] = useSearchParams();
  const resumeId = params.get("continue");
  // 从提示条点「接着录」过来：这台电脑上这场会议中断了的录音
  const resume = resumeId ? state.uploads.find((u) => u.meetingId === resumeId && u.status === "interrupted") : undefined;
  const starting = state.status === "starting";

  return (
    <div className="mx-auto flex max-w-3xl flex-col gap-5">
      <div className="flex flex-col gap-1.5">
        <h1 className="text-[26px] font-bold">录音</h1>
        <p className="text-sm text-muted">录下电脑里会议软件的声音和麦克风，边录边传到服务器。断网也能接着录，联网后自动补传，录完自动出纪要。</p>
      </div>
      {failure && <Notice tone="bad">{failure}</Notice>}
      {isLive(state.status) ? (
        <Live state={state} micQuiet={micQuiet} />
      ) : state.status === "stopping" ? (
        <Card className="flex items-center gap-3 p-6 text-sm text-ink-2">
          <span className="size-4 shrink-0 animate-spin rounded-full border-2 border-line-strong border-t-brand" aria-hidden="true" />
          正在结束录音，等最后一段录音写好…
        </Card>
      ) : resume ? (
        <Resume key={resume.meetingId} upload={resume} starting={starting} warnings={state.warnings} />
      ) : ended && state.status === "idle" ? (
        <EndedCard ended={ended} state={state} />
      ) : (
        <Setup starting={starting} warnings={state.warnings} />
      )}
    </div>
  );
}

// ---- 开始前：会议信息、录什么声音 ----

/** 电脑上的麦克风。插拔了麦克风、改了系统默认的麦克风时，回到窗口会重新列一次。 */
function useMicrophones() {
  return useQuery({ queryKey: ["recorder-devices"], queryFn: recorderDevices, refetchOnWindowFocus: true, staleTime: 0 });
}

/** 开始录音没成功：录音程序那边的原因（不是新建会议失败）。 */
class RecorderStartError extends Error {}

/**
 * 交给 App 开始录音。没能开始的话，刚在服务器上建的会议什么也没有，删掉它，免得留下一场空会议；
 * App 已经记下了这场录音的话（比如开始以后才出错）不删。echoCancel 是「外放时去掉回声」开没开。
 */
async function startRecorder(meetingId: string, sources: Sources, echoCancel: boolean, fresh: boolean) {
  try {
    await recorderStart({ meetingId, mic: micArg(sources.mic), system: sources.system, echoCancel: echoCancelArg(sources, echoCancel) });
  } catch (e) {
    if (fresh && !localMeeting(recorderState(), meetingId)) await deleteEmptyMeeting(meetingId);
    throw new RecorderStartError(errorMessage(e));
  }
  saveSources(sources);
}

/** 「外放时去掉回声（实验）」开没开：每台电脑记自己的，点了就记下来。 */
function useEchoCancel(): [boolean, (on: boolean) => void] {
  const [on, setOn] = useState(loadEchoCancel);
  return [
    on,
    (next: boolean) => {
      setOn(next);
      saveEchoCancel(next);
    },
  ];
}

/**
 * 正在更新 zsmeet 时录音页上的说明（updateView.ts 的 recordUpdateNote）：还在下载时照样能开始录音，这次就不装了；
 * 下载完在安装、装好了等重新打开时 App 不让开始录音（会被重启打断），blocks 是 true，开始录音的按钮变灰。
 * 没在更新时是 null。
 */
function useUpdateNote() {
  const { phase, progress } = useUpdater();
  return recordUpdateNote(phase, progress);
}

/** 开始一场新的录音：先在服务器上建好会议，再交给 App 开始录。warnings 是没能开始时录音程序给的提示。 */
function Setup({ starting, warnings }: { starting: boolean; warnings: RecorderWarning[] }) {
  const templates = useTemplates();
  const devices = useMicrophones();
  const offline = useOffline();
  const update = useUpdateNote();
  const [title, setTitle] = useState("");
  const [template, setTemplate] = useState("general");
  const [hotwords, setHotwords] = useState("");
  const [visibility, setVisibility] = useState<"team" | "private">("team");
  const [speakerCount, setSpeakerCount] = useState(0);
  const [chosen, setChosen] = useState(loadSources);
  const [echoCancel, setEchoCancel] = useEchoCancel();
  const sources = { ...chosen, mic: effectiveMic(chosen.mic, devices.data) };
  const problem = sourcesProblem(sources);

  const start = useMutation({
    mutationFn: async () => {
      const meeting = await unwrap(
        api.POST("/api/meetings", { body: { title, source: "desktop", template, hotwords, visibility, speaker_count: speakerCount } }),
      );
      await startRecorder(meeting.id, sources, echoCancel, true);
    },
    onSettled: () => queryClient.invalidateQueries({ queryKey: ["meetings"] }),
  });
  const busy = starting || start.isPending;
  const blocked = !!update?.blocks;
  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (!busy && !problem && !blocked) start.mutate();
  };

  return (
    <Card className="p-6">
      <form onSubmit={submit} className="flex flex-col gap-5">
        <Field label="会议标题" htmlFor="rec-title" hint="留空会用开始录音的时间起一个，之后可以改">
          <Input id="rec-title" value={title} onChange={(e) => setTitle(e.target.value)} maxLength={200} disabled={busy} />
        </Field>

        <SourceFields sources={sources} onChange={setChosen} echoCancel={echoCancel} onEchoCancel={setEchoCancel} devices={devices} disabled={busy} />

        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <Field label="纪要模板" htmlFor="rec-template">
            <Select id="rec-template" value={template} onChange={(e) => setTemplate(e.target.value)} disabled={busy}>
              {(templates.data?.items ?? []).map((t) => (
                <option key={t.id} value={t.id}>
                  {t.name}
                </option>
              ))}
            </Select>
          </Field>
          <Field label="本场热词" htmlFor="rec-hotwords">
            <Input id="rec-hotwords" value={hotwords} onChange={(e) => setHotwords(e.target.value)} maxLength={500} placeholder="人名、产品名，用逗号分开" disabled={busy} />
          </Field>
        </div>
        <SpeakerCountField id="rec-speakers" value={speakerCount} onChange={setSpeakerCount} disabled={busy} />
        <VisibilityField id="rec-visibility" value={visibility} onChange={setVisibility} disabled={busy} />

        {offline && !start.isError && <Notice tone="warn">现在连不上服务器。开始录音要先在服务器上建好这场会议，请连上以后再开始；开始以后断网也能接着录。</Notice>}
        {update && <Notice tone="info">{update.text}</Notice>}
        {start.isError && <StartFailure error={start.error} warnings={warnings} />}

        <div className="flex flex-wrap items-center justify-between gap-3">
          <span className="text-xs leading-relaxed text-muted">录音时可以关掉这个窗口，录音不会停：菜单栏的 zsmeet 图标旁边会显示录了多久。</span>
          <Button type="submit" variant="primary" disabled={busy || !!problem || blocked}>
            <Icon name="mic" />
            {busy ? "正在开始…" : "开始录音"}
          </Button>
        </div>
      </form>
    </Card>
  );
}

/** 从提示条点「接着录」：接着录一场中断了的录音，会议信息不用再填，只选声音来源。 */
function Resume({ upload, starting, warnings }: { upload: RecorderUpload; starting: boolean; warnings: RecorderWarning[] }) {
  const navigate = useNavigate();
  const devices = useMicrophones();
  const update = useUpdateNote();
  const [chosen, setChosen] = useState(loadSources);
  const [echoCancel, setEchoCancel] = useEchoCancel();
  const sources = { ...chosen, mic: effectiveMic(chosen.mic, devices.data) };
  const problem = sourcesProblem(sources);
  const start = useMutation({
    mutationFn: () => startRecorder(upload.meetingId, sources, echoCancel, false),
    // 开始了就不再需要网址里的 ?continue=
    onSuccess: () => navigate("/record", { replace: true }),
  });
  const busy = starting || start.isPending;
  const blocked = !!update?.blocks;
  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (!busy && !problem && !blocked) start.mutate();
  };

  return (
    <Card className="p-6">
      <form onSubmit={submit} className="flex flex-col gap-5">
        <div className="flex flex-col gap-1">
          <b className="text-lg font-bold">接着录「{upload.title}」</b>
          <span className="text-sm leading-relaxed text-ink-2">
            这场录音中断前已经录下{recordedLength(upload.recorded)}。接着录的部分会接在后面，标记的重点也都还在。
          </span>
        </div>
        <SourceFields sources={sources} onChange={setChosen} echoCancel={echoCancel} onEchoCancel={setEchoCancel} devices={devices} disabled={busy} />
        {update && <Notice tone="info">{update.text}</Notice>}
        {start.isError && <StartFailure error={start.error} warnings={warnings} />}
        <div className="flex flex-wrap justify-end gap-2.5">
          <AppLink to="/record" replace className={buttonClass("default")}>
            取消
          </AppLink>
          <Button type="submit" variant="primary" disabled={busy || !!problem || blocked}>
            <Icon name="mic" />
            {busy ? "正在开始…" : "接着录"}
          </Button>
        </div>
      </form>
    </Card>
  );
}

const choiceClass = (on: boolean) => `flex gap-3 rounded-xl border px-4 py-3 ${on ? "border-brand bg-brand-tint/50" : "border-line"}`;

/**
 * 录什么声音：电脑里的声音（会议软件里其他人的声音）、哪个麦克风（或者不录麦克风）；
 * 两样都录时，外放时去不去掉回声（实验功能）。
 */
function SourceFields({
  sources,
  onChange,
  echoCancel,
  onEchoCancel,
  devices,
  disabled,
}: {
  sources: Sources;
  onChange: (next: Sources) => void;
  echoCancel: boolean;
  onEchoCancel: (on: boolean) => void;
  devices: ReturnType<typeof useMicrophones>;
  disabled: boolean;
}) {
  const problem = sourcesProblem(sources);
  return (
    <fieldset className="flex flex-col gap-2.5" disabled={disabled}>
      <legend className="mb-1.5 text-[13px] font-semibold text-ink-2">录什么声音</legend>
      <label className={`${choiceClass(sources.system)} cursor-pointer hover:border-line-strong`}>
        <input
          type="checkbox"
          checked={sources.system}
          onChange={(e) => onChange({ ...sources, system: e.target.checked })}
          className="mt-1 shrink-0 self-start accent-brand"
        />
        <span className="flex flex-col gap-1">
          <b className="text-sm font-semibold">电脑里的声音</b>
          <span className="text-[13px] leading-relaxed text-muted">
            录下飞书、腾讯会议、Zoom 这些会议软件里其他人说话的声音（电脑上同时在放的其他声音也会录进去）。第一次录的时候，macOS 会问是否允许 zsmeet 录制系统音频，点「允许」。
          </span>
        </span>
      </label>
      <div className={choiceClass(sources.mic !== NO_MIC)}>
        <div className="flex min-w-0 flex-1 flex-col gap-2">
          <span className="flex flex-col gap-1">
            <label htmlFor="rec-mic" className="text-sm font-semibold">
              麦克风
            </label>
            <span className="text-[13px] leading-relaxed text-muted">录下你自己和身边的人说话的声音。第一次录的时候，macOS 会问是否允许 zsmeet 使用麦克风，点「允许」。</span>
          </span>
          <Select id="rec-mic" value={sources.mic} onChange={(e) => onChange({ ...sources, mic: e.target.value })}>
            {micOptions(devices.data ?? []).map((o) => (
              <option key={o.value} value={o.value}>
                {o.label}
              </option>
            ))}
          </Select>
          {devices.isError && <span className="text-xs leading-relaxed text-warn">没能列出电脑上的麦克风：{errorMessage(devices.error)}。可以先用系统默认的麦克风。</span>}
        </div>
      </div>
      <EchoCancelField on={echoCancel} usable={echoCancelUsable(sources)} onChange={onEchoCancel} />
      {problem ? (
        <p className="text-xs leading-relaxed text-warn">{problem}</p>
      ) : (
        <p className="text-xs leading-relaxed text-muted">开线上会议时建议戴耳机，免得对方的声音从扬声器放出来，又被麦克风录一遍。</p>
      )}
    </fieldset>
  );
}

/**
 * 「外放时去掉回声（实验）」：用录到的电脑声音，把外放出来又被麦克风录进去的会议声音去掉（docs/desktop.md 的「回声消除」）。
 * 实验功能，默认关，每台电脑记自己的。麦克风和电脑里的声音都录时才用得上，不然变灰（记下的选择不动）。
 */
function EchoCancelField({ on, usable, onChange }: { on: boolean; usable: boolean; onChange: (on: boolean) => void }) {
  const checked = on && usable;
  return (
    <label className={`${choiceClass(checked)} ${usable ? "cursor-pointer hover:border-line-strong" : "cursor-not-allowed"}`}>
      <input
        type="checkbox"
        checked={checked}
        disabled={!usable}
        onChange={(e) => onChange(e.target.checked)}
        className="mt-1 shrink-0 self-start accent-brand"
      />
      <span className="flex flex-col gap-1">
        <b className={`text-sm font-semibold ${usable ? "" : "text-muted"}`}>外放时去掉回声（实验）</b>
        <span className="text-[13px] leading-relaxed text-muted">
          用扬声器外放开会时，麦克风也会录到会议里其他人的声音，转写里同一句话会出现两次。打开后，zsmeet 会用录到的电脑声音把它从麦克风里去掉，不影响会议软件和别的 App 播放的声音。戴耳机（包括 AirPods）时没有回声，会自动不处理。
        </span>
        <span className="text-[13px] leading-relaxed text-muted">
          这是实验功能，默认关闭，效果还在验证：外放音量开得很大、用蓝牙音箱或电视外放时，回声可能去不干净；用 AirPlay 外放时不处理。你和别人同时说话时，如果转写里你说的话缺了，关掉这个选项、改用耳机。录音时页面上会显示回声消除现在开没开。
        </span>
        {!usable && <span className="text-xs leading-relaxed text-warn">要同时录麦克风和电脑里的声音才能用。</span>}
      </span>
    </label>
  );
}

/**
 * 没能开始录音：新建会议失败（多半是连不上服务器），或者录音程序没能开始。没有权限、设备用不了时，
 * 录音程序会说是哪一路、怎么打开（warnings，App 留到下次开始录音）。
 */
function StartFailure({ error, warnings }: { error: unknown; warnings: RecorderWarning[] }) {
  if (!(error instanceof RecorderStartError)) {
    const network = error instanceof ApiError && (error.status === 0 || error.status === 502);
    return (
      <Notice tone="bad">
        没能在服务器上建好这场会议：{sentence(errorMessage(error))}
        {network && "开始录音时要连得上服务器，开始以后断网也能接着录。"}
      </Notice>
    );
  }
  return (
    <Notice tone="bad">
      <div className="flex flex-col gap-1.5">
        <span>没能开始录音：{sentence(error.message)}</span>
        {warnings.map((w, i) => (
          <span key={i}>{warningText(w)}</span>
        ))}
      </div>
    </Notice>
  );
}

// ---- 录音中 ----

/** 「回声消除：…」的颜色：开着是绿色，不可用是黄色，没开（戴着耳机这些）和旁边的字一样。 */
const ECHO_TONE: Record<EchoLabel["tone"], string> = { ok: "text-ok", mute: "", warn: "text-warn" };

/**
 * 录音中。micQuiet：麦克风的声音（录音程序自动调大以后的）一直很小，提示把电脑挪近一些、调大输入音量
 * （recorderView.ts 的 micTooQuiet）。
 */
function Live({ state, micQuiet }: { state: RecorderState; micQuiet: boolean }) {
  const paused = state.status === "paused";
  const [confirming, setConfirming] = useState(false);
  const act = useMutation({ mutationFn: (run: () => Promise<unknown>) => run() });
  // 麦克风的名字跟着 App 报的走：选的是系统默认的麦克风、录音中换了（比如连上了 AirPods）时，这里也跟着换
  const labels = sourceLabels(state);
  const echo = echoLabel(state);
  const gain = micGainLabel(state);
  const upload = liveUpload(state.uploads.find((u) => u.meetingId === state.meetingId));

  return (
    <>
      <Card className="flex flex-col gap-5 p-6">
        <div className="flex flex-col gap-1">
          <b className="text-lg font-bold">{state.title}</b>
          <div className="flex flex-wrap gap-x-4 gap-y-1 text-[13px] text-muted">
            <span>麦克风：{labels.mic}</span>
            <span>电脑里的声音：{labels.system}</span>
            {echo && <span className={ECHO_TONE[echo.tone]}>{echo.text}</span>}
            {gain && <span className="text-warn">{gain}</span>}
          </div>
        </div>

        <div className="flex flex-wrap items-center gap-x-6 gap-y-3">
          <div className="flex items-center gap-3">
            <span className={`size-2.5 rounded-full ${paused ? "bg-muted" : "animate-pulse bg-bad"}`} aria-hidden="true" />
            <span className="text-sm font-semibold">{paused ? "已暂停" : "录音中"}</span>
            <span className="font-mono text-3xl font-semibold tabular-nums" role="timer" aria-label="已录时长">
              {hms(state.seconds)}
            </span>
          </div>
          {/* 两条音量条带着名字至少要 340px（40 根柱子加名字），放不下时换到计时的下面一行 */}
          <div className="flex min-w-[340px] flex-1 flex-col gap-1">
            {state.mic !== null && <Meter label="麦克风" read={() => recorderState().level?.mic ?? 0} paused={paused} />}
            {state.system && <Meter label="电脑" read={() => recorderState().level?.system ?? 0} paused={paused} />}
          </div>
        </div>

        <div className="flex flex-wrap gap-2.5">
          <Button disabled={act.isPending} onClick={() => act.mutate(paused ? recorderResume : recorderPause)}>
            <Icon name={paused ? "play" : "pause"} />
            {paused ? "继续录音" : "暂停"}
          </Button>
          <Button disabled={act.isPending} onClick={() => act.mutate(recorderMark)}>
            <Icon name="flag" />
            标记重点
            {state.marks.length > 0 && <span className="tabular-nums text-muted">{state.marks.length}</span>}
          </Button>
          <Button variant="danger" disabled={act.isPending || confirming} onClick={() => setConfirming(true)}>
            <Icon name="stop" />
            结束录音
          </Button>
        </div>
        {confirming && (
          <div className="flex flex-col gap-2.5 rounded-xl bg-warn-tint/60 px-4 py-3 text-sm">
            <span className="text-ink-2">确定结束录音吗？结束以后这一场就不能再接着录了。录音传完会自动开始转写、生成纪要。</span>
            <span className="flex gap-2">
              <Button size="sm" variant="danger" disabled={act.isPending} onClick={() => act.mutate(recorderStop, { onSettled: () => setConfirming(false) })}>
                确定结束
              </Button>
              <Button size="sm" onClick={() => setConfirming(false)}>
                取消
              </Button>
            </span>
          </div>
        )}

        {act.isError && <Notice tone="bad">{errorMessage(act.error)}</Notice>}
        {state.error && <Notice tone="bad">{state.error}</Notice>}
        {state.warnings.map((w, i) => (
          <Notice key={i} tone="warn">
            {warningText(w)}
          </Notice>
        ))}
        {micQuiet && state.mic !== null && <Notice tone="warn">{quietMicHint(state.micGain)}</Notice>}
        {upload.trouble && <Notice tone={upload.tone}>{upload.trouble}</Notice>}

        <div className="flex flex-col gap-1 border-t border-line pt-4 text-[13px]">
          <span className="font-semibold text-ink-2">上传情况（每 10 秒左右一片）</span>
          <span className="text-muted">{upload.text}</span>
        </div>
      </Card>

      <RecordingMarks marks={state.marks} />

      <p className="text-xs leading-relaxed text-muted">
        关掉这个窗口录音也不会停：菜单栏的 zsmeet 图标旁边显示录了多久，点开可以暂停、标记重点、结束录音。退出 zsmeet 会结束录音，没传完的部分下次打开时接着传。
      </p>
    </>
  );
}

function Meter({ label, read, paused }: { label: string; read: () => number; paused: boolean }) {
  return (
    <div className="flex items-center gap-3">
      <span className="w-12 shrink-0 text-xs text-muted">{label}</span>
      <LevelMeter read={read} active paused={paused} />
    </div>
  );
}

// ---- 结束以后 ----

/** 刚结束的那一场：上传进度，传完以后去会议页面看处理进度。 */
function EndedCard({ ended, state }: { ended: Ended; state: RecorderState }) {
  const upload = endedUpload(state, ended);
  if (!upload) return <EndedGoneCard ended={ended} />;
  const done = upload.status === "completed";
  const settled = done || upload.status === "interrupted" || upload.status === "failed";
  // 在这里看到已经传完了：页面顶部不用再提示一遍
  const seen = () => {
    if (done) dismissUpload(ended.meetingId);
  };
  return (
    <Card className="flex flex-col gap-4 p-6">
      <div className="flex flex-col gap-1">
        <b className="text-lg font-bold">
          {upload.status === "interrupted" ? "录音中断了" : "录音已结束"} · {hms(ended.seconds)}
        </b>
        <span className="text-sm text-muted">
          {ended.title}
          {ended.marks > 0 && ` · 标记了 ${ended.marks} 个重点`}
        </span>
      </div>
      {/* 录音自己停下来的原因（录满了时长、出错了）；和上传情况里说的一样时不重复 */}
      {state.error && state.error !== upload.error && <Notice tone="warn">{sentence(state.error)}</Notice>}
      <UploadRow upload={upload} where="record" canStart />
      {!settled && <p className="text-xs leading-relaxed text-muted">可以关掉这个窗口，也可以接着录下一场。退出 zsmeet 也没关系，没传完的部分下次打开时接着传。</p>}
      <div className="flex flex-wrap gap-2.5">
        <AppLink to={`/meetings/${ended.meetingId}`} className={buttonClass(done ? "primary" : "default")} onClick={seen}>
          {done ? "查看处理进度" : "打开会议页面"}
        </AppLink>
        <Button
          onClick={() => {
            seen();
            clearEnded();
          }}
        >
          再录一场
        </Button>
      </div>
    </Card>
  );
}

/** 刚结束的那一场在这台电脑上已经没有了：什么也没录下来（空会议已经删了），或者录音被删掉了。 */
function EndedGoneCard({ ended }: { ended: Ended }) {
  const gone = endedGone(ended);
  return (
    <Card className="flex flex-col gap-4 p-6">
      <div className="flex flex-col gap-1">
        <b className="text-lg font-bold">{gone.title}</b>
        <span className="text-sm text-muted">{ended.title}</span>
      </div>
      <p className="text-sm leading-relaxed text-ink-2">{gone.text}</p>
      <div className="flex flex-wrap gap-2.5">
        {!ended.empty && (
          <AppLink to={`/meetings/${ended.meetingId}`} className={buttonClass("default")}>
            打开会议页面
          </AppLink>
        )}
        <Button variant={ended.empty ? "primary" : "default"} onClick={clearEnded}>
          再录一场
        </Button>
      </div>
    </Card>
  );
}

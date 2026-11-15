import { useEffect, useState } from "react";
import type { FormEvent, ReactNode } from "react";
import { errorMessage } from "../api/client";
import type { User } from "../api/client";
import { AppLink } from "../components/AppLink";
import { LevelMeter, RecordingMarks } from "../components/RecordingParts";
import { Button, buttonClass, Card, Field, Icon, Input, Notice, Select } from "../components/ui";
import { isEn } from "../i18n/lang";
import { tr, useLang } from "../i18n/react";
import { hms } from "../lib/format";
import type { MeetingASRLanguage } from "../lib/format";
import { canRecordTab, microphones, session, SLICE_MS, unsupportedReason, useRecording } from "../lib/recording/session";
import type { Snapshot, Source } from "../lib/recording/session";
import type { Trouble } from "../lib/recording/upload";
import { useTicker } from "../lib/useTicker";
import { ASRLanguageField, SpeakerCountField, useTemplates, VisibilityField } from "./UploadDialog";

const SLICE_SEC = SLICE_MS / 1000;

export function RecordPage({ user }: { user: User }) {
  useLang();
  const rec = useRecording();
  return (
    <div className="mx-auto flex max-w-3xl flex-col gap-5">
      <div className="flex flex-col gap-1.5">
        <h1 className="text-[26px] font-bold">{tr("网页录音", "Web Recording")}</h1>
        <p className="text-sm text-muted">
          {tr("录音边录边传到服务器。断网也不会丢，录完自动出纪要。", "Streams audio to the server as you record. Safe across network drops, with minutes generated automatically afterwards.")}
        </p>
      </div>
      {rec.phase === "idle" || rec.phase === "starting" ? (
        <Setup user={user} rec={rec} />
      ) : rec.phase === "recording" || rec.phase === "paused" ? (
        <Live rec={rec} />
      ) : (
        <Ended rec={rec} />
      )}
    </div>
  );
}

// ---- 开始前：选声音来源、试音、填会议信息 ----

const SOURCES: { value: Source; titleZh: string; titleEn: string; textZh: string; textEn: string }[] = [
  {
    value: "mic",
    titleZh: "只录麦克风",
    titleEn: "Microphone only",
    textZh: "线下会议室、面对面聊天。电脑或手机放在桌子中间效果最好。",
    textEn: "In-person meetings or discussions. Place your laptop or phone near the center of the table for best results.",
  },
  {
    value: "mic+tab",
    titleZh: "麦克风 + 浏览器标签页的声音",
    titleEn: "Microphone + browser tab audio",
    textZh: "在浏览器里开的线上会议。开始后选中会议所在的标签页，并打开「同时共享标签页音频」。建议戴耳机，免得对方的声音被麦克风再录一遍。",
    textEn: "Online meetings running in a browser tab. Select the meeting tab after starting and enable \"Also share tab audio\". Headphones recommended to avoid echo.",
  },
];

function Setup({ user, rec }: { user: User; rec: Snapshot }) {
  useLang();
  const templates = useTemplates();
  const unsupported = unsupportedReason();
  const tabOk = canRecordTab();

  const [title, setTitle] = useState("");
  const [source, setSource] = useState<Source>("mic");
  const [template, setTemplate] = useState("general");
  const [hotwords, setHotwords] = useState("");
  const [visibility, setVisibility] = useState<"team" | "private">("team");
  const [speakerCount, setSpeakerCount] = useState(0);
  const [language, setLanguage] = useState<MeetingASRLanguage>("");
  const [devices, setDevices] = useState<MediaDeviceInfo[]>([]);
  const [deviceId, setDeviceId] = useState("");
  const [micError, setMicError] = useState("");

  // 打开选中的麦克风试音；离开这个页面时关掉（已经开始录音的话不关）
  useEffect(() => {
    if (unsupported) return;
    let cancelled = false;
    session
      .preview(deviceId)
      .then(async () => {
        if (cancelled) return;
        setMicError("");
        setDevices(await microphones());
      })
      .catch((e: unknown) => {
        if (cancelled) return;
        setMicError(micProblem(e));
      });
    return () => {
      cancelled = true;
    };
  }, [deviceId, unsupported]);
  useEffect(() => () => session.stopPreview(), []);

  // 插拔麦克风时更新列表
  useEffect(() => {
    if (unsupported) return;
    const refresh = () => void microphones().then(setDevices).catch(() => {});
    navigator.mediaDevices.addEventListener("devicechange", refresh);
    return () => navigator.mediaDevices.removeEventListener("devicechange", refresh);
  }, [unsupported]);

  const starting = rec.phase === "starting";
  const submit = (e: FormEvent) => {
    e.preventDefault();
    // 直接在提交（点击）里调用：选标签页的窗口只能由用户的点击触发
    void session.start({ userId: user.id, title, template, hotwords, visibility, speakerCount, language, source });
  };

  if (unsupported) return <Notice tone="warn">{unsupported}</Notice>;

  return (
    <Card className="p-6">
      <form onSubmit={submit} className="flex flex-col gap-5">
        <Field
          label={tr("会议标题", "Meeting title")}
          htmlFor="rec-title"
          hint={tr("留空会用开始录音的时间起一个，之后可以改", "Leave blank to name by start time; you can rename it later")}
        >
          <Input id="rec-title" value={title} onChange={(e) => setTitle(e.target.value)} maxLength={200} disabled={starting} />
        </Field>

        <fieldset className="flex flex-col gap-2.5" disabled={starting}>
          <legend className="mb-1.5 text-[13px] font-semibold text-ink-2">{tr("录什么声音", "Audio sources")}</legend>
          {SOURCES.map((s) => {
            const disabled = s.value === "mic+tab" && !tabOk;
            return (
              <label
                key={s.value}
                className={`flex gap-3 rounded-xl border px-4 py-3 ${source === s.value ? "border-brand bg-brand-tint/50" : "border-line"} ${
                  disabled ? "cursor-not-allowed opacity-60" : "cursor-pointer hover:border-line-strong"
                }`}
              >
                <input
                  type="radio"
                  name="rec-source"
                  value={s.value}
                  checked={source === s.value}
                  disabled={disabled}
                  onChange={() => setSource(s.value)}
                  className="mt-1 accent-brand"
                />
                <span className="flex flex-col gap-1">
                  <b className="text-sm font-semibold">{tr(s.titleZh, s.titleEn)}</b>
                  <span className="text-[13px] leading-relaxed text-muted">
                    {disabled
                      ? tr("只有电脑上的 Chrome 和 Edge 能录标签页的声音。", "Only desktop Chrome and Edge support capturing browser tab audio.")
                      : tr(s.textZh, s.textEn)}
                  </span>
                </span>
              </label>
            );
          })}
          <p className="text-xs leading-relaxed text-muted">
            {tr(
              "用桌面版的腾讯会议、Zoom 开会时，浏览器录不到对方的声音。可以用会议软件自带的录制，会后在会议列表里上传。",
              "Browsers cannot capture desktop apps like Zoom or Tencent Meeting. Use the zsmeet desktop app, or record within the meeting app and upload afterwards.",
            )}
          </p>
        </fieldset>

        <Field label={tr("麦克风", "Microphone")} htmlFor="rec-mic">
          <div className="flex flex-col gap-2.5">
            <Select id="rec-mic" value={deviceId} onChange={(e) => setDeviceId(e.target.value)} disabled={starting || devices.length === 0}>
              {devices.length === 0 && (
                <option value="">{micError ? tr("用不了麦克风", "Microphone unavailable") : tr("正在打开麦克风…", "Opening microphone…")}</option>
              )}
              {devices.length > 0 && <option value="">{tr("系统默认的麦克风", "System default microphone")}</option>}
              {devices
                .filter((d) => d.deviceId !== "default")
                .map((d, i) => (
                  <option key={d.deviceId} value={d.deviceId}>
                    {d.label || (isEn() ? `Microphone ${i + 1}` : `麦克风 ${i + 1}`)}
                  </option>
                ))}
            </Select>
            {micError ? (
              <Notice tone="warn">{micError}</Notice>
            ) : (
              <div className="flex items-center gap-3">
                <span className="shrink-0 text-xs text-muted">{tr("试一下：说句话", "Test mic: say something")}</span>
                <LevelMeter read={() => session.level("mic")} active={rec.micLive} />
              </div>
            )}
          </div>
        </Field>

        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <Field label={tr("纪要模板", "Minutes template")} htmlFor="rec-template">
            <Select id="rec-template" value={template} onChange={(e) => setTemplate(e.target.value)} disabled={starting}>
              {(templates.data?.items ?? []).map((t) => (
                <option key={t.id} value={t.id}>
                  {t.name}
                </option>
              ))}
            </Select>
          </Field>
          <Field label={tr("本场热词", "Hotwords for this meeting")} htmlFor="rec-hotwords">
            <Input
              id="rec-hotwords"
              value={hotwords}
              onChange={(e) => setHotwords(e.target.value)}
              maxLength={500}
              placeholder={tr("人名、产品名，用逗号分开", "Names, product terms, comma-separated")}
              disabled={starting}
            />
          </Field>
        </div>
        <ASRLanguageField id="rec-language" value={language} onChange={setLanguage} disabled={starting} />
        <SpeakerCountField id="rec-speakers" value={speakerCount} onChange={setSpeakerCount} disabled={starting} />
        <VisibilityField id="rec-visibility" value={visibility} onChange={setVisibility} disabled={starting} />

        {rec.error && <Notice tone="bad">{rec.error}</Notice>}

        <div className="flex flex-wrap items-center justify-between gap-3">
          <span className="text-xs leading-relaxed text-muted">
            {tr(
              "录音期间这个网页要一直开着，可以切到别的标签页，也可以在 zsmeet 里看别的页面。",
              "Keep this tab open while recording — you can switch to other tabs or browse other pages within zsmeet.",
            )}
          </span>
          <Button type="submit" variant="primary" disabled={starting || !rec.micLive}>
            <Icon name="mic" />
            {starting ? tr("正在开始…", "Starting…") : tr("开始录音", "Start recording")}
          </Button>
        </div>
      </form>
    </Card>
  );
}

/** 打不开麦克风的原因，写成用户能照着做的话。 */
function micProblem(e: unknown): string {
  const name = e instanceof DOMException ? e.name : "";
  if (name === "NotAllowedError" || name === "SecurityError") {
    return tr(
      "浏览器没有允许这个网页使用麦克风。点地址栏左边的图标，把「麦克风」改成「允许」，然后刷新页面。",
      "Microphone access was blocked by the browser. Click the site icon next to the address bar, allow Microphone, and reload the page.",
    );
  }
  if (name === "NotFoundError" || name === "OverconstrainedError") {
    return tr("没有找到麦克风。插上麦克风，或者换一个再试。", "No microphone found. Connect a microphone or choose another device.");
  }
  if (name === "NotReadableError") {
    return tr(
      "麦克风被别的程序占着（比如正在开的会议软件）。关掉它，或者换一个麦克风再试。",
      "The microphone is busy in another app. Close that app or select another microphone.",
    );
  }
  return isEn() ? `Could not access microphone: ${errorMessage(e)}` : `打不开麦克风：${errorMessage(e)}`;
}

// ---- 录音中 ----

function troubleText(t: Trouble): string {
  switch (t) {
    case "offline":
      return tr(
        "网络断开了，录音没有停。分片先存在这台电脑上，网络恢复后自动补传，不用做任何操作。",
        "Network disconnected — recording continues. Chunks are saved locally and upload automatically once reconnected.",
      );
    case "login":
      return tr(
        "登录失效了，录音没有停。分片先存在这台电脑上，在新标签页里重新登录 zsmeet 后会自动接着传。",
        "Session expired — recording continues. Chunks are saved locally; sign in to zsmeet in a new tab to resume uploading.",
      );
    case "server":
      return tr(
        "服务器暂时连不上，录音没有停。分片先存在这台电脑上，服务器恢复后自动补传。",
        "Server temporarily unreachable — recording continues. Chunks are saved locally and upload automatically once the server recovers.",
      );
  }
}

function Live({ rec }: { rec: Snapshot }) {
  useLang();
  useTicker(500);
  const paused = rec.phase === "paused";
  const pending = rec.chunks - rec.uploaded;
  const [action, setAction] = useState("");

  const retryMic = () => {
    setAction("");
    session.switchMic("").catch((e: unknown) => setAction(micProblem(e)));
  };
  const retryTab = () => {
    setAction("");
    session.reshareTab().catch((e: unknown) => setAction(errorMessage(e)));
  };

  return (
    <>
      <Card className="flex flex-col gap-5 p-6">
        <div className="flex flex-col gap-1">
          <b className="text-lg font-bold">{rec.title}</b>
          <div className="flex flex-wrap gap-x-4 gap-y-1 text-[13px] text-muted">
            <span>
              {isEn()
                ? `Microphone: ${rec.micLive ? rec.micLabel || "Connected" : "Disconnected"}`
                : `麦克风：${rec.micLive ? rec.micLabel || "已连接" : "已断开"}`}
            </span>
            {rec.source === "mic+tab" && (
              <span>
                {isEn()
                  ? `Tab audio: ${rec.tabLive ? "Sharing" : "Stopped sharing"}`
                  : `标签页的声音：${rec.tabLive ? "正在录" : "已停止共享"}`}
              </span>
            )}
          </div>
        </div>

        <div className="flex flex-wrap items-center gap-x-6 gap-y-3">
          <div className="flex items-center gap-3">
            <span className={`size-2.5 rounded-full ${paused ? "bg-muted" : "animate-pulse bg-bad"}`} aria-hidden="true" />
            <span className="text-sm font-semibold">{paused ? tr("已暂停", "Paused") : tr("录音中", "Recording")}</span>
            <span className="font-mono text-3xl font-semibold tabular-nums" role="timer" aria-label={tr("已录时长", "Elapsed time")}>
              {hms(session.elapsed())}
            </span>
          </div>
          <div className="flex min-w-[200px] flex-1 flex-col gap-1">
            <LevelMeter read={() => session.level("mic")} active={rec.micLive} paused={paused} />
            {rec.source === "mic+tab" && <LevelMeter read={() => session.level("tab")} active={rec.tabLive} paused={paused} />}
          </div>
        </div>

        <div className="flex flex-wrap gap-2.5">
          <Button onClick={() => (paused ? session.resume() : session.pause())}>
            <Icon name={paused ? "play" : "pause"} />
            {paused ? tr("继续录音", "Resume") : tr("暂停", "Pause")}
          </Button>
          <Button onClick={() => session.mark()}>
            <Icon name="flag" />
            {tr("标记重点", "Bookmark")}
          </Button>
          <Button variant="danger" onClick={() => void session.stop()}>
            <Icon name="stop" />
            {tr("结束录音", "Stop recording")}
          </Button>
        </div>

        {!rec.micLive && (
          <Problem tone="warn" action={<Button size="sm" onClick={retryMic}>{tr("重新连接麦克风", "Reconnect microphone")}</Button>}>
            {tr("麦克风断开了（拔掉了，或者被系统收回了权限），现在录不到现场的声音。", "Microphone disconnected or permission was revoked — room audio is not being captured.")}
          </Problem>
        )}
        {rec.source === "mic+tab" && !rec.tabLive && (
          <Problem tone="warn" action={<Button size="sm" onClick={retryTab}>{tr("重新选择标签页", "Reselect tab")}</Button>}>
            {tr("标签页的声音已经停止共享，现在只录麦克风。", "Browser tab audio sharing stopped — recording microphone only.")}
          </Problem>
        )}
        {action && <Notice tone="bad">{action}</Notice>}
        {!paused && rec.quietFor >= 60 && (
          <Notice tone="warn">
            {isEn()
              ? `No sound detected for ${Math.floor(rec.quietFor / 60)} min. If the meeting is active, check whether your microphone is muted.`
              : `已经有 ${Math.floor(rec.quietFor / 60)} 分钟没录到声音了。如果会议正在进行，看看麦克风是不是被静音了。`}
          </Notice>
        )}
        {rec.uploadError ? (
          <Notice tone="bad">
            {isEn()
              ? `Upload paused due to an error: ${rec.uploadError} Recording continues — download the audio after stopping and upload it from the Meetings page.`
              : `录音传不上去了：${rec.uploadError} 录音还在继续，结束后可以把它下载到电脑上，再到会议列表里上传。`}
          </Notice>
        ) : (
          rec.trouble && <Notice tone="warn">{troubleText(rec.trouble)}</Notice>
        )}
        {!rec.stored && (
          <Notice tone="warn">
            {tr(
              "这个浏览器没法把录音存在电脑上（可能是无痕模式，或者存储空间满了）。没传上去的部分只在这个网页里，千万别关掉它。",
              "This browser cannot store audio locally (possibly private browsing or full disk). Unuploaded audio lives only in memory — do not close this tab.",
            )}
          </Notice>
        )}

        <div className="flex flex-col gap-1 border-t border-line pt-4 text-[13px]">
          <span className="font-semibold text-ink-2">
            {isEn() ? `Upload status (${SLICE_SEC}s chunks)` : `上传情况（每 ${SLICE_SEC} 秒一片）`}
          </span>
          <span className="text-muted">
            {isEn()
              ? `${rec.uploaded} uploaded${pending > 0 ? ` · ${pending} pending` : ""} · ${rec.uploadError ? "Upload stopped" : rec.trouble ? "Waiting to retry" : "Streaming normally"}`
              : `已上传 ${rec.uploaded} 片${pending > 0 ? ` · 待上传 ${pending} 片` : ""} · ${rec.uploadError ? "已经停止上传" : rec.trouble ? "暂时传不上去" : "网络正常，边录边传"}`}
          </span>
        </div>
      </Card>

      <RecordingMarks marks={rec.marks} />

      <p className="text-xs leading-relaxed text-muted">
        {tr(
          "不要关闭或刷新这个网页。万一关了，已经存在这台电脑上的录音下次打开 zsmeet 时可以接着传。",
          "Do not close or reload this tab. If closed accidentally, chunks already saved on this computer can finish uploading next time you open zsmeet.",
        )}
      </p>
    </>
  );
}

function Problem({ tone, action, children }: { tone: "warn" | "bad"; action: ReactNode; children: ReactNode }) {
  return (
    <Notice tone={tone}>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <span>{children}</span>
        {action}
      </div>
    </Notice>
  );
}

// ---- 结束以后 ----

/** 录音已经停了、在等最后几片传上去时的提示。和录音中的不同：不能说「录音没有停」。 */
function finishingTroubleText(t: Trouble): string {
  switch (t) {
    case "offline":
      return tr("网络断开了。剩下的录音存在这台电脑上，网络恢复后自动补传。", "Network disconnected. Remaining audio is saved locally and will upload once reconnected.");
    case "login":
      return tr("登录失效了。剩下的录音存在这台电脑上，在新标签页里重新登录 zsmeet 后会自动接着传。", "Session expired. Remaining audio is saved locally — sign in to zsmeet in a new tab to finish uploading.");
    case "server":
      return tr("服务器暂时连不上。剩下的录音存在这台电脑上，服务器恢复后自动补传。", "Server unreachable. Remaining audio is saved locally and will upload once the server recovers.");
  }
}

function Ended({ rec }: { rec: Snapshot }) {
  useLang();
  const pending = rec.chunks - rec.uploaded;
  const [note, setNote] = useState<{ tone: "warn" | "bad"; text: string } | null>(null);
  const [confirming, setConfirming] = useState(false);
  const stopReason = rec.stopReason && <Notice tone="warn">{rec.stopReason}</Notice>;

  if (rec.phase === "finishing") {
    return (
      <Card className="flex flex-col gap-3 p-6">
        <b className="text-lg font-bold">{tr("录音已结束", "Recording finished")} · {hms(rec.seconds)}</b>
        {stopReason}
        {pending > 0 ? (
          <>
            <span className="text-sm leading-relaxed text-ink-2">
              {isEn()
                ? `${pending} chunk${pending === 1 ? "" : "s"} remaining (${rec.trouble ? "waiting to retry" : "uploading"}); processing starts once uploaded. Keep this tab open.`
                : `还有 ${pending} 片没传上去，${rec.trouble ? "恢复后自动补传" : "正在传"}，传完才开始处理。传完以前请不要关闭这个网页。`}
            </span>
            {rec.trouble && <Notice tone="warn">{finishingTroubleText(rec.trouble)}</Notice>}
          </>
        ) : (
          <span className="flex items-center gap-2.5 text-sm text-ink-2">
            <span className="size-4 animate-spin rounded-full border-2 border-line-strong border-t-brand" aria-hidden="true" />
            {rec.trouble ? finishingTroubleText(rec.trouble) : tr("正在交给服务器处理…", "Handing off to server for processing…")}
          </span>
        )}
      </Card>
    );
  }

  if (rec.phase === "done") {
    return (
      <Card className="flex flex-col gap-4 p-6">
        <div className="flex flex-col gap-1">
          <b className="text-lg font-bold">{tr("录音已结束", "Recording finished")} · {hms(rec.seconds)}</b>
          <span className="text-sm text-ink-2">
            {tr("录音已经全部传上去，正在转写。几分钟后就能看到转写和纪要。", "All audio uploaded — transcription has started. Transcript and minutes will be ready in a few minutes.")}
          </span>
        </div>
        {stopReason}
        <div className="flex flex-wrap gap-2.5">
          <AppLink to={`/meetings/${rec.meetingId}`} className={buttonClass("primary")}>
            {tr("查看处理进度", "View processing status")}
          </AppLink>
          <Button onClick={() => session.reset()}>{tr("再录一场", "Record another")}</Button>
        </div>
      </Card>
    );
  }

  const download = () => {
    setNote(null);
    session
      .download()
      .then((missing) => {
        if (missing > 0) {
          setNote({
            tone: "warn",
            text: isEn()
              ? `${missing} audio chunk${missing === 1 ? "" : "s"} could not be found locally and ${missing === 1 ? "is" : "are"} missing from the downloaded file.`
              : `有 ${missing} 片录音在这台电脑上找不到了，下载的文件里缺了这几段。`,
          });
        }
      })
      .catch((e: unknown) => setNote({ tone: "bad", text: errorMessage(e) }));
  };

  return (
    <Card className="flex flex-col gap-4 p-6">
      <b className="text-lg font-bold">{tr("录音没能交给服务器处理", "Could not finish uploading recording")}</b>
      {stopReason}
      <Notice tone="bad">{rec.error}</Notice>
      <span className="text-sm leading-relaxed text-ink-2">
        {rec.needsDownload
          ? tr(
              "这个浏览器没能把录音存在电脑上，有一部分录音只在这个网页里。请现在把它下载下来，关掉网页就没了。下载后可以到会议列表里用「上传录音」重新上传。",
              "This browser could not store the audio on disk — part of it exists only in memory. Download it now before closing this tab, then re-upload from the Meetings page.",
            )
          : rec.stored
            ? tr(
                "录音还存在这台电脑上。可以把它下载下来，到会议列表里用「上传录音」重新上传；问题解决以后，也可以在页面顶部的提示里接着传。",
                "The recording is still saved on this computer. You can download it and upload from the Meetings page, or resume uploading from the top banner once the issue is resolved.",
              )
            : tr(
                "可以把录音下载下来，到会议列表里用「上传录音」重新上传。",
                "Download the recording and re-upload it from the Meetings page.",
              )}
      </span>
      {note && <Notice tone={note.tone}>{note.text}</Notice>}
      {confirming ? (
        <div className="flex flex-col gap-2.5 rounded-xl bg-warn-tint/60 px-4 py-3 text-sm">
          <span className="text-ink-2">
            {tr("这段录音还没下载，开始下一场以后就找不回来了。确定不要它了吗？", "This recording hasn't been downloaded and will be lost if you start a new one. Discard it?")}
          </span>
          <span className="flex gap-2">
            <Button size="sm" variant="danger" onClick={() => session.reset(true)}>
              {tr("确定不要了", "Discard")}
            </Button>
            <Button size="sm" onClick={() => setConfirming(false)}>
              {tr("取消", "Cancel")}
            </Button>
          </span>
        </div>
      ) : (
        <div className="flex flex-wrap gap-2.5">
          <Button variant="primary" onClick={download}>
            <Icon name="download" />
            {tr("下载这段录音", "Download recording")}
          </Button>
          <Button onClick={() => session.reset() || setConfirming(true)}>{tr("再录一场", "Record another")}</Button>
        </div>
      )}
    </Card>
  );
}

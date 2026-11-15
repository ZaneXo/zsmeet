import { useMutation } from "@tanstack/react-query";
import { useState } from "react";
import type { ReactNode } from "react";
import { useLocation, useSearchParams } from "react-router";
import { errorMessage } from "../api/client";
import { AppLink } from "../components/AppLink";
import { Button, buttonClass, Notice, ProgressBar } from "../components/ui";
import { tr, useLang } from "../i18n/react";
import { hms } from "../lib/format";
import { dismissUpload, recorderDiscard, recorderFinish, useRecorder } from "./recorder";
import type { RecorderStatus, RecorderUpload } from "./recorder";
import { bannerUploads, BUSY_LABEL, discardConfirm, isLive, liveMeetingText, localMeeting, stuckHere, uploadView } from "./recorderView";
import type { UploadAction, UploadView, UploadWhere } from "./recorderView";

// 桌面版录音在录音页以外的地方：页面顶部的提示条、导航栏上的时长、会议页面上的说明。

/**
 * 页面顶部：正在录音（时长、「回到录音」），和这台电脑上没传完的录音。
 * skip 是当前页面自己会显示的那一场（会议详情页），这里不重复；录音页自己会显示正在录的、刚结束的、要接着录的那一场。
 */
export function DesktopRecorderBanner({ skip }: { skip?: string }) {
  useLang();
  const { state, ended, dismissed } = useRecorder();
  const { pathname } = useLocation();
  const [params] = useSearchParams();
  const onRecordPage = pathname === "/record";

  const hidden = new Set<string>();
  if (skip) hidden.add(skip);
  if (onRecordPage) {
    if (ended) hidden.add(ended.meetingId);
    const resume = params.get("continue");
    if (resume) hidden.add(resume);
  }
  const busy = state.status === "idle" || onRecordPage || state.meetingId === skip ? null : state.status;
  const uploads = bannerUploads(state, hidden, dismissed);
  if (!busy && uploads.length === 0) return null;

  return (
    <div className="flex flex-col gap-2.5">
      {busy && (
        <div className="flex flex-wrap items-center justify-between gap-3 rounded-xl border border-line bg-white px-4 py-2.5 text-sm">
          <span className="flex min-w-0 items-center gap-2.5">
            <RecordingDot status={busy} />
            <b className="shrink-0 font-semibold">{BUSY_LABEL[busy]}</b>
            {isLive(busy) && <span className="shrink-0 font-mono tabular-nums">{hms(state.seconds)}</span>}
            {state.title && <span className="min-w-0 truncate text-muted">{state.title}</span>}
          </span>
          <AppLink to="/record" className={buttonClass("default", "sm")}>
            {tr("回到录音", "Back to recorder")}
          </AppLink>
        </div>
      )}
      {uploads.map((u) => (
        <UploadRow key={u.meetingId} upload={u} where="banner" canStart={state.status === "idle"} />
      ))}
    </div>
  );
}

function RecordingDot({ status }: { status: RecorderStatus }) {
  return <span className={`size-2.5 shrink-0 rounded-full ${status === "recording" ? "animate-pulse bg-bad" : "bg-muted"}`} aria-hidden="true" />;
}

/** 导航栏「录音」旁边：在录时红点加已录时长；录完在传的时候显示「上传中」。 */
export function DesktopRecordingBadge() {
  useLang();
  const { state } = useRecorder();
  const uploading = state.uploads.some((u) => u.status === "uploading" || u.status === "completing");
  if (state.status === "idle" && !uploading) return null;
  const text = isLive(state.status) ? hms(state.seconds) : state.status === "idle" ? tr("上传中", "Uploading") : BUSY_LABEL[state.status];
  return (
    <span className="ml-auto flex items-center gap-1.5 text-xs font-normal tabular-nums text-bad" title={state.status === "idle" ? tr("录音正在上传", "Uploading recording") : BUSY_LABEL[state.status]}>
      <span className={`size-2 rounded-full ${state.status === "recording" ? "animate-pulse bg-bad" : "bg-muted"}`} aria-hidden="true" />
      {text}
    </span>
  );
}

export function ThisComputerRecording({ meetingId, elsewhere, stuck }: { meetingId: string; elsewhere: ReactNode; stuck?: ReactNode }) {
  useLang();
  const { state } = useRecorder();
  const local = localMeeting(state, meetingId);
  if (!local) return <>{elsewhere}</>;
  if (local.kind === "upload") {
    const row = <UploadRow upload={local.upload} where="meeting" canStart={state.status === "idle"} />;
    if (!stuck || !stuckHere(local)) return row;
    return (
      <div className="flex flex-col gap-3">
        {row}
        {stuck}
      </div>
    );
  }
  return (
    <div className="flex flex-wrap items-center justify-between gap-3 rounded-xl bg-brand-tint px-4 py-3 text-sm leading-relaxed text-brand-ink">
      <span>{liveMeetingText(local.status, hms(local.seconds))}</span>
      <AppLink to="/record" className={buttonClass("default", "sm")}>
        {tr("回到录音", "Back to recorder")}
      </AppLink>
    </div>
  );
}

const TONE: Record<UploadView["tone"], string> = {
  ok: "border-ok/25 bg-ok-tint/70",
  info: "border-line bg-white",
  warn: "border-warn/30 bg-warn-tint/60",
  bad: "border-bad/30 bg-bad-tint/40",
};

export function UploadRow({ upload, where, canStart }: { upload: RecorderUpload; where: UploadWhere; canStart: boolean }) {
  useLang();
  const view = uploadView(upload, { where, canStart });
  const [confirming, setConfirming] = useState(false);
  const act = useMutation({ mutationFn: (run: () => Promise<unknown>) => run() });
  const has = (a: UploadAction) => view.actions.includes(a);
  const id = upload.meetingId;

  return (
    <div className={`flex flex-col gap-3 rounded-xl border px-4 py-3.5 text-sm text-ink-2 ${TONE[view.tone]}`}>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <span className="flex min-w-0 items-center gap-2.5 leading-relaxed">
          {view.spinning && <span className="size-4 shrink-0 animate-spin rounded-full border-2 border-line-strong border-t-brand" aria-hidden="true" />}
          {view.text}
        </span>
        {!confirming && view.actions.length > 0 && (
          <span className="flex flex-wrap gap-2">
            {has("view") && (
              <AppLink to={`/meetings/${id}`} className={buttonClass("default", "sm")} onClick={() => dismissUpload(id)}>
                {tr("查看", "View")}
              </AppLink>
            )}
            {has("continue") && (
              <AppLink to={`/record?continue=${encodeURIComponent(id)}`} className={buttonClass("primary", "sm")}>
                {tr("接着录", "Resume")}
              </AppLink>
            )}
            {has("finish") && (
              <Button size="sm" variant={has("continue") ? "default" : "primary"} disabled={act.isPending} onClick={() => act.mutate(() => recorderFinish(id))}>
                {tr("传上去生成纪要", "Upload & generate minutes")}
              </Button>
            )}
            {has("discard") && (
              <Button size="sm" variant="ghost" disabled={act.isPending} onClick={() => setConfirming(true)}>
                {tr("删除", "Delete")}
              </Button>
            )}
            {has("dismiss") && (
              <Button size="sm" variant="ghost" onClick={() => dismissUpload(id)}>
                {tr("知道了", "Dismiss")}
              </Button>
            )}
          </span>
        )}
      </div>
      {view.progress !== null && <ProgressBar value={view.progress} />}
      {confirming && (
        <div className="flex flex-wrap items-center justify-between gap-3 border-t border-line pt-3">
          <span>{discardConfirm(upload)}</span>
          <span className="flex gap-2">
            <Button
              size="sm"
              variant="danger"
              disabled={act.isPending}
              onClick={() => act.mutate(() => recorderDiscard(id), { onSettled: () => setConfirming(false) })}
            >
              {tr("确定删除", "Delete")}
            </Button>
            <Button size="sm" onClick={() => setConfirming(false)}>
              {tr("取消", "Cancel")}
            </Button>
          </span>
        </div>
      )}
      {act.isError && <Notice tone="bad">{errorMessage(act.error)}</Notice>}
    </div>
  );
}

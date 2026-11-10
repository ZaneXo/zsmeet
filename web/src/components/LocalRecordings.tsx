import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { errorMessage, queryClient } from "../api/client";
import type { User } from "../api/client";
import { duration } from "../lib/format";
import { dismissResume, startResume, useResumes } from "../lib/recording/resume";
import type { ResumeState } from "../lib/recording/resume";
import { abandonedRecordings, downloadLocal } from "../lib/recording/session";
import { deleteRecording } from "../lib/recording/store";
import type { LocalRecording } from "../lib/recording/store";
import { NOTHING_SAVED } from "../lib/recording/upload";
import type { Trouble } from "../lib/recording/upload";
import { AppLink } from "./AppLink";
import { Button, buttonClass, Icon, Notice, ProgressBar } from "./ui";

/** 这台电脑上没传完、也没有网页在管的录音（录音的网页关掉了、浏览器崩溃了）。 */
export function useAbandonedRecordings(user: User) {
  return useQuery({
    queryKey: ["local-recordings", user.id],
    queryFn: () => abandonedRecordings(user.id),
    // 别的标签页可能刚录完、刚关掉，回到这个标签页时重新看一眼
    refetchOnWindowFocus: true,
    staleTime: 0,
  });
}

/** 一段没传完的录音，加上这个网页里「接着上传」它的进度（没点过就没有）。 */
export type LocalItem = { rec: LocalRecording; state?: ResumeState };

/**
 * 这台电脑上没传完的录音，和这个网页里正在接着传、刚传完、传失败的录音。
 * 传完以后本机的缓存就删了，但「已经传上去」的提示要留到用户点「知道了」。
 */
export function useLocalItems(user: User): LocalItem[] {
  const list = useAbandonedRecordings(user).data ?? [];
  const resumes = useResumes();
  const items: LocalItem[] = list.map((rec) => ({ rec, state: resumes.get(rec.meetingId)?.state }));
  for (const [id, r] of resumes) {
    if (!list.some((rec) => rec.meetingId === id)) items.push(r);
  }
  return items;
}

/** 页面顶部的提示：有录音还没传完。skip 是当前页面自己会显示的那一场（会议详情页），这里不重复。 */
export function AbandonedRecordingsBanner({ user, skip }: { user: User; skip?: string }) {
  const items = useLocalItems(user).filter((item) => item.rec.meetingId !== skip);
  if (items.length === 0) return null;
  return (
    <div className="flex flex-col gap-2.5">
      {items.map((item) => (
        <LocalRecordingCard key={item.rec.meetingId} item={item} />
      ))}
    </div>
  );
}

const TROUBLE: Record<Trouble, string> = {
  offline: "网络断开了，恢复后自动接着传。",
  login: "登录失效了，重新登录后自动接着传。",
  server: "服务器暂时连不上，恢复后自动接着传。",
};

/** 一段没传完的录音：接着传、下载到电脑，或者放弃。 */
export function LocalRecordingCard({ item: { rec, state } }: { item: LocalItem }) {
  const [error, setError] = useState("");
  const [confirming, setConfirming] = useState(false);

  const discard = async () => {
    try {
      await deleteRecording(rec.meetingId);
      dismissResume(rec.meetingId);
      void queryClient.invalidateQueries({ queryKey: ["local-recordings"] });
    } catch (e) {
      setError(errorMessage(e));
    }
  };
  const download = () => {
    setError("");
    downloadLocal(rec).catch((e: unknown) => setError(errorMessage(e)));
  };

  if (state?.status === "done") {
    return (
      <Notice tone="ok">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <span>「{rec.title}」的录音已经全部传上去，正在处理。</span>
          <span className="flex gap-2">
            <AppLink to={`/meetings/${state.meetingId}`} className={buttonClass("default", "sm")} onClick={() => dismissResume(rec.meetingId)}>
              查看
            </AppLink>
            <Button size="sm" variant="ghost" onClick={() => dismissResume(rec.meetingId)}>
              知道了
            </Button>
          </span>
        </div>
      </Notice>
    );
  }

  const uploading = state?.status === "uploading" ? state : null;
  const nothingSaved = rec.chunks === 0;
  const started = new Date(rec.startedAt).toLocaleString("zh-CN", { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit" });
  return (
    <div className="flex flex-col gap-3 rounded-xl border border-warn/30 bg-warn-tint/60 px-4 py-3.5 text-sm">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex min-w-0 flex-col gap-0.5">
          <b className="font-semibold">这台电脑上有一段录音还没传完：「{rec.title}」</b>
          <span className="text-[13px] text-ink-2">
            {nothingSaved
              ? `${started} 开始。${NOTHING_SAVED}`
              : `${started} 开始，录了 ${duration(rec.seconds)}${rec.finished ? "。" : "（录音的网页是中途关掉的，最后 10 秒左右可能没存下来）。"}`}
          </span>
        </div>
        {!uploading && !confirming && (
          <span className="flex flex-wrap gap-2">
            {!nothingSaved && (
              <>
                <Button size="sm" variant="primary" onClick={() => startResume(rec)}>
                  {state?.status === "error" ? "再试一次" : "接着上传"}
                </Button>
                <Button size="sm" onClick={download}>
                  <Icon name="download" size={15} />
                  下载到电脑
                </Button>
              </>
            )}
            <Button size="sm" variant="ghost" onClick={() => setConfirming(true)}>
              放弃
            </Button>
          </span>
        )}
      </div>
      {confirming && (
        <div className="flex flex-wrap items-center justify-between gap-3 border-t border-warn/20 pt-3">
          <span className="text-ink-2">
            {nothingSaved
              ? "确定放弃吗？"
              : "确定放弃吗？这台电脑上的这段录音会被删掉，不能恢复。服务器已经收到的部分（连同标记的重点）还在，可以在会议页面里用它生成纪要。"}
          </span>
          <span className="flex gap-2">
            <Button size="sm" variant="danger" onClick={() => void discard()}>
              确定放弃
            </Button>
            <Button size="sm" onClick={() => setConfirming(false)}>
              取消
            </Button>
          </span>
        </div>
      )}
      {uploading && (
        <div className="flex flex-col gap-1.5">
          <span className="text-[13px] text-ink-2">
            {uploading.uploaded < uploading.total ? `正在上传 ${uploading.uploaded} / ${uploading.total} 片` : "正在交给服务器处理…"}
            {uploading.trouble ? `：${TROUBLE[uploading.trouble]}` : ""}
          </span>
          <ProgressBar value={uploading.total ? uploading.uploaded / uploading.total : 1} />
        </div>
      )}
      {state?.status === "error" && <Notice tone="bad">{state.message}</Notice>}
      {error && <Notice tone="bad">{error}</Notice>}
    </div>
  );
}

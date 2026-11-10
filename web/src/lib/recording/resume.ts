import { useSyncExternalStore } from "react";
import { errorMessage, queryClient } from "../../api/client";
import type { LocalRecording } from "./store";
import { resumeUpload, withLock } from "./upload";
import type { Trouble } from "./upload";

// 「接着上传」的进度。放在页面外面：上传可能要好几分钟，期间切换页面、提示卡片重新渲染，
// 都还能看到同一份进度和结果。

export type ResumeState =
  | { status: "uploading"; uploaded: number; total: number; trouble: Trouble | null }
  | { status: "done"; meetingId: string }
  | { status: "error"; message: string };

export type Resume = { rec: LocalRecording; state: ResumeState };

let resumes: ReadonlyMap<string, Resume> = new Map();
const listeners = new Set<() => void>();

function set(meetingId: string, entry: Resume | null) {
  const next = new Map(resumes);
  if (entry) next.set(meetingId, entry);
  else next.delete(meetingId);
  resumes = next;
  for (const listener of listeners) listener();
}

function subscribe(listener: () => void) {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

/** 这个网页里发起过的「接着上传」：进行中的、传完的、失败的，直到用户点「知道了」。 */
export function useResumes(): ReadonlyMap<string, Resume> {
  return useSyncExternalStore(subscribe, () => resumes);
}

function refresh(meetingId: string) {
  void queryClient.invalidateQueries({ queryKey: ["local-recordings"] });
  void queryClient.invalidateQueries({ queryKey: ["meetings"] });
  void queryClient.invalidateQueries({ queryKey: ["meeting", meetingId] });
}

/** 接着上传一段没传完的录音。已经在传的不会再开一次。 */
export function startResume(rec: LocalRecording) {
  const id = rec.meetingId;
  if (resumes.get(id)?.state.status === "uploading") return;
  set(id, { rec, state: { status: "uploading", uploaded: 0, total: rec.chunks, trouble: null } });
  withLock(id, () => resumeUpload(rec, (uploaded, total, trouble) => set(id, { rec, state: { status: "uploading", uploaded, total, trouble } })))
    .then((meeting) =>
      set(id, { rec, state: meeting ? { status: "done", meetingId: meeting.id } : { status: "error", message: "这段录音正在另一个标签页里上传。" } }),
    )
    .catch((e: unknown) => set(id, { rec, state: { status: "error", message: errorMessage(e) } }))
    .finally(() => refresh(id));
}

/** 不再显示这一条（传完了点「知道了」，或者出错以后关掉提示）。 */
export function dismissResume(meetingId: string) {
  set(meetingId, null);
  refresh(meetingId);
}

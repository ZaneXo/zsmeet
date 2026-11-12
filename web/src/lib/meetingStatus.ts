import type { Meeting } from "../api/client";

/**
 * 会议在等录音传完：网页录音、桌面版录音的会议，录音边录边传，传完、结束以后服务器才开始处理。
 * 会议列表和会议页面要定时刷新，传完以后跟着变成「排队中」「处理中」。
 * 网页版、桌面版都要盯着桌面版录的会议：录音在某一台电脑上（可能是同事的），传完的时候这边不会收到别的通知。
 */
export function waitingForRecording(m: Pick<Meeting, "source" | "status"> | undefined): boolean {
  return (m?.source === "record" || m?.source === "desktop") && m.status === "uploading";
}

// 还没传完的录音：网页录音和桌面版录音都是边录边传，录音的电脑找不回来时，创建者和管理员可以在会议页面上
// 先用服务器已经收到的部分生成纪要（MeetingDetail.tsx 的 ReceivedSoFar）。
// 这里是不依赖 React 的部分：收到的能用几片、大约多长、录音是不是还在传，方便单独测试（unfinishedRecording.test.ts）。
// 引用别的文件里的值要带 .ts 后缀：测试用 Node 直接跑，它不会自己补后缀
import type { Meeting } from "../api/client";
import { dateTime } from "./format.ts";

/** 边录边传的会议来源：网页录音、桌面版录音。 */
export type RecordingSource = Extract<Meeting["source"], "record" | "desktop">;

/**
 * 服务器收到的分片里，从第 0 片开始连续的有几片：分片按序号接起来才是录音，生成纪要只能用这一段。
 * 网页和桌面版都按序号一片一片地传，正常情况下收到的就是从第 0 片开始连续的一段。
 */
export function contiguous(received: readonly number[]): number {
  const have = new Set(received);
  let n = 0;
  while (have.has(n)) n++;
  return n;
}

/**
 * 桌面版录音每片多少毫秒：App 交给录音程序的 --chunk-seconds（desktop/src-tauri/src/recorder/mod.rs 的
 * CHUNK_SECONDS，见 docs/desktop.md）。按录到的声音算，不算暂停的时间；暂停、结束时正在写的那一片提前写完，会短一些。
 * 桌面版里说「已经录下大约几分钟」也按它算（platform/recorderView.ts 的 recordedLength）。
 */
export const DESKTOP_CHUNK_MS = 10_000;

/**
 * 服务器收到的前 chunks 片大约有多长（秒）。网页录音每 sliceMs 切一片（recording/store.ts 的 SLICE_MS，
 * 由调用方传进来，这里就不用引用录音的代码）；桌面版按录音程序的分片长度算，和 sliceMs 无关。
 * 不满一片的（网页录音的第一片只有 1 秒左右；桌面版每次暂停、结束前的那一片）也按一整片算，
 * 所以实际的录音可能比算出来的短一些，界面上说「大约」。
 */
export function receivedSeconds(source: RecordingSource, chunks: number, sliceMs: number): number {
  const each = source === "desktop" ? DESKTOP_CHUNK_MS : sliceMs;
  return Math.round((Math.max(0, chunks) * each) / 1000);
}

/** 多久没收到新的分片，就不再当作录音还在进行（录音的网页关掉了、电脑断网了，或者录音暂停着）。 */
export const RECORDING_STALE_MS = 90_000;

/** 看录音还在不在传要用到的会议字段。 */
type ChunkTimes = Pick<Meeting, "upload_idle_sec" | "last_chunk_at" | "created_at">;

/**
 * 多久没收到新的分片了（毫秒）：从最后一次收到分片算起，还没收到过的从建会议算起。
 * 用服务器算好的 upload_idle_sec。last_chunk_at、created_at 是服务器记的时间，拿这台电脑的时钟去减，
 * 会差出两边时钟的误差（服务器没有自动对时的话，差几分钟也不奇怪）：录音明明还在传，也会被当成断了。
 * upload_idle_sec 是取会议时的值，会议页面、会议列表在等录音时每 10、15 秒重新取一次。
 * 连着还没有这一项的旧版本服务器时（桌面版连着没升级的服务器），才用这台电脑的时钟 now 去减。
 */
export function idleMs(m: ChunkTimes, now: number): number {
  if (m.upload_idle_sec !== undefined) return m.upload_idle_sec * 1000;
  return now - Date.parse(m.last_chunk_at ?? m.created_at);
}

/**
 * 最近一直有新的分片传上来：还在录，或者正在补传。还没收到分片的从建会议的时间算起。
 * 这时不让用已经收到的部分生成纪要，免得把正在录、正在传的录音截断。
 * 会议列表上的「录音中」「电脑录音中」也按它算（Meetings.tsx 的 meetingChip），两边说法一致。
 */
export function chunksFlowing(m: ChunkTimes, now: number): boolean {
  return idleMs(m, now) < RECORDING_STALE_MS;
}

/** 「最后一次收到录音：2 分钟前」，还没收到过的是「服务器还没有收到录音」。多久以前按 idleMs 算，超过一天的写日期和时间。 */
export function lastReceived(m: ChunkTimes, now: number): string {
  if (!m.last_chunk_at) return "服务器还没有收到录音";
  const sec = Math.max(0, idleMs(m, now) / 1000);
  if (sec < 60) return "最后一次收到录音：刚刚";
  if (sec < 3600) return `最后一次收到录音：${Math.floor(sec / 60)} 分钟前`;
  if (sec < 86400) return `最后一次收到录音：${Math.floor(sec / 3600)} 小时前`;
  return `最后一次收到录音：${dateTime(m.last_chunk_at)}`;
}

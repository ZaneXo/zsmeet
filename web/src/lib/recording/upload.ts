import { api, ApiError, errorMessage, unwrap } from "../../api/client";
import type { Meeting } from "../../api/client";
import { sendChunk } from "../upload";
import { deleteRecording, getChunk, SLICE_MS } from "./store";
import type { LocalRecording } from "./store";

// 录音分片的上传。和上传文件不同，这里遇到断网、服务器出错不会放弃：录音可能要录好几个小时，
// 中间网络断多久都要等它恢复了接着传。

/** 上传失败以后隔多久再试（毫秒），后面一直按最后一个间隔试。网络一恢复就立刻试，不等 */
const WAITS = [1000, 2000, 5000, 10_000, 20_000, 30_000];
const waitBefore = (attempt: number) => WAITS[Math.min(attempt, WAITS.length - 1)] ?? 30_000;

/** 一片最多传多久。一片只有几十 KB，传这么久还没完，多半是连接悄悄断了，断开重来 */
const PUT_TIMEOUT_MS = 60_000;

/** 暂时传不上去的原因，恢复以后自动消失。 */
export type Trouble = "offline" | "login" | "server";

/** 这几种失败等一等就会好，其他的（会议被删了、没有权限、会议已经在别处结束了）重试也没用。 */
function transient(e: ApiError): Trouble | null {
  if (e.status === 0) return "offline";
  if (e.status === 401) return "login";
  if (e.status >= 500 || e.status === 408 || e.status === 429) return "server";
  return null;
}

/**
 * 按序号从小到大一片一片地传。服务器上收到的永远是从第 0 片开始连续的一段，
 * 所以录音网页万一找不回来了，已经传上去的部分也能直接拿去生成纪要。
 */
export class Uploader {
  /** 第 0 到 uploaded-1 片已经传上去了 */
  uploaded = 0;
  /** 第 0 到 available-1 片可以传 */
  available = 0;
  trouble: Trouble | null = null;
  /** 传不上去了，不会再试 */
  fatal: ApiError | null = null;

  private running = false;
  private stopped = false;
  private wake: (() => void) | null = null;
  private waiters: (() => void)[] = [];
  private readonly abort = new AbortController();

  constructor(
    private readonly meetingId: string,
    private readonly read: (seq: number) => Promise<Blob | undefined>,
    private readonly onChange: () => void,
    private readonly onUploaded: (seq: number) => void = () => {},
  ) {
    window.addEventListener("online", this.retryNow);
  }

  /** 网络恢复了：正在等待重试的话马上试。 */
  retryNow = () => this.wake?.();

  /** 现在一共有 count 片可以传。 */
  add(count: number) {
    this.available = Math.max(this.available, count);
    void this.loop();
  }

  /** 等到全部传完。传不上去了就抛出原因。 */
  async drained(): Promise<void> {
    for (;;) {
      if (this.fatal) throw this.fatal;
      if (this.stopped) throw new ApiError(0, "上传已经停止");
      if (this.uploaded >= this.available) return;
      await new Promise<void>((resolve) => this.waiters.push(resolve));
    }
  }

  dispose() {
    this.stopped = true;
    window.removeEventListener("online", this.retryNow);
    this.abort.abort();
    this.wake?.();
    this.notify();
  }

  private notify() {
    const waiters = this.waiters;
    this.waiters = [];
    for (const resolve of waiters) resolve();
    this.onChange();
  }

  private async loop() {
    if (this.running) return;
    this.running = true;
    let attempt = 0;
    try {
      while (!this.stopped && !this.fatal && this.uploaded < this.available) {
        const seq = this.uploaded;
        let body: Blob | undefined;
        try {
          body = await this.read(seq);
        } catch (e) {
          this.fatal = new ApiError(0, `读不出这台电脑上存的第 ${seq + 1} 片录音：${errorMessage(e)}`);
          break;
        }
        if (!body) {
          this.fatal = new ApiError(0, `第 ${seq + 1} 片录音在这台电脑上找不到了`);
          break;
        }
        let failure: ApiError | undefined;
        try {
          failure = await sendChunk(this.meetingId, seq, body, this.abort.signal, PUT_TIMEOUT_MS);
        } catch {
          break; // 只有 dispose 时会走到这里
        }
        if (!failure) {
          this.uploaded++;
          this.trouble = null;
          attempt = 0;
          this.onUploaded(seq);
          this.notify();
          continue;
        }
        const trouble = transient(failure);
        if (!trouble) {
          this.fatal = failure;
          break;
        }
        this.trouble = trouble;
        this.notify();
        await this.sleep(waitBefore(attempt++));
      }
    } finally {
      this.running = false;
      this.notify();
    }
  }

  private sleep(ms: number) {
    return new Promise<void>((resolve) => {
      const done = () => {
        clearTimeout(timer);
        this.wake = null;
        resolve();
      };
      const timer = setTimeout(done, ms);
      this.wake = done;
    });
  }
}

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

/**
 * 告诉服务器录音传完了，开始处理。断网、服务器出错时一直重试，onTrouble 报告当前的问题。
 * 上一次其实已经成功、只是没收到回复的话，再提交会得到 409，这时看一眼会议：已经不在上传状态就算成功。
 */
export async function complete(meetingId: string, chunks: number, marks: number[], onTrouble: (t: Trouble | null) => void): Promise<Meeting> {
  const path = { params: { path: { id: meetingId } } };
  for (let attempt = 0; ; attempt++) {
    try {
      const meeting = await unwrap(api.POST("/api/meetings/{id}/complete", { ...path, body: { chunks, marks } }));
      onTrouble(null);
      return meeting;
    } catch (e) {
      if (!(e instanceof ApiError)) throw e;
      if (e.status === 409) {
        const meeting = await unwrap(api.GET("/api/meetings/{id}", path)).catch(() => null);
        if (meeting && meeting.status !== "uploading") {
          onTrouble(null);
          return meeting;
        }
        throw e;
      }
      const trouble = transient(e);
      if (!trouble) throw e;
      onTrouble(trouble);
      await sleep(waitBefore(attempt));
    }
  }
}

/** 开始录音以后网页很快就关掉了，什么也没存下来。 */
export const NOTHING_SAVED = "这段录音什么也没存下来（开始录音后网页很快就关掉了），只能放弃。";

/**
 * 接着上传一段之前没传完的录音（录音的网页关掉了、浏览器崩溃了），传完交给服务器处理，然后删掉本机的缓存。
 * 先问服务器已经收到了哪些，传过的不再传。调用方要先拿到这段录音的锁（withLock），免得两个标签页同时传。
 */
export async function resumeUpload(rec: LocalRecording, onProgress: (uploaded: number, total: number, trouble: Trouble | null) => void): Promise<Meeting> {
  const { received } = await unwrap(api.GET("/api/meetings/{id}/chunks", { params: { path: { id: rec.meetingId } } }));
  const have = new Set(received);
  let prefix = 0;
  while (have.has(prefix)) prefix++;
  // 本机存下的片数一般不会比服务器上的少；万一少了（比如本机存储中途出了问题），以服务器为准
  const total = Math.max(rec.chunks, prefix);
  if (total === 0) throw new ApiError(0, NOTHING_SAVED);

  const uploader: Uploader = new Uploader(
    rec.meetingId,
    (seq) => getChunk(rec.meetingId, seq, rec.mimeType),
    () => onProgress(uploader.uploaded, total, uploader.trouble),
  );
  uploader.uploaded = Math.min(prefix, total);
  uploader.add(total);
  try {
    await uploader.drained();
  } finally {
    uploader.dispose();
  }
  // 录音中途关掉的：最后一片之后点的标记，对应的那段录音没存下来，不要了
  const marks = rec.finished ? rec.marks : rec.marks.filter((t) => t <= (total * SLICE_MS) / 1000);
  const meeting = await complete(rec.meetingId, total, marks, (t) => onProgress(total, total, t));
  await deleteRecording(rec.meetingId);
  return meeting;
}

// ---- 哪个标签页在管哪段录音 ----
//
// 正在录、或者正在补传的标签页占着这段录音的锁（Web Locks）。别的标签页看到锁被占着，就不把它当成
// 没人管的录音。查锁时也看得到自己占着的锁，所以自己占着的另外记一份，不算「别人在管」。

export const lockName = (meetingId: string) => `zsmeet-recording:${meetingId}`;
const ownLocks = new Set<string>();

/** 这段录音有没有被别的标签页占着（正在录，或者正在补传）。 */
export async function lockedElsewhere(meetingId: string): Promise<boolean> {
  if (!navigator.locks) return false;
  const name = lockName(meetingId);
  if (ownLocks.has(name)) return false;
  const { held = [] } = await navigator.locks.query();
  return held.some((l) => l.name === name);
}

/** 占着这段录音的锁，直到调用返回的函数。录音时用。 */
export function holdLock(meetingId: string): () => void {
  const name = lockName(meetingId);
  ownLocks.add(name);
  let released = false;
  let release = () => {};
  if (navigator.locks) {
    // 锁是异步拿到的：拿到之前就已经放手的话，拿到后立刻还回去
    void navigator.locks.request(name, () => (released ? Promise.resolve() : new Promise<void>((resolve) => (release = resolve))));
  }
  return () => {
    released = true;
    ownLocks.delete(name);
    release();
  };
}

/**
 * 拿到一段录音的锁，在 work 做完之前一直占着。别的标签页已经占着时返回 null，不等。
 * 浏览器不支持锁时直接做。
 */
export async function withLock<T>(meetingId: string, work: () => Promise<T>): Promise<T | null> {
  if (!navigator.locks) return work();
  const name = lockName(meetingId);
  let ran = false;
  const result = await navigator.locks.request(name, { ifAvailable: true }, async (lock) => {
    if (!lock) return null;
    ran = true;
    ownLocks.add(name);
    try {
      return await work();
    } finally {
      ownLocks.delete(name);
    }
  });
  return ran ? (result as T) : null;
}

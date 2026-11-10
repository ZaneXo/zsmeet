import { useSyncExternalStore } from "react";
import { api, ApiError, errorMessage, queryClient, unwrap } from "../../api/client";
import * as store from "./store";
import { SLICE_MS } from "./store";
import type { LocalRecording } from "./store";
import { complete, holdLock, lockedElsewhere, Uploader } from "./upload";
import type { Trouble } from "./upload";

// 网页录音。整个网页只有一个录音（session），不跟着页面走：录音时切到会议列表、看别的会议，录音照常进行，
// 导航栏上会一直显示正在录。
//
// 声音的走向：麦克风（和共享的标签页）→ 混成一路单声道 → MediaRecorder 每 10 秒切一片 →
// 先存进本机（store.ts），再按顺序传到服务器（upload.ts）。结束时等全部传完，告诉服务器开始处理。

export { SLICE_MS };
/** 最长录多久。服务器最多处理 6 小时，留两分钟余量（页面在后台时计时器可能晚一会儿才触发） */
export const MAX_SECONDS = 6 * 3600 - 120;
/** 一场最多标记多少个重点，和服务器一致 */
const MAX_MARKS = 500;
/** 音量（均方根）低于这个就算没声音 */
const QUIET = 0.001;
/** 开始录音后多久先切出第一片。第一片里有文件头，越早存下来，网页刚开始录就关掉时丢得越少 */
const FIRST_SLICE_MS = 1000;

export type Source = "mic" | "mic+tab";

/**
 * idle：还没开始（在设置页试音）；starting：正在新建会议、选标签页；recording / paused：录音中；
 * finishing：已经停止录音，在等剩下的分片传完；done：已经交给服务器处理；failed：录音传不上去了。
 */
export type Phase = "idle" | "starting" | "recording" | "paused" | "finishing" | "done" | "failed";

export type Snapshot = Readonly<{
  phase: Phase;
  /** 开始录音失败、录音没能交给服务器的原因 */
  error: string;
  /** 录音自己停下来的原因（浏览器的录音出错、录满了时长）。用户点「结束录音」时为空 */
  stopReason: string;
  meetingId: string;
  title: string;
  source: Source;
  micLabel: string;
  /** 麦克风还连着。拔掉了、被系统收回权限时变成 false */
  micLive: boolean;
  /** 标签页的声音还在共享。在浏览器的提示条上点了「停止共享」时变成 false */
  tabLive: boolean;
  /** 一共切出了多少片 */
  chunks: number;
  /** 传上去了多少片 */
  uploaded: number;
  /** 暂时传不上去的原因，恢复后自动清掉 */
  trouble: Trouble | null;
  /** 录音过程中出现、以后再也传不上去的原因（比如会议在别处被结束了）。录音还在继续，结束后可以下载 */
  uploadError: string;
  /** 分片存得进这台电脑。存不进时没传上去的部分只在这个网页里，网页一关就没了 */
  stored: boolean;
  /** 录音没能交给服务器，而且有一部分只在这个网页里：下载之前不能关网页、不能直接开始下一场 */
  needsDownload: boolean;
  marks: readonly number[];
  /** 已经多少秒没声音了 */
  quietFor: number;
  /** 结束时一共录了多少秒 */
  seconds: number;
}>;

const IDLE: Snapshot = {
  phase: "idle",
  error: "",
  stopReason: "",
  meetingId: "",
  title: "",
  source: "mic",
  micLabel: "",
  micLive: false,
  tabLive: false,
  chunks: 0,
  uploaded: 0,
  trouble: null,
  uploadError: "",
  stored: true,
  needsDownload: false,
  marks: [],
  quietFor: 0,
  seconds: 0,
};

export type StartOptions = {
  userId: string;
  title: string;
  template: string;
  hotwords: string;
  source: Source;
};

/** 这个浏览器能不能录音；不能的话说明原因。 */
export function unsupportedReason(): string {
  if (!window.isSecureContext) {
    return "浏览器只允许 HTTPS 网页使用麦克风，现在这个地址不是 HTTPS。请管理员给 zsmeet 配上 HTTPS（在服务器本机上用 localhost 打开也可以）。";
  }
  if (!navigator.mediaDevices?.getUserMedia || typeof MediaRecorder === "undefined" || typeof AudioContext === "undefined") {
    return "这个浏览器不支持网页录音，请换用最新版的 Chrome、Edge、Safari 或 Firefox。";
  }
  return "";
}

/** 能不能录标签页的声音：只有电脑上的 Chrome 和 Edge 可以。 */
export function canRecordTab(): boolean {
  return (
    typeof navigator.mediaDevices?.getDisplayMedia === "function" &&
    /Chrome\/|Edg\//.test(navigator.userAgent) &&
    !/Mobile|Android/.test(navigator.userAgent)
  );
}

/** 电脑上的麦克风。要先允许使用麦克风，才拿得到名字。 */
export async function microphones(): Promise<MediaDeviceInfo[]> {
  const all = await navigator.mediaDevices.enumerateDevices();
  // Windows 上会多一个「通讯」设备，和默认设备重复
  return all.filter((d) => d.kind === "audioinput" && d.deviceId !== "communications");
}

function micConstraints(deviceId: string): MediaTrackConstraints {
  return {
    deviceId: deviceId ? { exact: deviceId } : undefined,
    echoCancellation: true,
    noiseSuppression: true,
    autoGainControl: true,
  };
}

/** 让用户选一个标签页并共享它的声音。必须在点击按钮的处理函数里调用。 */
async function pickTab(): Promise<MediaStream> {
  let stream: MediaStream;
  try {
    stream = await navigator.mediaDevices.getDisplayMedia({
      // 选择窗口默认停在「标签页」，并默认打开「共享标签页音频」
      video: { displaySurface: "browser" },
      audio: { echoCancellation: false, noiseSuppression: false, autoGainControl: false },
      preferCurrentTab: false,
      selfBrowserSurface: "exclude",
      systemAudio: "include",
      surfaceSwitching: "include",
    } as DisplayMediaStreamOptions);
  } catch (e) {
    if (e instanceof DOMException && e.name === "NotAllowedError") throw new Error("没有选择要录的标签页。");
    throw e;
  }
  if (stream.getAudioTracks().length === 0) {
    stream.getTracks().forEach((t) => t.stop());
    throw new Error("没有共享到声音：在弹出的窗口里选「标签页」，选中开会的那个标签页，并打开「同时共享标签页音频」，再试一次。");
  }
  return stream;
}

/** 浏览器能录的格式里挑一个。服务器用 ffmpeg 解码，这几种都认识。 */
function pickMime(): string {
  for (const type of ["audio/webm;codecs=opus", "audio/ogg;codecs=opus", "audio/mp4;codecs=mp4a.40.2", "audio/mp4"]) {
    if (MediaRecorder.isTypeSupported(type)) return type;
  }
  return "";
}

/** 下载录音时的扩展名。 */
function extension(mimeType: string): string {
  if (mimeType.includes("ogg")) return "ogg";
  if (mimeType.includes("mp4")) return "m4a";
  return "webm";
}

function warnBeforeLeaving(e: BeforeUnloadEvent) {
  e.preventDefault();
  // 老一些的浏览器要设置 returnValue 才会弹出确认
  e.returnValue = "";
}

type Input = { stream: MediaStream; node: MediaStreamAudioSourceNode; analyser: AnalyserNode };

/** 分析音量用的采样数。最多能取到最近 32768 个采样（48kHz 下约 0.7 秒），判断「有没有声音」用全部，音量条只用最后一小段 */
const WINDOW = 32768;
const METER = 2048;

class RecordingSession {
  private snap: Snapshot = IDLE;
  private readonly listeners = new Set<() => void>();

  private ctx: AudioContext | null = null;
  private mixer: MediaStreamAudioDestinationNode | null = null;
  private mic: Input | null = null;
  private tab: Input | null = null;
  private recorder: MediaRecorder | null = null;
  private previewSeq = 0;
  private readonly samples = new Float32Array(WINDOW);

  /** 之前几段（暂停以前）一共录了多少秒 */
  private before = 0;
  /** 这一段从什么时候开始录（performance.now()），暂停时为 null */
  private since: number | null = null;
  /** 从什么时候开始没声音的（performance.now()），有声音时为 null */
  private quietSince: number | null = null;

  private rec: LocalRecording | null = null;
  /**
   * 分片在内存里的副本。一片要等「已经传上去」和「已经存进本机」两件事都成了才从内存里删：
   * 只做成一件就删的话，另一件失败时这一片就哪儿都没有了。
   */
  private readonly memory = new Map<number, Blob>();
  private readonly persisted = new Set<number>();
  /** 写本机存储的操作排成一队，按顺序写 */
  private writes: Promise<unknown> = Promise.resolve();
  private uploader: Uploader | null = null;
  private ticker = 0;
  private firstSlice = 0;
  private wakeLock: WakeLockSentinel | null = null;
  private releaseLock: (() => void) | null = null;
  /** 标记有改动、还没存到服务器 */
  private marksDirty = false;
  private syncingMarks = false;

  constructor() {
    document.addEventListener("visibilitychange", () => {
      // 页面切到后台时浏览器会自动收回「保持屏幕常亮」，回来时重新要
      if (document.visibilityState === "visible" && this.recording) void this.keepAwake();
    });
  }

  subscribe = (listener: () => void) => {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  };

  getSnapshot = () => this.snap;

  private set(patch: Partial<Snapshot>) {
    this.snap = { ...this.snap, ...patch };
    for (const listener of this.listeners) listener();
  }

  /** 正在录（含暂停）。 */
  get recording(): boolean {
    return this.snap.phase === "recording" || this.snap.phase === "paused";
  }

  /** 这一场录音还没了结：在录，或者在等剩下的分片传完。这时关掉页面会丢东西。 */
  get busy(): boolean {
    return this.snap.phase === "starting" || this.recording || this.snap.phase === "finishing";
  }

  /** 录了多少秒（不算暂停的时间）。 */
  elapsed(): number {
    return this.before + (this.since === null ? 0 : (performance.now() - this.since) / 1000);
  }

  /** 现在的音量，0 到 1。 */
  level(which: "mic" | "tab"): number {
    const analyser = (which === "mic" ? this.mic : this.tab)?.analyser;
    if (!analyser) return 0;
    analyser.getFloatTimeDomainData(this.samples);
    const rms = rootMeanSquare(this.samples, WINDOW - METER);
    // -60 dB 到 0 dB 对应 0 到 1
    return Math.max(0, Math.min(1, 1 + (20 * Math.log10(rms || 1e-9)) / 60));
  }

  // ---- 试音 ----

  /** 打开（或换成）指定的麦克风，用来试音。开始录音时直接用它。 */
  async preview(deviceId: string): Promise<void> {
    if (this.busy) return;
    const seq = ++this.previewSeq;
    const stream = await navigator.mediaDevices.getUserMedia({ audio: micConstraints(deviceId) });
    if (seq !== this.previewSeq || this.busy) {
      // 等权限的时候又换了一个麦克风，或者已经离开了录音页
      stream.getTracks().forEach((t) => t.stop());
      return;
    }
    this.attachMic(stream);
  }

  /** 离开录音页时关掉试音用的麦克风（录音中不关）。 */
  stopPreview() {
    if (this.busy) return;
    this.previewSeq++;
    this.releaseMedia();
  }

  private context(): AudioContext {
    if (!this.ctx || this.ctx.state === "closed") {
      this.ctx = new AudioContext();
      this.mixer = this.ctx.createMediaStreamDestination();
      // 混成单声道：识别只用单声道，文件也小一半
      this.mixer.channelCount = 1;
    }
    const ctx = this.ctx;
    if (ctx.state === "suspended") {
      // 用户还没在页面上点过东西时浏览器不让声音处理跑起来，点一下就好
      void ctx.resume().catch(() => {});
      document.addEventListener("pointerdown", () => void ctx.resume().catch(() => {}), { once: true });
    }
    return ctx;
  }

  private connect(stream: MediaStream): Input {
    const ctx = this.context();
    const node = ctx.createMediaStreamSource(stream);
    const analyser = ctx.createAnalyser();
    analyser.fftSize = WINDOW;
    node.connect(analyser);
    node.connect(this.mixer!);
    return { stream, node, analyser };
  }

  private attachMic(stream: MediaStream) {
    this.detach(this.mic);
    this.mic = this.connect(stream);
    const track = stream.getAudioTracks()[0];
    track?.addEventListener("ended", () => {
      if (this.mic?.stream === stream) this.set({ micLive: false });
    });
    this.set({ micLabel: track?.label ?? "", micLive: true });
  }

  private attachTab(stream: MediaStream) {
    this.detach(this.tab);
    this.tab = this.connect(new MediaStream(stream.getAudioTracks()));
    this.tab.stream = stream; // 记着整个共享，停的时候连画面一起停
    const ended = () => {
      if (this.tab?.stream === stream) this.set({ tabLive: false });
    };
    // 在浏览器的提示条上点「停止共享」时，声音和画面都会结束
    for (const track of stream.getTracks()) track.addEventListener("ended", ended);
    this.set({ tabLive: true });
  }

  private detach(input: Input | null) {
    if (!input) return;
    input.node.disconnect();
    input.stream.getTracks().forEach((t) => t.stop());
  }

  /** 关掉麦克风、标签页共享和声音处理。 */
  private releaseMedia() {
    this.detach(this.mic);
    this.detach(this.tab);
    this.mic = this.tab = null;
    void this.ctx?.close().catch(() => {});
    this.ctx = this.mixer = null;
    this.set({ micLive: false, tabLive: false });
  }

  // ---- 录音 ----

  /** 开始录音。必须在点击按钮的处理函数里调用（选标签页需要）。 */
  async start(opts: StartOptions): Promise<void> {
    if (this.busy || this.snap.needsDownload) return;
    this.set({ ...IDLE, phase: "starting", source: opts.source, micLabel: this.snap.micLabel, micLive: this.snap.micLive });
    try {
      if (!this.mic) throw new Error("还没有打开麦克风：请允许这个网页使用麦克风。");
      void this.context().resume();
      if (opts.source === "mic+tab") this.attachTab(await pickTab());

      const mimeType = pickMime();
      const recorder = new MediaRecorder(this.mixer!.stream, { mimeType: mimeType || undefined, audioBitsPerSecond: 32_000 });
      // 录音器能建出来再新建会议，免得在服务器上留下一场什么也没有的会议
      const meeting = await unwrap(
        api.POST("/api/meetings", { body: { title: opts.title, source: "record", template: opts.template, hotwords: opts.hotwords } }),
      );

      this.rec = {
        meetingId: meeting.id,
        userId: opts.userId,
        title: meeting.title,
        startedAt: Date.now(),
        mimeType: recorder.mimeType || mimeType || "audio/webm",
        chunks: 0,
        seconds: 0,
        marks: [],
        finished: false,
      };
      const stored = await store.saveRecording(this.rec).then(
        () => true,
        () => false,
      );
      this.memory.clear();
      this.persisted.clear();
      this.writes = Promise.resolve();
      this.marksDirty = false;
      const uploader: Uploader = new Uploader(
        meeting.id,
        (seq) => this.readChunk(seq),
        () => this.set({ uploaded: uploader.uploaded, trouble: uploader.trouble, uploadError: uploader.fatal?.message ?? "" }),
        (seq) => {
          this.evict(seq);
          if (this.marksDirty) void this.syncMarks();
        },
      );
      this.uploader = uploader;

      this.recorder = recorder;
      recorder.addEventListener("dataavailable", (e) => this.onData(e.data));
      recorder.addEventListener("error", () => {
        this.set({ stopReason: "浏览器的录音出错了，录音已经自动停止。停止前录下的部分会照常处理。" });
        void this.stop();
      });
      recorder.start(SLICE_MS);
      this.firstSlice = window.setTimeout(() => {
        if (recorder.state === "recording") recorder.requestData();
      }, FIRST_SLICE_MS);
      this.before = 0;
      this.since = performance.now();
      this.quietSince = null;

      this.releaseLock = holdLock(meeting.id);
      void this.keepAwake();
      window.addEventListener("beforeunload", warnBeforeLeaving);
      this.ticker = window.setInterval(this.tick, 1000);
      this.set({ phase: "recording", meetingId: meeting.id, title: meeting.title, stored });
      void queryClient.invalidateQueries({ queryKey: ["meetings"] });
    } catch (e) {
      this.detach(this.tab);
      this.tab = null;
      this.recorder = null;
      this.set({ phase: "idle", tabLive: false, error: errorMessage(e) });
    }
  }

  private onData(data: Blob) {
    const rec = this.rec;
    if (!rec || data.size === 0) return;
    const seq = rec.chunks;
    const next = { ...rec, chunks: seq + 1, seconds: Math.round(this.elapsed()) };
    this.rec = next;
    this.memory.set(seq, data);
    if (this.snap.stored) {
      // 一旦有一片没存进去，后面的也不再存：本机存着的永远是从第 0 片开始连续的一段
      this.writes = this.writes
        .then(async () => {
          if (!this.snap.stored) return;
          await store.saveChunk(next, seq, data);
          this.persisted.add(seq);
          this.evict(seq);
        })
        .catch(() => this.set({ stored: false }));
    }
    this.uploader?.add(seq + 1);
    this.set({ chunks: seq + 1 });
  }

  /** 一片已经传上去、也已经存进本机时，从内存里删掉。 */
  private evict(seq: number) {
    if (this.persisted.has(seq) && seq < (this.uploader?.uploaded ?? 0)) this.memory.delete(seq);
  }

  private readChunk(seq: number): Promise<Blob | undefined> {
    const inMemory = this.memory.get(seq);
    if (inMemory) return Promise.resolve(inMemory);
    const rec = this.rec;
    return rec ? store.getChunk(rec.meetingId, seq, rec.mimeType) : Promise.resolve(undefined);
  }

  /** 把录音信息（时长、标记）写进本机。 */
  private persist(): Promise<unknown> {
    const rec = this.rec;
    if (rec && this.snap.stored) {
      const snapshot = { ...rec, seconds: Math.round(this.elapsed()) };
      // 执行时再看一眼：前面排着的某一片没存进去的话，这里也不能再写，免得记录上的片数比真正存下的多
      this.writes = this.writes.then(() => (this.snap.stored ? store.saveRecording(snapshot) : undefined)).catch(() => this.set({ stored: false }));
    }
    return this.writes;
  }

  /**
   * 把标记存到服务器。这样录音的电脑万一找不回来，在会议页面上用已经收到的部分生成纪要时，标记也还在。
   * 失败了不要紧：下一片传上去以后再试，结束录音时也会一起提交。
   */
  private async syncMarks() {
    const rec = this.rec;
    if (!rec || this.syncingMarks || !this.marksDirty) return;
    this.syncingMarks = true;
    this.marksDirty = false;
    try {
      await unwrap(api.PUT("/api/meetings/{id}/marks", { params: { path: { id: rec.meetingId } }, body: { marks: [...rec.marks] } }));
    } catch (e) {
      // 409：会议已经在别处结束了，以后也存不进去，不再试
      if (!(e instanceof ApiError && e.status === 409)) this.marksDirty = true;
    } finally {
      this.syncingMarks = false;
    }
  }

  pause() {
    if (this.snap.phase !== "recording" || !this.recorder) return;
    this.recorder.pause();
    this.before = this.elapsed();
    this.since = null;
    this.set({ phase: "paused" });
    void this.persist();
  }

  resume() {
    if (this.snap.phase !== "paused" || !this.recorder) return;
    this.recorder.resume();
    this.since = performance.now();
    this.quietSince = null;
    this.set({ phase: "recording", quietFor: 0 });
  }

  /** 标记重点：记下现在是第几秒。连着点的只算一次。 */
  mark() {
    if (!this.recording || !this.rec) return;
    const at = Math.round(this.elapsed() * 10) / 10;
    const marks = this.snap.marks;
    const last = marks.at(-1);
    if (marks.length >= MAX_MARKS || (last !== undefined && at - last < 1)) return;
    const next = [...marks, at];
    this.rec = { ...this.rec, marks: next };
    this.set({ marks: next });
    void this.persist();
    this.marksDirty = true;
    void this.syncMarks();
  }

  /** 换一个麦克风，或者麦克风断开以后重新连上。录音不停。 */
  async switchMic(deviceId: string): Promise<void> {
    const stream = await navigator.mediaDevices.getUserMedia({ audio: micConstraints(deviceId) });
    if (!this.recording) {
      stream.getTracks().forEach((t) => t.stop());
      return;
    }
    this.attachMic(stream);
  }

  /** 标签页停止共享以后重新选一个。必须在点击按钮的处理函数里调用。 */
  async reshareTab(): Promise<void> {
    const stream = await pickTab();
    if (!this.recording) {
      stream.getTracks().forEach((t) => t.stop());
      return;
    }
    this.attachTab(stream);
  }

  /** 结束录音：等最后一片存好、全部传完，再交给服务器处理。 */
  async stop(): Promise<void> {
    const recorder = this.recorder;
    if (!this.recording || !recorder || !this.rec) return;
    this.before = this.elapsed();
    this.since = null;
    this.set({ phase: "finishing", seconds: Math.round(this.before) });
    window.clearInterval(this.ticker);
    window.clearTimeout(this.firstSlice);
    // 最后一片的 dataavailable 在 stop 事件之前触发。录音器出错时浏览器已经自己停了，只等事件；
    // 万一一直等不到，5 秒后也往下走
    const stopped = new Promise<void>((resolve) => {
      recorder.addEventListener("stop", () => resolve(), { once: true });
      setTimeout(resolve, 5000);
    });
    if (recorder.state !== "inactive") recorder.stop();
    await stopped;
    this.recorder = null;
    this.releaseMedia();
    void this.wakeLock?.release().catch(() => {});
    this.wakeLock = null;

    this.rec = { ...this.rec, finished: true };
    await this.persist();
    const rec = this.rec;
    try {
      if (rec.chunks === 0) {
        await store.deleteRecording(rec.meetingId).catch(() => {});
        throw new ApiError(0, "什么也没有录下来。");
      }
      await this.uploader!.drained();
      const meeting = await complete(rec.meetingId, rec.chunks, [...rec.marks], (trouble) => this.set({ trouble }));
      await store.deleteRecording(rec.meetingId).catch(() => {});
      this.memory.clear();
      this.finish(false);
      this.set({ phase: "done", meetingId: meeting.id, trouble: null });
      void queryClient.invalidateQueries({ queryKey: ["meetings"] });
    } catch (e) {
      // 录音还在本机或者内存里，可以下载下来。存进本机的那部分以后还能在提示条里接着传；
      // 只在内存里的那部分必须现在下载，所以下载之前继续拦着关网页
      const onlyHere = !this.snap.stored && rec.chunks > 0;
      this.finish(onlyHere);
      this.set({ phase: "failed", error: errorMessage(e), trouble: null, needsDownload: onlyHere });
    }
  }

  /** 录音了结（成功或失败）以后放开锁。keepGuard：录音只在这个网页里，还要继续拦着关网页。 */
  private finish(keepGuard: boolean) {
    this.uploader?.dispose();
    this.uploader = null;
    this.releaseLock?.();
    this.releaseLock = null;
    if (!keepGuard) window.removeEventListener("beforeunload", warnBeforeLeaving);
    void queryClient.invalidateQueries({ queryKey: ["local-recordings"] });
  }

  /**
   * 录完（或失败）以后回到设置页，准备录下一场。失败时存在本机的录音留着，提示条里可以接着传或者下载。
   * 录音只在这个网页里、还没下载时不做，返回 false；force 表示用户确认了不要它。
   */
  reset(force = false): boolean {
    if (this.busy) return false;
    if (this.snap.needsDownload && !force) return false;
    window.removeEventListener("beforeunload", warnBeforeLeaving);
    this.rec = null;
    this.memory.clear();
    this.persisted.clear();
    this.set({ ...IDLE });
    void queryClient.invalidateQueries({ queryKey: ["local-recordings"] });
    return true;
  }

  /** 把这一场录音下载到电脑上（录音传不上去时用）。返回缺了几片（本机和内存里都找不到的）。 */
  async download(): Promise<number> {
    const rec = this.rec;
    if (!rec) return 0;
    const parts: Blob[] = [];
    let missing = 0;
    for (let seq = 0; seq < rec.chunks; seq++) {
      const part = await this.readChunk(seq).catch(() => undefined);
      if (part) parts.push(part);
      else missing++;
    }
    saveFile(new Blob(parts, { type: rec.mimeType }), rec);
    if (this.snap.needsDownload) {
      window.removeEventListener("beforeunload", warnBeforeLeaving);
      this.set({ needsDownload: false });
    }
    return missing;
  }

  private tick = () => {
    if (this.snap.phase !== "recording") return;
    const now = performance.now();
    const loud = Math.max(this.loudness(this.mic), this.loudness(this.tab)) > QUIET;
    if (loud) this.quietSince = null;
    else this.quietSince ??= now;
    const quietFor = this.quietSince === null ? 0 : Math.floor((now - this.quietSince) / 1000);
    if (quietFor !== this.snap.quietFor) this.set({ quietFor });
    if (this.elapsed() >= MAX_SECONDS) {
      this.set({ stopReason: `已经录了 ${Math.floor(MAX_SECONDS / 3600)} 个小时，达到单场录音的上限，自动结束了。` });
      void this.stop();
    }
  };

  private loudness(input: Input | null): number {
    if (!input) return 0;
    input.analyser.getFloatTimeDomainData(this.samples);
    return rootMeanSquare(this.samples, 0);
  }

  private async keepAwake() {
    if (!navigator.wakeLock || this.wakeLock) return;
    try {
      const lock = await navigator.wakeLock.request("screen");
      lock.addEventListener("release", () => {
        if (this.wakeLock === lock) this.wakeLock = null;
      });
      this.wakeLock = lock;
    } catch {
      // 拿不到也没关系：只是电脑可能会按设置的时间睡眠
    }
  }
}

function rootMeanSquare(samples: Float32Array, from: number): number {
  let sum = 0;
  for (const v of samples.subarray(from)) sum += v * v;
  return Math.sqrt(sum / (samples.length - from));
}

function saveFile(blob: Blob, rec: LocalRecording) {
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = `${rec.title.replace(/[\\/:*?"<>|]/g, "_")}.${extension(rec.mimeType)}`;
  a.click();
  setTimeout(() => URL.revokeObjectURL(url), 60_000);
}

export const session = new RecordingSession();

/** 录音的整体状态。整个网页共用一个录音，切到别的页面再回来，看到的还是同一个。 */
export function useRecording(): Snapshot {
  return useSyncExternalStore(session.subscribe, session.getSnapshot);
}

/** 时:分:秒，录音的计时用。 */
export function hms(sec: number): string {
  const s = Math.floor(sec);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${pad(Math.floor(s / 3600))}:${pad(Math.floor((s % 3600) / 60))}:${pad(s % 60)}`;
}

/** 下载一段存在这台电脑上的录音。 */
export async function downloadLocal(rec: LocalRecording): Promise<void> {
  saveFile(await store.assemble(rec), rec);
}

/**
 * 这台电脑上还没传完、也没有标签页在管的录音（录音的网页关掉了、浏览器崩溃了，或者这个网页里录完没能交上去）。
 * 只列当前登录的人自己录的。
 */
export async function abandonedRecordings(userId: string): Promise<LocalRecording[]> {
  const all = await store.listRecordings().catch(() => [] as LocalRecording[]);
  const own = session.getSnapshot();
  const out: LocalRecording[] = [];
  for (const rec of all) {
    if (rec.userId !== userId) continue;
    // 这个网页正在录、或者正在传最后几片的那一场
    if (rec.meetingId === own.meetingId && session.busy) continue;
    if (await lockedElsewhere(rec.meetingId)) continue;
    out.push(rec);
  }
  return out.sort((a, b) => b.startedAt - a.startedAt);
}

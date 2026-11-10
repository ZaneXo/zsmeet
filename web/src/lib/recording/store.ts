// 录音在这台电脑上的缓存（浏览器的 IndexedDB）。
//
// 录音每 10 秒切一片，每一片先存在这里再上传。断网、服务器暂时连不上时分片留在这里，恢复后补传；
// 页面关掉、浏览器崩溃以后，下次打开还能把没传完的接着传上去。传完、交给服务器处理以后删掉。

/** 一段存在这台电脑上的录音。分片另外存，按序号取。 */
export type LocalRecording = {
  meetingId: string;
  /** 谁录的。换了人登录时不给下一个人看 */
  userId: string;
  title: string;
  /** 开始录音的时间（毫秒时间戳） */
  startedAt: number;
  mimeType: string;
  /** 存下了多少片：第 0 到 chunks-1 片都在这台电脑上 */
  chunks: number;
  /** 录了多长（秒，不算暂停的时间） */
  seconds: number;
  /** 标记的重点：录音开始后的秒数 */
  marks: number[];
  /** 录音正常结束了（最后一片也存下了）。为 false 说明页面是在录音中途关掉或者崩溃的 */
  finished: boolean;
};

/** 每片的长度。浏览器崩溃、电脑断电时最多丢最后这么长 */
export const SLICE_MS = 10_000;

const DB_NAME = "zsmeet";
const RECORDINGS = "recordings";
const CHUNKS = "chunks";

let opening: Promise<IDBDatabase> | null = null;

function open(): Promise<IDBDatabase> {
  opening ??= new Promise<IDBDatabase>((resolve, reject) => {
    const req = indexedDB.open(DB_NAME, 1);
    req.onupgradeneeded = () => {
      const db = req.result;
      db.createObjectStore(RECORDINGS, { keyPath: "meetingId" });
      // 键是 [会议 ID, 序号]，同一场录音的分片挨在一起，可以整段删
      db.createObjectStore(CHUNKS);
    };
    req.onsuccess = () => {
      const db = req.result;
      // 别的标签页升级数据库时让出来，下次用时重新打开
      db.onversionchange = () => {
        db.close();
        opening = null;
      };
      resolve(db);
    };
    req.onerror = () => reject(req.error ?? new Error("打不开浏览器的本地存储"));
    req.onblocked = () => reject(new Error("浏览器的本地存储被别的标签页占着"));
  });
  // 打开失败的话下次再试，不把失败缓存下来
  opening.catch(() => {
    opening = null;
  });
  return opening;
}

/**
 * 在一个事务里做一件事，等事务真正写完（durability: strict：写进磁盘）再返回。
 * 录音的分片宁可慢一点，也要保证浏览器崩溃前写下的不丢。
 */
async function run<T>(stores: string[], mode: IDBTransactionMode, fn: (tx: IDBTransaction) => IDBRequest<T> | void): Promise<T> {
  const db = await open();
  return new Promise<T>((resolve, reject) => {
    const tx = db.transaction(stores, mode, { durability: "strict" });
    const req = fn(tx);
    tx.oncomplete = () => resolve(req ? req.result : (undefined as T));
    tx.onerror = () => reject(tx.error ?? new Error("浏览器的本地存储出错了"));
    tx.onabort = () => reject(tx.error ?? new Error("浏览器的本地存储写入被中止了（可能是空间不够）"));
  });
}

const range = (meetingId: string) => IDBKeyRange.bound([meetingId, 0], [meetingId, Infinity]);

export function saveRecording(rec: LocalRecording): Promise<void> {
  return run([RECORDINGS], "readwrite", (tx) => void tx.objectStore(RECORDINGS).put(rec));
}

export function listRecordings(): Promise<LocalRecording[]> {
  return run([RECORDINGS], "readonly", (tx) => tx.objectStore(RECORDINGS).getAll() as IDBRequest<LocalRecording[]>);
}

export async function getRecording(meetingId: string): Promise<LocalRecording | undefined> {
  return run([RECORDINGS], "readonly", (tx) => tx.objectStore(RECORDINGS).get(meetingId) as IDBRequest<LocalRecording | undefined>);
}

/**
 * 存一片，同时更新录音信息（片数、时长、标记），两件事在一个事务里：
 * 记录上的片数永远不会多于真正存下的片数。
 * 存的是 ArrayBuffer 而不是 Blob：老一些的 Safari 往 IndexedDB 里存 Blob 会出错。
 */
export async function saveChunk(rec: LocalRecording, seq: number, data: Blob): Promise<void> {
  const buffer = await data.arrayBuffer();
  await run([RECORDINGS, CHUNKS], "readwrite", (tx) => {
    tx.objectStore(CHUNKS).put(buffer, [rec.meetingId, seq]);
    tx.objectStore(RECORDINGS).put(rec);
  });
}

export async function getChunk(meetingId: string, seq: number, mimeType: string): Promise<Blob | undefined> {
  const buffer = await run([CHUNKS], "readonly", (tx) => tx.objectStore(CHUNKS).get([meetingId, seq]) as IDBRequest<ArrayBuffer | undefined>);
  return buffer ? new Blob([buffer], { type: mimeType }) : undefined;
}

/** 删掉一段录音和它的全部分片。 */
export function deleteRecording(meetingId: string): Promise<void> {
  return run([RECORDINGS, CHUNKS], "readwrite", (tx) => {
    tx.objectStore(CHUNKS).delete(range(meetingId));
    tx.objectStore(RECORDINGS).delete(meetingId);
  });
}

/** 把一段录音的全部分片拼成一个文件，给「下载到电脑」用。 */
export async function assemble(rec: LocalRecording): Promise<Blob> {
  const parts: Blob[] = [];
  for (let seq = 0; seq < rec.chunks; seq++) {
    const part = await getChunk(rec.meetingId, seq, rec.mimeType);
    if (!part) break;
    parts.push(part);
  }
  return new Blob(parts, { type: rec.mimeType });
}

/** 这个浏览器能不能用本地存储。隐私模式、禁用了网站数据时可能用不了。 */
export async function storageWorks(): Promise<boolean> {
  try {
    await open();
    return true;
  } catch {
    return false;
  }
}

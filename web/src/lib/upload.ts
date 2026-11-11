import { ApiError } from "../api/client";
import { apiCredentials, apiHeaders, apiUrl } from "../platform";
import { noteResponse } from "../platform/offline";

/** 上传文件时每片的大小。后端单片上限是 8 MB。 */
const CHUNK_BYTES = 4 * 1024 * 1024;

/** 后端允许的单场会议上传总大小。 */
export const MAX_UPLOAD_BYTES = 4 * 1024 * 1024 * 1024;

/** 网络错误和服务器 5xx 之后，隔多久再试。试完这几次还不行就放弃 */
const RETRY_WAITS = [1000, 3000, 8000];

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

/**
 * 发一次上传请求。成功返回 undefined，失败返回原因；只有上传被取消（signal）时才抛出。
 * timeoutMs：过了这么久还没传完，就当成网络断了（返回 status 0），由调用方决定要不要重试。
 * 连接悄悄断掉（换 Wi-Fi、路由器掉线）时，浏览器自己可能要等好几分钟才报错。
 */
export async function sendChunk(meetingId: string, seq: number, body: Blob, signal: AbortSignal, timeoutMs?: number): Promise<ApiError | undefined> {
  const limited = timeoutMs && typeof AbortSignal.any === "function" ? AbortSignal.any([signal, AbortSignal.timeout(timeoutMs)]) : signal;
  try {
    const response = await fetch(apiUrl(`/api/meetings/${meetingId}/chunks/${seq}`), {
      method: "PUT",
      headers: { ...apiHeaders(), "Content-Type": "application/octet-stream" },
      body,
      credentials: apiCredentials(),
      signal: limited,
    });
    noteResponse(response);
    if (response.ok) return undefined;
    const problem = (await response.json().catch(() => ({}))) as { detail?: string };
    return new ApiError(response.status, problem.detail ?? `上传失败（${response.status}）`);
  } catch (e) {
    if (signal.aborted) throw e;
    return new ApiError(0, "网络中断，上传没有完成");
  }
}

/**
 * 上传一个分片。同一片重复上传是安全的（后端会用后一次覆盖前一次），所以这两种失败会自动再试：
 *
 * - 网络错误和服务器 5xx：隔一会儿再试，最多三次。
 * - 401：只再试一次。上传途中在另一个标签页改了自己的密码时，有一瞬间这个浏览器带着的还是旧的登录状态
 *   （密码已经换了，新的登录状态还没写回浏览器），正好撞上的那一片会被当成没登录，稍等一下就好了。
 *   真的被退出登录的话，再试一次还是 401，照常报错。
 *
 * 其他 4xx 是请求本身的问题，重试也没用。
 */
async function putChunk(meetingId: string, seq: number, body: Blob, signal: AbortSignal): Promise<void> {
  let attempt = 0;
  let retriedLogin = false;
  for (;;) {
    const failure = await sendChunk(meetingId, seq, body, signal);
    if (!failure) return;

    let wait: number | undefined;
    if (failure.status === 401) {
      if (!retriedLogin) wait = 1000;
      retriedLogin = true;
    } else if (failure.status === 0 || failure.status >= 500) {
      wait = RETRY_WAITS[attempt++];
    }
    if (wait === undefined) throw failure;
    await sleep(wait);
  }
}

/**
 * 把文件切片后按顺序上传，返回一共传了多少片。
 * onProgress 的参数是已经传完的字节数。
 */
export async function uploadFile(
  meetingId: string,
  file: File,
  onProgress: (sentBytes: number) => void,
  signal: AbortSignal,
): Promise<number> {
  const total = Math.max(1, Math.ceil(file.size / CHUNK_BYTES));
  for (let seq = 0; seq < total; seq++) {
    await putChunk(meetingId, seq, file.slice(seq * CHUNK_BYTES, (seq + 1) * CHUNK_BYTES), signal);
    onProgress(Math.min(file.size, (seq + 1) * CHUNK_BYTES));
  }
  return total;
}

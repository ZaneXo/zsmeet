// App（桌面版）登录用的授权页（/app/authorize）：读地址里的参数、拼跳回 App 的地址。
// 流程见 docs/desktop.md：桌面版打开
//   /app/authorize?client=desktop&device=<电脑名>&challenge=<S256(code_verifier)>&state=<state>
// 用户点「允许」后网页拿到一次性的授权码，跳到 zsmeet://auth?code=…&state=…；点「取消」跳到 zsmeet://auth?error=denied&state=…

/** 授权页的地址。 */
export const APP_AUTHORIZE_PATH = "/app/authorize";

/** 只会跳到这一个地址：桌面版注册的 deep link。 */
const APP_CALLBACK = "zsmeet://auth";

/** 认识的 App 和它在页面上的名字。 */
export const APP_NAMES = { desktop: "zsmeet 桌面版" } as const;
export type AppClient = keyof typeof APP_NAMES;

export type AppAuthorizeRequest = { client: AppClient; device: string; challenge: string; state: string };

// 和服务器的检查保持一致（service/apps.go）
const CHALLENGE_RE = /^[A-Za-z0-9_-]{43}$/;
const MAX_DEVICE = 100;
// state 是 App 自己生成、原样带回去的随机串，这里只挡住离谱的长度
const MAX_STATE = 512;
// 控制字符（换行之类）：设备名里不该有
const CONTROL_RE = /[\u0000-\u001f\u007f-\u009f]/;

const RETRY = "请回到 zsmeet 桌面版，重新点「在浏览器里登录」，不要手动改链接。";

/** 读出地址里的参数并检查。不对时返回给人看的说明。 */
export function parseAppAuthorize(params: URLSearchParams): { request: AppAuthorizeRequest } | { error: string } {
  const client = params.get("client") ?? "";
  if (!Object.hasOwn(APP_NAMES, client)) {
    return { error: client ? `这个登录链接来自不认识的 App（${client}），没法继续。${RETRY}` : `这个登录链接不完整（没有说明是哪个 App）。${RETRY}` };
  }
  const device = (params.get("device") ?? "").trim();
  if (!device || [...device].length > MAX_DEVICE || CONTROL_RE.test(device)) {
    return { error: `这个登录链接里的电脑名不对（不能为空，最长 ${MAX_DEVICE} 个字）。${RETRY}` };
  }
  const challenge = params.get("challenge") ?? "";
  if (!CHALLENGE_RE.test(challenge)) {
    return { error: `这个登录链接不完整（校验码不对）。${RETRY}` };
  }
  const state = params.get("state") ?? "";
  if (!state || state.length > MAX_STATE) {
    return { error: `这个登录链接不完整（缺少 state）。${RETRY}` };
  }
  return { request: { client: client as AppClient, device, challenge, state } };
}

/** 跳回 App 的地址：允许时带授权码，取消时带 error=denied。state 原样带回去。 */
export function appCallbackURL(result: { code: string } | { error: "denied" }, state: string): string {
  const first = "code" in result ? `code=${encodeURIComponent(result.code)}` : `error=${encodeURIComponent(result.error)}`;
  return `${APP_CALLBACK}?${first}&state=${encodeURIComponent(state)}`;
}

// 用飞书登录时整页跳到飞书、再跳回首页，授权页的地址就丢了。跳走之前把它记在这个标签页的 sessionStorage 里，
// 登录完回到首页时接着打开授权页。只记 10 分钟（和飞书登录的有效期一样）
const PENDING_KEY = "zsmeet.appAuthorize";
const PENDING_TTL_MS = 10 * 60_000;

type Pending = { search: string; at: number };

/** 记下要回来的授权页（地址里 ? 开始的那一段）。 */
export function rememberPendingAuthorize(search: string, now = Date.now()) {
  try {
    sessionStorage.setItem(PENDING_KEY, JSON.stringify({ search, at: now } satisfies Pending));
  } catch {
    // 存不了（隐私模式之类）就算了：登录完回到首页，再从桌面版点一次就行
  }
}

/** 还没回去的授权页（地址里 ? 开始的那一段），没有或者过期了返回空。过期的顺手删掉。 */
export function pendingAuthorize(now = Date.now()): string | null {
  try {
    const raw = sessionStorage.getItem(PENDING_KEY);
    if (!raw) return null;
    const p = JSON.parse(raw) as Partial<Pending>;
    if (typeof p.search === "string" && typeof p.at === "number" && now - p.at >= 0 && now - p.at < PENDING_TTL_MS) return p.search;
    sessionStorage.removeItem(PENDING_KEY);
  } catch {
    // 读不了就当没有
  }
  return null;
}

/** 已经回到授权页了，不再需要记着。 */
export function clearPendingAuthorize() {
  try {
    sessionStorage.removeItem(PENDING_KEY);
  } catch {
    // 同上
  }
}

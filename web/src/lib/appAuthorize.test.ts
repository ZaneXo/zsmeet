// 用 Node 自带的测试运行：pnpm -C web test
import assert from "node:assert/strict";
import { test } from "node:test";
import { appCallbackURL, clearPendingAuthorize, parseAppAuthorize, pendingAuthorize, rememberPendingAuthorize } from "./appAuthorize.ts";

const challenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM";

function parse(query: Record<string, string>) {
  return parseAppAuthorize(new URLSearchParams(query));
}

test("合格的授权链接", () => {
  const r = parse({ client: "desktop", device: "  李航的 MacBook Pro  ", challenge, state: "abc_-." });
  assert.deepEqual(r, { request: { client: "desktop", device: "李航的 MacBook Pro", challenge, state: "abc_-." } });
});

test("不合格的授权链接都给出说明", () => {
  const good = { client: "desktop", device: "MacBook", challenge, state: "s" };
  for (const bad of [
    { ...good, client: "" },
    { ...good, client: "android" },
    { ...good, client: "toString" },
    { ...good, device: "" },
    { ...good, device: "   " },
    { ...good, device: "电".repeat(101) },
    { ...good, device: "Mac\nBook" },
    { ...good, challenge: "" },
    { ...good, challenge: challenge.slice(1) },
    { ...good, challenge: challenge.slice(1) + "=" },
    { ...good, challenge: challenge.slice(1) + "+" },
    { ...good, state: "" },
    { ...good, state: "x".repeat(513) },
  ]) {
    const r = parse(bad);
    assert.ok("error" in r && r.error.includes("重新点「在浏览器里登录」"), JSON.stringify(bad));
  }
  // 设备名正好 100 个字可以（按字数，不按 UTF-16 的长度）
  assert.ok("request" in parse({ ...good, device: "😀".repeat(100) }));
});

test("跳回 App 的地址只有 zsmeet://auth 一种，参数都编码过", () => {
  assert.equal(appCallbackURL({ code: "abc-_" }, "s t&u=v"), "zsmeet://auth?code=abc-_&state=s%20t%26u%3Dv");
  assert.equal(appCallbackURL({ error: "denied" }, "xyz"), "zsmeet://auth?error=denied&state=xyz");
  const u = new URL(appCallbackURL({ code: "a+b/c" }, "#?"));
  assert.equal(u.protocol, "zsmeet:");
  assert.equal(u.searchParams.get("code"), "a+b/c");
  assert.equal(u.searchParams.get("state"), "#?");
});

test("用飞书登录时记下的授权页，10 分钟内有效", () => {
  const store = new Map<string, string>();
  const g = globalThis as { sessionStorage?: unknown };
  const saved = g.sessionStorage;
  g.sessionStorage = {
    getItem: (k: string) => store.get(k) ?? null,
    setItem: (k: string, v: string) => void store.set(k, v),
    removeItem: (k: string) => void store.delete(k),
  };
  try {
    assert.equal(pendingAuthorize(), null);
    rememberPendingAuthorize("?client=desktop", 1_000);
    assert.equal(pendingAuthorize(1_000 + 9 * 60_000), "?client=desktop");
    clearPendingAuthorize();
    assert.equal(pendingAuthorize(1_000), null);

    rememberPendingAuthorize("?client=desktop", 1_000);
    assert.equal(pendingAuthorize(1_000 + 10 * 60_000), null);
    assert.equal(store.size, 0, "过期的要删掉");

    store.set("zsmeet.appAuthorize", "不是 JSON");
    assert.equal(pendingAuthorize(), null);
  } finally {
    g.sessionStorage = saved;
  }
});

test("没有 sessionStorage 时不出错", () => {
  rememberPendingAuthorize("?x");
  assert.equal(pendingAuthorize(), null);
  clearPendingAuthorize();
});

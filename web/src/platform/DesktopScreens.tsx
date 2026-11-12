import { useMutation } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import type { FormEvent, ReactNode } from "react";
import { errorMessage } from "../api/client";
import { Button, Field, Input, Notice, Spinner } from "../components/ui";
import { AuthLayout } from "../pages/Auth";
import {
  currentIdentity,
  loginCancel,
  loginStart,
  sessionExpired,
  setServer,
  SIGNING_OUT,
  startDesktop,
  switchServer,
  useDesktopState,
  useLoginError,
  useSigningOut,
} from "./desktop";
import type { DesktopState } from "./desktop";
import { useRecorder } from "./recorder";
import { signOutNote } from "./recorderView";

/**
 * 桌面版的入口：还没填服务器地址时填地址，没登录时「在浏览器里登录」，登录了才显示主界面。
 * 主界面按「服务器 + 用户」重新挂载：换了人以后不留上一个人的界面状态。
 */
export function DesktopGate({ children }: { children: ReactNode }) {
  const state = useDesktopState();
  const [bootError, setBootError] = useState("");

  useEffect(() => {
    startDesktop().catch((e: unknown) => setBootError(errorMessage(e)));
  }, []);

  if (bootError) {
    return (
      <div className="mx-auto mt-24 flex max-w-md flex-col gap-4 px-4">
        <Notice tone="bad">zsmeet 没能启动：{bootError}。请退出 App 再打开试试。</Notice>
      </div>
    );
  }
  if (!state) return <Spinner label="正在启动…" />;
  if (!state.server) return <ServerSetup />;
  if (!state.signed_in) return <BrowserLogin state={state} />;
  return <div key={currentIdentity()} className="contents">{children}</div>;
}

/** 第一次打开（或者换服务器以后）：填服务器地址。 */
function ServerSetup() {
  const [url, setUrl] = useState("");
  const connect = useMutation({ mutationFn: () => setServer(url) });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    connect.mutate();
  };

  return (
    <AuthLayout title="连接 zsmeet 服务器" subtitle="填写团队部署的 zsmeet 的地址，也就是在浏览器里打开 zsmeet 用的那个地址。">
      <form onSubmit={submit} className="flex flex-col gap-4">
        <Field label="服务器地址" htmlFor="server-url" hint="不知道地址的话，问一下团队里管理 zsmeet 的同事。">
          <Input
            id="server-url"
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            placeholder="https://meet.example.com"
            autoComplete="url"
            autoCapitalize="off"
            spellCheck={false}
            required
            autoFocus
          />
        </Field>
        {connect.isError && <Notice tone="bad">{errorMessage(connect.error)}</Notice>}
        <Button type="submit" variant="primary" disabled={connect.isPending || !url.trim()}>
          {connect.isPending ? "正在检查…" : "连接"}
        </Button>
      </form>
    </AuthLayout>
  );
}

/**
 * 「在浏览器里登录」：密码、飞书扫码都在浏览器里完成，登录后点「允许」，浏览器跳回 App，App 换出令牌。
 * 等待时可以重新打开浏览器，或者取消。
 */
function BrowserLogin({ state }: { state: DesktopState }) {
  const loginError = useLoginError();
  const start = useMutation({ mutationFn: loginStart });
  const cancel = useMutation({ mutationFn: loginCancel });
  const failure = start.error ?? cancel.error;
  // 还在退出登录、切换服务器（比如退出到一半登录失效了，先到了这里）：这时登录的话会被它退掉，等它做完再登录
  const signingOut = useSigningOut();
  const reason = signingOut ? SIGNING_OUT : undefined;

  if (state.login_pending) {
    return (
      <AuthLayout title="在浏览器里完成登录" subtitle={state.server ?? undefined}>
        <div className="flex flex-col gap-4">
          <p className="text-sm leading-relaxed text-ink-2">
            已经在浏览器里打开了 zsmeet 的登录页。登录以后点「允许」，会自动回到这里。
          </p>
          <Spinner label="正在等浏览器里的登录…" />
          {loginError && <Notice tone="bad">{loginError}</Notice>}
          {failure && <Notice tone="bad">{errorMessage(failure)}</Notice>}
          <div className="flex flex-col gap-2">
            <Button onClick={() => start.mutate()} disabled={start.isPending || signingOut} title={reason}>
              重新打开浏览器
            </Button>
            <Button variant="ghost" onClick={() => cancel.mutate()} disabled={cancel.isPending}>
              取消
            </Button>
          </div>
          <p className="text-xs leading-relaxed text-muted">浏览器没有打开，或者登录页关掉了？点「重新打开浏览器」。</p>
        </div>
      </AuthLayout>
    );
  }

  return (
    <AuthLayout title="登录 zsmeet" subtitle={state.server ?? undefined}>
      <div className="flex flex-col gap-4">
        {state.expired && <Notice tone="warn">登录已经失效（可能在别的地方退出了这台电脑，或者改过密码），请重新登录。</Notice>}
        {loginError && <Notice tone="bad">{loginError}</Notice>}
        {failure && <Notice tone="bad">{errorMessage(failure)}</Notice>}
        <Button variant="primary" onClick={() => start.mutate()} disabled={start.isPending || signingOut} title={reason}>
          在浏览器里登录
        </Button>
        <p className="text-center text-xs leading-relaxed text-muted">
          会在浏览器里打开 zsmeet 的登录页，用密码或者飞书登录，再点「允许」，就会回到这里。
        </p>
        <div className="border-t border-line pt-3 text-center">
          <SwitchServerButton variant="ghost" size="sm" />
        </div>
      </div>
    </AuthLayout>
  );
}

/**
 * 「换一个服务器」。切换服务器会先结束正在录的那一场，没传完的录音要等这个账号再登录这台服务器时才接着传：
 * 正在录音、有录音没传完时先说清楚，确认了再换。登录失效以后录音照常进行，所以登录页上也要问。
 */
export function SwitchServerButton({ variant = "default", size = "md" }: { variant?: "default" | "ghost"; size?: "md" | "sm" }) {
  const { state } = useRecorder();
  const note = signOutNote(state, "切换服务器");
  const [confirming, setConfirming] = useState(false);
  const change = useMutation({ mutationFn: switchServer });
  const failure = change.isError && <Notice tone="bad">{errorMessage(change.error)}</Notice>;
  // 退出登录、切换服务器一次只做一个（见 desktop.ts）：不管是在哪里点的，正在做的时候这里也变灰
  const signingOut = useSigningOut();
  const busy = signingOut || change.isPending;
  const reason = signingOut ? SIGNING_OUT : undefined;

  if (confirming && note) {
    return (
      <div className="flex flex-col gap-2.5 rounded-xl bg-warn-tint/60 px-4 py-3 text-left text-sm">
        <span className="text-ink-2">{note}</span>
        <span className="flex gap-2">
          <Button size="sm" variant="primary" disabled={busy} title={reason} onClick={() => change.mutate()}>
            仍然切换
          </Button>
          <Button size="sm" onClick={() => setConfirming(false)}>
            取消
          </Button>
        </span>
        {failure}
      </div>
    );
  }
  return (
    <>
      <Button variant={variant} size={size} disabled={busy} title={reason} onClick={() => (note ? setConfirming(true) : change.mutate())}>
        换一个服务器
      </Button>
      {failure}
    </>
  );
}

/** 服务器已经不认这次登录了（/api/status 里没有用户）：在 App 这边也退出，DesktopGate 会切到登录页。 */
export function DesktopSessionExpired() {
  useEffect(() => {
    void sessionExpired();
  }, []);
  return <Spinner label="登录已失效，正在退出…" />;
}

/** 服务器还没初始化（比如被重新部署过）：在浏览器里初始化，或者换一个服务器。 */
export function DesktopNotInitialized() {
  return (
    <div className="mx-auto mt-24 flex max-w-md flex-col gap-4 px-4">
      <Notice tone="warn">这台服务器还没完成初始化：请先在浏览器里打开它，创建管理员账号，再回来登录。</Notice>
      <SwitchServerButton />
    </div>
  );
}

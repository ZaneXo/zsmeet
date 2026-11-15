import { useMutation } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import type { FormEvent, ReactNode } from "react";
import { errorMessage } from "../api/client";
import { Button, Field, Input, Notice, Spinner } from "../components/ui";
import { isEn } from "../i18n/lang";
import { LanguageSelect, tr, useLang } from "../i18n/react";
import { AuthLayout } from "../pages/Auth";
import {
  currentIdentity,
  loginCancel,
  loginStart,
  sessionExpired,
  setServer,
  signingOutMessage,
  startDesktop,
  switchServer,
  useDesktopState,
  useLoginError,
  useSigningOut,
} from "./desktop";
import type { DesktopState } from "./desktop";
import { FloatingUpdateNotice } from "./DesktopUpdate";
import { useRecorder } from "./recorder";
import { signOutNote } from "./recorderView";

/**
 * 桌面版的入口：还没填服务器地址时填地址，没登录时「在浏览器里登录」，登录了才显示主界面。
 * 主界面按「服务器 + 用户」重新挂载：换了人以后不留上一个人的界面状态。
 * 有新版本时，登录前在右下角提示，登录以后在页面顶上提示（Shell 里的 UpdateBanner）。
 */
export function DesktopGate({ children }: { children: ReactNode }) {
  useLang();
  const state = useDesktopState();
  const [bootError, setBootError] = useState("");

  useEffect(() => {
    startDesktop().catch((e: unknown) => setBootError(errorMessage(e)));
  }, []);

  if (bootError) {
    return (
      <div className="mx-auto mt-24 flex max-w-md flex-col gap-4 px-4">
        <Notice tone="bad">
          {isEn()
            ? `zsmeet failed to start: ${bootError}. Please quit and reopen the app.`
            : `zsmeet 没能启动：${bootError}。请退出 App 再打开试试。`}
        </Notice>
      </div>
    );
  }
  if (!state) return <Spinner label={tr("正在启动…", "Starting…")} />;
  if (!state.signed_in) {
    return (
      <>
        {state.server ? <BrowserLogin state={state} /> : <ServerSetup />}
        <FloatingUpdateNotice />
      </>
    );
  }
  return <div key={currentIdentity()} className="contents">{children}</div>;
}

/** 第一次打开（或者换服务器以后）：填服务器地址。 */
function ServerSetup() {
  useLang();
  const [url, setUrl] = useState("");
  const connect = useMutation({ mutationFn: () => setServer(url) });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    connect.mutate();
  };

  return (
    <AuthLayout
      title={tr("连接 zsmeet 服务器", "Connect to zsmeet server")}
      subtitle={tr(
        "填写团队部署的 zsmeet 的地址，也就是在浏览器里打开 zsmeet 用的那个地址。",
        "Enter the URL of your team's zsmeet server — the same address you open in your browser.",
      )}
    >
      <form onSubmit={submit} className="flex flex-col gap-4">
        <Field
          label={tr("服务器地址", "Server URL")}
          htmlFor="server-url"
          hint={tr("不知道地址的话，问一下团队里管理 zsmeet 的同事。", "Ask your team's zsmeet admin if you're unsure of the URL.")}
        >
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
          {connect.isPending ? tr("正在检查…", "Checking…") : tr("连接", "Connect")}
        </Button>
      </form>
    </AuthLayout>
  );
}

function BrowserLogin({ state }: { state: DesktopState }) {
  useLang();
  const loginError = useLoginError();
  const start = useMutation({ mutationFn: loginStart });
  const cancel = useMutation({ mutationFn: loginCancel });
  const failure = start.error ?? cancel.error;
  const signingOut = useSigningOut();
  const reason = signingOut ? signingOutMessage() : undefined;

  if (state.login_pending) {
    return (
      <AuthLayout title={tr("在浏览器里完成登录", "Finish signing in with browser")} subtitle={state.server ?? undefined}>
        <div className="flex flex-col gap-4">
          <p className="text-sm leading-relaxed text-ink-2">
            {tr(
              "已经在浏览器里打开了 zsmeet 的登录页。登录以后点「允许」，会自动回到这里。",
              "Opened zsmeet sign-in in your browser. After signing in, click \"Allow\" to return here automatically.",
            )}
          </p>
          <Spinner label={tr("正在等浏览器里的登录…", "Waiting for browser sign-in…")} />
          {loginError && <Notice tone="bad">{loginError}</Notice>}
          {failure && <Notice tone="bad">{errorMessage(failure)}</Notice>}
          <div className="flex flex-col gap-2">
            <Button onClick={() => start.mutate()} disabled={start.isPending || signingOut} title={reason}>
              {tr("重新打开浏览器", "Reopen browser")}
            </Button>
            <Button variant="ghost" onClick={() => cancel.mutate()} disabled={cancel.isPending}>
              {tr("取消", "Cancel")}
            </Button>
          </div>
          <p className="text-xs leading-relaxed text-muted">
            {tr("浏览器没有打开，或者登录页关掉了？点「重新打开浏览器」。", "Browser didn't open or you closed the tab? Click \"Reopen browser\".")}
          </p>
        </div>
      </AuthLayout>
    );
  }

  return (
    <AuthLayout title={tr("登录 zsmeet", "Sign in to zsmeet")} subtitle={state.server ?? undefined}>
      <div className="flex flex-col gap-4">
        {state.expired && (
          <Notice tone="warn">
            {tr(
              "登录已经失效（可能在别的地方退出了这台电脑，或者改过密码），请重新登录。",
              "Your session has expired (this device may have been signed out elsewhere or your password changed). Please sign in again.",
            )}
          </Notice>
        )}
        {loginError && <Notice tone="bad">{loginError}</Notice>}
        {failure && <Notice tone="bad">{errorMessage(failure)}</Notice>}
        <Button variant="primary" onClick={() => start.mutate()} disabled={start.isPending || signingOut} title={reason}>
          {tr("在浏览器里登录", "Sign in with browser")}
        </Button>
        <p className="text-center text-xs leading-relaxed text-muted">
          {tr(
            "会在浏览器里打开 zsmeet 的登录页，用密码或者飞书登录，再点「允许」，就会回到这里。",
            "Opens zsmeet in your browser to sign in with password or Feishu, then returns here after you click \"Allow\".",
          )}
        </p>
        <div className="border-t border-line pt-3 text-center">
          <SwitchServerButton variant="ghost" size="sm" />
        </div>
      </div>
    </AuthLayout>
  );
}

export function SwitchServerButton({ variant = "default", size = "md" }: { variant?: "default" | "ghost"; size?: "md" | "sm" }) {
  useLang();
  const { state } = useRecorder();
  const note = signOutNote(state, tr("切换服务器", "switching servers"));
  const [confirming, setConfirming] = useState(false);
  const change = useMutation({ mutationFn: switchServer });
  const failure = change.isError && <Notice tone="bad">{errorMessage(change.error)}</Notice>;
  const signingOut = useSigningOut();
  const busy = signingOut || change.isPending;
  const reason = signingOut ? signingOutMessage() : undefined;

  if (confirming && note) {
    return (
      <div className="flex flex-col gap-2.5 rounded-xl bg-warn-tint/60 px-4 py-3 text-left text-sm">
        <span className="text-ink-2">{note}</span>
        <span className="flex gap-2">
          <Button size="sm" variant="primary" disabled={busy} title={reason} onClick={() => change.mutate()}>
            {tr("仍然切换", "Switch anyway")}
          </Button>
          <Button size="sm" onClick={() => setConfirming(false)}>
            {tr("取消", "Cancel")}
          </Button>
        </span>
        {failure}
      </div>
    );
  }
  return (
    <>
      <Button variant={variant} size={size} disabled={busy} title={reason} onClick={() => (note ? setConfirming(true) : change.mutate())}>
        {tr("换一个服务器", "Switch server")}
      </Button>
      {failure}
    </>
  );
}

export function DesktopSessionExpired() {
  useLang();
  useEffect(() => {
    void sessionExpired();
  }, []);
  return <Spinner label={tr("登录已失效，正在退出…", "Session expired, signing out…")} />;
}

export function DesktopNotInitialized() {
  useLang();
  return (
    <div className="mx-auto mt-24 flex max-w-md flex-col gap-4 px-4">
      <div className="flex justify-end">
        <LanguageSelect compact />
      </div>
      <Notice tone="warn">
        {tr(
          "这台服务器还没完成初始化：请先在浏览器里打开它，创建管理员账号，再回来登录。",
          "This server has not been initialized yet: open it in a browser to create the admin account first, then return here to sign in.",
        )}
      </Notice>
      <SwitchServerButton />
    </div>
  );
}

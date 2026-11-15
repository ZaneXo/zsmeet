import { useMutation } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { useLocation, useSearchParams } from "react-router";
import { api, errorMessage, queryClient, unwrap } from "../api/client";
import type { Status, User } from "../api/client";
import { Button, Notice } from "../components/ui";
import { isEn } from "../i18n/lang";
import { tr, useLang } from "../i18n/react";
import { APP_NAMES, appCallbackURL, clearPendingAuthorize, parseAppAuthorize, rememberPendingAuthorize } from "../lib/appAuthorize";
import type { AppAuthorizeRequest } from "../lib/appAuthorize";
import { AuthLayout, LoginPage } from "./Auth";

/**
 * App（桌面版）登录的授权页：/app/authorize?client=desktop&device=…&challenge=…&state=…
 * 不在平常的界面框架里。链接不对时只显示原因；没登录先登录（密码、飞书都行），登录后回到这里；
 * 登录着的人确认以后拿到一次性的授权码，跳回 zsmeet://auth。见 docs/desktop.md。
 */
export function AppAuthorizePage({ status }: { status: Status }) {
  useLang();
  const [params] = useSearchParams();
  const { search } = useLocation();
  // 已经回到授权页了：用飞书登录前记下的地址不用再留着
  useEffect(() => clearPendingAuthorize(), []);

  const parsed = parseAppAuthorize(params);
  if ("error" in parsed) {
    return (
      <AuthLayout title={tr("登录链接有问题", "Invalid sign-in link")}>
        <Notice tone="bad">{parsed.error}</Notice>
      </AuthLayout>
    );
  }
  const req = parsed.request;
  const appName = APP_NAMES[req.client];

  if (!status.initialized) {
    return (
      <AuthLayout title={isEn() ? `Sign in to ${appName}` : `登录 ${appName}`}>
        <Notice tone="warn">
          {isEn()
            ? `This server has not set up an admin account yet. Open zsmeet's home page in your browser to finish setup, then return to ${appName} to sign in.`
            : `这台服务器还没有创建管理员账号。请先在浏览器里打开 zsmeet 的首页完成初始化，再回到 ${appName} 登录。`}
        </Notice>
      </AuthLayout>
    );
  }
  if (!status.user) {
    return (
      <LoginPage
        feishuLogin={status.feishu_login}
        subtitle={
          isEn()
            ? `Sign in to confirm whether to allow ${appName} (${req.device}) to access your account`
            : `登录以后，确认是否允许 ${appName}（${req.device}）使用你的账号`
        }
        // 用飞书登录会整页跳走、登录完回到首页：先记下这个页面，回来以后接着打开
        beforeFeishu={() => rememberPendingAuthorize(search)}
      />
    );
  }
  return <Consent req={req} appName={appName} user={status.user} />;
}

function Consent({ req, appName, user }: { req: AppAuthorizeRequest; appName: string; user: User }) {
  useLang();
  // 点了允许或者取消以后跳去的 zsmeet:// 地址。浏览器多半会问要不要打开 App，页面本身留着
  const [sent, setSent] = useState<{ url: string; allowed: boolean } | null>(null);
  const send = (url: string, allowed: boolean) => {
    setSent({ url, allowed });
    window.location.assign(url);
  };
  const allow = useMutation({
    mutationFn: () =>
      unwrap(api.POST("/api/apps/authorize", { body: { client: req.client, device_name: req.device, code_challenge: req.challenge } })),
    onSuccess: ({ code }) => send(appCallbackURL({ code }, req.state), true),
  });
  const logout = useMutation({
    mutationFn: () => unwrap(api.POST("/api/logout")),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["status"] }),
  });
  // 允许和退出不能同时进行：两个请求赛跑时，退出以后旧账号的授权码还是会发给 App
  const busy = allow.isPending || logout.isPending;

  if (sent) {
    return (
      <AuthLayout title={sent.allowed ? tr("已允许", "Allowed") : tr("已取消", "Cancelled")}>
        <div className="flex flex-col gap-4 text-sm leading-relaxed">
          {sent.allowed ? (
            <>
              <p>
                {isEn() ? (
                  <>
                    Returning to {appName}. When your browser asks to open &ldquo;zsmeet&rdquo;, choose <b>Open</b> and {appName} will finish signing in automatically.
                  </>
                ) : (
                  <>
                    正在回到 {appName}。浏览器问要不要打开「zsmeet」时，请选<b>打开</b>，{appName} 会自动完成登录。
                  </>
                )}
              </p>
              <p className="text-muted">
                {isEn()
                  ? `You can close this tab afterwards. If nothing happens, make sure ${appName} is running and click below again (valid for 2 minutes; if expired, go back to ${appName} and click "Sign in with browser" again).`
                  : `完成以后可以关掉这个页面。没有反应的话，确认 ${appName} 开着，再点一次下面的按钮（2 分钟内有效，过期了请回到 ${appName} 重新点「在浏览器里登录」）。`}
              </p>
              <div>
                <a href={sent.url} className="font-medium text-brand hover:underline">
                  {isEn() ? `Open ${appName} again` : `再次打开 ${appName}`}
                </a>
              </div>
            </>
          ) : (
            <p>
              {isEn()
                ? `${appName} will not sign in to your account. You can close this tab now.`
                : `${appName} 不会登录你的账号。可以关掉这个页面了。`}
            </p>
          )}
        </div>
      </AuthLayout>
    );
  }

  return (
    <AuthLayout title={isEn() ? `Allow ${appName}?` : `允许 ${appName} 登录？`}>
      <div className="flex flex-col gap-4 text-sm leading-relaxed">
        <p className="text-base font-semibold">
          {isEn() ? `Allow ${appName} (${req.device}) to sign in to your account?` : `允许 ${appName}（${req.device}）登录你的账号吗？`}
        </p>
        <div>
          <p className="text-ink-2">
            {isEn() ? (
              <>
                Signed in as <b>{user.name}</b> ({user.email}), it will be able to:
              </>
            ) : (
              <>
                将以 <b>{user.name}</b>（{user.email}）的身份：
              </>
            )}
          </p>
          <ul className="mt-1.5 list-disc pl-5 text-ink-2">
            <li>{tr("查看、搜索你能看到的会议和纪要，问会议", "View and search meetings and minutes you have access to, and ask questions across meetings")}</li>
            <li>{tr("录下这台电脑上的会议并上传到你的账号", "Record meetings on this computer and upload them to your account")}</li>
            <li>{tr("做你在网页上能做的其他操作", "Perform any other actions available to your account on the web")}</li>
          </ul>
        </div>
        <p className="text-[13px] text-muted">
          {isEn()
            ? `You can sign it out anytime under Settings → Account → Signed-in Apps. If you didn't just start signing in from ${appName}, click Cancel.`
            : `随时可以在「设置 → 账号 → 已登录的 App」里让它退出。如果不是你刚刚在 ${appName} 里点的登录，请点取消。`}
        </p>
        {allow.isError && <Notice tone="bad">{errorMessage(allow.error)}</Notice>}
        {logout.isError && <Notice tone="bad">{errorMessage(logout.error)}</Notice>}
        <div className="flex gap-2">
          <Button variant="primary" className="flex-1" disabled={busy} onClick={() => allow.mutate()}>
            {allow.isPending ? tr("正在允许…", "Allowing…") : tr("允许", "Allow")}
          </Button>
          <Button className="flex-1" disabled={busy} onClick={() => send(appCallbackURL({ error: "denied" }, req.state), false)}>
            {tr("取消", "Cancel")}
          </Button>
        </div>
        <p className="text-center text-xs text-muted">
          {tr("不是你的账号？", "Not your account? ")}
          <button type="button" className="cursor-pointer text-brand hover:underline disabled:opacity-45" disabled={busy} onClick={() => logout.mutate()}>
            {tr("退出后换一个账号登录", "Sign out and use another account")}
          </button>
        </p>
      </div>
    </AuthLayout>
  );
}

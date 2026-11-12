import { useMutation } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { useLocation, useSearchParams } from "react-router";
import { api, errorMessage, queryClient, unwrap } from "../api/client";
import type { Status, User } from "../api/client";
import { Button, Notice } from "../components/ui";
import { APP_NAMES, appCallbackURL, clearPendingAuthorize, parseAppAuthorize, rememberPendingAuthorize } from "../lib/appAuthorize";
import type { AppAuthorizeRequest } from "../lib/appAuthorize";
import { AuthLayout, LoginPage } from "./Auth";

/**
 * App（桌面版）登录的授权页：/app/authorize?client=desktop&device=…&challenge=…&state=…
 * 不在平常的界面框架里。链接不对时只显示原因；没登录先登录（密码、飞书都行），登录后回到这里；
 * 登录着的人确认以后拿到一次性的授权码，跳回 zsmeet://auth。见 docs/desktop.md。
 */
export function AppAuthorizePage({ status }: { status: Status }) {
  const [params] = useSearchParams();
  const { search } = useLocation();
  // 已经回到授权页了：用飞书登录前记下的地址不用再留着
  useEffect(() => clearPendingAuthorize(), []);

  const parsed = parseAppAuthorize(params);
  if ("error" in parsed) {
    return (
      <AuthLayout title="登录链接有问题">
        <Notice tone="bad">{parsed.error}</Notice>
      </AuthLayout>
    );
  }
  const req = parsed.request;
  const appName = APP_NAMES[req.client];

  if (!status.initialized) {
    return (
      <AuthLayout title={`登录 ${appName}`}>
        <Notice tone="warn">这台服务器还没有创建管理员账号。请先在浏览器里打开 zsmeet 的首页完成初始化，再回到 {appName} 登录。</Notice>
      </AuthLayout>
    );
  }
  if (!status.user) {
    return (
      <LoginPage
        feishuLogin={status.feishu_login}
        subtitle={`登录以后，确认是否允许 ${appName}（${req.device}）使用你的账号`}
        // 用飞书登录会整页跳走、登录完回到首页：先记下这个页面，回来以后接着打开
        beforeFeishu={() => rememberPendingAuthorize(search)}
      />
    );
  }
  return <Consent req={req} appName={appName} user={status.user} />;
}

function Consent({ req, appName, user }: { req: AppAuthorizeRequest; appName: string; user: User }) {
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
      <AuthLayout title={sent.allowed ? "已允许" : "已取消"}>
        <div className="flex flex-col gap-4 text-sm leading-relaxed">
          {sent.allowed ? (
            <>
              <p>
                正在回到 {appName}。浏览器问要不要打开「zsmeet」时，请选<b>打开</b>，{appName} 会自动完成登录。
              </p>
              <p className="text-muted">
                完成以后可以关掉这个页面。没有反应的话，确认 {appName} 开着，再点一次下面的按钮（2 分钟内有效，过期了请回到 {appName} 重新点「在浏览器里登录」）。
              </p>
              <div>
                <a href={sent.url} className="font-medium text-brand hover:underline">
                  再次打开 {appName}
                </a>
              </div>
            </>
          ) : (
            <p>{appName} 不会登录你的账号。可以关掉这个页面了。</p>
          )}
        </div>
      </AuthLayout>
    );
  }

  return (
    <AuthLayout title={`允许 ${appName} 登录？`}>
      <div className="flex flex-col gap-4 text-sm leading-relaxed">
        <p className="text-base font-semibold">
          允许 {appName}（{req.device}）登录你的账号吗？
        </p>
        <div>
          <p className="text-ink-2">
            将以 <b>{user.name}</b>（{user.email}）的身份：
          </p>
          <ul className="mt-1.5 list-disc pl-5 text-ink-2">
            <li>查看、搜索你能看到的会议和纪要，问会议</li>
            <li>录下这台电脑上的会议并上传到你的账号</li>
            <li>做你在网页上能做的其他操作</li>
          </ul>
        </div>
        <p className="text-[13px] text-muted">
          随时可以在「设置 → 账号 → 已登录的 App」里让它退出。如果不是你刚刚在 {appName} 里点的登录，请点取消。
        </p>
        {allow.isError && <Notice tone="bad">{errorMessage(allow.error)}</Notice>}
        {logout.isError && <Notice tone="bad">{errorMessage(logout.error)}</Notice>}
        <div className="flex gap-2">
          <Button variant="primary" className="flex-1" disabled={busy} onClick={() => allow.mutate()}>
            {allow.isPending ? "正在允许…" : "允许"}
          </Button>
          <Button className="flex-1" disabled={busy} onClick={() => send(appCallbackURL({ error: "denied" }, req.state), false)}>
            取消
          </Button>
        </div>
        <p className="text-center text-xs text-muted">
          不是你的账号？
          <button type="button" className="cursor-pointer text-brand hover:underline disabled:opacity-45" disabled={busy} onClick={() => logout.mutate()}>
            退出后换一个账号登录
          </button>
        </p>
      </div>
    </AuthLayout>
  );
}

import { useMutation, useQuery } from "@tanstack/react-query";
import { useState } from "react";
import type { FormEvent } from "react";
import { api, errorMessage, unwrap } from "../api/client";
import { AppLink } from "../components/AppLink";
import { Button, Field, Input, Notice, Spinner } from "../components/ui";
import { isEn } from "../i18n/lang";
import { tr, useLang } from "../i18n/react";
import { AuthLayout } from "./Auth";

/**
 * 邀请链接和重置密码链接的落地页，不需要登录就能打开。
 *
 * 链接长这样：`/invite#<token>`、`/reset#<token>`。token 放在 # 后面，
 * 浏览器不会把它发给服务器，所以不会留在访问日志里；这里拿到以后放在请求体里发给接口。
 *
 * token 由 App 从地址栏读出来传进来，并且用它做 key：有人在同一个标签页里把新链接粘贴到地址栏时
 * （只有 # 后面变了，浏览器不会重新加载页面），这里会整个重来，而不是还显示上一个链接的结果。
 */
export function AccountLinkPage({ token }: { token: string }) {
  useLang();
  const [name, setName] = useState("");
  const [password, setPassword] = useState("");

  const info = useQuery({
    queryKey: ["account-link", token],
    queryFn: () => unwrap(api.POST("/api/account-links/lookup", { body: { token } })),
    enabled: token.length > 0,
    staleTime: Infinity,
  });

  const accept = useMutation({
    mutationFn: () => unwrap(api.POST("/api/account-links/accept", { body: { token, name: name || undefined, password } })),
    onSuccess: () => {
      // 整页跳到首页：已经登录好了，顺便把带 token 的地址从页面上换掉
      window.location.replace("/");
    },
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    accept.mutate();
  };

  if (!token) {
    return (
      <Problem
        message={tr(
          "这个链接不完整，缺少后面那一长串字符。请把管理员发来的链接完整地复制到地址栏里再打开。",
          "This link is incomplete and missing the token at the end. Copy the full link from your admin and paste it into the address bar.",
        )}
      />
    );
  }
  if (info.isPending) return <Spinner label={tr("正在检查链接…", "Checking link…")} />;
  if (info.isError) return <Problem message={errorMessage(info.error)} />;

  const link = info.data;
  const invite = link.kind === "invite";
  return (
    <AuthLayout
      title={invite ? tr("加入 zsmeet", "Join zsmeet") : tr("设置新密码", "Set new password")}
      subtitle={
        invite
          ? isEn()
            ? `${link.invited_by || "Your admin"} invited you to join the team's meeting minutes workspace. Fill in the details below to get started.`
            : `${link.invited_by || "管理员"}邀请你加入团队的会议纪要系统。填好下面两项就能开始用。`
          : isEn()
            ? `Set a new password for ${link.name}'s account. Saving will sign out all other browsers and signed-in desktop apps for this account.`
            : `给 ${link.name} 的账号设置一个新密码。保存后，这个账号在其他设备上的登录会全部退出，已登录的 App 也会退出。`
      }
    >
      <form onSubmit={submit} className="flex flex-col gap-4">
        <Field label={tr("邮箱", "Email")} htmlFor="link-email" hint={invite ? tr("以后用这个邮箱登录", "Use this email to sign in later") : undefined}>
          <Input id="link-email" type="email" value={link.email} readOnly disabled autoComplete="username" />
        </Field>
        {invite && (
          <Field
            label={tr("姓名", "Name")}
            htmlFor="link-name"
            hint={tr("同事会在会议和纪要的修改记录里看到这个名字", "Teammates will see this name on meetings and minutes revisions")}
          >
            <Input id="link-name" value={name} onChange={(e) => setName(e.target.value)} required maxLength={50} autoComplete="name" autoFocus />
          </Field>
        )}
        <Field
          label={invite ? tr("设置密码", "Set password") : tr("新密码", "New password")}
          htmlFor="link-password"
          hint={tr("至少 8 位", "At least 8 characters")}
        >
          <Input
            id="link-password"
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            required
            minLength={8}
            autoComplete="new-password"
            autoFocus={!invite}
          />
        </Field>
        {accept.isError && <Notice tone="bad">{errorMessage(accept.error)}</Notice>}
        <Button type="submit" variant="primary" disabled={accept.isPending || accept.isSuccess}>
          {accept.isPending || accept.isSuccess
            ? tr("请稍候…", "Please wait…")
            : invite
              ? tr("加入", "Join")
              : tr("保存并登录", "Save & sign in")}
        </Button>
      </form>
    </AuthLayout>
  );
}

/** 链接用不了时的说明页。 */
function Problem({ message }: { message: string }) {
  useLang();
  return (
    <AuthLayout title={tr("这个链接用不了", "Link unavailable")}>
      <div className="flex flex-col gap-4">
        <Notice tone="warn">{message}</Notice>
        <AppLink to="/" className="text-center text-sm font-medium text-brand hover:text-brand-hover">
          {tr("打开 zsmeet", "Open zsmeet")}
        </AppLink>
      </div>
    </AuthLayout>
  );
}

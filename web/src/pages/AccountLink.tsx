import { useMutation, useQuery } from "@tanstack/react-query";
import { useState } from "react";
import type { FormEvent } from "react";
import { api, errorMessage, unwrap } from "../api/client";
import { AppLink } from "../components/AppLink";
import { Button, Field, Input, Notice, Spinner } from "../components/ui";
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
      <Problem message="这个链接不完整，缺少后面那一长串字符。请把管理员发来的链接完整地复制到地址栏里再打开。" />
    );
  }
  if (info.isPending) return <Spinner label="正在检查链接…" />;
  if (info.isError) return <Problem message={errorMessage(info.error)} />;

  const link = info.data;
  const invite = link.kind === "invite";
  return (
    <AuthLayout
      title={invite ? "加入 zsmeet" : "设置新密码"}
      subtitle={
        invite
          ? `${link.invited_by || "管理员"}邀请你加入团队的会议纪要系统。填好下面两项就能开始用。`
          : `给 ${link.name} 的账号设置一个新密码。保存后，这个账号在其他设备上的登录会全部退出，已登录的 App 也会退出。`
      }
    >
      <form onSubmit={submit} className="flex flex-col gap-4">
        <Field label="邮箱" htmlFor="link-email" hint={invite ? "以后用这个邮箱登录" : undefined}>
          <Input id="link-email" type="email" value={link.email} readOnly disabled autoComplete="username" />
        </Field>
        {invite && (
          <Field label="姓名" htmlFor="link-name" hint="同事会在会议和纪要的修改记录里看到这个名字">
            <Input id="link-name" value={name} onChange={(e) => setName(e.target.value)} required maxLength={50} autoComplete="name" autoFocus />
          </Field>
        )}
        <Field label={invite ? "设置密码" : "新密码"} htmlFor="link-password" hint="至少 8 位">
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
          {accept.isPending || accept.isSuccess ? "请稍候…" : invite ? "加入" : "保存并登录"}
        </Button>
      </form>
    </AuthLayout>
  );
}

/** 链接用不了时的说明页。 */
function Problem({ message }: { message: string }) {
  return (
    <AuthLayout title="这个链接用不了">
      <div className="flex flex-col gap-4">
        <Notice tone="warn">{message}</Notice>
        <AppLink to="/" className="text-center text-sm font-medium text-brand hover:text-brand-hover">
          打开 zsmeet
        </AppLink>
      </div>
    </AuthLayout>
  );
}

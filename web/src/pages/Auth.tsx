import { useMutation } from "@tanstack/react-query";
import { useState } from "react";
import type { FormEvent, ReactNode } from "react";
import { api, errorMessage, queryClient, unwrap } from "../api/client";
import { Button, buttonClass, Card, Field, Input, Notice } from "../components/ui";
import { useFeishuResult } from "../lib/feishuAuth";
import { feishuErrorText } from "../lib/feishuErrors";

export function AuthLayout({ title, subtitle, children }: { title: string; subtitle?: string; children: ReactNode }) {
  return (
    <div className="flex min-h-full items-center justify-center px-4 py-10">
      <div className="flex w-full max-w-sm flex-col gap-5">
        <div className="flex flex-col items-center gap-3 text-center">
          <div className="flex size-11 items-center justify-center rounded-xl bg-ink text-lg font-bold text-white">记</div>
          <h1 className="text-2xl font-bold">{title}</h1>
          {subtitle && <p className="text-sm leading-relaxed text-muted">{subtitle}</p>}
        </div>
        <Card className="p-6">{children}</Card>
      </div>
    </div>
  );
}

// 登录或初始化成功后重新读系统状态，App 会据此切到主界面
const refreshStatus = () => queryClient.invalidateQueries({ queryKey: ["status"] });

/** 第一次打开时的初始化向导：创建管理员账号。 */
export function SetupPage() {
  const [name, setName] = useState("");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const setup = useMutation({
    mutationFn: () => unwrap(api.POST("/api/setup", { body: { name, email, password } })),
    onSuccess: refreshStatus,
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    setup.mutate();
  };

  return (
    <AuthLayout title="欢迎使用 zsmeet" subtitle="这是第一次启动。先创建管理员账号，之后的团队配置都由管理员来填。">
      <form onSubmit={submit} className="flex flex-col gap-4">
        <Field label="姓名" htmlFor="setup-name">
          <Input id="setup-name" value={name} onChange={(e) => setName(e.target.value)} required maxLength={50} autoComplete="name" autoFocus />
        </Field>
        <Field label="邮箱" htmlFor="setup-email" hint="用来登录">
          <Input id="setup-email" type="email" value={email} onChange={(e) => setEmail(e.target.value)} required autoComplete="email" />
        </Field>
        <Field label="密码" htmlFor="setup-password" hint="至少 8 位">
          <Input id="setup-password" type="password" value={password} onChange={(e) => setPassword(e.target.value)} required minLength={8} autoComplete="new-password" />
        </Field>
        {setup.isError && <Notice tone="bad">{errorMessage(setup.error)}</Notice>}
        <Button type="submit" variant="primary" disabled={setup.isPending}>
          {setup.isPending ? "正在创建…" : "创建管理员并进入"}
        </Button>
      </form>
    </AuthLayout>
  );
}

/**
 * 登录页。管理员打开了飞书登录时多一个「用飞书登录」：普通的链接，整页跳到飞书的授权页，登录完飞书跳回首页。
 * 用飞书登录没成功时，飞书跳回来的地址里带着原因的代号（见 useFeishuResult）。
 * App 的授权页借用它时会换一句副标题，并在跳去飞书之前记下授权页的地址（beforeFeishu）。
 */
export function LoginPage({ feishuLogin, subtitle, beforeFeishu }: { feishuLogin: boolean; subtitle?: string; beforeFeishu?: () => void }) {
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const { error: feishuError } = useFeishuResult();
  const lastError = feishuError && feishuErrorText(feishuError, "login");
  const login = useMutation({
    mutationFn: () => unwrap(api.POST("/api/login", { body: { email, password } })),
    onSuccess: refreshStatus,
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    login.mutate();
  };

  return (
    <AuthLayout title="登录 zsmeet" subtitle={subtitle ?? "会议录音自动转写，生成纪要"}>
      {lastError && (
        <div className="mb-4">
          <Notice tone="bad">{lastError}</Notice>
        </div>
      )}
      {feishuLogin && (
        <>
          <a href="/api/auth/feishu/start" onClick={beforeFeishu} className={`${buttonClass("primary")} w-full`}>
            用飞书登录
          </a>
          <p className="mt-2 text-center text-xs leading-relaxed text-muted">用飞书扫码，或者在飞书网页版已经登录时点一下就行</p>
          <div className="my-5 flex items-center gap-3 text-xs text-muted" aria-hidden="true">
            <span className="h-px flex-1 bg-line" />
            或者用邮箱和密码
            <span className="h-px flex-1 bg-line" />
          </div>
        </>
      )}
      <form onSubmit={submit} className="flex flex-col gap-4">
        <Field label="邮箱" htmlFor="login-email">
          <Input id="login-email" type="email" value={email} onChange={(e) => setEmail(e.target.value)} required autoComplete="email" autoFocus={!feishuLogin} />
        </Field>
        <Field label="密码" htmlFor="login-password">
          <Input id="login-password" type="password" value={password} onChange={(e) => setPassword(e.target.value)} required autoComplete="current-password" />
        </Field>
        {login.isError && <Notice tone="bad">{errorMessage(login.error)}</Notice>}
        <Button type="submit" variant={feishuLogin ? "default" : "primary"} disabled={login.isPending}>
          {login.isPending ? "正在登录…" : "登录"}
        </Button>
      </form>
    </AuthLayout>
  );
}

import { useMutation, useQuery } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import type { FormEvent } from "react";
import { useParams } from "react-router";
import { AppLink } from "../components/AppLink";
import { api, errorMessage, queryClient, unwrap } from "../api/client";
import type { Device, User } from "../api/client";
import { Button, Card, Chip, Field, Input, Notice, Select, Spinner } from "../components/ui";
import { APP_NAMES } from "../lib/appAuthorize";
import { useFeishuResult } from "../lib/feishuAuth";
import { feishuErrorText } from "../lib/feishuErrors";
import { dateTime } from "../lib/format";
import { isDesktop } from "../platform";
import { DesktopAccountSection, OpenAdminSettings } from "../platform/DesktopAccount";
import { FeishuSettingsSection } from "./FeishuSettings";
import { MembersSection } from "./Members";
import { TemplateSettingsSection } from "./TemplateSettings";
import { VoiceprintSettingsSection } from "./VoiceprintSettings";
import { WebhookSettingsSection } from "./WebhookSettings";

const SECTIONS = [
  { key: "account", label: "账号", adminOnly: false },
  { key: "members", label: "成员", adminOnly: true },
  { key: "llm", label: "大模型", adminOnly: true },
  { key: "templates", label: "纪要模板", adminOnly: true },
  { key: "feishu", label: "飞书", adminOnly: true },
  { key: "webhooks", label: "群机器人", adminOnly: true },
  { key: "retention", label: "录音保留", adminOnly: true },
  { key: "voiceprints", label: "声纹", adminOnly: true },
] as const;

type SectionKey = (typeof SECTIONS)[number]["key"];

/**
 * 设置页。左边是分区：自己的账号人人都有，成员、大模型和飞书只有管理员能看到。
 * 桌面版只有账号；团队管理先放一个「在网页里打开」的入口。
 */
export function SettingsPage({ user }: { user: User }) {
  const params = useParams();
  const isAdmin = user.role === "admin";
  const visible = SECTIONS.filter((s) => (isAdmin && !isDesktop) || !s.adminOnly);
  const current: SectionKey = visible.find((s) => s.key === params.section)?.key ?? "account";

  return (
    <div className="mx-auto flex max-w-5xl flex-col gap-5">
      <h1 className="text-[26px] font-bold">设置</h1>
      <div className="flex flex-col gap-5 md:flex-row md:items-start">
        <nav aria-label="设置分区" className="flex shrink-0 flex-row flex-wrap gap-1 md:w-44 md:flex-col">
          {visible.map((s, i) => (
            <div key={s.key} className="contents">
              {/* 第一个管理员才有的分区前面加一个小标题，说明下面这些是团队级的配置 */}
              {s.adminOnly && !visible[i - 1]?.adminOnly && (
                <div className="hidden px-3 pb-1 pt-3 text-xs font-semibold text-muted md:block">团队管理 · 仅管理员</div>
              )}
              <AppLink
                to={s.key === "account" ? "/settings" : `/settings/${s.key}`}
                aria-current={current === s.key ? "page" : undefined}
                className={`flex min-h-10 items-center whitespace-nowrap rounded-lg px-3 text-sm ${
                  current === s.key ? "bg-white font-semibold text-ink shadow-[0_0_0_1px_var(--color-line)]" : "text-ink-2 hover:bg-subtle"
                }`}
              >
                {s.label}
              </AppLink>
            </div>
          ))}
          {isDesktop && isAdmin && (
            <>
              <div className="hidden px-3 pb-1 pt-3 text-xs font-semibold text-muted md:block">团队管理 · 仅管理员</div>
              <OpenAdminSettings />
            </>
          )}
        </nav>

        <div className="min-w-0 flex-1">
          {current === "account" && (isDesktop ? <DesktopAccountSection user={user} /> : <AccountSection user={user} />)}
          {current === "members" && <MembersSection me={user} />}
          {current === "llm" && <LLMSettingsCard />}
          {current === "templates" && <TemplateSettingsSection />}
          {current === "feishu" && <FeishuSettingsSection />}
          {current === "webhooks" && <WebhookSettingsSection />}
          {current === "retention" && <RetentionCard />}
          {current === "voiceprints" && <VoiceprintSettingsSection />}
        </div>
      </div>
    </div>
  );
}

function AccountSection({ user }: { user: User }) {
  return (
    <div className="flex flex-col gap-5">
      <Card className="flex flex-col gap-1 p-6">
        <h2 className="text-lg font-bold">账号</h2>
        <dl className="mt-2 grid grid-cols-[72px_1fr] gap-y-2 text-sm">
          <dt className="text-muted">姓名</dt>
          <dd>{user.name}</dd>
          <dt className="text-muted">邮箱</dt>
          <dd>{user.email}</dd>
          <dt className="text-muted">角色</dt>
          <dd>{user.role === "admin" ? "管理员" : "成员"}</dd>
        </dl>
        {user.role !== "admin" && (
          <p className="mt-3 text-[13px] leading-relaxed text-muted">成员、大模型、飞书、群机器人这些团队配置由管理员统一管理。</p>
        )}
      </Card>
      <FeishuLinkCard />
      <DevicesCard />
      <ChangePasswordCard email={user.email} />
    </div>
  );
}

/** 已登录的 App：在哪些电脑上用 zsmeet 桌面版登录着，可以让某一台退出。 */
function DevicesCard() {
  const devices = useQuery({
    queryKey: ["me", "devices"],
    queryFn: () => unwrap(api.GET("/api/me/devices")),
  });
  const revoke = useMutation({
    mutationFn: (id: string) => unwrap(api.DELETE("/api/me/devices/{id}", { params: { path: { id } } })),
    onSettled: () => queryClient.invalidateQueries({ queryKey: ["me", "devices"] }),
  });
  const items = devices.data?.items;
  return (
    <Card className="flex flex-col gap-3 p-6">
      <div className="flex flex-col gap-1">
        <h2 className="text-lg font-bold">已登录的 App</h2>
        <p className="text-[13px] leading-relaxed text-muted">
          在 zsmeet 桌面版里点「在浏览器里登录」并允许以后，那台电脑会出现在这里。电脑丢了、不再用了，就点「退出」，那台电脑要重新登录才能用。
        </p>
      </div>
      {devices.isPending && <Spinner label="正在读取…" />}
      {devices.isError && (
        <div className="flex flex-col items-start gap-2">
          <Notice tone="bad">读不到已登录的 App：{errorMessage(devices.error)}</Notice>
          <Button size="sm" disabled={devices.isFetching} onClick={() => devices.refetch()}>
            {devices.isFetching ? "正在重试…" : "重试"}
          </Button>
        </div>
      )}
      {items && items.length === 0 && <p className="text-sm text-ink-2">还没有在 App 里登录过。</p>}
      {items && items.length > 0 && (
        <ul className="divide-y divide-line rounded-xl border border-line">
          {items.map((d) => (
            <DeviceRow key={d.id} device={d} busy={revoke.isPending && revoke.variables === d.id} onRevoke={() => revoke.mutate(d.id)} />
          ))}
        </ul>
      )}
      {revoke.isError && <Notice tone="bad">{errorMessage(revoke.error)}</Notice>}
    </Card>
  );
}

function DeviceRow({ device: d, busy, onRevoke }: { device: Device; busy: boolean; onRevoke: () => void }) {
  return (
    <li className="flex flex-wrap items-center gap-x-4 gap-y-2 px-4 py-3">
      <div className="flex min-w-0 flex-1 flex-col gap-0.5">
        <div className="flex items-center gap-2">
          <span className="truncate text-sm font-semibold" title={d.device_name}>
            {d.device_name}
          </span>
          {d.current && <Chip tone="info">就是这台</Chip>}
        </div>
        <span className="text-xs text-muted">
          {APP_NAMES[d.client]} · {dateTime(d.created_at)} 登录 · {d.last_used_at ? `上次使用 ${dateTime(d.last_used_at)}` : "登录以后还没用过"}
        </span>
      </div>
      <Button size="sm" disabled={busy} onClick={onRevoke}>
        {busy ? "正在退出…" : "退出"}
      </Button>
    </li>
  );
}

/**
 * 飞书账号：关联以后可以在登录页上用飞书登录。管理员打开了飞书登录时才能关联（「关联飞书」整页跳到飞书的授权页，
 * 完成后跳回这里，地址里带着结果）；已经关联的随时可以解除。没打开飞书登录、也没关联过时不显示。
 */
function FeishuLinkCard() {
  const link = useQuery({
    queryKey: ["me", "feishu"],
    queryFn: () => unwrap(api.GET("/api/me/feishu")),
  });
  const result = useFeishuResult();
  const [confirm, setConfirm] = useState(false);
  // 关联前要输入当前密码：只拿到登录状态的人（比如别人的电脑上没退出）关联不了自己的飞书账号
  const [pass, setPass] = useState("");
  const start = useMutation({
    mutationFn: () => unwrap(api.POST("/api/me/feishu/link", { body: { password: pass } })),
    onSuccess: (data) => {
      // 整页跳到飞书的授权页，完成后跳回这里，地址里带着结果
      if (data.url) window.location.assign(data.url);
    },
  });
  const unlink = useMutation({
    mutationFn: () => unwrap(api.DELETE("/api/me/feishu")),
    onSettled: () => {
      setConfirm(false);
      void queryClient.invalidateQueries({ queryKey: ["me", "feishu"] });
    },
  });
  const data = link.data;
  // 读不到关联情况：不能悄悄不显示，飞书跳回来时带的错误也要让人看到
  if (!data && link.isError) {
    return (
      <Card className="flex flex-col items-start gap-3 p-6">
        <h2 className="text-lg font-bold">飞书账号</h2>
        {result.error && <Notice tone="bad">{feishuErrorText(result.error, "account")}</Notice>}
        <Notice tone="bad">读不到你的飞书账号关联情况：{errorMessage(link.error)}</Notice>
        <Button size="sm" disabled={link.isFetching} onClick={() => link.refetch()}>
          {link.isFetching ? "正在重试…" : "重试"}
        </Button>
      </Card>
    );
  }
  if (!data || (!data.enabled && !data.linked && !result.error)) return null;
  return (
    <Card className="flex flex-col gap-3 p-6">
      <div className="flex flex-col gap-1">
        <h2 className="text-lg font-bold">飞书账号</h2>
        <p className="text-[13px] leading-relaxed text-muted">
          {data.linked
            ? `已关联飞书账号${data.linked_at ? `（${dateTime(data.linked_at)} 起）` : ""}，可以在登录页上用飞书登录。换了飞书账号的话，先解除关联，再关联新的。`
            : data.enabled
              ? "还没有关联飞书账号。关联以后，可以在登录页上用飞书扫码登录。"
              : "管理员没有打开飞书登录。"}
        </p>
      </div>
      {result.linked && data.linked && !unlink.isSuccess && <Notice tone="ok">已关联飞书账号。</Notice>}
      {result.error && <Notice tone="bad">{feishuErrorText(result.error, "account")}</Notice>}
      {unlink.isError && <Notice tone="bad">{errorMessage(unlink.error)}</Notice>}
      {unlink.isSuccess && !data.linked && <Notice tone="ok">已解除关联。</Notice>}
      {data.linked ? (
        confirm ? (
          <div className="flex flex-wrap items-center gap-2">
            <span className="text-sm text-ink-2">解除以后不能再用这个飞书账号登录，确定吗？</span>
            <Button size="sm" variant="danger" disabled={unlink.isPending} onClick={() => unlink.mutate()}>
              解除关联
            </Button>
            <Button size="sm" onClick={() => setConfirm(false)}>
              取消
            </Button>
          </div>
        ) : (
          <div>
            <Button size="sm" onClick={() => setConfirm(true)}>
              解除关联
            </Button>
          </div>
        )
      ) : (
        data.enabled && (
          <form
            className="flex flex-col gap-3"
            onSubmit={(e) => {
              e.preventDefault();
              start.mutate();
            }}
          >
            <Field label="当前密码" htmlFor="feishu-link-password" hint="关联前先确认是你本人。用飞书自动创建的账号没有密码，请管理员给你生成重置密码的链接">
              <Input
                id="feishu-link-password"
                type="password"
                autoComplete="current-password"
                className="max-w-xs"
                required
                value={pass}
                onChange={(e) => {
                  setPass(e.target.value);
                  start.reset();
                }}
              />
            </Field>
            {start.isError && <Notice tone="bad">{errorMessage(start.error)}</Notice>}
            {start.data?.error && <Notice tone="bad">{feishuErrorText(start.data.error, "account")}</Notice>}
            <div>
              <Button type="submit" variant="primary" size="sm" disabled={start.isPending || !pass}>
                {start.isPending ? "正在核对…" : "关联飞书"}
              </Button>
            </div>
          </form>
        )
      )}
    </Card>
  );
}

function ChangePasswordCard({ email }: { email: string }) {
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [again, setAgain] = useState("");
  const change = useMutation({
    mutationFn: () => unwrap(api.POST("/api/me/password", { body: { current_password: current, new_password: next } })),
    onSuccess: () => {
      setCurrent("");
      setNext("");
      setAgain("");
      // 改了密码，飞书的关联也解除了，App 也都退出了
      void queryClient.invalidateQueries({ queryKey: ["me", "feishu"] });
      void queryClient.invalidateQueries({ queryKey: ["me", "devices"] });
    },
  });
  const mismatch = again.length > 0 && next !== again;
  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (next !== again) return;
    change.mutate();
  };
  return (
    <Card className="p-6">
      <form onSubmit={submit} className="flex flex-col gap-4">
        {/* 给浏览器的密码管理器看：改的是哪个账号的密码，它才能更新存着的那一条 */}
        <input type="email" name="username" autoComplete="username" value={email} readOnly hidden />
        <div className="flex flex-col gap-1">
          <h2 className="text-lg font-bold">修改密码</h2>
          <p className="text-[13px] leading-relaxed text-muted">改完以后这台设备保持登录，其他设备上的登录会全部退出，已登录的 App 也会退出（要在 App 里重新登录）；关联了飞书的话，关联也会解除，需要时再关联一次。忘了当前密码的话，请管理员帮你重置。</p>
        </div>
        <Field label="当前密码" htmlFor="pw-current">
          <Input id="pw-current" type="password" className="max-w-sm" value={current} onChange={(e) => { setCurrent(e.target.value); change.reset(); }} required autoComplete="current-password" />
        </Field>
        <Field label="新密码" htmlFor="pw-new" hint="至少 8 位">
          <Input id="pw-new" type="password" className="max-w-sm" value={next} onChange={(e) => { setNext(e.target.value); change.reset(); }} required minLength={8} autoComplete="new-password" />
        </Field>
        <Field label="再输一遍新密码" htmlFor="pw-again">
          <Input id="pw-again" type="password" className="max-w-sm" value={again} onChange={(e) => { setAgain(e.target.value); change.reset(); }} required minLength={8} autoComplete="new-password" aria-invalid={mismatch} />
        </Field>
        {mismatch && <Notice tone="warn">两次输入的新密码不一样。</Notice>}
        {change.isError && <Notice tone="bad">{errorMessage(change.error)}</Notice>}
        {change.isSuccess && <Notice tone="ok">密码已修改。其他设备上的登录和已登录的 App 都已经退出，和飞书的关联（如果有）也解除了。</Notice>}
        <div>
          <Button type="submit" variant="primary" disabled={change.isPending || mismatch}>
            {change.isPending ? "正在保存…" : "修改密码"}
          </Button>
        </div>
      </form>
    </Card>
  );
}

// 上下文长度能选的几档（token 数），和后端的 settings.ContextChoices 一致
const CONTEXT_CHOICES = [
  [8192, "8K"],
  [16384, "16K"],
  [32768, "32K（默认）"],
  [65536, "64K"],
  [131072, "128K"],
  [200000, "200K 及以上"],
] as const;

/** 大模型配置：全团队共用，只有管理员能看、能改。 */
function LLMSettingsCard() {
  const saved = useQuery({
    queryKey: ["settings", "llm"],
    queryFn: () => unwrap(api.GET("/api/settings/llm")),
  });

  const [baseURL, setBaseURL] = useState("");
  const [model, setModel] = useState("");
  const [apiKey, setAPIKey] = useState("");
  const [contextTokens, setContextTokens] = useState(32768);
  // 表单只在第一次拿到已保存的值时填充，之后不覆盖用户正在改的内容
  const [loaded, setLoaded] = useState(false);
  useEffect(() => {
    if (saved.data && !loaded) {
      setBaseURL(saved.data.base_url);
      setModel(saved.data.model);
      setContextTokens(saved.data.context_tokens);
      setLoaded(true);
    }
  }, [saved.data, loaded]);

  const body = { base_url: baseURL, model, api_key: apiKey || undefined, context_tokens: contextTokens };

  const test = useMutation({
    mutationFn: () => unwrap(api.POST("/api/settings/llm/test", { body })),
  });
  // 列出接口上的模型给管理员挑。只需要接口地址和 Key（Key 留空用已保存的）
  const models = useMutation({
    mutationFn: () => unwrap(api.POST("/api/settings/llm/models", { body: { base_url: baseURL, api_key: apiKey || undefined } })),
  });
  const save = useMutation({
    mutationFn: () => unwrap(api.PUT("/api/settings/llm", { body })),
    onSuccess: (data) => {
      queryClient.setQueryData(["settings", "llm"], data);
      setAPIKey("");
      // 首页和会议页要知道大模型已经配好了
      void queryClient.invalidateQueries({ queryKey: ["status"] });
    },
  });

  // 改了任何一项，之前的测试和保存结果就不再算数
  const edited = () => {
    test.reset();
    save.reset();
  };
  const submit = (e: FormEvent) => {
    e.preventDefault();
    test.reset();
    save.mutate();
  };

  if (saved.isPending) return <Spinner label="正在加载配置…" />;
  if (saved.isError) return <Notice tone="bad">{errorMessage(saved.error)}</Notice>;

  const pending = test.isPending || save.isPending;

  return (
    <Card className="p-6">
      <form onSubmit={submit} className="flex flex-col gap-4">
        <div className="flex flex-col gap-1">
          <h2 className="text-lg font-bold">大模型</h2>
          <p className="text-[13px] leading-relaxed text-muted">
            用来把转写写成纪要。支持任意 OpenAI 兼容接口：自己部署的 Ollama、vLLM，或者各家云服务。全团队共用这一份配置。
          </p>
        </div>

        <Field label="接口地址" htmlFor="llm-url" hint="一般以 /v1 结尾。例如 Ollama 是 http://ollama:11434/v1">
          <Input
            id="llm-url"
            type="url"
            required
            maxLength={500}
            placeholder="https://api.openai.com/v1"
            value={baseURL}
            onChange={(e) => {
              setBaseURL(e.target.value);
              models.reset();
              edited();
            }}
          />
        </Field>
        <Field label="API Key" htmlFor="llm-key" hint={saved.data.api_key_set ? "已保存。留空表示不修改" : "自己部署的模型不需要的话可以不填"}>
          <Input
            id="llm-key"
            type="password"
            autoComplete="off"
            placeholder={saved.data.api_key_set ? "••••••••" : ""}
            value={apiKey}
            onChange={(e) => {
              setAPIKey(e.target.value);
              models.reset();
              edited();
            }}
          />
        </Field>
        <Field label="模型" htmlFor="llm-model" hint="不知道填什么的话，填好上面两项后点「获取模型列表」，从里面选">
          <div className="flex flex-col gap-2">
            <div className="flex gap-2">
              <Input
                id="llm-model"
                required
                maxLength={200}
                placeholder="模型名"
                className="min-w-0"
                value={model}
                onChange={(e) => {
                  setModel(e.target.value);
                  edited();
                }}
              />
              <Button disabled={!baseURL || models.isPending} onClick={() => models.mutate()}>
                {models.isPending ? "正在获取…" : "获取模型列表"}
              </Button>
            </div>
            {models.data && (
              <Select
                aria-label="从列表里选一个模型"
                value={models.data.models.includes(model) ? model : ""}
                onChange={(e) => {
                  setModel(e.target.value);
                  edited();
                }}
              >
                <option value="" disabled>
                  从接口上的 {models.data.models.length} 个模型里选一个…
                </option>
                {models.data.models.map((m) => (
                  <option key={m} value={m}>
                    {m}
                  </option>
                ))}
              </Select>
            )}
            {models.isError && <Notice tone="warn">{errorMessage(models.error)}</Notice>}
          </div>
        </Field>
        <Field
          label="上下文长度"
          htmlFor="llm-context"
          hint="模型一次能读多少内容，看模型的说明（常写作 context length）。Ollama 的本地模型没调大 num_ctx 的话一般只有 8K 到 32K。长会议会按它分段写纪要，拿不准就选小一点的。"
        >
          <Select
            id="llm-context"
            className="max-w-xs"
            value={contextTokens}
            onChange={(e) => {
              setContextTokens(Number(e.target.value));
              edited();
            }}
          >
            {!CONTEXT_CHOICES.some(([n]) => n === contextTokens) && <option value={contextTokens}>{contextTokens} tokens</option>}
            {CONTEXT_CHOICES.map(([n, label]) => (
              <option key={n} value={n}>
                {label}
              </option>
            ))}
          </Select>
        </Field>

        {test.isSuccess && <Notice tone="ok">连接正常，模型有响应。记得点「保存」。</Notice>}
        {test.isError && <Notice tone="bad">{errorMessage(test.error)}</Notice>}
        {save.isSuccess && <Notice tone="ok">已保存。之后处理的会议会用这个模型写纪要。</Notice>}
        {save.isError && <Notice tone="bad">{errorMessage(save.error)}</Notice>}

        <div className="flex flex-wrap gap-2.5">
          <Button
            disabled={pending || !baseURL || !model}
            onClick={() => {
              save.reset();
              test.mutate();
            }}
          >
            {test.isPending ? "正在测试…" : "测试连接"}
          </Button>
          <Button type="submit" variant="primary" disabled={pending}>
            {save.isPending ? "正在保存…" : "保存"}
          </Button>
        </div>

        <p className="rounded-xl bg-canvas px-4 py-3 text-[13px] leading-relaxed text-ink-2">
          语音识别在你自己的服务器上完成，录音不会发给任何外部服务。只有转写出来的文字会发给上面填的大模型；用自己部署的模型，数据就完全不出内网。
        </p>
      </form>
    </Card>
  );
}

// 录音保留期能选的几档（天），0 表示一直保留
const RETENTION_CHOICES = [
  [30, "30 天"],
  [90, "90 天"],
  [180, "半年"],
  [365, "一年"],
  [730, "两年"],
  [0, "一直保留"],
] as const;

/** 录音保留多久：过了保留期只删录音文件，转写和纪要一直留着。 */
function RetentionCard() {
  const saved = useQuery({
    queryKey: ["settings", "retention"],
    queryFn: () => unwrap(api.GET("/api/settings/retention")),
  });
  const [days, setDays] = useState<number | null>(null);
  const save = useMutation({
    mutationFn: (audio_days: number) => unwrap(api.PUT("/api/settings/retention", { body: { audio_days } })),
    onSuccess: (data) => {
      queryClient.setQueryData(["settings", "retention"], data);
      setDays(null);
      // 已经过期的录音删掉以后，会议页上要跟着变
      void queryClient.invalidateQueries({ queryKey: ["meeting"] });
    },
  });

  if (saved.isPending) return <Spinner label="正在加载配置…" />;
  if (saved.isError) return <Notice tone="bad">{errorMessage(saved.error)}</Notice>;

  const current = saved.data.audio_days;
  const chosen = days ?? current;
  const known = RETENTION_CHOICES.some(([d]) => d === current);
  // 改短了：已经超过新期限的录音保存以后马上就会被删
  const shorter = chosen !== 0 && (current === 0 || chosen < current);

  return (
    <Card className="p-6">
      <form
        className="flex flex-col gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          save.mutate(chosen);
        }}
      >
        <div className="flex flex-col gap-1">
          <h2 className="text-lg font-bold">录音保留多久</h2>
          <p className="text-[13px] leading-relaxed text-muted">
            录音文件比较占地方，也是最敏感的数据。过了保留期只删录音文件，转写和纪要一直留着，照样能看、能搜；只是不能再回放、重新转写。
          </p>
        </div>
        <Field label="录音保留" htmlFor="retention-days" hint="从会议创建的时候算起。">
          <Select
            id="retention-days"
            className="max-w-xs"
            value={chosen}
            onChange={(e) => {
              setDays(Number(e.target.value));
              save.reset();
            }}
          >
            {!known && <option value={current}>{current} 天</option>}
            {RETENTION_CHOICES.map(([d, label]) => (
              <option key={d} value={d}>
                {label}
              </option>
            ))}
          </Select>
        </Field>
        {shorter && chosen !== current && (
          <Notice tone="warn">保存以后，已经超过 {chosen} 天的会议的录音会在一分钟内删除，删了不能恢复。</Notice>
        )}
        {save.isError && <Notice tone="bad">{errorMessage(save.error)}</Notice>}
        {save.isSuccess && <Notice tone="ok">已保存。</Notice>}
        <div>
          <Button type="submit" variant="primary" disabled={save.isPending || chosen === current}>
            保存
          </Button>
        </div>
      </form>
    </Card>
  );
}

import { useMutation, useQuery } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import type { FormEvent } from "react";
import { useParams } from "react-router";
import { AppLink } from "../components/AppLink";
import { api, errorMessage, queryClient, unwrap } from "../api/client";
import type { User } from "../api/client";
import { Button, Card, Field, Input, Notice, Select, Spinner } from "../components/ui";
import { MembersSection } from "./Members";

const SECTIONS = [
  { key: "account", label: "账号", adminOnly: false },
  { key: "members", label: "成员", adminOnly: true },
  { key: "llm", label: "大模型", adminOnly: true },
] as const;

type SectionKey = (typeof SECTIONS)[number]["key"];

/** 设置页。左边是分区：自己的账号人人都有，成员和大模型只有管理员能看到。 */
export function SettingsPage({ user }: { user: User }) {
  const params = useParams();
  const isAdmin = user.role === "admin";
  const visible = SECTIONS.filter((s) => isAdmin || !s.adminOnly);
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
        </nav>

        <div className="min-w-0 flex-1">
          {current === "account" && <AccountSection user={user} />}
          {current === "members" && <MembersSection me={user} />}
          {current === "llm" && <LLMSettingsCard />}
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
          <p className="mt-3 text-[13px] leading-relaxed text-muted">成员、大模型这些团队配置由管理员统一管理。</p>
        )}
      </Card>
      <ChangePasswordCard email={user.email} />
    </div>
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
          <p className="text-[13px] leading-relaxed text-muted">改完以后这台设备保持登录，其他设备上的登录会全部退出。忘了当前密码的话，请管理员帮你重置。</p>
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
        {change.isSuccess && <Notice tone="ok">密码已修改。其他设备上的登录已经退出。</Notice>}
        <div>
          <Button type="submit" variant="primary" disabled={change.isPending || mismatch}>
            {change.isPending ? "正在保存…" : "修改密码"}
          </Button>
        </div>
      </form>
    </Card>
  );
}

/** 大模型配置：全团队共用，只有管理员能看、能改。 */
function LLMSettingsCard() {
  const saved = useQuery({
    queryKey: ["settings", "llm"],
    queryFn: () => unwrap(api.GET("/api/settings/llm")),
  });

  const [baseURL, setBaseURL] = useState("");
  const [model, setModel] = useState("");
  const [apiKey, setAPIKey] = useState("");
  // 表单只在第一次拿到已保存的值时填充，之后不覆盖用户正在改的内容
  const [loaded, setLoaded] = useState(false);
  useEffect(() => {
    if (saved.data && !loaded) {
      setBaseURL(saved.data.base_url);
      setModel(saved.data.model);
      setLoaded(true);
    }
  }, [saved.data, loaded]);

  const body = { base_url: baseURL, model, api_key: apiKey || undefined };

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

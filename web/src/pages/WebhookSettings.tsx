import { useMutation, useQuery } from "@tanstack/react-query";
import { useState } from "react";
import type { FormEvent, ReactNode } from "react";
import { api, errorMessage, queryClient, unwrap } from "../api/client";
import type { components } from "../api/schema";
import { Button, Card, Chip, Field, Input, Notice, Select, Spinner } from "../components/ui";

type Webhook = components["schemas"]["Webhook"];
type Body = components["schemas"]["WebhookBody"];
export type Platform = Webhook["platform"];

export const PLATFORM_LABEL: Record<Platform, string> = { dingtalk: "钉钉", wecom: "企业微信", slack: "Slack" };

const URL_PLACEHOLDER: Record<Platform, string> = {
  dingtalk: "https://oapi.dingtalk.com/robot/send?access_token=…",
  wecom: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=…",
  slack: "https://hooks.slack.com/services/T…/B…/…",
};

const URL_HINT: Record<Platform, string> = {
  dingtalk: "在钉钉群的「设置 → 机器人 → 添加机器人 → 自定义」里添加，复制它的 Webhook 地址。",
  wecom: "在企业微信群的「群机器人 → 添加机器人」里添加，在机器人的资料页复制 Webhook 地址。",
  slack: "在 Slack 应用的「Incoming Webhooks」页面打开功能、选一个频道，复制生成的地址。",
};

function refresh() {
  void queryClient.invalidateQueries({ queryKey: ["settings", "webhooks"] });
  // 会议页上「发到群里」能选的群跟着变
  void queryClient.invalidateQueries({ queryKey: ["meeting-webhooks"] });
}

/** 只有这台电脑自己能打开的地址。消息里的链接用它拼的话，同事点了打不开。 */
function localOnly(url: string): boolean {
  try {
    const host = new URL(url).hostname;
    return host === "localhost" || host.endsWith(".localhost") || host === "127.0.0.1" || host === "0.0.0.0" || host === "[::1]";
  } catch {
    return false;
  }
}

/** 群机器人：钉钉、企业微信、Slack 群里的自定义机器人，纪要可以发到这些群里。只有管理员能看、能改。 */
export function WebhookSettingsSection() {
  const hooks = useQuery({
    queryKey: ["settings", "webhooks"],
    queryFn: () => unwrap(api.GET("/api/settings/webhooks")),
  });
  const [creating, setCreating] = useState(false);

  return (
    <div className="flex flex-col gap-5">
      <SiteCard />

      <Card className="flex flex-col gap-4 p-6">
        <div className="flex flex-col gap-1">
          <h2 className="text-lg font-bold">群机器人</h2>
          <p className="text-[13px] leading-relaxed text-muted">
            在钉钉、企业微信或 Slack 的群里加一个自定义机器人，把它的 Webhook 地址填到这里，纪要就能发到那个群：标题、时间、摘要、待办，和查看完整纪要的链接。
            能编辑会议的人可以在会议页面上点「发到群里」；打开了自动发送的群，纪要第一次生成后会自动收到（只发团队里所有人都能看的会议）。
          </p>
        </div>
        <p className="rounded-xl bg-canvas px-4 py-3 text-[13px] leading-relaxed text-ink-2">
          拿到 Webhook 地址的人就能往群里发消息，地址加密保存，不会再完整显示出来。钉钉机器人要选一种安全设置：选「加签」的话把密钥填到下面；选「自定义关键词」的话，加一个关键词「会议纪要」（zsmeet 发的消息都带着这四个字）；选「IP 地址」的话填 zsmeet 服务器的出口 IP。
        </p>
        {creating ? (
          <WebhookForm submitLabel="添加" onDone={() => setCreating(false)} mutate={(body) => unwrap(api.POST("/api/settings/webhooks", { body }))} />
        ) : (
          <div>
            <Button variant="primary" onClick={() => setCreating(true)}>
              添加群机器人
            </Button>
          </div>
        )}
      </Card>

      {hooks.isPending ? (
        <Spinner label="正在加载群机器人…" />
      ) : hooks.isError ? (
        <Notice tone="bad">{errorMessage(hooks.error)}</Notice>
      ) : (
        hooks.data.items.length > 0 && (
          <Card className="flex flex-col divide-y divide-line">
            {hooks.data.items.map((w) => (
              <WebhookRow key={w.id} hook={w} />
            ))}
          </Card>
        )
      )}
    </div>
  );
}

/** zsmeet 的访问地址：群里的消息用它链接回会议。和飞书设置里的是同一个；还没保存过时用以前存在飞书配置里的，都没有就不带链接。 */
function SiteCard() {
  const saved = useQuery({
    queryKey: ["settings", "site"],
    queryFn: () => unwrap(api.GET("/api/settings/site")),
  });
  const [draft, setDraft] = useState<string | null>(null);
  const save = useMutation({
    mutationFn: (url: string) => unwrap(api.PUT("/api/settings/site", { body: { url } })),
    onSuccess: (data) => {
      queryClient.setQueryData(["settings", "site"], data);
      // 和「设置 → 飞书」里的访问地址是同一个
      void queryClient.invalidateQueries({ queryKey: ["settings", "feishu"] });
      setDraft(null);
    },
  });

  if (saved.isPending) return <Spinner label="正在加载配置…" />;
  if (saved.isError) return <Notice tone="bad">{errorMessage(saved.error)}</Notice>;

  // 输入框里只放保存了的地址：没保存的不填进去，免得看着像已经在用了。现在打开 zsmeet 的地址只作为建议，点一下才填进去
  const value = draft ?? saved.data.url;
  const origin = window.location.origin;
  const suggest = !saved.data.url && !value.trim() && !localOnly(origin) ? origin : "";
  const hint = (): ReactNode => {
    if (localOnly(value)) {
      return <span className="text-warn">这个地址只有这台电脑自己能打开，同事点群里的链接会打不开。请改成大家都能打开的地址。</span>;
    }
    // 还没保存过：说清楚现在群里的链接用的是什么
    if (!saved.data.url && saved.data.feishu_url) {
      return <span className="text-warn">还没保存访问地址，现在群里消息的链接用的是飞书设置里填的 {saved.data.feishu_url}。</span>;
    }
    if (!saved.data.url) return <span className="text-warn">还没保存访问地址，群里的消息不带链接。填好以后点「保存」。</span>;
    return "群里的消息上「查看完整纪要」的链接用它拼，要填同事们都能打开的地址。";
  };

  return (
    <Card className="p-6">
      <form
        className="flex flex-col gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          save.mutate(value.trim());
        }}
      >
        <div className="flex flex-col gap-1">
          <h2 className="text-lg font-bold">zsmeet 的访问地址</h2>
          <p className="text-[13px] leading-relaxed text-muted">
            发到群里的消息带着回到会议的链接，同事点了就能看完整的纪要、听录音。这个地址和「设置 → 飞书」里的是同一个，在哪边改都行。
          </p>
        </div>
        <Field label="访问地址" htmlFor="site-url" hint={hint()}>
          <Input
            id="site-url"
            type="url"
            className="max-w-lg"
            maxLength={500}
            placeholder="https://meet.example.com"
            value={value}
            onChange={(e) => {
              setDraft(e.target.value);
              save.reset();
            }}
          />
        </Field>
        {suggest && (
          <div className="flex flex-wrap items-center gap-2.5 text-[13px] text-muted">
            <span>你现在是用 {suggest} 打开的 zsmeet，同事们也用这个地址的话：</span>
            <Button
              size="sm"
              onClick={() => {
                setDraft(suggest);
                save.reset();
              }}
            >
              填入这个地址
            </Button>
          </div>
        )}
        {save.isError && <Notice tone="bad">{errorMessage(save.error)}</Notice>}
        {save.isSuccess && <Notice tone="ok">已保存。</Notice>}
        <div>
          <Button type="submit" variant="primary" disabled={save.isPending || value.trim().replace(/\/+$/, "") === saved.data.url}>
            保存
          </Button>
        </div>
      </form>
    </Card>
  );
}

function WebhookRow({ hook: w }: { hook: Webhook }) {
  const [editing, setEditing] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const remove = useMutation({
    mutationFn: () => unwrap(api.DELETE("/api/settings/webhooks/{id}", { params: { path: { id: w.id } } })),
    onSuccess: refresh,
  });
  const test = useMutation({
    mutationFn: () => unwrap(api.POST("/api/settings/webhooks/{id}/test", { params: { path: { id: w.id } } })),
  });

  if (editing) {
    return (
      <div className="px-6 py-5">
        <WebhookForm
          initial={w}
          submitLabel="保存"
          onDone={() => {
            setEditing(false);
            test.reset();
          }}
          mutate={(body) => unwrap(api.PUT("/api/settings/webhooks/{id}", { params: { path: { id: w.id } }, body }))}
        />
      </div>
    );
  }
  return (
    <div className="flex flex-col gap-2.5 px-6 py-5">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <span className="flex min-w-0 flex-wrap items-center gap-2.5">
          <Chip tone="info">{PLATFORM_LABEL[w.platform]}</Chip>
          <span className="font-semibold">{w.name}</span>
          {w.platform === "dingtalk" && w.secret_set && <Chip tone="mute">加签</Chip>}
          {w.auto_push && <Chip tone="ok">自动发送</Chip>}
        </span>
        {!confirming && (
          <span className="flex gap-2">
            <Button size="sm" disabled={test.isPending} onClick={() => test.mutate()}>
              {test.isPending ? "正在发送…" : "发一条测试消息"}
            </Button>
            <Button size="sm" onClick={() => setEditing(true)}>
              修改
            </Button>
            <Button size="sm" variant="ghost" onClick={() => setConfirming(true)}>
              删除
            </Button>
          </span>
        )}
      </div>
      <span className="break-all font-mono text-xs text-muted">{w.url_masked}</span>
      {test.isSuccess && <Notice tone="ok">测试消息发出去了，去群里看看。</Notice>}
      {test.isError && <Notice tone="bad">{errorMessage(test.error)}</Notice>}
      {confirming && (
        <div className="flex flex-wrap items-center justify-between gap-3 rounded-lg bg-bad-tint/60 px-4 py-3 text-sm">
          <span className="text-ink-2">删除以后不能再往这个群发纪要；已经发到群里的消息不会撤回。</span>
          <span className="flex gap-2">
            <Button size="sm" variant="danger" disabled={remove.isPending} onClick={() => remove.mutate()}>
              确定删除
            </Button>
            <Button size="sm" onClick={() => setConfirming(false)}>
              取消
            </Button>
          </span>
        </div>
      )}
      {remove.isError && <Notice tone="bad">{errorMessage(remove.error)}</Notice>}
    </div>
  );
}

function WebhookForm({
  initial,
  submitLabel,
  onDone,
  mutate,
}: {
  initial?: Webhook;
  submitLabel: string;
  onDone: () => void;
  mutate: (body: Body) => Promise<Webhook>;
}) {
  const [platform, setPlatform] = useState<Platform>(initial?.platform ?? "dingtalk");
  const [name, setName] = useState(initial?.name ?? "");
  const [url, setURL] = useState("");
  const [secret, setSecret] = useState("");
  const [clearSecret, setClearSecret] = useState(false);
  const [autoPush, setAutoPush] = useState(initial?.auto_push ?? false);

  // 改的时候地址留空表示沿用；换了平台就得重新填
  const urlKept = !!initial && platform === initial.platform;
  const secretKept = !!initial && initial.platform === "dingtalk" && initial.secret_set && platform === "dingtalk";

  const save = useMutation({
    mutationFn: () =>
      mutate({
        platform,
        name,
        url: url.trim() || undefined,
        secret: platform === "dingtalk" && !clearSecret ? secret.trim() || undefined : undefined,
        clear_secret: platform === "dingtalk" && clearSecret ? true : undefined,
        auto_push: autoPush,
      }),
    onSuccess: () => {
      refresh();
      onDone();
    },
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    save.mutate();
  };
  const id = initial?.id ?? "new";

  return (
    <form onSubmit={submit} className="flex flex-col gap-4">
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-[180px_1fr]">
        <Field label="平台" htmlFor={`hook-platform-${id}`}>
          <Select
            id={`hook-platform-${id}`}
            value={platform}
            onChange={(e) => {
              setPlatform(e.target.value as Platform);
              save.reset();
            }}
          >
            <option value="dingtalk">钉钉</option>
            <option value="wecom">企业微信</option>
            <option value="slack">Slack</option>
          </Select>
        </Field>
        <Field label="名字" htmlFor={`hook-name-${id}`} hint="写群的名字就行，发送时从这里选">
          <Input id={`hook-name-${id}`} maxLength={50} required value={name} placeholder="比如：产品组" onChange={(e) => setName(e.target.value)} />
        </Field>
      </div>
      <Field label="Webhook 地址" htmlFor={`hook-url-${id}`} hint={urlKept ? `已保存：${initial.url_masked}。留空表示不修改。` : URL_HINT[platform]}>
        <Input
          id={`hook-url-${id}`}
          type="password"
          autoComplete="off"
          spellCheck={false}
          maxLength={1000}
          required={!urlKept}
          placeholder={urlKept ? "已保存，留空表示不修改" : URL_PLACEHOLDER[platform]}
          value={url}
          onChange={(e) => {
            setURL(e.target.value);
            save.reset();
          }}
        />
      </Field>
      {platform === "dingtalk" && (
        <Field
          label="加签的密钥（可选）"
          htmlFor={`hook-secret-${id}`}
          hint={
            secretKept
              ? "已保存。留空表示不修改。"
              : "机器人安全设置选了「加签」的话，填它下面以 SEC 开头的那一串。选的是关键词或 IP 地址的话不用填。"
          }
        >
          <Input
            id={`hook-secret-${id}`}
            type="password"
            autoComplete="off"
            spellCheck={false}
            maxLength={200}
            disabled={clearSecret}
            placeholder={secretKept ? "已保存，留空表示不修改" : "SEC…"}
            value={secret}
            onChange={(e) => {
              setSecret(e.target.value);
              save.reset();
            }}
          />
        </Field>
      )}
      {secretKept && (
        <Check id={`hook-clear-${id}`} label="不再加签（清掉保存的密钥）" checked={clearSecret} onChange={setClearSecret} />
      )}
      <Check
        id={`hook-auto-${id}`}
        label="纪要生成后自动发到这个群"
        hint="每场会议第一次生成纪要时自动发一次，只发团队里所有人都能看的会议；重新生成的纪要不会自动发。"
        checked={autoPush}
        onChange={setAutoPush}
      />
      {save.isError && <Notice tone="bad">{errorMessage(save.error)}</Notice>}
      <span className="flex gap-2">
        <Button type="submit" variant="primary" disabled={save.isPending}>
          {save.isPending ? "正在保存…" : submitLabel}
        </Button>
        <Button onClick={onDone}>取消</Button>
      </span>
    </form>
  );
}

function Check({ id, label, hint, checked, onChange }: { id: string; label: string; hint?: string; checked: boolean; onChange: (checked: boolean) => void }) {
  return (
    <div className="flex flex-col gap-1">
      <label className="flex cursor-pointer items-start gap-2.5 text-sm font-medium">
        <input
          id={id}
          type="checkbox"
          className="mt-0.5 size-4 shrink-0 cursor-pointer accent-brand"
          checked={checked}
          aria-describedby={hint ? `${id}-hint` : undefined}
          onChange={(e) => onChange(e.target.checked)}
        />
        {label}
      </label>
      {hint && (
        <div id={`${id}-hint`} className="pl-6.5 text-xs leading-relaxed text-muted">
          {hint}
        </div>
      )}
    </div>
  );
}

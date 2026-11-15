import { useMutation, useQuery } from "@tanstack/react-query";
import { useState } from "react";
import type { FormEvent, ReactNode } from "react";
import { api, errorMessage, queryClient, unwrap } from "../api/client";
import type { components } from "../api/schema";
import { Button, Card, Chip, Field, Input, Notice, Select, Spinner } from "../components/ui";
import { isEn } from "../i18n/lang";
import { tr, useLang } from "../i18n/react";
import { localOnly } from "../lib/site";

type Webhook = components["schemas"]["Webhook"];
type Body = components["schemas"]["WebhookBody"];
export type Platform = Webhook["platform"];

export const PLATFORM_LABEL: Record<Platform, string> = {
  get dingtalk() {
    return tr("钉钉", "DingTalk");
  },
  get wecom() {
    return tr("企业微信", "WeCom");
  },
  slack: "Slack",
};

const URL_PLACEHOLDER: Record<Platform, string> = {
  dingtalk: "https://oapi.dingtalk.com/robot/send?access_token=…",
  wecom: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=…",
  slack: "https://hooks.slack.com/services/T…/B…/…",
};

function urlHint(platform: Platform): string {
  switch (platform) {
    case "dingtalk":
      return tr(
        "在钉钉群的「设置 → 机器人 → 添加机器人 → 自定义」里添加，复制它的 Webhook 地址。",
        "Add a custom bot in DingTalk under Group Settings → Group Assistant → Add Robot → Custom, then copy its Webhook URL.",
      );
    case "wecom":
      return tr(
        "在企业微信群的「群机器人 → 添加机器人」里添加，在机器人的资料页复制 Webhook 地址。",
        "Add a bot in WeCom under Group Bots → Add Bot, then copy the Webhook URL from its profile.",
      );
    case "slack":
      return tr(
        "在 Slack 应用的「Incoming Webhooks」页面打开功能、选一个频道，复制生成的地址。",
        "Enable Incoming Webhooks in your Slack app, choose a channel, and copy the generated Webhook URL.",
      );
  }
}

function refresh() {
  void queryClient.invalidateQueries({ queryKey: ["settings", "webhooks"] });
  // 会议页上「发到群里」能选的群跟着变
  void queryClient.invalidateQueries({ queryKey: ["meeting-webhooks"] });
}

/** 群机器人：钉钉、企业微信、Slack 群里的自定义机器人，纪要可以发到这些群里。只有管理员能看、能改。 */
export function WebhookSettingsSection() {
  useLang();
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
          <h2 className="text-lg font-bold">{tr("群机器人", "Group Webhooks")}</h2>
          <p className="text-[13px] leading-relaxed text-muted">
            {tr(
              "在钉钉、企业微信或 Slack 的群里加一个自定义机器人，把它的 Webhook 地址填到这里，纪要就能发到那个群：标题、时间、摘要、待办，和查看完整纪要的链接。能编辑会议的人可以在会议页面上点「发到群里」；打开了自动发送的群，纪要第一次生成后会自动收到（只发团队里所有人都能看的会议）。",
              "Add a custom webhook bot in DingTalk, WeCom, or Slack and paste its URL here to send meeting minutes (title, time, summary, action items, and a link to full minutes). Meeting editors can click \"Send to group\" manually, or enable auto-send for team-visible meetings upon first minutes generation.",
            )}
          </p>
        </div>
        <p className="rounded-xl bg-canvas px-4 py-3 text-[13px] leading-relaxed text-ink-2">
          {tr(
            "拿到 Webhook 地址的人就能往群里发消息，地址加密保存，不会再完整显示出来。钉钉机器人要选一种安全设置：选「加签」的话把密钥填到下面；选「自定义关键词」的话，加一个关键词「会议纪要」（zsmeet 发的消息都带着这四个字）；选「IP 地址」的话填 zsmeet 服务器的出口 IP。",
            "Anyone with a Webhook URL can post to that group — URLs are encrypted and masked after saving. For DingTalk security settings: enter the SEC… secret below if using \"Signature\", add keyword \"会议纪要\" if using \"Custom Keywords\", or allow your zsmeet server's outbound IP.",
          )}
        </p>
        {creating ? (
          <WebhookForm
            submitLabel={tr("添加", "Add")}
            onDone={() => setCreating(false)}
            mutate={(body) => unwrap(api.POST("/api/settings/webhooks", { body }))}
          />
        ) : (
          <div>
            <Button variant="primary" onClick={() => setCreating(true)}>
              {tr("添加群机器人", "Add webhook")}
            </Button>
          </div>
        )}
      </Card>

      {hooks.isPending ? (
        <Spinner label={tr("正在加载群机器人…", "Loading webhooks…")} />
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
  useLang();
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

  if (saved.isPending) return <Spinner label={tr("正在加载配置…", "Loading settings…")} />;
  if (saved.isError) return <Notice tone="bad">{errorMessage(saved.error)}</Notice>;

  // 输入框里只放保存了的地址：没保存的不填进去，免得看着像已经在用了。现在打开 zsmeet 的地址只作为建议，点一下才填进去
  const value = draft ?? saved.data.url;
  const origin = window.location.origin;
  const suggest = !saved.data.url && !value.trim() && !localOnly(origin) ? origin : "";
  const hint = (): ReactNode => {
    if (localOnly(value)) {
      return (
        <span className="text-warn">
          {tr(
            "这个地址只有这台电脑自己能打开，同事点群里的链接会打不开。请改成大家都能打开的地址。",
            "This URL is only reachable from this machine — teammates won't be able to open links shared in chat. Enter a URL accessible to your team.",
          )}
        </span>
      );
    }
    // 还没保存过：说清楚现在群里的链接用的是什么
    if (!saved.data.url && saved.data.feishu_url) {
      return (
        <span className="text-warn">
          {isEn()
            ? `No site URL saved yet; group message links currently use ${saved.data.feishu_url} from Feishu settings.`
            : `还没保存访问地址，现在群里消息的链接用的是飞书设置里填的 ${saved.data.feishu_url}。`}
        </span>
      );
    }
    if (!saved.data.url) {
      return (
        <span className="text-warn">
          {tr("还没保存访问地址，群里的消息不带链接。填好以后点「保存」。", "No site URL saved yet — group messages won't include links. Enter one and click \"Save\".")}
        </span>
      );
    }
    return tr(
      "群里的消息上「查看完整纪要」的链接用它拼，要填同事们都能打开的地址。",
      "Used to build \"View full minutes\" links in group messages — enter an address reachable by your team.",
    );
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
          <h2 className="text-lg font-bold">{tr("zsmeet 的访问地址", "zsmeet Site URL")}</h2>
          <p className="text-[13px] leading-relaxed text-muted">
            {tr(
              "发到群里的消息带着回到会议的链接，同事点了就能看完整的纪要、听录音。这个地址和「设置 → 飞书」里的是同一个，在哪边改都行。",
              "Messages posted to chat include a link back to the meeting so teammates can read full minutes and play audio. Shared with Settings → Feishu.",
            )}
          </p>
        </div>
        <Field label={tr("访问地址", "Site URL")} htmlFor="site-url" hint={hint()}>
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
            <span>
              {isEn()
                ? `You opened zsmeet at ${suggest} — if your teammates use this address too:`
                : `你现在是用 ${suggest} 打开的 zsmeet，同事们也用这个地址的话：`}
            </span>
            <Button
              size="sm"
              onClick={() => {
                setDraft(suggest);
                save.reset();
              }}
            >
              {tr("填入这个地址", "Use this URL")}
            </Button>
          </div>
        )}
        {save.isError && <Notice tone="bad">{errorMessage(save.error)}</Notice>}
        {save.isSuccess && <Notice tone="ok">{tr("已保存。", "Saved.")}</Notice>}
        <div>
          <Button type="submit" variant="primary" disabled={save.isPending || value.trim().replace(/\/+$/, "") === saved.data.url}>
            {tr("保存", "Save")}
          </Button>
        </div>
      </form>
    </Card>
  );
}

function WebhookRow({ hook: w }: { hook: Webhook }) {
  useLang();
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
          submitLabel={tr("保存", "Save")}
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
          {w.platform === "dingtalk" && w.secret_set && <Chip tone="mute">{tr("加签", "Signed")}</Chip>}
          {w.auto_push && <Chip tone="ok">{tr("自动发送", "Auto-send")}</Chip>}
        </span>
        {!confirming && (
          <span className="flex gap-2">
            <Button size="sm" disabled={test.isPending} onClick={() => test.mutate()}>
              {test.isPending ? tr("正在发送…", "Sending…") : tr("发一条测试消息", "Send test message")}
            </Button>
            <Button size="sm" onClick={() => setEditing(true)}>
              {tr("修改", "Edit")}
            </Button>
            <Button size="sm" variant="ghost" onClick={() => setConfirming(true)}>
              {tr("删除", "Delete")}
            </Button>
          </span>
        )}
      </div>
      <span className="break-all font-mono text-xs text-muted">{w.url_masked}</span>
      {test.isSuccess && <Notice tone="ok">{tr("测试消息发出去了，去群里看看。", "Test message sent — check your chat group.")}</Notice>}
      {test.isError && <Notice tone="bad">{errorMessage(test.error)}</Notice>}
      {confirming && (
        <div className="flex flex-wrap items-center justify-between gap-3 rounded-lg bg-bad-tint/60 px-4 py-3 text-sm">
          <span className="text-ink-2">
            {tr("删除以后不能再往这个群发纪要；已经发到群里的消息不会撤回。", "Deleting stops future messages to this group; previously sent messages are not recalled.")}
          </span>
          <span className="flex gap-2">
            <Button size="sm" variant="danger" disabled={remove.isPending} onClick={() => remove.mutate()}>
              {tr("确定删除", "Delete")}
            </Button>
            <Button size="sm" onClick={() => setConfirming(false)}>
              {tr("取消", "Cancel")}
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
  useLang();
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
        <Field label={tr("平台", "Platform")} htmlFor={`hook-platform-${id}`}>
          <Select
            id={`hook-platform-${id}`}
            value={platform}
            onChange={(e) => {
              setPlatform(e.target.value as Platform);
              save.reset();
            }}
          >
            <option value="dingtalk">{tr("钉钉", "DingTalk")}</option>
            <option value="wecom">{tr("企业微信", "WeCom")}</option>
            <option value="slack">Slack</option>
          </Select>
        </Field>
        <Field
          label={tr("名字", "Name")}
          htmlFor={`hook-name-${id}`}
          hint={tr("写群的名字就行，发送时从这里选", "Group name shown when choosing where to send")}
        >
          <Input
            id={`hook-name-${id}`}
            maxLength={50}
            required
            value={name}
            placeholder={tr("比如：产品组", "e.g. Product Team")}
            onChange={(e) => setName(e.target.value)}
          />
        </Field>
      </div>
      <Field
        label={tr("Webhook 地址", "Webhook URL")}
        htmlFor={`hook-url-${id}`}
        hint={
          urlKept
            ? isEn()
              ? `Saved: ${initial.url_masked}. Leave blank to keep unchanged.`
              : `已保存：${initial.url_masked}。留空表示不修改。`
            : urlHint(platform)
        }
      >
        <Input
          id={`hook-url-${id}`}
          type="password"
          autoComplete="off"
          spellCheck={false}
          maxLength={1000}
          required={!urlKept}
          placeholder={urlKept ? tr("已保存，留空表示不修改", "Saved — leave blank to keep") : URL_PLACEHOLDER[platform]}
          value={url}
          onChange={(e) => {
            setURL(e.target.value);
            save.reset();
          }}
        />
      </Field>
      {platform === "dingtalk" && (
        <Field
          label={tr("加签的密钥（可选）", "Signing secret (optional)")}
          htmlFor={`hook-secret-${id}`}
          hint={
            secretKept
              ? tr("已保存。留空表示不修改。", "Saved. Leave blank to keep unchanged.")
              : tr(
                  "机器人安全设置选了「加签」的话，填它下面以 SEC 开头的那一串。选的是关键词或 IP 地址的话不用填。",
                  "If \"Signature\" security is enabled on the DingTalk bot, enter the SEC… secret. Leave blank if using keywords or IP allowlist.",
                )
          }
        >
          <Input
            id={`hook-secret-${id}`}
            type="password"
            autoComplete="off"
            spellCheck={false}
            maxLength={200}
            disabled={clearSecret}
            placeholder={secretKept ? tr("已保存，留空表示不修改", "Saved — leave blank to keep") : "SEC…"}
            value={secret}
            onChange={(e) => {
              setSecret(e.target.value);
              save.reset();
            }}
          />
        </Field>
      )}
      {secretKept && (
        <Check
          id={`hook-clear-${id}`}
          label={tr("不再加签（清掉保存的密钥）", "Disable signing (clear saved secret)")}
          checked={clearSecret}
          onChange={setClearSecret}
        />
      )}
      <Check
        id={`hook-auto-${id}`}
        label={tr("纪要生成后自动发到这个群", "Automatically send minutes to this group")}
        hint={tr(
          "每场会议第一次生成纪要时自动发一次，只发团队里所有人都能看的会议；重新生成的纪要不会自动发。",
          "Sent once when minutes are first generated for team-visible meetings; regenerated minutes are not sent automatically.",
        )}
        checked={autoPush}
        onChange={setAutoPush}
      />
      {save.isError && <Notice tone="bad">{errorMessage(save.error)}</Notice>}
      <span className="flex gap-2">
        <Button type="submit" variant="primary" disabled={save.isPending}>
          {save.isPending ? tr("正在保存…", "Saving…") : submitLabel}
        </Button>
        <Button onClick={onDone}>{tr("取消", "Cancel")}</Button>
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

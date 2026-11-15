import { useMutation, useQuery } from "@tanstack/react-query";
import { useRef, useState } from "react";
import type { FormEvent, ReactNode } from "react";
import { api, errorMessage, queryClient, unwrap } from "../api/client";
import type { components } from "../api/schema";
import { AppLink } from "../components/AppLink";
import { Button, buttonClass, Card, Chip, Field, Input, Notice, Select, Spinner } from "../components/ui";
import type { Tone } from "../components/ui";
import { isEn } from "../i18n/lang";
import { tr, useLang } from "../i18n/react";
import { dateTime } from "../lib/format";
import { localOnly } from "../lib/site";

type Saved = components["schemas"]["FeishuSettings"];
type Chat = components["schemas"]["FeishuChat"];
type RunStatus = components["schemas"]["FeishuStatusOutputBody"];
type Import = components["schemas"]["FeishuImport"];

/** 表单里的值：和保存的配置一一对应，再加一个新填的 App Secret（已保存的 Secret 拿不回来）。 */
type Form = Omit<Saved, "app_secret_set" | "console_url" | "login_redirect_url"> & { app_secret: string };

function formFrom(s: Saved): Form {
  return {
    domain: s.domain,
    app_id: s.app_id,
    app_secret: "",
    // 还没填过的话，先用现在打开 zsmeet 的地址，多数时候就是对的
    site_url: s.site_url || window.location.origin,
    chat_id: s.chat_id,
    chat_name: s.chat_name,
    auto_push: s.auto_push,
    with_doc: s.with_doc,
    doc_folder: s.doc_folder,
    link_share: s.link_share,
    import_recordings: s.import_recordings,
    login: s.login,
    login_auto_join: s.login_auto_join,
  };
}

// 飞书登录完跳回 zsmeet 的路径，和后端的 service.FeishuCallbackPath 一致
const CALLBACK_PATH = "/api/auth/feishu/callback";
// 用飞书登录要向用户申请的权限，和后端的 feishu.LoginScopes 一致
const LOGIN_SCOPES = ["contact:user.email:readonly", "contact:user.employee:readonly"];

/** 要填进飞书应用「重定向 URL」的地址：访问地址加回调路径。访问地址还没填好时为空。 */
function redirectURL(siteURL: string): string {
  const site = siteURL.trim().replace(/\/+$/, "");
  try {
    const u = new URL(site);
    if (u.protocol !== "http:" && u.protocol !== "https:") return "";
  } catch {
    return "";
  }
  return site + CALLBACK_PATH;
}

/**
 * 飞书：全团队共用一个飞书企业自建应用。它的机器人把纪要发到群里，可以同时导入成飞书文档；
 * 还能自动导入组织者是成员的飞书会议的云录制。只有管理员能看、能改。
 */
export function FeishuSettingsSection() {
  useLang();
  const saved = useQuery({
    queryKey: ["settings", "feishu"],
    queryFn: () => unwrap(api.GET("/api/settings/feishu")),
  });
  if (saved.isPending) return <Spinner label={tr("正在加载配置…", "Loading settings…")} />;
  if (!saved.data) return <Notice tone="bad">{errorMessage(saved.error)}</Notice>;
  return <FeishuForm saved={saved.data} />;
}

/** 三块配置共用一个表单、一个「保存」。表单在拿到已保存的配置时填好，之后不被刷新覆盖。 */
function FeishuForm({ saved }: { saved: Saved }) {
  useLang();
  const [form, setForm] = useState(() => formFrom(saved));
  // 清空 App ID 再保存就是不用飞书了，配置全部清掉：保存前先确认一次
  const [confirmOff, setConfirmOff] = useState(false);

  const appID = form.app_id.trim();
  // 已经保存好、能调用飞书的应用
  const ready = saved.app_id !== "" && saved.app_secret_set;
  // 表单里还是保存着的那个应用（平台和 App ID 都没改）
  const sameApp = form.domain === saved.domain && appID === saved.app_id;
  // App Secret 留空时沿用已保存的；换了 App ID 就得重新填
  const secretKept = saved.app_secret_set && appID === saved.app_id;
  const turningOff = saved.app_id !== "" && appID === "";

  // 用表单里现在的凭证测试，不用先保存
  const test = useMutation({
    mutationFn: () =>
      unwrap(api.POST("/api/settings/feishu/test", { body: { domain: form.domain, app_id: appID, app_secret: form.app_secret || undefined } })),
  });
  // 已保存的应用的机器人在哪些群里
  const savedChats = useQuery({
    queryKey: ["feishu", "chats"],
    queryFn: () => unwrap(api.GET("/api/feishu/chats")),
    enabled: ready,
  });
  // 能选的群：最近一次测试连接读到的；没测过的话，用已保存的应用去读
  const listFromSaved = ready && sameApp && !test.data;
  const chats: Chat[] | undefined = test.data?.chats ?? (listFromSaved ? savedChats.data?.items : undefined);
  const chatName = (id: string) => chats?.find((c) => c.id === id)?.name;
  // 选着的群不在列表里（还没读到列表，或者机器人已经不在那个群了）时，也要在下拉框里显示出来
  const chatMissing = form.chat_id !== "" && chatName(form.chat_id) === undefined;

  const save = useMutation({
    mutationFn: () =>
      unwrap(
        api.PUT("/api/settings/feishu", {
          body: {
            ...form,
            app_id: appID,
            app_secret: form.app_secret || undefined,
            // 群名以飞书那边现在的为准（群可能改过名）
            chat_name: form.chat_id ? (chatName(form.chat_id) ?? form.chat_name) : "",
          },
        }),
      ),
    onSuccess: (data) => {
      queryClient.setQueryData(["settings", "feishu"], data);
      setForm(formFrom(data));
      setConfirmOff(false);
      // 首页和会议页要知道飞书配好了没有；群列表、运行情况都跟着新的配置走。
      // 刚停用飞书时只标记成过期、不马上重取：这时再去取群列表只会得到「还没有配置飞书」
      void queryClient.invalidateQueries({ queryKey: ["status"] });
      const refetchType = data.app_id ? "active" : "none";
      void queryClient.invalidateQueries({ queryKey: ["feishu"], refetchType });
      // 访问地址和「设置 → 群机器人」里的是同一个
      void queryClient.invalidateQueries({ queryKey: ["settings", "site"] });
    },
  });

  // 改了任何一项，上一次的保存结果就不再算数
  const edit = (patch: Partial<Form>) => {
    setForm((f) => ({ ...f, ...patch }));
    save.reset();
  };
  // 改了平台、App ID 或 App Secret：测试结果（连同读到的群）是原来那个应用的，不能再用
  const editApp = (patch: Partial<Form>) => {
    edit(patch);
    test.reset();
    setConfirmOff(false);
  };
  const pickChat = (id: string) => {
    // 不设默认的群，就不能自动发
    edit({ chat_id: id, chat_name: id ? (chatName(id) ?? form.chat_name) : "", auto_push: id ? form.auto_push : false });
  };
  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (turningOff && !confirmOff) {
      setConfirmOff(true);
      return;
    }
    save.mutate();
  };

  const initial = formFrom(saved);
  const dirty = (Object.keys(initial) as (keyof Form)[]).some((k) => form[k] !== initial[k]);
  const pending = test.isPending || save.isPending;
  // 没填 App ID 时下面两块不能改（保存时反正会被清掉）。第一次配置时说一声为什么
  const needApp = !appID && !saved.app_id && (
    <p className="text-[13px] text-muted">{tr("先在上面填好 App ID 和 App Secret。", "Enter App ID and App Secret above first.")}</p>
  );

  const chatHint = (): ReactNode => {
    if (!chats) {
      if (listFromSaved && savedChats.isError) {
        return (
          <span className="text-warn">
            {isEn() ? `Could not load bot groups: ${errorMessage(savedChats.error)}` : `读不到机器人所在的群：${errorMessage(savedChats.error)}`}
          </span>
        );
      }
      return listFromSaved
        ? tr("正在读取机器人所在的群…", "Loading groups the bot is in…")
        : tr("点上面的「测试连接」以后，可以从机器人所在的群里选。", "Click \"Test connection\" above to list groups the bot has joined.");
    }
    if (chatMissing) {
      return (
        <span className="text-warn">
          {isEn()
            ? `The bot is no longer in "${form.chat_name || form.chat_id}" and cannot post there. Add it back or choose another group.`
            : `机器人已经不在「${form.chat_name || form.chat_id}」里了，发不进去。把它拉回这个群，或者换一个群。`}
        </span>
      );
    }
    if (chats.length === 0) {
      return tr(
        "机器人还没有加入任何群。在飞书里把它拉进要接收纪要的群，再点上面的「测试连接」。",
        "The bot hasn't joined any groups yet. Add it to a Feishu group first, then click \"Test connection\" above.",
      );
    }
    return tr(
      "列出的是机器人所在的群。把它拉进新的群以后，点上面的「测试连接」刷新。",
      "Shows groups the bot has joined. After adding it to a new group, click \"Test connection\" above to refresh.",
    );
  };

  return (
    <form onSubmit={submit} className="flex flex-col gap-5">
      <Card className="flex flex-col gap-4 p-6">
        <div className="flex flex-col gap-1">
          <h2 className="text-lg font-bold">{tr("连接飞书应用", "Connect Feishu App")}</h2>
          <p className="text-[13px] leading-relaxed text-muted">
            {tr(
              "用一个飞书企业自建应用，把纪要发到飞书群、导入成飞书文档，还能自动导入飞书会议的录制。全团队共用这一个应用。",
              "Use a custom Feishu/Lark enterprise app to post minutes to groups, import full minutes as Feishu Docs, and auto-import Feishu meeting cloud recordings. Shared across the team.",
            )}
          </p>
        </div>

        <Guide />

        <div className="grid grid-cols-1 gap-4 sm:grid-cols-[200px_1fr]">
          <Field label={tr("平台", "Platform")} htmlFor="feishu-domain">
            <Select id="feishu-domain" value={form.domain} onChange={(e) => editApp({ domain: e.target.value as Form["domain"] })}>
              <option value="feishu">{tr("飞书", "Feishu")}</option>
              <option value="lark">{tr("Lark（飞书国际版）", "Lark (International)")}</option>
            </Select>
          </Field>
          <Field
            label="App ID"
            htmlFor="feishu-app-id"
            hint={
              turningOff ? (
                <span className="text-warn">{tr("清空后保存，就是不用飞书了，下面的配置会全部清掉。", "Saving with an empty App ID disables Feishu and clears all settings below.")}</span>
              ) : (
                tr("开发者后台「凭证与基础信息」里以 cli_ 开头的那一串", "Starts with cli_ under Credentials & Basic Info in the Developer Console")
              )
            }
          >
            <Input
              id="feishu-app-id"
              maxLength={100}
              autoComplete="off"
              spellCheck={false}
              placeholder="cli_…"
              value={form.app_id}
              onChange={(e) => editApp({ app_id: e.target.value })}
            />
          </Field>
        </div>
        <Field
          label="App Secret"
          htmlFor="feishu-secret"
          hint={
            secretKept
              ? tr("加密保存，不会再显示出来", "Encrypted and never shown again")
              : saved.app_secret_set && appID
                ? tr("换了 App ID，要填这个应用的 App Secret", "App ID changed — enter the App Secret for this app")
                : tr("和 App ID 在同一个页面。加密保存，不会再显示出来", "Found on the same page as App ID. Encrypted and never shown again")
          }
        >
          <Input
            id="feishu-secret"
            type="password"
            maxLength={200}
            autoComplete="off"
            placeholder={secretKept ? tr("已保存，留空表示不修改", "Saved — leave blank to keep") : ""}
            value={form.app_secret}
            onChange={(e) => editApp({ app_secret: e.target.value })}
          />
        </Field>

        {test.data && (
          <Notice tone="ok">
            {isEn() ? (
              <>
                Connected: {test.data.bot_name ? `bot "${test.data.bot_name}" ` : "bot "}
                {test.data.chats.length > 0
                  ? `is in ${test.data.chats.length} group${test.data.chats.length === 1 ? "" : "s"}. `
                  : "hasn't joined any groups yet — add it to a group and test again. "}
                {!ready || !sameApp || form.app_secret ? 'Remember to click "Save" at the bottom.' : ""}
              </>
            ) : (
              <>
                连上了：{test.data.bot_name ? `机器人「${test.data.bot_name}」` : "机器人"}
                {test.data.chats.length > 0
                  ? `在 ${test.data.chats.length} 个群里。`
                  : "还没有加入任何群，把它拉进要接收纪要的群以后再测一次。"}
                {!ready || !sameApp || form.app_secret ? "记得点最下面的「保存」。" : ""}
              </>
            )}
          </Notice>
        )}
        {test.isError && <Notice tone="bad">{errorMessage(test.error)}</Notice>}

        <div className="flex flex-wrap gap-2.5">
          <Button disabled={pending || !appID} onClick={() => test.mutate()}>
            {test.isPending ? tr("正在测试…", "Testing…") : tr("测试连接", "Test connection")}
          </Button>
          {saved.console_url && sameApp && (
            <a href={saved.console_url} target="_blank" rel="noreferrer" className={buttonClass("ghost")}>
              {tr("打开开发者后台", "Open Developer Console")}
            </a>
          )}
        </div>
      </Card>

      <Card className="flex flex-col gap-4 p-6">
        <div className="flex flex-col gap-1">
          <h2 className="text-lg font-bold">{tr("发到群里", "Send to Group")}</h2>
          <p className="text-[13px] leading-relaxed text-muted">
            {tr(
              "纪要生成以后，机器人把摘要和待办发到飞书群里，卡片上有回到 zsmeet 的链接。会议页面上也能手动发。",
              "When minutes are ready, the bot posts a card with summary, action items, and a link back to zsmeet. Can also be sent manually from any meeting page.",
            )}
          </p>
        </div>
        {needApp}
        <fieldset disabled={!appID} className="flex min-w-0 flex-col gap-4 disabled:opacity-60">
          <Field
            label={tr("zsmeet 的访问地址", "zsmeet Site URL")}
            htmlFor="feishu-site"
            hint={
              localOnly(form.site_url) ? (
                <span className="text-warn">
                  {tr(
                    "这个地址只有这台电脑自己能打开，同事点卡片上的链接会打不开。请改成大家都能打开的地址。",
                    "This URL is only reachable from this machine — teammates won't be able to open links on cards. Enter an address accessible to your team.",
                  )}
                </span>
              ) : (
                tr(
                  "卡片和飞书文档里的链接、飞书登录完跳回来的地址都用它，要填同事们都能打开的地址。和「设置 → 群机器人」里的访问地址是同一个",
                  "Used for links in cards/docs and Feishu sign-in callbacks — must be reachable by your team. Shared with Settings → Webhooks",
                )
              )
            }
          >
            <Input
              id="feishu-site"
              type="url"
              maxLength={500}
              required
              placeholder="https://meet.example.com"
              value={form.site_url}
              onChange={(e) => edit({ site_url: e.target.value })}
            />
          </Field>
          <Field label={tr("默认的群", "Default group")} htmlFor="feishu-chat" hint={chatHint()}>
            <Select id="feishu-chat" value={form.chat_id} onChange={(e) => pickChat(e.target.value)}>
              <option value="">{tr("不设默认的群", "No default group")}</option>
              {chatMissing && (
                <option value={form.chat_id}>
                  {`${form.chat_name || form.chat_id}${chats ? tr("（机器人不在这个群里了）", " (bot no longer in group)") : ""}`}
                </option>
              )}
              {chats?.map((c) => (
                <option key={c.id} value={c.id}>
                  {c.name}
                </option>
              ))}
            </Select>
          </Field>
          <Check
            id="feishu-auto-push"
            label={tr("纪要生成后自动发到这个群", "Automatically post minutes to this group")}
            hint={
              form.chat_id
                ? tr("每场会议第一次生成纪要时自动发一次；重新生成的纪要不会自动发。", "Posted once when minutes are first generated; regenerated minutes are not auto-posted.")
                : tr("先选一个默认的群。", "Select a default group first.")
            }
            checked={form.auto_push}
            disabled={!form.chat_id}
            onChange={(v) => edit({ auto_push: v })}
          />

          <div className="flex flex-col gap-4 border-t border-line pt-4">
            <Check
              id="feishu-with-doc"
              label={tr("同时导入成飞书文档", "Also import as Feishu Doc")}
              hint={tr(
                "卡片上多一个「飞书文档」按钮，打开是完整的纪要。会议页面上手动发时，默认也勾上。",
                "Adds a \"Feishu Doc\" button to the card with the full minutes. Also checked by default when sharing manually.",
              )}
              checked={form.with_doc}
              onChange={(v) => edit({ with_doc: v })}
            />
            <Field
              label={tr("文档放在哪个文件夹（可选）", "Target folder for docs (optional)")}
              htmlFor="feishu-folder"
              hint={tr(
                "留空时放在应用自己的云空间，只能通过 zsmeet 发出的链接打开。想放进自己的文件夹：先把那个文件夹共享给一个有这个机器人的群，给「可编辑」权限，再把文件夹的链接粘贴到这里。文档按月份放在子文件夹里。",
                "Leave blank to store in the app's own Drive space. To use your own folder: share the folder with a group containing this bot with \"Can edit\" permission, then paste the folder URL here. Docs are organized into monthly subfolders.",
              )}
            >
              <Input
                id="feishu-folder"
                maxLength={500}
                autoComplete="off"
                spellCheck={false}
                placeholder={form.domain === "lark" ? "https://xxx.larksuite.com/drive/folder/…" : "https://xxx.feishu.cn/drive/folder/…"}
                value={form.doc_folder}
                onChange={(e) => edit({ doc_folder: e.target.value })}
              />
            </Field>
            <Check
              id="feishu-link-share"
              label={tr("公司里拿到链接的人都能看文档", "Anyone in the organization with the link can view docs")}
              hint={tr("不勾的话，只有发到的那个群里的人能看。", "If unchecked, only members of the target group can view.")}
              checked={form.link_share}
              onChange={(v) => edit({ link_share: v })}
            />
          </div>
        </fieldset>
      </Card>

      <Card className="flex flex-col gap-4 p-6">
        <div className="flex flex-col gap-1">
          <h2 className="text-lg font-bold">{tr("自动导入飞书会议的录制", "Auto-import Feishu Cloud Recordings")}</h2>
          <p className="text-[13px] leading-relaxed text-muted">
            {tr(
              "飞书会议结束后，把云录制拿过来转写、写纪要。zsmeet 主动和飞书保持连接来接收消息，服务器不需要公网地址。",
              "Automatically fetches cloud recordings after Feishu meetings end for transcription and minutes. Uses an outbound persistent connection so your server doesn't need a public IP.",
            )}
          </p>
        </div>
        {needApp}
        <fieldset disabled={!appID} className="min-w-0 disabled:opacity-60">
          <Check
            id="feishu-import"
            label={tr("自动导入飞书会议的云录制", "Auto-import Feishu cloud recordings")}
            hint={tr(
              "只导入组织者是 zsmeet 成员的会议（按邮箱对应），导入后记在组织者名下，自动转写、写纪要。其他人组织的会议直接忽略，什么都不存。",
              "Imports only meetings organized by zsmeet members (matched by email) under their account; meetings organized by others are ignored.",
            )}
            checked={form.import_recordings}
            onChange={(v) => edit({ import_recordings: v })}
          />
        </fieldset>
        {ready && <ImportStatus on={saved.import_recordings} />}
      </Card>

      <Card className="flex flex-col gap-4 p-6">
        <div className="flex flex-col gap-1">
          <h2 className="text-lg font-bold">{tr("用飞书登录", "Sign in with Feishu")}</h2>
          <p className="text-[13px] leading-relaxed text-muted">
            {tr(
              "登录页上多一个「用飞书登录」：用飞书扫码，或者在飞书网页版已经登录时点一下就行。按飞书里的企业邮箱对应到 zsmeet 的成员，第一次对应上以后记住这个飞书账号；成员也可以在「设置 → 账号」里自己关联。",
              "Adds \"Sign in with Feishu\" on the sign-in page. Matches members by verified corporate email on first login, or members can link manually under Settings → Account.",
            )}
          </p>
        </div>
        {needApp}
        <fieldset disabled={!appID} className="flex min-w-0 flex-col gap-4 disabled:opacity-60">
          <Check
            id="feishu-login"
            label={tr("允许用飞书登录", "Enable Sign in with Feishu")}
            hint={tr("原来的邮箱和密码照样能用。停用的成员用飞书也登录不了。", "Email/password sign-in remains available. Disabled members cannot sign in via Feishu.")}
            checked={form.login}
            onChange={(v) => edit({ login: v })}
          />
          <div role="radiogroup" aria-label={tr("飞书账号还不是成员时", "When Feishu user is not yet a member")} className="flex flex-col gap-2 pl-6.5">
            <span className="text-xs font-medium text-ink-2">{tr("飞书账号还不是成员时", "When Feishu user is not yet a member")}</span>
            <Radio
              name="feishu-login-join"
              label={tr("不让登录，请管理员先邀请（推荐）", "Require an admin invite first (recommended)")}
              hint={tr(
                "登录页上会提示他找管理员邀请。邀请发到他的飞书企业邮箱的话，他用飞书登录就直接加入了，角色按邀请里的。",
                "Uninvited users are asked to contact an admin. If an invite exists for their corporate email, signing in with Feishu accepts it automatically.",
              )}
              checked={!form.login_auto_join}
              disabled={!form.login}
              onChange={() => edit({ login_auto_join: false })}
            />
            <Radio
              name="feishu-login-join"
              label={tr("自动创建成员账号", "Automatically create a member account")}
              hint={tr(
                "公司里能用这个飞书应用的人都能直接进来，角色是成员，看得到团队里所有人都能看的会议。账号的邮箱用他的企业邮箱，没有时用通讯录里的邮箱。",
                "Anyone in your organization authorized to use this Feishu app can join automatically as a Member and view team-visible meetings.",
              )}
              checked={form.login_auto_join}
              disabled={!form.login}
              onChange={() => edit({ login_auto_join: true })}
            />
          </div>
          {form.login && <LoginGuide siteURL={form.site_url} />}
        </fieldset>
      </Card>

      <div className="flex flex-col gap-3">
        {save.isError && <Notice tone="bad">{errorMessage(save.error)}</Notice>}
        {save.isSuccess && (
          <Notice tone="ok">
            {saved.app_id ? tr("已保存。", "Saved.") : tr("已停用飞书，飞书的配置都清掉了。", "Feishu integration disabled and settings cleared.")}
          </Notice>
        )}
        {turningOff && confirmOff ? (
          <div className="flex flex-col gap-2.5 rounded-xl bg-warn-tint/60 px-4 py-3 text-sm">
            <span className="leading-relaxed text-ink-2">
              {tr(
                "App ID 清空了。保存以后就不用飞书了：飞书的配置全部清掉，不再自动发到群里，也不再自动导入会议录制。确定吗？",
                "App ID is empty. Saving will disable Feishu, clear all Feishu settings, and stop auto-posting and recording imports. Continue?",
              )}
            </span>
            <span className="flex flex-wrap gap-2">
              <Button type="submit" size="sm" variant="danger" disabled={save.isPending}>
                {tr("确定，不用飞书了", "Disable Feishu")}
              </Button>
              <Button size="sm" onClick={() => setConfirmOff(false)}>
                {tr("取消", "Cancel")}
              </Button>
            </span>
          </div>
        ) : (
          <div className="flex flex-wrap items-center gap-3">
            {/* 从来没配置过、也没填 App ID 时，没有可保存的 */}
            <Button type="submit" variant="primary" disabled={pending || (!appID && !saved.app_id)}>
              {save.isPending ? tr("正在保存…", "Saving…") : tr("保存", "Save")}
            </Button>
            {dirty && !save.isPending && <span className="text-[13px] text-muted">{tr("有改动还没保存", "Unsaved changes")}</span>}
          </div>
        )}
      </div>
    </form>
  );
}

/** 勾选框加一行说明。说明不放进 label：读屏时先念名字，说明作为补充。 */
export function Check({
  id,
  label,
  hint,
  checked,
  disabled,
  onChange,
}: {
  id: string;
  label: string;
  hint?: ReactNode;
  checked: boolean;
  disabled?: boolean;
  onChange: (checked: boolean) => void;
}) {
  return (
    <div className="flex flex-col gap-1">
      <label className="flex cursor-pointer items-start gap-2.5 text-sm font-medium has-disabled:cursor-not-allowed has-disabled:text-muted">
        <input
          id={id}
          type="checkbox"
          className="mt-0.5 size-4 shrink-0 cursor-pointer accent-brand disabled:cursor-not-allowed"
          checked={checked}
          disabled={disabled}
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

function ExternalLink({ href, children }: { href: string; children: ReactNode }) {
  return (
    <a href={href} target="_blank" rel="noreferrer" className="font-medium text-brand underline hover:text-brand-hover">
      {children}
    </a>
  );
}

/** 单选项加一行说明。 */
function Radio({
  name,
  label,
  hint,
  checked,
  disabled,
  onChange,
}: {
  name: string;
  label: string;
  hint: string;
  checked: boolean;
  disabled?: boolean;
  onChange: () => void;
}) {
  return (
    <label className="flex cursor-pointer items-start gap-2.5 text-sm has-disabled:cursor-not-allowed has-disabled:text-muted">
      <input
        type="radio"
        name={name}
        className="mt-0.5 size-4 shrink-0 cursor-pointer accent-brand disabled:cursor-not-allowed"
        checked={checked}
        disabled={disabled}
        onChange={onChange}
      />
      <span className="flex flex-col gap-0.5">
        <span className="font-medium">{label}</span>
        <span className="text-xs leading-relaxed text-muted">{hint}</span>
      </span>
    </label>
  );
}

/** 打开飞书登录以后，管理员还要在飞书开发者后台做的几件事。 */
function LoginGuide({ siteURL }: { siteURL: string }) {
  useLang();
  const url = redirectURL(siteURL);
  return (
    <div className="flex flex-col gap-3 rounded-xl bg-canvas px-4 py-3 text-[13px] leading-relaxed text-ink-2">
      <b className="font-semibold text-ink">
        {tr("还要在飞书开发者后台做三件事，然后发布新版本：", "Complete these 3 steps in the Feishu Developer Console, then publish a new app version:")}
      </b>
      <ol className="flex list-decimal flex-col gap-3 pl-5 marker:text-muted">
        <li className="flex flex-col gap-1.5">
          <span>
            {tr(
              "在「安全设置 → 重定向 URL」里添加下面这个地址（和上面的访问地址一起变）：",
              "Under Security Settings → Redirect URLs, add the URL below (updates with the Site URL above):",
            )}
          </span>
          {url ? (
            <CopyField key={url} value={url} label={tr("重定向 URL", "Redirect URL")} />
          ) : (
            <span className="text-warn">{tr("先在上面填好 zsmeet 的访问地址。", "Enter the zsmeet Site URL above first.")}</span>
          )}
          {localOnly(siteURL) && (
            <span className="text-warn">
              {tr("这个地址只有这台电脑自己能打开，别的同事用飞书登录完会回不来。", "This URL is only reachable on this machine — teammates won't be redirected back after signing in.")}
            </span>
          )}
        </li>
        <li className="flex flex-col gap-1.5">
          <span>
            {tr(
              "在「权限管理」里开通这两个权限（没开通的话，同事在飞书的授权页上会看到报错）：",
              "Under Permissions & Scopes, enable both scopes below (otherwise teammates will see an error on Feishu's consent page):",
            )}
          </span>
          <Scopes names={LOGIN_SCOPES} />
          <span className="text-xs leading-relaxed text-muted">
            {isEn() ? (
              <>
                <Code>contact:user.employee:readonly</Code> reads verified corporate emails used to match existing members and accept invites.{" "}
                <Code>contact:user.email:readonly</Code> reads directory emails (unverified) and is never used to sign into existing accounts.
              </>
            ) : (
              <>
                <Code>contact:user.employee:readonly</Code> 用来读企业邮箱（公司发的飞书邮箱，飞书验证过），zsmeet 只按它自动对应成员、接受发到这个邮箱的邀请。
                <Code>contact:user.email:readonly</Code> 读的是通讯录里的邮箱，它是飞书管理员填的、没有经过本人验证，不会拿来登录已有的账号。
              </>
            )}
          </span>
        </li>
        <li>
          {tr(
            "在「版本管理与发布」里把应用的可用范围设成要用 zsmeet 的同事（不在范围里的人用飞书登录不了），创建版本并发布。",
            "Under Version Management & Release, set availability to the teammates who will use zsmeet, then create and publish a new version.",
          )}
        </li>
      </ol>
      <span className="text-xs text-muted">
        {tr(
          "两个权限都要开通并发布新版本，否则同事在飞书的授权页上会看到 20027 报错，用飞书登录和关联飞书都用不了。公司没用飞书邮箱时拿不到企业邮箱：成员先用邮箱和密码登录（新同事先用邀请链接创建账号），再在「设置 → 账号」里输入密码、点「关联飞书」，以后就能用飞书登录了。",
          "Both scopes must be enabled and published, otherwise Feishu shows error 20027. If your organization doesn't use Feishu Mail (no verified corporate email), members can sign in with email/password first and link Feishu under Settings → Account.",
        )}
      </span>
    </div>
  );
}

/** 一段要原样复制的文字，带复制按钮。浏览器不让自动复制时（没用 HTTPS），选中文字让用户自己复制。 */
function CopyField({ value, label }: { value: string; label: string }) {
  useLang();
  const input = useRef<HTMLInputElement>(null);
  const [state, setState] = useState<"idle" | "copied" | "manual">("idle");
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(value);
      setState("copied");
    } catch {
      input.current?.select();
      setState("manual");
    }
  };
  return (
    <div className="flex flex-col gap-1">
      <div className="flex gap-2">
        <Input
          ref={input}
          readOnly
          value={value}
          aria-label={label}
          onFocus={(e) => e.currentTarget.select()}
          className="bg-white font-mono text-xs"
        />
        <Button size="sm" onClick={copy}>
          {state === "copied" ? tr("已复制", "Copied") : tr("复制", "Copy")}
        </Button>
      </div>
      {state === "manual" && (
        <span className="text-xs text-warn">
          {tr("浏览器不允许自动复制。地址已经选中，按 Ctrl+C（Mac 上是 ⌘C）复制。", "Automatic copy blocked by browser. The URL is selected — press Ctrl+C (⌘C on Mac) to copy.")}
        </span>
      )}
    </div>
  );
}

/** 权限名、事件名这类要原样复制的字。点一下整个选中，方便复制。 */
function Code({ children }: { children: string }) {
  return <code className="select-all rounded-md bg-white px-1.5 py-0.5 font-mono text-xs text-ink wrap-anywhere">{children}</code>;
}

function Scopes({ names }: { names: string[] }) {
  return (
    <div className="flex flex-wrap gap-1.5">
      {names.map((name) => (
        <Code key={name}>{name}</Code>
      ))}
    </div>
  );
}

/** 在飞书开发者后台创建应用的步骤。默认收起。 */
function Guide() {
  useLang();
  return (
    <details className="rounded-xl bg-canvas px-4 py-3 text-[13px] leading-relaxed text-ink-2">
      <summary className="cursor-pointer font-semibold text-ink marker:text-muted">
        {tr("怎么在飞书开发者后台创建应用", "How to create a custom app in the Feishu Developer Console")}
      </summary>
      <ol className="mt-3 flex list-decimal flex-col gap-3 pl-5 marker:text-muted">
        <li>
          {isEn() ? (
            <>
              Create a &ldquo;Custom Enterprise App&rdquo; in the developer console at{" "}
              <ExternalLink href="https://open.feishu.cn/app">open.feishu.cn</ExternalLink> (or{" "}
              <ExternalLink href="https://open.larksuite.com/app">open.larksuite.com</ExternalLink> for Lark). Copy App ID and App Secret from
              &ldquo;Credentials &amp; Basic Info&rdquo; into the fields below.
            </>
          ) : (
            <>
              在 <ExternalLink href="https://open.feishu.cn/app">open.feishu.cn</ExternalLink>（Lark 是{" "}
              <ExternalLink href="https://open.larksuite.com/app">open.larksuite.com</ExternalLink>
              ）的开发者后台创建一个「企业自建应用」，应用的名字就是机器人在群里显示的名字。App ID 和 App Secret 在应用的「凭证与基础信息」里，填到下面。
            </>
          )}
        </li>
        <li>{tr("在「添加应用能力」里添加「机器人」。", "Under Add Features, enable \"Bot\".")}</li>
        <li>
          {tr("在「权限管理」里开通权限，用到哪项功能就开哪几个：", "Under Permissions & Scopes, enable scopes for the features you need:")}
          <dl className="mt-2 flex flex-col gap-2.5">
            <div className="flex flex-col gap-1">
              <dt className="font-medium text-ink">{tr("发到群里", "Send to group")}</dt>
              <dd>
                <Scopes names={["im:message:send_as_bot", "im:chat:readonly"]} />
              </dd>
            </div>
            <div className="flex flex-col gap-1">
              <dt className="font-medium text-ink">{tr("导入成飞书文档", "Import as Feishu Doc")}</dt>
              <dd>
                <Scopes
                  names={[
                    "docs:document.media:upload",
                    "docs:document:import",
                    "docs:permission.member:create",
                    "docs:permission.setting:write_only",
                    "drive:drive.metadata:readonly",
                    "space:folder:create",
                  ]}
                />
              </dd>
            </div>
            <div className="flex flex-col gap-1">
              <dt className="font-medium text-ink">{tr("自动导入会议录制", "Auto-import cloud recordings")}</dt>
              <dd className="flex flex-col gap-1.5">
                <Scopes names={["vc:meeting.all_meeting:readonly", "vc:record:readonly", "minutes:minutes.media:export", "contact:user.email:readonly"]} />
                <span>{tr("还要把「妙记」和「通讯录」的数据权限（可访问的数据范围）设为全员。", "Also set the data scope for Minutes and Contacts to All Employees.")}</span>
              </dd>
            </div>
          </dl>
        </li>
        <li>
          {isEn() ? (
            <>
              For auto-importing recordings: enable &ldquo;Auto-import Feishu cloud recordings&rdquo; below and save first, wait until it shows
              &ldquo;Connected&rdquo;, then under Events &amp; Callbacks select &ldquo;Receive events via persistent connection&rdquo; and subscribe to{" "}
              <Code>vc.meeting.all_meeting_ended_v1</Code>.
            </>
          ) : (
            <>
              要自动导入会议录制的话：先在下面「自动导入飞书会议的录制」里勾上并保存，等显示「已连上」；再到「事件与回调」里选择「使用长连接接收事件」，添加事件{" "}
              <Code>vc.meeting.all_meeting_ended_v1</Code>。飞书要看到 zsmeet 已经连上，才让保存长连接这种方式。
            </>
          )}
        </li>
        <li>
          {tr(
            "在「版本管理与发布」里创建版本并发布，等企业管理员审核通过。以后改了权限或事件，都要再发布一个版本才生效。",
            "Under Version Management & Release, create and publish a version (re-publish whenever permissions or events change).",
          )}
        </li>
        <li>
          {tr(
            "把机器人拉进每个要接收纪要的群（群设置 → 群机器人 → 添加机器人），再回到这里点「测试连接」。",
            "Add the bot to each group chat that should receive minutes (Group Settings → Bots → Add Bot), then click \"Test connection\" below.",
          )}
        </li>
      </ol>
    </details>
  );
}

/** 和飞书的长连接现在怎么样。没打开自动导入时，不管实际状态，都算「未开启」。 */
function linkChip(state: RunStatus["state"], on: boolean): { tone: Tone; text: string } {
  if (!on) return { tone: "mute", text: tr("未开启", "Off") };
  switch (state) {
    case "connected":
      return { tone: "ok", text: tr("已连上", "Connected") };
    case "error":
      return { tone: "bad", text: tr("连接出错", "Connection error") };
    default:
      // off：刚打开，后台还没来得及去连（每十秒看一次配置）
      return { tone: "info", text: tr("正在连接", "Connecting") };
  }
}

function importChip(status: Import["status"]): { tone: Tone; text: string } {
  switch (status) {
    case "waiting":
      return { tone: "info", text: tr("等飞书生成录制", "Waiting for recording") };
    case "imported":
      return { tone: "ok", text: tr("已导入", "Imported") };
    case "skipped":
      return { tone: "mute", text: tr("没有云录制", "No cloud recording") };
    case "failed":
      return { tone: "bad", text: tr("失败", "Failed") };
  }
}

/** 自动导入的运行情况：和飞书的连接、最近一次收到的会议、最近导入的会议。on 是已保存的「自动导入」开关。 */
function ImportStatus({ on }: { on: boolean }) {
  useLang();
  const status = useQuery({
    queryKey: ["feishu", "status"],
    queryFn: () => unwrap(api.GET("/api/settings/feishu/status")),
    // 打开了自动导入时每 5 秒刷新一次，连上没有、导入到哪一步都跟得上
    refetchInterval: on ? 5000 : false,
  });
  const data = status.data;
  if (!data) {
    if (status.isPending) return on ? <span className="text-[13px] text-muted">{tr("正在读取运行情况…", "Loading connection status…")}</span> : null;
    return (
      <Notice tone="warn">
        {isEn() ? `Could not load connection status: ${errorMessage(status.error)}` : `读不到运行情况：${errorMessage(status.error)}`}
      </Notice>
    );
  }
  // 没打开、也没导入过：没什么可看的
  if (!on && data.imports.length === 0) return null;
  return (
    <div className="flex flex-col gap-3">
      <LinkState status={data} on={on} />
      {status.isError && (
        <Notice tone="warn">
          {isEn()
            ? `Could not refresh status (${errorMessage(status.error)}); showing cached state.`
            : `运行情况没能刷新（${errorMessage(status.error)}），下面显示的可能不是最新的。`}
        </Notice>
      )}
      {data.imports.length > 0 && <ImportList items={data.imports} />}
    </div>
  );
}

function LinkState({ status: s, on }: { status: RunStatus; on: boolean }) {
  useLang();
  const chip = linkChip(s.state, on);
  let note: ReactNode = null;
  if (on) {
    switch (s.state) {
      case "off":
        note = tr("刚打开，十几秒内开始连接。", "Just enabled — connecting within ~10 seconds.");
        break;
      case "connecting":
        note = s.detail;
        break;
      case "error":
        note = (
          <span className="text-bad">
            {s.detail
              ? isEn()
                ? `${s.detail} (retrying in 1 min)`
                : `${s.detail}（过一分钟自动重连）`
              : tr("过一分钟自动重连", "Retrying in 1 minute")}
          </span>
        );
        break;
      case "connected":
        // 收到过会议结束的消息，说明事件也订阅好了
        if (!s.last_event_at) {
          note = tr(
            "还没收到过会议结束的消息。还没订阅事件的话，现在可以去开发者后台订阅了（见上面创建应用的第 4 步）。",
            "No meeting-ended events received yet. If you haven't subscribed to the event yet, you can add it in the Developer Console now (see Step 4 above).",
          );
        }
        break;
    }
  }
  return (
    <div className="flex flex-col gap-2 rounded-xl bg-canvas px-4 py-3 text-[13px] leading-relaxed text-ink-2">
      <div className="flex flex-wrap items-center gap-x-2.5 gap-y-1">
        <span className="text-muted">{tr("和飞书的连接", "Feishu connection")}</span>
        <Chip tone={chip.tone}>{chip.text}</Chip>
        {note && <span>{note}</span>}
      </div>
      {on && s.last_event_at && (
        <div>
          <span className="text-muted">{isEn() ? `Last event (${dateTime(s.last_event_at)}): ` : `最近一次 ${dateTime(s.last_event_at)}：`}</span>
          {s.last_event}
        </div>
      )}
    </div>
  );
}

function ImportList({ items }: { items: Import[] }) {
  useLang();
  return (
    <div className="flex flex-col gap-1">
      <b className="text-sm font-semibold">{tr("最近导入的会议", "Recent Imports")}</b>
      <ul className="flex flex-col">
        {items.map((imp) => {
          const chip = importChip(imp.status);
          return (
            <li key={imp.feishu_meeting_id} className="flex items-start justify-between gap-4 border-b border-line py-2.5 last:border-b-0">
              <div className="flex min-w-0 flex-1 flex-col gap-0.5">
                {imp.status === "imported" && imp.meeting_id ? (
                  <AppLink to={`/meetings/${imp.meeting_id}`} className="truncate text-sm font-medium text-brand hover:text-brand-hover hover:underline">
                    {imp.topic}
                  </AppLink>
                ) : (
                  <span className="truncate text-sm font-medium">{imp.topic}</span>
                )}
                <span className="text-xs text-muted">
                  {imp.owner_name} · {dateTime(imp.created_at)}
                </span>
                {imp.status === "failed" && imp.detail && <span className="text-xs leading-relaxed text-bad">{imp.detail}</span>}
              </div>
              <Chip tone={chip.tone}>{chip.text}</Chip>
            </li>
          );
        })}
      </ul>
    </div>
  );
}

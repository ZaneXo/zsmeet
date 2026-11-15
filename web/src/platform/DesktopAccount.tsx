import { useMutation } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { useSearchParams } from "react-router";
import { errorMessage } from "../api/client";
import type { User } from "../api/client";
import { Button, Card, Field, Notice } from "../components/ui";
import { LanguageSelect, tr, useLang } from "../i18n/react";
import { logout, openServerPage, switchServer, useDesktopState, useSigningOut } from "./desktop";
import { UpdateSection } from "./DesktopUpdate";
import { useRecorder } from "./recorder";
import { signOutNote } from "./recorderView";

/**
 * 桌面版的「账号」：连的哪台服务器、退出登录、切换服务器；最下面是版本和「检查更新」。
 * 改密码、关联飞书这些要跳来跳去的事在网页里做（在 App 里改密码会让这台电脑的登录一起失效）。
 */
export function DesktopAccountSection({ user }: { user: User }) {
  useLang();
  const state = useDesktopState();
  const { state: recorder } = useRecorder();
  const [switching, setSwitching] = useState(false);
  const [leaving, setLeaving] = useState(false);
  const [params, setParams] = useSearchParams();
  const asked = params.get("logout") === "1";
  useEffect(() => {
    if (!asked) return;
    setSwitching(false);
    setLeaving(true);
    setParams({}, { replace: true });
  }, [asked, setParams]);
  const signOut = useMutation({ mutationFn: logout });
  const change = useMutation({ mutationFn: switchServer });
  const open = useMutation({ mutationFn: (path: string) => openServerPage(path) });
  const busy = useSigningOut() || signOut.isPending || change.isPending;
  const failure = signOut.error ?? change.error ?? open.error;
  const leaveNote = signOutNote(recorder, tr("退出登录", "signing out"));
  const switchNote = signOutNote(recorder, tr("切换服务器", "switching servers"));

  return (
    <div className="flex flex-col gap-5">
      <Card className="flex flex-col gap-1 p-6">
        <h2 className="text-lg font-bold">{tr("账号", "Account")}</h2>
        <dl className="mt-2 grid grid-cols-[72px_1fr] gap-y-2 text-sm">
          <dt className="text-muted">{tr("姓名", "Name")}</dt>
          <dd>{user.name}</dd>
          <dt className="text-muted">{tr("邮箱", "Email")}</dt>
          <dd>{user.email}</dd>
          <dt className="text-muted">{tr("角色", "Role")}</dt>
          <dd>{user.role === "admin" ? tr("管理员", "Admin") : tr("成员", "Member")}</dd>
          <dt className="text-muted">{tr("服务器", "Server")}</dt>
          <dd className="break-all">{state?.server}</dd>
        </dl>
        <div className="mt-4 flex flex-wrap gap-2.5">
          {!leaving && (
            <Button
              onClick={() => {
                setSwitching(false);
                if (leaveNote) setLeaving(true);
                else signOut.mutate();
              }}
              disabled={busy}
            >
              {signOut.isPending ? tr("正在退出…", "Signing out…") : tr("退出登录", "Sign out")}
            </Button>
          )}
          {!switching && (
            <Button
              onClick={() => {
                setLeaving(false);
                setSwitching(true);
              }}
              disabled={busy}
            >
              {tr("切换服务器", "Switch server")}
            </Button>
          )}
        </div>
        {leaving && (
          <div className="mt-3 flex flex-col gap-2.5 rounded-xl bg-warn-tint/60 px-4 py-3 text-sm">
            <span className="text-ink-2">{leaveNote || tr("确定退出登录吗？", "Sign out of zsmeet?")}</span>
            <span className="flex gap-2">
              <Button size="sm" variant="primary" disabled={busy} onClick={() => signOut.mutate()}>
                {signOut.isPending ? tr("正在退出…", "Signing out…") : tr("退出登录", "Sign out")}
              </Button>
              <Button size="sm" onClick={() => setLeaving(false)}>
                {tr("取消", "Cancel")}
              </Button>
            </span>
          </div>
        )}
        {switching && (
          <div className="mt-3 flex flex-col gap-2.5 rounded-xl bg-warn-tint/60 px-4 py-3 text-sm">
            <span className="text-ink-2">
              {tr(
                "切换服务器会先退出这台服务器上的登录，这台电脑上保存的离线内容也会删掉。",
                "Switching servers signs you out of this server and clears cached offline content on this Mac. ",
              )}
              {switchNote}
            </span>
            <span className="flex gap-2">
              <Button size="sm" variant="primary" disabled={busy} onClick={() => change.mutate()}>
                {change.isPending ? tr("正在切换…", "Switching…") : tr("退出并切换", "Sign out & switch")}
              </Button>
              <Button size="sm" onClick={() => setSwitching(false)}>
                {tr("取消", "Cancel")}
              </Button>
            </span>
          </div>
        )}
        {failure && (
          <div className="mt-3">
            <Notice tone="bad">{errorMessage(failure)}</Notice>
          </div>
        )}
      </Card>

      <Card className="flex flex-col gap-3 p-6">
        <h2 className="text-lg font-bold">{tr("界面语言", "Language")}</h2>
        <Field
          label={tr("显示语言", "Display language")}
          htmlFor="desktop-ui-lang"
          hint={tr("切换后立即生效，菜单栏提示也会同步更新。", "Takes effect immediately, including the macOS menu bar tray.")}
        >
          <LanguageSelect id="desktop-ui-lang" className="w-64 max-w-full" />
        </Field>
      </Card>

      <Card className="flex flex-col gap-3 p-6">
        <h2 className="text-lg font-bold">{tr("密码和飞书", "Password & Feishu")}</h2>
        <p className="text-sm leading-relaxed text-ink-2">
          {tr("改密码、关联飞书账号请在网页里操作。改了密码以后，这台电脑要重新登录。", "Change your password or link Feishu in the web app. After changing your password, sign in again on this Mac.")}
        </p>
        <div>
          <Button onClick={() => open.mutate("/settings")}>{tr("在网页里打开", "Open in browser")}</Button>
        </div>
      </Card>

      <UpdateSection />
    </div>
  );
}

/** 管理员的团队设置（成员、大模型、飞书……）先只在网页里改。 */
export function OpenAdminSettings() {
  useLang();
  const open = useMutation({ mutationFn: () => openServerPage("/settings/members") });
  return (
    <div className="flex flex-col gap-1">
      <button
        type="button"
        onClick={() => open.mutate()}
        className="flex min-h-10 cursor-pointer items-center whitespace-nowrap rounded-lg px-3 text-left text-sm text-ink-2 hover:bg-subtle"
      >
        {tr("在网页里打开 ↗", "Open in browser ↗")}
      </button>
      {open.isError && <p className="px-3 text-xs text-bad">{errorMessage(open.error)}</p>}
    </div>
  );
}

import { useMutation } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { useSearchParams } from "react-router";
import { errorMessage } from "../api/client";
import type { User } from "../api/client";
import { Button, Card, Notice } from "../components/ui";
import { logout, openServerPage, switchServer, useDesktopState } from "./desktop";
import { useRecorder } from "./recorder";
import { signOutNote } from "./recorderView";

/**
 * 桌面版的「账号」：连的哪台服务器、退出登录、切换服务器。
 * 改密码、关联飞书这些要跳来跳去的事在网页里做（在 App 里改密码会让这台电脑的登录一起失效）。
 */
export function DesktopAccountSection({ user }: { user: User }) {
  const state = useDesktopState();
  const { state: recorder } = useRecorder();
  const [switching, setSwitching] = useState(false);
  // 正在录音、有录音没传完时，退出登录前先说清楚会怎样。导航栏上点「退出登录」也会带着 ?logout=1 来到这里
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
  const failure = signOut.error ?? change.error ?? open.error;
  const leaveNote = signOutNote(recorder, "退出登录");
  const switchNote = signOutNote(recorder, "切换服务器");

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
          <dt className="text-muted">服务器</dt>
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
              disabled={signOut.isPending}
            >
              {signOut.isPending ? "正在退出…" : "退出登录"}
            </Button>
          )}
          {!switching && (
            <Button
              onClick={() => {
                setLeaving(false);
                setSwitching(true);
              }}
            >
              切换服务器
            </Button>
          )}
        </div>
        {leaving && (
          <div className="mt-3 flex flex-col gap-2.5 rounded-xl bg-warn-tint/60 px-4 py-3 text-sm">
            <span className="text-ink-2">{leaveNote || "确定退出登录吗？"}</span>
            <span className="flex gap-2">
              <Button size="sm" variant="primary" disabled={signOut.isPending} onClick={() => signOut.mutate()}>
                {signOut.isPending ? "正在退出…" : "退出登录"}
              </Button>
              <Button size="sm" onClick={() => setLeaving(false)}>
                取消
              </Button>
            </span>
          </div>
        )}
        {switching && (
          <div className="mt-3 flex flex-col gap-2.5 rounded-xl bg-warn-tint/60 px-4 py-3 text-sm">
            <span className="text-ink-2">
              切换服务器会先退出这台服务器上的登录，这台电脑上保存的离线内容也会删掉。{switchNote}
            </span>
            <span className="flex gap-2">
              <Button size="sm" variant="primary" disabled={change.isPending} onClick={() => change.mutate()}>
                退出并切换
              </Button>
              <Button size="sm" onClick={() => setSwitching(false)}>
                取消
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
        <h2 className="text-lg font-bold">密码和飞书</h2>
        <p className="text-sm leading-relaxed text-ink-2">改密码、关联飞书账号请在网页里操作。改了密码以后，这台电脑要重新登录。</p>
        <div>
          <Button onClick={() => open.mutate("/settings")}>在网页里打开</Button>
        </div>
      </Card>

      {state && <p className="text-xs text-muted">zsmeet 桌面版 {state.version}</p>}
    </div>
  );
}

/** 管理员的团队设置（成员、大模型、飞书……）先只在网页里改。 */
export function OpenAdminSettings() {
  const open = useMutation({ mutationFn: () => openServerPage("/settings/members") });
  return (
    <div className="flex flex-col gap-1">
      <button
        type="button"
        onClick={() => open.mutate()}
        className="flex min-h-10 cursor-pointer items-center whitespace-nowrap rounded-lg px-3 text-left text-sm text-ink-2 hover:bg-subtle"
      >
        在网页里打开 ↗
      </button>
      {open.isError && <p className="px-3 text-xs text-bad">{errorMessage(open.error)}</p>}
    </div>
  );
}

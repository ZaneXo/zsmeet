import { useMutation, useQuery } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import type { FormEvent } from "react";
import { api, errorMessage, queryClient, unwrap } from "../api/client";
import type { Invite, Member, User } from "../api/client";
import { Button, Card, Chip, Field, Input, Notice, Select, Spinner } from "../components/ui";
import { dateTime } from "../lib/format";

const ROLE_LABEL = { admin: "管理员", member: "成员" } as const;

/**
 * 刚生成的一次性链接。只在生成的那一刻拿得到，所以要醒目地显示出来让管理员复制。
 *
 * owner 是它属于谁（邀请是邮箱，重置密码是成员 ID）：给同一个人再生成一次时替换旧的。
 * stamp 是服务器给这一次生成记的时间（邀请的创建时间、重置链接的过期时间），用来和之后取回的列表对照：
 * 列表里对应的那一条不见了或者换了时间，说明这个链接已经失效（被接受、被撤销、被别人重新生成、
 * 对方自己改了密码……），要从页面上拿掉，免得管理员把一个没用的链接发出去。
 */
type FreshLink = {
  owner: string;
  kind: "invite" | "reset";
  stamp: string;
  /** 这一块是什么时候显示出来的（performance.now()）。在这之前发出的列表请求不一定包含这个链接，不能拿来对照 */
  shownAt: number;
  title: string;
  url: string;
  hint: string;
};

const inviteLink = (token: string, invite: Invite): FreshLink => ({
  owner: invite.email,
  kind: "invite",
  stamp: invite.created_at,
  shownAt: performance.now(),
  title: `给 ${invite.email} 的邀请链接`,
  url: `${window.location.origin}/invite#${token}`,
  hint: "7 天内有效，只能用一次。系统不会发邮件，请你自己通过飞书、微信等发给对方。",
});

const sameTime = (a: string, b: string) => new Date(a).getTime() === new Date(b).getTime();

const refresh = () => queryClient.invalidateQueries({ queryKey: ["members"] });

/** 成员管理：邀请同事、改角色、停用、帮忘了密码的人重置。只有管理员能看到。 */
export function MembersSection({ me }: { me: User }) {
  const members = useQuery({
    queryKey: ["members"],
    // 顺手记下这次请求是什么时候发出的，对照链接框时要用（见下面的 useEffect）
    queryFn: async () => {
      const requestedAt = performance.now();
      return { ...(await unwrap(api.GET("/api/members"))), requestedAt };
    },
    // 别的管理员也可能在改，对方接受邀请后列表也会变：切回这个页面时重新取一次
    refetchOnWindowFocus: true,
  });
  const [inviting, setInviting] = useState(false);
  // 可能连着邀请好几个人，每个人的链接各占一块，互不覆盖
  const [links, setLinks] = useState<FreshLink[]>([]);
  const [error, setError] = useState("");

  const showLink = (fresh: FreshLink) => setLinks((current) => [...current.filter((l) => l.owner !== fresh.owner), fresh]);
  const dropLink = (owner: string) => setLinks((current) => current.filter((l) => l.owner !== owner));

  // 每次取回新的列表，就把已经失效的链接从页面上拿掉。
  // 只认「链接显示出来以后才发出的请求」：在那之前发出的请求，服务器处理它的时候链接可能还没生成，
  // 结果里没有这个链接并不代表它失效了
  const data = members.data;
  useEffect(() => {
    if (!data) return;
    setLinks((current) => {
      const alive = current.filter((link) => {
        if (data.requestedAt <= link.shownAt) return true;
        if (link.kind === "invite") {
          const invite = (data.invites ?? []).find((i) => i.email === link.owner);
          return !!invite && !invite.expired && sameTime(invite.created_at, link.stamp);
        }
        const reset = (data.members ?? []).find((m) => m.id === link.owner)?.reset_link;
        return !!reset && sameTime(reset.expires_at, link.stamp);
      });
      return alive.length === current.length ? current : alive;
    });
  }, [data]);

  // 表格里的各种操作共用一个错误提示，每次操作前清掉上一次的。
  //
  // 一次只做一个：操作做完、列表也刷新回来之前，表格里的按钮都先不能点。否则连点两下「撤销」，
  // 第二下会撞上「已经撤销了」，在一个成功的操作后面留下一条吓人的报错。
  //
  // 不管成功还是失败都重新取一次列表：失败往往正是因为页面上的数据旧了（比如对方刚接受了邀请）
  const [busy, setBusy] = useState(false);
  const running = useRef(false);
  const run: Run = (action, onSuccess) => {
    if (running.current) return;
    running.current = true;
    setBusy(true);
    setError("");
    action()
      .then((result) => onSuccess?.(result))
      .catch((e: unknown) => setError(errorMessage(e)))
      .then(refresh)
      .finally(() => {
        running.current = false;
        setBusy(false);
      });
  };

  if (members.isPending) return <Spinner label="正在加载成员…" />;
  // 一次都没取到：只能整块报错。取到过的话，即使后来刷新失败也继续显示，不能把还没复制的链接盖掉
  if (!data) {
    return (
      <div className="flex flex-col items-start gap-3">
        <Notice tone="bad">{errorMessage(members.error)}</Notice>
        <Button onClick={() => members.refetch()}>重试</Button>
      </div>
    );
  }

  const list = data.members ?? [];
  const invites = data.invites ?? [];

  return (
    <div className="flex flex-col gap-5">
      <Card className="flex flex-col gap-4 p-6">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <div className="flex flex-col gap-1">
            <h2 className="text-lg font-bold">成员</h2>
            <p className="text-[13px] leading-relaxed text-muted">会议默认所有成员都能看，只有创建者和管理员能改；创建者可以改成只给指定的人看，也可以指定谁还能编辑。管理员能看、能改所有会议。</p>
          </div>
          {!inviting && (
            <Button variant="primary" size="sm" onClick={() => setInviting(true)}>
              邀请成员
            </Button>
          )}
        </div>

        {inviting && (
          <InviteForm
            onCancel={() => setInviting(false)}
            onCreated={(fresh) => {
              showLink(fresh);
              setInviting(false);
              void refresh();
            }}
          />
        )}
        {/* key 用链接本身：换了一个链接就是一块新的，「已复制」的状态不会带到下一个链接上 */}
        {links.map((link) => (
          <LinkBox key={link.url} link={link} onClose={() => dropLink(link.owner)} />
        ))}
        {error && <Notice tone="bad">{error}</Notice>}
        {members.isError && (
          <Notice tone="warn">
            成员列表没能刷新（{errorMessage(members.error)}），下面显示的可能不是最新的。
            <button type="button" className="ml-2 cursor-pointer font-semibold underline" onClick={() => members.refetch()}>
              重试
            </button>
          </Notice>
        )}

        <div className="overflow-x-auto">
          <table className="w-full min-w-[560px] border-collapse text-sm">
            <thead>
              <tr className="border-b border-line text-left text-xs font-semibold text-muted">
                <th className="px-3 py-2.5">姓名</th>
                <th className="px-3 py-2.5">邮箱</th>
                <th className="px-3 py-2.5">角色</th>
                <th className="px-3 py-2.5">状态</th>
                <th className="px-3 py-2.5 text-right">操作</th>
              </tr>
            </thead>
            <tbody>
              {list.map((m) => (
                <MemberRow key={m.id} member={m} isMe={m.id === me.id} busy={busy} run={run} onLink={showLink} onLinkGone={() => dropLink(m.id)} />
              ))}
            </tbody>
          </table>
        </div>
      </Card>

      {invites.length > 0 && (
        <Card className="flex flex-col gap-3 p-6">
          <h2 className="text-lg font-bold">还没接受的邀请</h2>
          {invites.map((invite) => (
            <InviteRow key={invite.id} invite={invite} busy={busy} run={run} onLink={showLink} onRevoked={() => dropLink(invite.email)} />
          ))}
        </Card>
      )}
    </div>
  );
}

type Run = <T>(action: () => Promise<T>, onSuccess?: (result: T) => void) => void;

function MemberRow({
  member: m,
  isMe,
  busy,
  run,
  onLink,
  onLinkGone,
}: {
  member: Member;
  isMe: boolean;
  /** 表格里有操作正在进行，按钮先不能点 */
  busy: boolean;
  run: Run;
  onLink: (l: FreshLink) => void;
  /** 这个人的重置链接失效了（被停用，或者链接被撤销） */
  onLinkGone: () => void;
}) {
  // 停用和改角色都不是小事（踢下线、给出或收回管理权限、他发出去的链接作废），而且下拉框很容易手滑选错，
  // 所以点了以后先在这一行里再确认一次。值是要确认的操作：停用，或者要改成的角色
  // unlink 是解除他和飞书账号的关联
  const [confirming, setConfirming] = useState<"disable" | "unlink" | Member["role"] | null>(null);
  // 确认以后、还在保存的时候，下拉框先显示新角色；保存完、列表刷新回来以后以服务器的结果为准
  const [savingRole, setSavingRole] = useState<Member["role"] | null>(null);
  useEffect(() => {
    if (!busy) setSavingRole(null);
  }, [busy]);

  const path = { params: { path: { id: m.id } } };
  const update = (body: { role?: Member["role"]; disabled?: boolean }) =>
    run(
      () => unwrap(api.PATCH("/api/members/{id}", { ...path, body })),
      // 停用以后，给他生成的重置密码链接也作废了
      () => body.disabled && onLinkGone(),
    );
  const confirm = () => {
    const what = confirming;
    setConfirming(null);
    if (what === "disable") {
      update({ disabled: true });
    } else if (what === "unlink") {
      run(() => unwrap(api.DELETE("/api/members/{id}/feishu", path)));
    } else if (what) {
      setSavingRole(what);
      update({ role: what });
    }
  };

  // 在任的管理员卸任（降级或停用）时，他凭管理员身份发出去的链接跟着失效
  const activeAdmin = m.role === "admin" && !m.disabled;
  const consequence = {
    disable: "停用后他会立刻被退出登录，不能再登录。",
    member: "降为成员后，他不能再管理成员和配置大模型。",
    admin: "设为管理员后，他可以管理成员、配置大模型，并修改任何一场会议。",
    unlink: "解除后他不能再用这个飞书账号登录。之后他用飞书登录时会按企业邮箱重新对应，也可以用邮箱和密码登录后在「设置 → 账号」里重新关联。",
  };
  const changingRole = confirming === "admin" || confirming === "member";

  return (
    <tr className="border-b border-line/70 align-top last:border-b-0">
      <td className="px-3 py-3 font-medium">
        {m.name}
        {isMe && <span className="ml-1.5 text-xs font-normal text-muted">（你）</span>}
      </td>
      <td className="px-3 py-3 text-muted">{m.email}</td>
      <td className="px-3 py-3">
        {isMe ? (
          ROLE_LABEL[m.role]
        ) : (
          <Select
            aria-label={`${m.name} 的角色`}
            className="w-28 min-h-8 py-1"
            value={changingRole ? confirming : (savingRole ?? m.role)}
            disabled={busy}
            // 选回现在的角色，就是不改了
            onChange={(e) => setConfirming(e.target.value === m.role ? null : (e.target.value as Member["role"]))}
          >
            <option value="member">成员</option>
            <option value="admin">管理员</option>
          </Select>
        )}
      </td>
      <td className="px-3 py-3">
        <div className="flex flex-wrap gap-1.5">
          {m.disabled ? <Chip tone="mute">已停用</Chip> : <Chip tone="ok">正常</Chip>}
          {m.feishu_linked && <Chip tone="mute">已关联飞书</Chip>}
        </div>
      </td>
      <td className="px-3 py-3">
        <div className="flex flex-col items-end gap-2">
          {isMe ? (
            <div className="text-right text-xs text-muted" title="自己的角色要请另一位管理员来改">
              不能修改自己
            </div>
          ) : confirming ? (
            <div className="flex flex-wrap items-center justify-end gap-2">
              <span className="max-w-xs text-right text-xs leading-relaxed text-muted">
                {consequence[confirming]}
                {activeAdmin && (confirming === "disable" || confirming === "member") && "他生成的、还没被用掉的邀请和重置密码链接也会失效；邀请可以在下面重新生成。"}
              </span>
              <Button size="sm" variant={confirming === "admin" ? "primary" : "danger"} disabled={busy} onClick={confirm}>
                {{ disable: "确定停用", member: "确定降为成员", admin: "确定设为管理员", unlink: "确定解除" }[confirming]}
              </Button>
              <Button size="sm" onClick={() => setConfirming(null)}>
                取消
              </Button>
            </div>
          ) : m.disabled ? (
            <Button size="sm" disabled={busy} onClick={() => update({ disabled: false })}>
              启用
            </Button>
          ) : (
            <div className="flex flex-wrap justify-end gap-2">
              <Button
                size="sm"
                title="他忘了密码时用"
                disabled={busy}
                onClick={() =>
                  run(
                    () => unwrap(api.POST("/api/members/{id}/reset-link", path)),
                    (r) =>
                      onLink({
                        owner: m.id,
                        kind: "reset",
                        stamp: r.expires_at,
                        shownAt: performance.now(),
                        title: `给 ${m.name} 重置密码的链接`,
                        url: `${window.location.origin}/reset#${r.token}`,
                        hint: `${dateTime(r.expires_at)} 之前有效，只能用一次。请你自己发给 ${m.name}，对方打开后设置新密码。`,
                      }),
                  )
                }
              >
                重置密码
              </Button>
              <Button size="sm" disabled={busy} onClick={() => setConfirming("disable")}>
                停用
              </Button>
            </div>
          )}

          {/* 他换了飞书账号、自己又进不来（比如用飞书自动创建的账号不知道密码）时，管理员替他解除。自己的在账号页上解除 */}
          {m.feishu_linked && !isMe && !confirming && (
            <Button size="sm" title="他换了飞书账号时用。解除后他用新的飞书账号登录时，按企业邮箱重新对应" disabled={busy} onClick={() => setConfirming("unlink")}>
              解除飞书关联
            </Button>
          )}

          {/* 拿到重置链接的人能直接进这个账号，所以谁名下有一个还没用的，要让管理员看得到、撤得掉 */}
          {m.reset_link && (
            <div className="flex flex-wrap items-center justify-end gap-2 text-xs text-warn">
              <span>
                有一个还没用的重置密码链接（
                {m.reset_link.from_server ? "在服务器上用命令生成" : `${m.reset_link.created_by} 生成`}，{dateTime(m.reset_link.expires_at)} 前有效）
              </span>
              <Button size="sm" disabled={busy} onClick={() => run(() => unwrap(api.DELETE("/api/members/{id}/reset-link", path)), onLinkGone)}>
                撤销链接
              </Button>
            </div>
          )}
        </div>
      </td>
    </tr>
  );
}

function InviteRow({
  invite,
  busy,
  run,
  onLink,
  onRevoked,
}: {
  invite: Invite;
  busy: boolean;
  run: Run;
  onLink: (l: FreshLink) => void;
  onRevoked: () => void;
}) {
  const path = { params: { path: { id: invite.id } } };
  return (
    <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2 rounded-xl border border-line px-4 py-3">
      <div className="flex min-w-0 flex-col gap-1">
        <div className="flex flex-wrap items-center gap-2">
          <b className="truncate text-sm font-semibold">{invite.email}</b>
          <Chip tone="mute">{ROLE_LABEL[invite.role]}</Chip>
          {invite.expired && <Chip tone="warn">已失效</Chip>}
        </div>
        <span className="text-xs text-muted">
          {invite.invited_by} 邀请
          {invite.expired ? "，链接已经用不了了，重新生成后再发给对方" : `，${dateTime(invite.expires_at)} 前有效`}
        </span>
      </div>
      <div className="flex gap-2">
        <Button
          size="sm"
          title="链接弄丢了或者失效了时用。之前的链接不能再用"
          disabled={busy}
          onClick={() => run(() => unwrap(api.POST("/api/invites/{id}/renew", path)), (r) => onLink(inviteLink(r.token, r.invite)))}
        >
          重新生成链接
        </Button>
        <Button size="sm" disabled={busy} onClick={() => run(() => unwrap(api.DELETE("/api/invites/{id}", path)), onRevoked)}>
          撤销
        </Button>
      </div>
    </div>
  );
}

function InviteForm({ onCancel, onCreated }: { onCancel: () => void; onCreated: (link: FreshLink) => void }) {
  const [email, setEmail] = useState("");
  const [role, setRole] = useState<Member["role"]>("member");
  const create = useMutation({
    mutationFn: () => unwrap(api.POST("/api/invites", { body: { email, role } })),
    onSuccess: (r) => onCreated(inviteLink(r.token, r.invite)),
    // 失败常常是因为列表旧了（比如这个人刚刚已经加入）：顺手刷新一下
    onError: () => void refresh(),
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    create.mutate();
  };
  return (
    <form onSubmit={submit} className="flex flex-col gap-3 rounded-xl border border-brand/25 bg-brand-tint/50 p-4">
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-[1fr_140px]">
        <Field label="对方的邮箱" htmlFor="invite-email" hint="对方以后用它登录。不会往这个邮箱发邮件">
          <Input id="invite-email" type="email" value={email} onChange={(e) => setEmail(e.target.value)} required maxLength={200} autoFocus />
        </Field>
        <Field label="角色" htmlFor="invite-role">
          <Select id="invite-role" value={role} onChange={(e) => setRole(e.target.value as Member["role"])}>
            <option value="member">成员</option>
            <option value="admin">管理员</option>
          </Select>
        </Field>
      </div>
      <p className="text-xs leading-relaxed text-muted">成员可以上传会议、查看团队里的会议。管理员还能管理成员、配置大模型，看得到、改得了所有会议。</p>
      {create.isError && <Notice tone="bad">{errorMessage(create.error)}</Notice>}
      <div className="flex gap-2">
        <Button type="submit" variant="primary" size="sm" disabled={create.isPending}>
          生成邀请链接
        </Button>
        <Button size="sm" onClick={onCancel}>
          取消
        </Button>
      </div>
    </form>
  );
}

/** 显示刚生成的链接，带复制按钮。浏览器不让自动复制时（没用 HTTPS），选中文字让用户自己复制。 */
function LinkBox({ link, onClose }: { link: FreshLink; onClose: () => void }) {
  const input = useRef<HTMLInputElement>(null);
  const [state, setState] = useState<"idle" | "copied" | "manual">("idle");
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(link.url);
      setState("copied");
    } catch {
      input.current?.select();
      setState("manual");
    }
  };
  return (
    <div className="flex flex-col gap-2.5 rounded-xl border border-ok/30 bg-ok-tint/50 p-4" role="status">
      <b className="text-sm font-semibold">{link.title}</b>
      <div className="flex gap-2">
        <Input ref={input} readOnly value={link.url} aria-label={link.title} onFocus={(e) => e.currentTarget.select()} className="font-mono text-xs" />
        <Button variant="primary" onClick={copy}>
          {state === "copied" ? "已复制" : "复制"}
        </Button>
      </div>
      {state === "manual" && <span className="text-xs text-warn">浏览器不允许自动复制。链接已经选中，按 Ctrl+C（Mac 上是 ⌘C）复制。</span>}
      <span className="text-xs leading-relaxed text-ink-2">{link.hint}</span>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <span className="text-xs text-muted">这个链接只显示这一次。关掉以后找不回来，但可以重新生成一个。</span>
        <Button size="sm" onClick={onClose}>
          我已经复制好了
        </Button>
      </div>
    </div>
  );
}

import { useMutation, useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { api, errorMessage, queryClient, unwrap } from "../api/client";
import type { Meeting } from "../api/client";
import type { components } from "../api/schema";
import { Button, Modal, Notice, Select, Spinner } from "../components/ui";

type Visibility = Meeting["visibility"];
type Member = components["schemas"]["MeetingMember"];

export const VISIBILITY_LABEL: Record<Visibility, string> = {
  team: "团队所有人可见",
  members: "指定的人可见",
  private: "仅创建者可见",
};

/** 可见范围的选项。管理员改别人的会议时，「只有我」要说成「只有创建者」。 */
function options(isOwner: boolean, ownerName: string): { value: Visibility; label: string; hint: string }[] {
  return [
    { value: "team", label: "团队里所有人", hint: "团队里的人都能看。下面加的人还能编辑" },
    { value: "members", label: "指定的人", hint: "只有下面这些人能看，勾上「能编辑」的还能改" },
    isOwner
      ? { value: "private", label: "只有我", hint: "只有你自己能看（管理员也能看）" }
      : { value: "private", label: `只有创建者（${ownerName}）`, hint: "只有创建者和管理员能看" },
  ];
}

/** 会议页上的「谁能看」：创建者和管理员可以改可见范围、指定谁能看、谁能编辑。 */
export function AccessDialog({
  meeting: m,
  isOwner,
  feishu,
  groups = false,
  onClose,
}: {
  meeting: Meeting;
  isOwner: boolean;
  feishu: boolean;
  // 管理员加了钉钉、企业微信、Slack 的群机器人
  groups?: boolean;
  onClose: () => void;
}) {
  // 每次打开都重新取：表单拿它当初始值、保存时整份替换，用缓存里的旧名单会把别人刚改的覆盖掉
  const access = useQuery({
    queryKey: ["meeting-access", m.id],
    queryFn: () => unwrap(api.GET("/api/meetings/{id}/access", { params: { path: { id: m.id } } })),
    gcTime: 0,
  });
  const people = useQuery({ queryKey: ["people"], queryFn: () => unwrap(api.GET("/api/people")) });

  return (
    <Modal title="谁能看、谁能编辑" onClose={onClose}>
      {access.isPending || people.isPending ? (
        <Spinner label="正在加载…" />
      ) : access.isError || people.isError ? (
        <Notice tone="bad">{errorMessage(access.error ?? people.error)}</Notice>
      ) : (
        <AccessForm meeting={m} isOwner={isOwner} feishu={feishu} groups={groups} initial={access.data} people={people.data.items} onClose={onClose} />
      )}
    </Modal>
  );
}

function AccessForm({
  meeting: m,
  isOwner,
  feishu,
  groups,
  initial,
  people,
  onClose,
}: {
  meeting: Meeting;
  isOwner: boolean;
  feishu: boolean;
  groups: boolean;
  initial: components["schemas"]["MeetingAccess"];
  people: components["schemas"]["Person"][];
  onClose: () => void;
}) {
  const [visibility, setVisibility] = useState<Visibility>(initial.visibility);
  const [members, setMembers] = useState<Member[]>(initial.members);
  // 团队所有人可见时，单独加的人只用来给编辑权限
  const shown = visibility === "team" ? members.filter((x) => x.can_edit) : visibility === "members" ? members : [];
  const candidates = people.filter((p) => p.id !== m.owner_id && !shown.some((x) => x.user_id === p.id));

  const save = useMutation({
    mutationFn: () =>
      unwrap(
        api.PUT("/api/meetings/{id}/access", {
          params: { path: { id: m.id } },
          body: { visibility, members: shown.map((x) => ({ user_id: x.user_id, can_edit: x.can_edit })) },
        }),
      ),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["meeting", m.id] });
      void queryClient.invalidateQueries({ queryKey: ["meeting-access", m.id] });
      void queryClient.invalidateQueries({ queryKey: ["meetings"] });
      onClose();
    },
  });

  const add = (id: string) => {
    const p = people.find((x) => x.id === id);
    if (!p) return;
    const rest = members.filter((x) => x.user_id !== id);
    setMembers([...rest, { user_id: p.id, name: p.name, email: p.email, can_edit: visibility === "team", disabled: false }]);
  };
  const update = (id: string, canEdit: boolean) => setMembers(members.map((x) => (x.user_id === id ? { ...x, can_edit: canEdit } : x)));
  const remove = (id: string) => setMembers(members.filter((x) => x.user_id !== id));

  return (
    <div className="flex flex-col gap-4">
      <fieldset className="flex flex-col gap-2">
        <legend className="mb-1.5 text-[13px] font-semibold text-ink-2">谁能看</legend>
        {options(isOwner, m.owner_name).map((o) => (
          <label
            key={o.value}
            className={`flex cursor-pointer items-start gap-2.5 rounded-lg border px-3.5 py-2.5 ${
              visibility === o.value ? "border-brand bg-brand-tint/40" : "border-line hover:border-line-strong"
            }`}
          >
            <input
              type="radio"
              name="visibility"
              className="mt-0.5 size-4 shrink-0 cursor-pointer accent-brand"
              checked={visibility === o.value}
              onChange={() => setVisibility(o.value)}
            />
            <span className="flex flex-col gap-0.5">
              <span className="text-sm font-medium">{o.label}</span>
              <span className="text-xs leading-relaxed text-muted">{o.hint}</span>
            </span>
          </label>
        ))}
      </fieldset>

      {visibility !== "private" && (
        <div className="flex flex-col gap-2.5">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <span className="text-[13px] font-semibold text-ink-2">{visibility === "team" ? "还能编辑的人" : "能看的人"}</span>
            <Select
              aria-label="加一个人"
              className="w-56"
              value=""
              disabled={candidates.length === 0}
              onChange={(e) => add(e.target.value)}
            >
              <option value="">{candidates.length === 0 ? "团队里的人都加上了" : "加一个人…"}</option>
              {candidates.map((p) => (
                <option key={p.id} value={p.id}>
                  {p.name}（{p.email}）
                </option>
              ))}
            </Select>
          </div>
          {shown.length === 0 ? (
            <p className="rounded-lg bg-subtle px-3.5 py-3 text-[13px] leading-relaxed text-muted">
              {visibility === "team" ? "还没有加人：只有创建者和管理员能编辑。" : "还没有加人：现在只有创建者和管理员能看。"}
            </p>
          ) : (
            <ul className="flex max-h-64 flex-col divide-y divide-line overflow-y-auto rounded-lg border border-line">
              {shown.map((x) => (
                <li key={x.user_id} className="flex flex-wrap items-center gap-x-3 gap-y-1.5 px-3.5 py-2">
                  <span className="min-w-0 flex-1 text-sm">
                    {x.name}
                    <span className="ml-1.5 text-xs text-muted">
                      {x.email}
                      {x.disabled && "（已停用）"}
                    </span>
                  </span>
                  {visibility === "members" && (
                    <label className="flex cursor-pointer items-center gap-1.5 text-[13px] text-ink-2">
                      <input
                        type="checkbox"
                        className="size-4 cursor-pointer accent-brand"
                        checked={x.can_edit}
                        onChange={(e) => update(x.user_id, e.target.checked)}
                      />
                      能编辑
                    </label>
                  )}
                  <Button size="sm" variant="ghost" onClick={() => remove(x.user_id)}>
                    移除
                  </Button>
                </li>
              ))}
            </ul>
          )}
        </div>
      )}

      <p className="text-xs leading-relaxed text-muted">
        创建者和管理员总能看、能编辑；删除会议、改这里的设置只有他们能做。
        {feishu && "已经发到飞书群的卡片、导入的飞书文档不受这里的设置影响；只有团队所有人可见的会议才会自动发到飞书群。"}
        {groups && "已经发到钉钉、企业微信、Slack 群里的消息不受这里的设置影响；只有团队所有人可见的会议才会自动发到这些群。"}
      </p>
      {save.isError && <Notice tone="bad">{errorMessage(save.error)}</Notice>}
      <div className="flex justify-end gap-2">
        <Button onClick={onClose}>取消</Button>
        <Button variant="primary" disabled={save.isPending} onClick={() => save.mutate()}>
          {save.isPending ? "正在保存…" : "保存"}
        </Button>
      </div>
    </div>
  );
}

import { useMutation, useQuery } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import type { FormEvent } from "react";
import { api, errorMessage, queryClient, unwrap } from "../api/client";
import type { Meeting } from "../api/client";
import type { components } from "../api/schema";
import { Button, Chip, Modal, Notice } from "../components/ui";
import type { Tone } from "../components/ui";
import { dateTime } from "../lib/format";
import { newestFirst } from "../lib/time";
import { PLATFORM_LABEL } from "./WebhookSettings";

// 把纪要发到钉钉、企业微信、Slack 的群机器人：会议页上「发到群里」的弹窗（选群、发送、每个群最近一次的情况），
// 和标题下面那一行（最近发到群里的情况）。

type Send = components["schemas"]["WebhookSend"];
type Hook = components["schemas"]["MeetingWebhook"];

const SEND_STATUS: Record<Send["status"], { tone: Tone; text: string; dot: string }> = {
  pending: { tone: "info", text: "发送中", dot: "animate-pulse bg-brand" },
  done: { tone: "ok", text: "已发送", dot: "bg-ok" },
  failed: { tone: "bad", text: "发送失败", dot: "bg-bad" },
};

const hooksKey = (meetingId: string) => ["meeting-webhooks", meetingId];

/**
 * 管理员加的群机器人，和这场会议最近一次发到每个群的情况。有还在发送的，就每 2 秒刷新一次，直到发完。
 * 后台任务丢了的那种不会一直刷新下去：发送中超过半小时的，服务器报成失败（也就允许重新发了）。
 * 会议还没加载好时（minutesVersion 为 undefined）先不取。
 */
export function useMeetingWebhooks(meetingId: string, minutesVersion: number | undefined) {
  const query = useQuery({
    queryKey: hooksKey(meetingId),
    queryFn: () => unwrap(api.GET("/api/meetings/{id}/webhooks", { params: { path: { id: meetingId } } })),
    enabled: minutesVersion !== undefined,
    refetchInterval: (q) => ((q.state.data?.items ?? []).some((h) => h.latest?.status === "pending") ? 2000 : false),
  });

  // 纪要第一次生成后可能自动发到了群里：纪要的版本变了就重新取一次。刚打开页面时不用，上面的查询自己会取
  const version = useRef(minutesVersion);
  useEffect(() => {
    if (version.current === minutesVersion) return;
    const first = version.current === undefined;
    version.current = minutesVersion;
    if (!first) void queryClient.invalidateQueries({ queryKey: hooksKey(meetingId) });
  }, [meetingId, minutesVersion]);
  return query;
}

/**
 * 标题下面的一行：最近发到群里的情况，没发过时不显示。
 * 能发的人（传了 onOpen）看每个群最近一次，不管成没成功，点它打开「发到群里」；其他人只看发成功了的。
 */
export function LatestWebhookSends({ hooks, meeting, onOpen }: { hooks: Hook[]; meeting: Meeting; onOpen?: () => void }) {
  const sends = hooks
    .map((h) => h.latest)
    .filter((s): s is Send => !!s && (!!onOpen || s.status === "done"))
    .sort((a, b) => newestFirst(a.created_at, b.created_at));
  const [latest] = sends;
  if (!latest) return null;

  const names = (status: Send["status"]) =>
    sends
      .filter((s) => s.status === status)
      .map((s) => `「${s.webhook_name}」`)
      .join("");
  const parts = [
    names("pending") && `正在发到${names("pending")}…`,
    names("done") && `已发到${names("done")}`,
    names("failed") && `发到${names("failed")}失败 · 查看原因`,
  ].filter(Boolean);
  // 发出去以后纪要又改过：说一声群里的是旧的那一版
  const stale = sends.find((s) => s.status === "done" && s.minutes_version < meeting.minutes_version);
  const old = stale ? `（发的是第 ${stale.minutes_version} 版，现在是第 ${meeting.minutes_version} 版）` : "";
  const status = sends.some((s) => s.status === "failed") ? "failed" : sends.some((s) => s.status === "pending") ? "pending" : "done";
  const text = `${parts.join("，")}${old} · ${dateTime(latest.created_at)}`;
  const content = (
    <>
      <span className={`size-1.5 shrink-0 rounded-full ${SEND_STATUS[status].dot}`} aria-hidden="true" />
      {text}
    </>
  );
  const color = status === "failed" ? "text-bad" : "text-muted hover:text-ink";
  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-[13px]">
      {onOpen ? (
        <button type="button" onClick={onOpen} title="查看发到群里的情况" className={`inline-flex cursor-pointer items-center gap-2 text-left hover:underline ${color}`}>
          {content}
        </button>
      ) : (
        <span className="inline-flex items-center gap-2 text-muted">{content}</span>
      )}
    </div>
  );
}

/** 「发到群里」弹窗：勾上要发的群，发送；每个群旁边是这场会议最近一次发到那里的情况。发送在后台进行，弹窗开着就能看到结果。 */
export function WebhookShareDialog({ meeting, hooks, onClose }: { meeting: Meeting; hooks: Hook[]; onClose: () => void }) {
  const [chosen, setChosen] = useState<string[]>([]);
  const pending = (h: Hook) => h.latest?.status === "pending";
  // 真正要发的：选了以后别人（或者自动发送）开始往那个群发了、群被删了的不算。不然复选框灰着取消不掉，发送按钮还亮着
  const sendable = chosen.filter((id) => hooks.some((h) => h.id === id && !pending(h)));
  const send = useMutation({
    // 带上看到的版本：纪要在这期间重新生成或者被改过的话，服务器不发，免得发出去的不是这里写的那一版
    mutationFn: () =>
      unwrap(
        api.POST("/api/meetings/{id}/webhooks", {
          params: { path: { id: meeting.id } },
          body: { webhook_ids: sendable, minutes_version: meeting.minutes_version },
        }),
      ),
    onSuccess: () => setChosen([]),
    // 版本对不上时重新取会议，上面写的版本跟着变成新的
    onError: () => queryClient.invalidateQueries({ queryKey: ["meeting", meeting.id] }),
    // 成功、失败都重新取一次：失败可能是别人（或者自动发送）正在发，要显示出来
    onSettled: () => queryClient.invalidateQueries({ queryKey: hooksKey(meeting.id) }),
  });
  const toggle = (id: string, on: boolean) => {
    setChosen(on ? [...sendable, id] : sendable.filter((x) => x !== id));
    send.reset();
  };
  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (sendable.length > 0) send.mutate();
  };
  // 这一版已经发到过选中的群：提醒一下，再发群里会多一条
  const repeat = hooks.some((h) => sendable.includes(h.id) && h.latest?.status === "done" && h.latest.minutes_version === meeting.minutes_version);

  return (
    <Modal title="发到群里" onClose={onClose}>
      <p className="-mt-2 text-[13px] leading-relaxed text-muted">
        发送当前版本（第 {meeting.minutes_version} 版）纪要的摘要和待办，带着回到这场会议的链接。只能发到管理员在「设置 → 群机器人」里加好的群。
      </p>
      <form onSubmit={submit} className="flex flex-col gap-4">
        <ul className="flex max-h-80 flex-col divide-y divide-line overflow-y-auto rounded-xl border border-line">
          {hooks.map((h) => (
            <li key={h.id} className="flex flex-col gap-1.5 px-4 py-3">
              <label className="flex cursor-pointer items-center gap-2.5 text-sm has-disabled:cursor-not-allowed">
                <input
                  type="checkbox"
                  className="size-4 shrink-0 cursor-pointer accent-brand disabled:cursor-not-allowed"
                  checked={sendable.includes(h.id)}
                  disabled={pending(h)}
                  onChange={(e) => toggle(h.id, e.target.checked)}
                />
                <span className="font-medium">{h.name}</span>
                <span className="text-xs text-muted">{PLATFORM_LABEL[h.platform]}</span>
                {h.latest && (
                  <span className="ml-auto">
                    <Chip tone={SEND_STATUS[h.latest.status].tone}>{SEND_STATUS[h.latest.status].text}</Chip>
                  </span>
                )}
              </label>
              {h.latest && <SendDetail send={h.latest} />}
            </li>
          ))}
        </ul>
        {send.isError && <Notice tone="bad">{errorMessage(send.error)}</Notice>}
        {send.isSuccess && <Notice tone="ok">已经开始发送，结果会显示在上面。</Notice>}
        <div className="flex flex-wrap items-center justify-end gap-3">
          {repeat && <span className="mr-auto text-[13px] text-muted">这一版已经发到过选中的群，再发一次群里会多一条。</span>}
          <Button type="submit" variant="primary" disabled={sendable.length === 0 || send.isPending}>
            {send.isPending ? "正在发送…" : sendable.length > 1 ? `发到 ${sendable.length} 个群` : "发送"}
          </Button>
        </div>
      </form>
    </Modal>
  );
}

/** 一个群最近一次的发送：第几版、谁发的、什么时候；失败的写上原因，还在重试的写上上一次为什么没发出去。 */
function SendDetail({ send: s }: { send: Send }) {
  return (
    <div className="flex flex-col gap-1 pl-6.5 text-[13px]">
      <span className="text-muted">
        最近一次：第 {s.minutes_version} 版 · {s.requested_by_name || "自动发送"} · {dateTime(s.created_at)}
      </span>
      {s.status === "failed" && s.error && <p className="leading-relaxed text-bad">{s.error}</p>}
      {s.status === "pending" && s.error && <p className="leading-relaxed text-warn">{s.error}</p>}
    </div>
  );
}

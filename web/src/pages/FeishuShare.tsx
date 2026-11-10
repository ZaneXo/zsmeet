import { useMutation, useQuery } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import type { FormEvent } from "react";
import { api, errorMessage, queryClient, unwrap } from "../api/client";
import type { Meeting } from "../api/client";
import type { components } from "../api/schema";
import { Button, Chip, Field, Modal, Notice, Select, Spinner } from "../components/ui";
import type { Tone } from "../components/ui";
import { dateTime } from "../lib/format";

// 把纪要发到飞书：会议页上「发到飞书」的弹窗（选群、发送、发送记录），和标题下面那一行（最近一次发到飞书的情况）。

type Share = components["schemas"]["FeishuShare"];
type Shares = components["schemas"]["FeishuSharesOutputBody"];
type Chats = components["schemas"]["FeishuChatsOutputBody"];

const SHARE_STATUS: Record<Share["status"], { tone: Tone; text: string; dot: string }> = {
  pending: { tone: "info", text: "发送中", dot: "animate-pulse bg-brand" },
  done: { tone: "ok", text: "已发送", dot: "bg-ok" },
  failed: { tone: "bad", text: "发送失败", dot: "bg-bad" },
};

// 下拉框里「不发到群，只导入成飞书文档」那一项的值。飞书群的 ID 都以 oc_ 开头，不会和它重
const DOC_ONLY = "doc-only";

const sharesKey = (meetingId: string) => ["feishu-shares", meetingId];

/** 这场会议发到飞书的记录，最新的在前。有还在发送的，就每 2 秒刷新一次，直到发完。 */
function useShares(meetingId: string) {
  return useQuery({
    queryKey: sharesKey(meetingId),
    queryFn: () => unwrap(api.GET("/api/meetings/{id}/feishu", { params: { path: { id: meetingId } } })),
    refetchInterval: (query) => ((query.state.data?.items ?? []).some((s) => s.status === "pending") ? 2000 : false),
  });
}

/**
 * 标题下面的一行：最近一次发到飞书的情况，没发过时不显示。
 * 能发的人（传了 onOpen）看最近一次，不管成没成功，点它打开「发到飞书」看记录；其他人只看最近发成功的那一次。
 */
export function LatestFeishuShare({ meeting, onOpen }: { meeting: Meeting; onOpen?: () => void }) {
  const shares = useShares(meeting.id);

  // 管理员打开了自动发送的话，纪要第一次生成后会自动发到默认的群：纪要的版本变了就重新取一次记录。
  // 刚打开页面时不用，上面的查询自己会取
  const version = useRef(meeting.minutes_version);
  useEffect(() => {
    if (version.current === meeting.minutes_version) return;
    version.current = meeting.minutes_version;
    void queryClient.invalidateQueries({ queryKey: sharesKey(meeting.id) });
  }, [meeting.id, meeting.minutes_version]);

  const items = shares.data?.items ?? [];
  const s = onOpen ? items[0] : items.find((item) => item.status === "done");
  if (!s) return null;

  const group = s.chat_id ? `飞书群${s.chat_name ? `「${s.chat_name}」` : ""}` : "";
  const what = {
    pending: group ? `正在发到${group}…` : "正在导入成飞书文档…",
    done: group ? `已发到${group}` : "已导入成飞书文档",
    failed: group ? `发到${group}失败` : "导入飞书文档失败",
  }[s.status];
  // 发出去以后纪要又改过：说一声飞书上的是旧的那一版
  const old = s.status === "done" && s.minutes_version < meeting.minutes_version ? `（第 ${s.minutes_version} 版，现在是第 ${meeting.minutes_version} 版）` : "";
  const when = s.status === "pending" ? "" : ` · ${dateTime(s.created_at)}`;
  const text = `${what}${old}${when}${s.status === "failed" ? " · 查看原因" : ""}`;

  const content = (
    <>
      <span className={`size-1.5 shrink-0 rounded-full ${SHARE_STATUS[s.status].dot}`} aria-hidden="true" />
      {text}
    </>
  );
  const color = s.status === "failed" ? "text-bad" : "text-muted hover:text-ink";
  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-[13px]">
      {onOpen ? (
        <button type="button" onClick={onOpen} title="查看发到飞书的记录" className={`inline-flex cursor-pointer items-center gap-2 text-left hover:underline ${color}`}>
          {content}
        </button>
      ) : (
        <span className="inline-flex items-center gap-2 text-muted">{content}</span>
      )}
      {s.doc_url && (
        <a href={s.doc_url} target="_blank" rel="noreferrer" className="text-brand hover:text-brand-hover">
          打开飞书文档
        </a>
      )}
    </div>
  );
}

/** 「发到飞书」弹窗：选群、要不要同时导入成飞书文档，发送；下面是这场会议发到飞书的记录。发送在后台进行，弹窗开着就能看到结果。 */
export function FeishuShareDialog({ meeting, onClose }: { meeting: Meeting; onClose: () => void }) {
  const chats = useQuery({
    // 和「设置 → 飞书」里的群列表是同一份，那边保存配置时会刷新它
    queryKey: ["feishu", "chats"],
    queryFn: () => unwrap(api.GET("/api/feishu/chats")),
  });
  const shares = useShares(meeting.id);
  const items = shares.data?.items ?? [];

  return (
    <Modal title="发到飞书" onClose={onClose}>
      <p className="-mt-2 text-[13px] leading-relaxed text-muted">
        发送当前版本（第 {meeting.minutes_version} 版）的纪要。发到群里的是一张卡片，上面有摘要、待办和回到这场会议的链接。
      </p>

      {chats.data ? (
        <ShareForm meeting={meeting} chats={chats.data} shares={items} />
      ) : chats.isError && !chats.isFetching ? (
        <div className="flex flex-col items-start gap-3">
          <Notice tone="bad">{errorMessage(chats.error)}</Notice>
          <Button size="sm" onClick={() => chats.refetch()}>
            重试
          </Button>
        </div>
      ) : (
        <Spinner label="正在读取飞书群…" />
      )}

      {shares.isError && <Notice tone="warn">发送记录没能刷新：{errorMessage(shares.error)}</Notice>}
      {items.length > 0 && (
        <section className="flex flex-col gap-1 border-t border-line pt-4">
          <h3 className="text-sm font-semibold">发送记录</h3>
          <ul className="flex max-h-72 flex-col divide-y divide-line overflow-y-auto">
            {items.map((s) => (
              <ShareRow key={s.id} share={s} />
            ))}
          </ul>
        </section>
      )}
    </Modal>
  );
}

function ShareForm({ meeting, chats, shares }: { meeting: Meeting; chats: Chats; shares: Share[] }) {
  // 先选上管理员设的默认群。没设（或者机器人已经不在那个群里）时让人自己选，不替他挑，免得发错群；
  // 机器人一个群都不在的话只能导入成文档
  const [chat, setChat] = useState(() => {
    if (chats.items.length === 0) return DOC_ONLY;
    return chats.items.some((c) => c.id === chats.default_chat_id) ? chats.default_chat_id : "";
  });
  const [withDoc, setWithDoc] = useState(chats.with_doc);
  const docOnly = chat === DOC_ONLY;

  const send = useMutation({
    mutationFn: () =>
      unwrap(
        api.POST("/api/meetings/{id}/feishu", {
          params: { path: { id: meeting.id } },
          body: { chat_id: docOnly ? "" : chat, with_doc: docOnly || withDoc },
        }),
      ),
    // 新的这一条马上放到记录最前面（发送中），之后每 2 秒刷新一次，直到发完
    onSuccess: (share) => queryClient.setQueryData<Shares>(sharesKey(meeting.id), (old) => ({ items: [share, ...(old?.items ?? [])] })),
    // 失败了也重新取一次：可能是别人（或者自动发送）正在发，记录里要显示出来
    onSettled: () => queryClient.invalidateQueries({ queryKey: sharesKey(meeting.id) }),
  });

  // 同一场会议一次只能发一个，上一次发完才能再发
  const busy = send.isPending || shares.some((s) => s.status === "pending");
  const repeat = repeatHint(shares, meeting.minutes_version, chat);

  const submit = (e: FormEvent) => {
    e.preventDefault();
    send.mutate();
  };

  return (
    <form onSubmit={submit} className="flex flex-col gap-4">
      <Field
        label="飞书群"
        htmlFor="feishu-chat"
        hint={
          chats.items.length > 0
            ? "只能发到应用机器人所在的群。要发到别的群，先在那个群的设置里添加机器人。"
            : "应用机器人还没有加入任何群，现在只能导入成飞书文档。在群设置里添加机器人以后，这里就能选。"
        }
      >
        <Select
          id="feishu-chat"
          value={chat}
          onChange={(e) => {
            setChat(e.target.value);
            send.reset();
          }}
        >
          <option value="" disabled>
            选一个群…
          </option>
          {chats.items.map((c) => (
            <option key={c.id} value={c.id}>
              {c.name}
            </option>
          ))}
          <option value={DOC_ONLY}>不发到群，只导入成飞书文档</option>
        </Select>
      </Field>

      {!docOnly && (
        <label className="flex cursor-pointer items-start gap-2.5">
          <input
            type="checkbox"
            className="mt-0.5 size-4 shrink-0 cursor-pointer accent-brand"
            checked={withDoc}
            onChange={(e) => {
              setWithDoc(e.target.checked);
              send.reset();
            }}
          />
          <span className="flex flex-col gap-0.5">
            <span className="text-sm">同时导入成飞书文档</span>
            <span className="text-xs leading-relaxed text-muted">完整的纪要放进一篇飞书文档，卡片上附文档的链接，群里的人都能打开。</span>
          </span>
        </label>
      )}

      {send.isError && <Notice tone="bad">{errorMessage(send.error)}</Notice>}

      <div className="flex flex-wrap items-center justify-end gap-3">
        {repeat && <span className="mr-auto text-[13px] text-muted">{repeat}</span>}
        <Button type="submit" variant="primary" disabled={!chat || busy}>
          {busy ? "正在发送…" : "发送"}
        </Button>
      </div>
    </form>
  );
}

/**
 * 这一版纪要已经发过的话，在发送按钮旁边提醒一下：再发到同一个群，群里会多一张一样的卡片；
 * 同一版纪要只会导入一篇文档，再导入一次也还是那一篇。
 */
function repeatHint(shares: Share[], version: number, chat: string): string {
  if (chat === DOC_ONLY) {
    return shares.some((s) => s.minutes_version === version && s.doc_url) ? "这一版已经导入过飞书文档，在下面的记录里能打开。" : "";
  }
  const sent = shares.some((s) => s.status === "done" && s.chat_id === chat && s.minutes_version === version);
  return sent ? "这一版已经发到过这个群，再发一次群里会多一张卡片。" : "";
}

/** 发送记录里的一条：状态、发到哪个群、第几版、谁发的、什么时候，导入了文档的带上文档的链接，失败的写上原因。 */
function ShareRow({ share: s }: { share: Share }) {
  const status = SHARE_STATUS[s.status];
  return (
    <li className="flex flex-col gap-1.5 py-2.5">
      <div className="flex flex-wrap items-center gap-x-2.5 gap-y-1 text-[13px]">
        <Chip tone={status.tone}>{status.text}</Chip>
        <span className="font-medium">{s.chat_id ? s.chat_name || "飞书群" : s.status === "done" ? "只导入了文档" : "只导入文档"}</span>
        <span className="text-muted">
          第 {s.minutes_version} 版 · {s.requested_by_name || "自动发送"} · {dateTime(s.created_at)}
        </span>
        {s.doc_url && (
          <a href={s.doc_url} target="_blank" rel="noreferrer" className="text-brand hover:text-brand-hover">
            打开飞书文档
          </a>
        )}
      </div>
      {s.status === "failed" && s.error && <p className="text-[13px] leading-relaxed text-bad">{s.error}</p>}
      {/* 还在发送中、但上一次没发出去：写着原因和多久以后重试 */}
      {s.status === "pending" && s.error && <p className="text-[13px] leading-relaxed text-warn">{s.error}</p>}
    </li>
  );
}

import { useMutation, useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { api, errorMessage, queryClient, unwrap } from "../api/client";
import type { components } from "../api/schema";
import { Button, Icon, Notice, Spinner } from "../components/ui";
import { dateTime, duration, SOURCE_LABEL } from "../lib/format";

type Trashed = components["schemas"]["TrashedMeeting"];

function refresh() {
  void queryClient.invalidateQueries({ queryKey: ["trash"] });
  void queryClient.invalidateQueries({ queryKey: ["meetings"] });
}

/** 回收站：删除的会议放在这里，过期前可以恢复，也可以立即彻底删除。 */
export function TrashList() {
  const trash = useQuery({ queryKey: ["trash"], queryFn: () => unwrap(api.GET("/api/trash")) });
  if (trash.isPending) return <Spinner label="正在加载回收站…" />;
  if (trash.isError) return <Notice tone="bad">{errorMessage(trash.error)}</Notice>;
  const { items, days } = trash.data;
  return (
    <div className="flex flex-col gap-2.5">
      <p className="text-[13px] leading-relaxed text-muted">
        删除的会议在这里放 {days} 天，可以恢复；过期后连同录音、转写和纪要一起彻底删除。管理员看得到所有人删除的会议，其他人只看得到自己创建的。
      </p>
      {items.length === 0 ? (
        <div className="rounded-2xl border border-dashed border-line-strong bg-white px-5 py-14 text-center text-sm text-muted">回收站是空的。</div>
      ) : (
        items.map((item) => <TrashRow key={item.id} item={item} />)
      )}
    </div>
  );
}

function TrashRow({ item }: { item: Trashed }) {
  const [confirming, setConfirming] = useState(false);
  const path = { params: { path: { id: item.id } } };
  const restore = useMutation({ mutationFn: () => unwrap(api.POST("/api/trash/{id}/restore", path)), onSuccess: refresh });
  const purge = useMutation({ mutationFn: () => unwrap(api.DELETE("/api/trash/{id}", path)), onSuccess: refresh });
  const busy = restore.isPending || purge.isPending;
  const daysLeft = Math.max(0, Math.ceil((Date.parse(item.purge_at) - Date.now()) / 86_400_000));

  return (
    <div className="flex flex-col gap-3 rounded-xl border border-line bg-white px-5 py-4">
      <div className="flex flex-wrap items-start justify-between gap-x-5 gap-y-3">
        <div className="flex min-w-0 flex-col gap-1.5">
          <div className="truncate text-base font-semibold text-ink-2">{item.title}</div>
          <div className="flex flex-wrap gap-x-3.5 gap-y-1 text-[13px] text-muted">
            <span>{dateTime(item.created_at)}</span>
            {item.duration_sec > 0 && <span>{duration(item.duration_sec)}</span>}
            <span>{SOURCE_LABEL[item.source]}</span>
            <span>{item.owner_name}</span>
          </div>
          <div className="text-[13px] text-muted">
            {item.deleted_by_name || "有人"}在 {dateTime(item.deleted_at)} 删除 ·{" "}
            <span className={daysLeft <= 3 ? "text-bad" : ""}>{daysLeft > 0 ? `${daysLeft} 天后彻底删除` : "马上就会彻底删除"}</span>
          </div>
        </div>
        {item.can_edit && !confirming && (
          <span className="flex gap-2">
            <Button size="sm" disabled={busy} onClick={() => restore.mutate()}>
              <Icon name="retry" size={15} />
              恢复
            </Button>
            <Button size="sm" variant="ghost" disabled={busy} onClick={() => setConfirming(true)}>
              彻底删除
            </Button>
          </span>
        )}
      </div>
      {confirming && (
        <div className="flex flex-wrap items-center justify-between gap-3 rounded-lg bg-bad-tint/60 px-4 py-3 text-sm">
          <span className="text-ink-2">彻底删除以后，录音、转写和纪要都没了，不能恢复。已经发到飞书群的卡片和导入的飞书文档不会跟着删除。</span>
          <span className="flex gap-2">
            <Button size="sm" variant="danger" disabled={busy} onClick={() => purge.mutate()}>
              确定彻底删除
            </Button>
            <Button size="sm" onClick={() => setConfirming(false)}>
              取消
            </Button>
          </span>
        </div>
      )}
      {restore.isError && <Notice tone="bad">{errorMessage(restore.error)}</Notice>}
      {purge.isError && <Notice tone="bad">{errorMessage(purge.error)}</Notice>}
    </div>
  );
}

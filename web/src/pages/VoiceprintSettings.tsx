import { useMutation, useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { api, errorMessage, queryClient, unwrap } from "../api/client";
import type { components } from "../api/schema";
import { Button, Card, Notice, Spinner } from "../components/ui";
import { dateTime, duration } from "../lib/format";
import { Check } from "./FeishuSettings";

type Voiceprint = components["schemas"]["Voiceprint"];

/**
 * 声纹（记住说话人）：给说话人改过一次名字，之后的会议按声音认出这个人。
 * 声音特征属于生物特征信息，默认关着，打开前把存什么、怎么用说清楚；可以随时全部删掉。
 */
export function VoiceprintSettingsSection() {
  const saved = useQuery({
    queryKey: ["settings", "voiceprints"],
    queryFn: () => unwrap(api.GET("/api/settings/voiceprints")),
  });
  const save = useMutation({
    mutationFn: (enabled: boolean) => unwrap(api.PUT("/api/settings/voiceprints", { body: { enabled } })),
    onSuccess: (data) => {
      queryClient.setQueryData(["settings", "voiceprints"], data);
      // 会议页上改名的地方据此决定要不要提示「打开声纹」
      void queryClient.invalidateQueries({ queryKey: ["status"] });
    },
  });

  if (saved.isPending) return <Spinner label="正在加载配置…" />;
  if (saved.isError) return <Notice tone="bad">{errorMessage(saved.error)}</Notice>;
  const enabled = saved.data.enabled;

  return (
    <div className="flex flex-col gap-5">
      <Card className="flex flex-col gap-4 p-6">
        <div className="flex flex-col gap-1">
          <h2 className="text-lg font-bold">声纹</h2>
          <p className="text-[13px] leading-relaxed text-muted">
            在会议里给说话人改过一次名字（比如把「说话人 1」改成「张敏」）以后，之后的会议就能按声音认出他，自动用这个名字，纪要里也直接写名字。认错了在会议里改过来就行。
          </p>
        </div>

        <ul className="flex list-disc flex-col gap-1.5 rounded-xl bg-canvas py-3 pl-8 pr-4 text-[13px] leading-relaxed text-ink-2">
          <li>
            <span className="font-semibold text-ink">存什么：</span>
            每个人一串数字形式的声音特征（声纹），从他在会上说的话里算出来，不能还原成声音。只存在这台服务器上，不会发给任何外部服务，也不会发给大模型。
          </li>
          <li>
            <span className="font-semibold text-ink">怎么用：</span>
            只有有人亲手给说话人起名字时，才把这位说话人的声音记在这个名字下面。之后的会议转写完，拿每位说话人的声音和记下的声纹比对，足够像才自动起名字，会议页上标着「按声音认出」。
          </li>
          <li>
            <span className="font-semibold text-ink">关掉以后：</span>
            不再收集，也不再比对。已经记下的声纹还留着，需要的话在下面删除。
          </li>
        </ul>
        <p className="text-[13px] leading-relaxed text-ink-2">声纹属于个人的生物特征信息，打开之前请先告诉同事，取得他们的同意。</p>

        <Check
          id="voiceprints-enabled"
          label="记住说话人"
          hint={enabled ? "已打开。之后有人给说话人改名，就会记下这个人的声音。" : "已关闭。不收集、不比对任何声音特征。"}
          checked={enabled}
          disabled={save.isPending}
          onChange={(on) => save.mutate(on)}
        />
        {save.isError && <Notice tone="bad">{errorMessage(save.error)}</Notice>}
      </Card>

      <VoiceprintList />
    </div>
  );
}

/** 已经记下的声纹：每人从几场会议里学到、一共多少秒。可以删一个，也可以全部删掉。 */
function VoiceprintList() {
  const list = useQuery({
    queryKey: ["voiceprints"],
    queryFn: () => unwrap(api.GET("/api/voiceprints")),
  });
  const [confirmingAll, setConfirmingAll] = useState(false);
  const removeAll = useMutation({
    mutationFn: () => unwrap(api.DELETE("/api/voiceprints")),
    onSuccess: () => {
      setConfirmingAll(false);
      void queryClient.invalidateQueries({ queryKey: ["voiceprints"] });
    },
  });

  if (list.isPending) return <Spinner label="正在加载声纹…" />;
  if (list.isError) return <Notice tone="bad">{errorMessage(list.error)}</Notice>;
  const items = list.data.items;

  return (
    <Card className="flex flex-col">
      <div className="flex flex-wrap items-center justify-between gap-3 px-6 py-5">
        <div className="flex flex-col gap-1">
          <h3 className="font-semibold">已经记下的声纹</h3>
          <p className="text-[13px] text-muted">{items.length > 0 ? `一共 ${items.length} 个人。` : "还没有记下任何人的声纹。"}</p>
        </div>
        {!confirmingAll && (
          <Button onClick={() => setConfirmingAll(true)}>
            删除全部声纹
          </Button>
        )}
      </div>
      {confirmingAll && (
        <div className="mx-6 mb-5 flex flex-wrap items-center justify-between gap-3 rounded-lg bg-bad-tint/60 px-4 py-3 text-sm">
          <span className="text-ink-2">
            删除所有人的声纹，以及每场会议里存的说话人声音特征，删了不能恢复。会议里已经起好的名字不变。
          </span>
          <span className="flex gap-2">
            <Button size="sm" variant="danger" disabled={removeAll.isPending} onClick={() => removeAll.mutate()}>
              确定全部删除
            </Button>
            <Button size="sm" onClick={() => setConfirmingAll(false)}>
              取消
            </Button>
          </span>
        </div>
      )}
      {removeAll.isError && (
        <div className="px-6 pb-5">
          <Notice tone="bad">{errorMessage(removeAll.error)}</Notice>
        </div>
      )}
      {removeAll.isSuccess && items.length === 0 && (
        <div className="px-6 pb-5">
          <Notice tone="ok">已经全部删除，服务器上不再留有任何声音特征。</Notice>
        </div>
      )}
      {items.length > 0 && (
        <div className="divide-y divide-line border-t border-line">
          {items.map((v) => (
            <VoiceprintRow key={v.id} voiceprint={v} />
          ))}
        </div>
      )}
    </Card>
  );
}

function VoiceprintRow({ voiceprint: v }: { voiceprint: Voiceprint }) {
  const [confirming, setConfirming] = useState(false);
  const remove = useMutation({
    mutationFn: () => unwrap(api.DELETE("/api/voiceprints/{id}", { params: { path: { id: v.id } } })),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ["voiceprints"] }),
  });
  return (
    <div className="flex flex-col gap-2.5 px-6 py-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex flex-col gap-0.5">
          <span className="font-semibold">{v.name}</span>
          <span className="text-xs text-muted">
            从 {v.meetings} 场会议里学到，共 {duration(Math.round(v.seconds))}的说话 · 更新于 {dateTime(v.updated_at)}
          </span>
        </div>
        {!confirming && (
          <Button size="sm" variant="ghost" onClick={() => setConfirming(true)}>
            删除
          </Button>
        )}
      </div>
      {confirming && (
        <div className="flex flex-wrap items-center justify-between gap-3 rounded-lg bg-bad-tint/60 px-4 py-3 text-sm">
          <span className="text-ink-2">删除以后不再按声音认出{v.name}，会议里已经起好的名字不变。以后再有人把说话人改成这个名字，会重新开始记。</span>
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

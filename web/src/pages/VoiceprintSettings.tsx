import { useMutation, useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { api, errorMessage, queryClient, unwrap } from "../api/client";
import type { components } from "../api/schema";
import { Button, Card, Notice, Spinner } from "../components/ui";
import { isEn } from "../i18n/lang";
import { tr, useLang } from "../i18n/react";
import { dateTime, duration } from "../lib/format";
import { Check } from "./FeishuSettings";

type Voiceprint = components["schemas"]["Voiceprint"];

/**
 * 声纹（记住说话人）：给说话人改过一次名字，之后的会议按声音认出这个人。
 * 声音特征属于生物特征信息，默认关着，打开前把存什么、怎么用说清楚；可以随时全部删掉。
 */
export function VoiceprintSettingsSection() {
  useLang();
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

  if (saved.isPending) return <Spinner label={tr("正在加载配置…", "Loading settings…")} />;
  if (saved.isError) return <Notice tone="bad">{errorMessage(saved.error)}</Notice>;
  const enabled = saved.data.enabled;

  return (
    <div className="flex flex-col gap-5">
      <Card className="flex flex-col gap-4 p-6">
        <div className="flex flex-col gap-1">
          <h2 className="text-lg font-bold">{tr("声纹", "Voiceprints")}</h2>
          <p className="text-[13px] leading-relaxed text-muted">
            {tr(
              "在会议里给说话人改过一次名字（比如把「说话人 1」改成「张敏」）以后，之后的会议就能按声音认出他，自动用这个名字，纪要里也直接写名字。认错了在会议里改过来就行。",
              "After you rename a speaker once in a meeting (e.g. changing \"Speaker 1\" to \"Alex\"), future meetings can recognize them by voice and fill in their name automatically in transcripts and minutes. If misidentified, simply rename them in the meeting.",
            )}
          </p>
        </div>

        <ul className="flex list-disc flex-col gap-1.5 rounded-xl bg-canvas py-3 pl-8 pr-4 text-[13px] leading-relaxed text-ink-2">
          <li>
            <span className="font-semibold text-ink">{tr("存什么：", "What is stored: ")}</span>
            {tr(
              "每个人一串数字形式的声音特征（声纹），从他在会上说的话里算出来，不能还原成声音。只存在这台服务器上，不会发给任何外部服务，也不会发给大模型。",
              "A numeric voice embedding computed from their speech in meetings, which cannot reconstruct audio. Stored strictly on this server — never sent to external services or LLMs.",
            )}
          </li>
          <li>
            <span className="font-semibold text-ink">{tr("怎么用：", "How it works: ")}</span>
            {tr(
              "只有有人亲手给说话人起名字时，才把这位说话人的声音记在这个名字下面。之后的会议转写完，拿每位说话人的声音和记下的声纹比对，足够像才自动起名字，会议页上标着「按声音认出」。",
              "Voice embeddings are saved only when someone manually names a speaker. When future meetings finish transcribing, speakers with a close voice match are named automatically and marked \"Matched by voice\".",
            )}
          </li>
          <li>
            <span className="font-semibold text-ink">{tr("关掉以后：", "When turned off: ")}</span>
            {tr(
              "不再收集，也不再比对。已经记下的声纹还留着，需要的话在下面删除。",
              "No new voiceprints are collected or matched. Previously saved voiceprints remain stored unless deleted below.",
            )}
          </li>
        </ul>
        <p className="text-[13px] leading-relaxed text-ink-2">
          {tr("声纹属于个人的生物特征信息，打开之前请先告诉同事，取得他们的同意。", "Voiceprints are biometric data — inform your teammates and obtain their consent before enabling.")}
        </p>

        <Check
          id="voiceprints-enabled"
          label={tr("记住说话人", "Remember speakers by voice")}
          hint={
            enabled
              ? tr("已打开。之后有人给说话人改名，就会记下这个人的声音。", "Enabled. Renaming a speaker will save their voice embedding.")
              : tr("已关闭。不收集、不比对任何声音特征。", "Disabled. No voice features are collected or matched.")
          }
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
  useLang();
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

  if (list.isPending) return <Spinner label={tr("正在加载声纹…", "Loading voiceprints…")} />;
  if (list.isError) return <Notice tone="bad">{errorMessage(list.error)}</Notice>;
  const items = list.data.items;

  return (
    <Card className="flex flex-col">
      <div className="flex flex-wrap items-center justify-between gap-3 px-6 py-5">
        <div className="flex flex-col gap-1">
          <h3 className="font-semibold">{tr("已经记下的声纹", "Saved Voiceprints")}</h3>
          <p className="text-[13px] text-muted">
            {items.length > 0
              ? isEn()
                ? `${items.length} speaker${items.length === 1 ? "" : "s"} saved.`
                : `一共 ${items.length} 个人。`
              : tr("还没有记下任何人的声纹。", "No voiceprints saved yet.")}
          </p>
        </div>
        {!confirmingAll && (
          <Button onClick={() => setConfirmingAll(true)}>
            {tr("删除全部声纹", "Delete all voiceprints")}
          </Button>
        )}
      </div>
      {confirmingAll && (
        <div className="mx-6 mb-5 flex flex-wrap items-center justify-between gap-3 rounded-lg bg-bad-tint/60 px-4 py-3 text-sm">
          <span className="text-ink-2">
            {tr(
              "删除所有人的声纹，以及每场会议里存的说话人声音特征，删了不能恢复。会议里已经起好的名字不变。",
              "Permanently delete all saved voiceprints and per-meeting speaker embeddings. Existing speaker names in meetings remain unchanged.",
            )}
          </span>
          <span className="flex gap-2">
            <Button size="sm" variant="danger" disabled={removeAll.isPending} onClick={() => removeAll.mutate()}>
              {tr("确定全部删除", "Delete all")}
            </Button>
            <Button size="sm" onClick={() => setConfirmingAll(false)}>
              {tr("取消", "Cancel")}
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
          <Notice tone="ok">{tr("已经全部删除，服务器上不再留有任何声音特征。", "All voiceprints and embeddings have been deleted from the server.")}</Notice>
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
  useLang();
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
            {isEn()
              ? `Learned from ${v.meetings} meeting${v.meetings === 1 ? "" : "s"}, ${duration(Math.round(v.seconds))} of speech · Updated ${dateTime(v.updated_at)}`
              : `从 ${v.meetings} 场会议里学到，共 ${duration(Math.round(v.seconds))}的说话 · 更新于 ${dateTime(v.updated_at)}`}
          </span>
        </div>
        {!confirming && (
          <Button size="sm" variant="ghost" onClick={() => setConfirming(true)}>
            {tr("删除", "Delete")}
          </Button>
        )}
      </div>
      {confirming && (
        <div className="flex flex-wrap items-center justify-between gap-3 rounded-lg bg-bad-tint/60 px-4 py-3 text-sm">
          <span className="text-ink-2">
            {isEn()
              ? `Stop matching ${v.name} by voice in future meetings. Existing names in meetings stay unchanged; renaming a speaker to ${v.name} again will start learning anew.`
              : `删除以后不再按声音认出${v.name}，会议里已经起好的名字不变。以后再有人把说话人改成这个名字，会重新开始记。`}
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

import { useMutation, useQuery } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import Markdown from "react-markdown";
import { AppLink } from "../components/AppLink";
import remarkGfm from "remark-gfm";
import { api, errorMessage, queryClient, unwrap } from "../api/client";
import type { Meeting, MinutesVersion } from "../api/client";
import { Button, Icon, Notice, Select, Spinner } from "../components/ui";
import { dateTime, parseClock, VERSION_SOURCE_LABEL } from "../lib/format";
import { useTemplates } from "./UploadDialog";

// 纪要里的时间点，如 [12:34] 或 [1:02:03]。后面紧跟括号的是已有的 Markdown 链接，不动它
const TIMESTAMP = /\[((?:\d{1,2}:)?\d{1,2}:\d{2})\](?!\()/g;

/** 把纪要里的时间点变成链接，渲染时再换成「点了就跳过去播放」的按钮。 */
function linkTimestamps(markdown: string): string {
  return markdown.replace(TIMESTAMP, "[$1](#t=$1)");
}

type Props = {
  meeting: Meeting;
  hasTranscript: boolean;
  llmReady: boolean;
  isAdmin: boolean;
  canSeek: boolean;
  onSeek: (sec: number) => void;
};

/** 纪要：查看、编辑、重新生成、切换历史版本。每次修改都是一个新版本，旧的不会丢。 */
export function MinutesPane({ meeting, hasTranscript, llmReady, isAdmin, canSeek, onSeek }: Props) {
  const id = meeting.id;
  // 0 表示看当前版本
  const [version, setVersion] = useState(0);
  const [mode, setMode] = useState<"view" | "edit" | "regenerate">("view");
  const [draft, setDraft] = useState("");
  const [template, setTemplate] = useState(meeting.template);
  const templates = useTemplates();
  // 会议原来用的自定义模板可能已经被管理员删掉了，那就改用通用模板（后台写纪要时也是这样）
  const templateItems = templates.data?.items;
  const chosen = !templateItems || templateItems.some((t) => t.id === template) ? template : "general";
  const busy = meeting.status === "queued" || meeting.status === "processing";

  const minutes = useQuery({
    queryKey: ["minutes", id, version],
    queryFn: () => unwrap(api.GET("/api/meetings/{id}/minutes", { params: { path: { id }, query: { version: version || undefined } } })),
  });

  // 有了新版本（重新生成完成、别人编辑过）就回到当前版本
  useEffect(() => setVersion(0), [meeting.minutes_version]);

  const save = useMutation({
    mutationFn: () => unwrap(api.PUT("/api/meetings/{id}/minutes", { params: { path: { id } }, body: { content: draft } })),
    onSuccess: () => {
      setMode("view");
      void queryClient.invalidateQueries({ queryKey: ["minutes", id] });
      void queryClient.invalidateQueries({ queryKey: ["meeting", id] });
    },
  });

  const regenerate = useMutation({
    mutationFn: () => unwrap(api.POST("/api/meetings/{id}/minutes/regenerate", { params: { path: { id } }, body: { template: chosen } })),
    onSuccess: (updated) => {
      setMode("view");
      queryClient.setQueryData(["meeting", id], updated);
    },
  });

  if (minutes.isPending) return <Spinner label="正在加载纪要…" />;
  if (minutes.isError) {
    return (
      <div className="p-6">
        <Notice tone="bad">{errorMessage(minutes.error)}</Notice>
      </div>
    );
  }

  const data = minutes.data;
  const versions = data.versions ?? [];
  const templateSelect = (
    <Select aria-label="纪要模板" className="w-40" value={chosen} onChange={(e) => setTemplate(e.target.value)}>
      {(templateItems ?? []).map((t) => (
        <option key={t.id} value={t.id}>
          {t.name}
        </option>
      ))}
    </Select>
  );

  // 还没有纪要
  if (data.version === 0) {
    return (
      <div className="flex flex-col items-center gap-4 px-6 py-12 text-center text-sm leading-relaxed text-muted">
        {busy ? (
          <span>纪要正在生成，完成后会出现在这里。</span>
        ) : !hasTranscript ? (
          <span>转写完成后才能生成纪要。</span>
        ) : !llmReady ? (
          <>
            <span>
              转写已经完成，但还没有配置写纪要用的大模型。
              {isAdmin
                ? "配置好以后回到这里点「生成纪要」。"
                : meeting.can_edit
                  ? "请管理员在「设置」里配置，之后回到这里点「生成纪要」。"
                  : `请管理员在「设置」里配置，之后 ${meeting.owner_name} 或管理员可以在这里生成纪要。`}
            </span>
            {isAdmin && (
              <AppLink to="/settings/llm" className="font-medium text-brand hover:text-brand-hover">
                去配置大模型
              </AppLink>
            )}
          </>
        ) : !meeting.can_edit ? (
          <span>转写已经完成，还没有纪要。{meeting.owner_name} 或管理员可以在这里生成。</span>
        ) : (
          <>
            <span>转写已经完成，还没有纪要。</span>
            {regenerate.isError && <Notice tone="bad">{errorMessage(regenerate.error)}</Notice>}
            <div className="flex flex-wrap items-center justify-center gap-2.5">
              {templateSelect}
              <Button variant="primary" disabled={regenerate.isPending} onClick={() => regenerate.mutate()}>
                生成纪要
              </Button>
            </div>
          </>
        )}
      </div>
    );
  }

  const current = versions.find((v) => v.version === data.version);
  const isLatest = data.version === meeting.minutes_version;

  return (
    <div className="flex flex-wrap items-start">
      <div className="flex min-w-0 flex-[999_1_480px] flex-col gap-4 px-7 pb-7 pt-5">
        <div className="flex flex-wrap items-center justify-between gap-2.5 border-b border-line pb-3">
          <span className="text-xs text-muted">{current ? versionLabel(current) : `v${data.version}`}</span>
          {mode === "view" && isLatest && meeting.can_edit && (
            <div className="flex gap-2">
              <Button
                size="sm"
                disabled={busy}
                onClick={() => {
                  setDraft(data.content);
                  setMode("edit");
                }}
              >
                <Icon name="edit" size={15} />
                编辑
              </Button>
              <Button size="sm" disabled={busy || !llmReady} title={llmReady ? undefined : "还没有配置大模型"} onClick={() => setMode("regenerate")}>
                <Icon name="retry" size={15} />
                重新生成
              </Button>
            </div>
          )}
        </div>

        {!isLatest && (
          <Notice tone="warn">
            你在看旧版本 v{data.version}。
            <button type="button" className="ml-2 cursor-pointer font-semibold underline" onClick={() => setVersion(0)}>
              回到当前版本
            </button>
          </Notice>
        )}

        {mode === "regenerate" && (
          <div className="flex flex-col gap-3 rounded-xl border border-brand/25 bg-brand-tint/50 p-4">
            <b className="text-sm font-semibold">重新生成纪要</b>
            <div className="flex flex-wrap items-center gap-2.5">
              {templateSelect}
              <Button variant="primary" disabled={regenerate.isPending} onClick={() => regenerate.mutate()}>
                重新生成
              </Button>
              <Button onClick={() => setMode("view")}>取消</Button>
            </div>
            <span className="text-xs leading-relaxed text-muted">会生成一个新版本。现在这一版（包括手动改过的内容）保留在右边的版本记录里，随时能回去看。</span>
            {regenerate.isError && <Notice tone="bad">{errorMessage(regenerate.error)}</Notice>}
          </div>
        )}

        {mode === "edit" ? (
          <div className="flex flex-col gap-2.5">
            <textarea
              aria-label="编辑纪要"
              className="min-h-[420px] w-full rounded-lg border border-line-strong p-3 font-mono text-[13px] leading-relaxed focus:outline-2 focus:-outline-offset-1 focus:outline-brand"
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
            />
            {save.isError && <Notice tone="bad">{errorMessage(save.error)}</Notice>}
            <div className="flex flex-wrap items-center justify-between gap-2">
              <span className="text-xs text-muted">Markdown 格式。保存后生成一个新版本，原来的版本保留。</span>
              <div className="flex gap-2">
                <Button size="sm" onClick={() => setMode("view")}>
                  取消
                </Button>
                <Button size="sm" variant="primary" disabled={save.isPending || !draft.trim()} onClick={() => save.mutate()}>
                  保存
                </Button>
              </div>
            </div>
          </div>
        ) : (
          <div className="prose-minutes">
            <Markdown
              remarkPlugins={[remarkGfm]}
              components={{
                a({ href, children }) {
                  if (href?.startsWith("#t=")) {
                    const sec = parseClock(href.slice(3));
                    if (!canSeek) return <span className="tabular-nums text-muted">[{children}]</span>;
                    return (
                      <button
                        type="button"
                        title="从这里开始播放"
                        onClick={() => onSeek(sec)}
                        className="mr-1 inline-flex min-h-6 cursor-pointer items-center rounded-md bg-brand-tint px-1.5 align-baseline text-xs font-semibold tabular-nums text-brand-ink hover:bg-[#d7e3fa]"
                      >
                        {children}
                      </button>
                    );
                  }
                  return (
                    <a href={href} target="_blank" rel="noreferrer noopener" className="text-brand underline hover:text-brand-hover">
                      {children}
                    </a>
                  );
                },
              }}
            >
              {linkTimestamps(data.content)}
            </Markdown>
          </div>
        )}
      </div>

      <aside className="flex flex-[1_1_220px] flex-col gap-1 border-line px-5 pb-6 pt-5 lg:border-l">
        <b className="mb-1 text-sm font-semibold">版本记录</b>
        {versions.map((v) => (
          <button
            key={v.version}
            type="button"
            onClick={() => setVersion(v.version === meeting.minutes_version ? 0 : v.version)}
            aria-current={v.version === data.version}
            className={`flex cursor-pointer flex-col gap-0.5 rounded-lg px-2.5 py-2 text-left text-[13px] hover:bg-canvas ${
              v.version === data.version ? "bg-subtle" : ""
            }`}
          >
            <span>
              <b className="font-semibold">v{v.version}</b>　{VERSION_SOURCE_LABEL[v.source]}
              {v.author ? ` · ${v.author}` : ""}
            </span>
            <span className="text-xs text-muted">{dateTime(v.created_at)}</span>
          </button>
        ))}
      </aside>
    </div>
  );
}

function versionLabel(v: MinutesVersion): string {
  const who = v.author ? `${v.author} ` : "";
  return `v${v.version} · ${who}${VERSION_SOURCE_LABEL[v.source]}于 ${dateTime(v.created_at)}`;
}

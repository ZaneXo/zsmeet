import { useMutation } from "@tanstack/react-query";
import { useState } from "react";
import type { FormEvent } from "react";
import { api, errorMessage, queryClient, unwrap } from "../api/client";
import type { components } from "../api/schema";
import { Button, Card, Chip, Field, Input, Notice, Spinner } from "../components/ui";
import { tr, useLang } from "../i18n/react";
import { useTemplates } from "./UploadDialog";

type Template = components["schemas"]["Template"];

const GUIDE_EXAMPLE_ZH = `1. 摘要：两三句话说清楚这次评审的结论
2. 需求变更：逐条列出改了什么、谁提的、原因
3. 风险：可能影响上线时间的问题
4. 待办：每条写清楚谁、做什么、什么时候之前`;

const GUIDE_EXAMPLE_EN = `1. Summary: 2–3 sentences stating the outcome of this review
2. Scope Changes: List each change, who raised it, and why
3. Risks: Issues that could impact the release schedule
4. Action Items: Owner, action, and deadline for each item`;

function refresh() {
  // 上传、录音、重新生成时选模板的下拉框都用这份列表
  void queryClient.invalidateQueries({ queryKey: ["templates"] });
}

/** 纪要模板：内置的四种只能看；管理员可以自己加模板，规定纪要要有哪几部分、每部分写什么。 */
export function TemplateSettingsSection() {
  useLang();
  const templates = useTemplates();
  const [creating, setCreating] = useState(false);
  if (templates.isPending) return <Spinner label={tr("正在加载模板…", "Loading templates…")} />;
  if (templates.isError) return <Notice tone="bad">{errorMessage(templates.error)}</Notice>;
  const items = templates.data.items ?? [];

  return (
    <div className="flex flex-col gap-5">
      <Card className="flex flex-col gap-4 p-6">
        <div className="flex flex-col gap-1">
          <h2 className="text-lg font-bold">{tr("纪要模板", "Minutes Templates")}</h2>
          <p className="text-[13px] leading-relaxed text-muted">
            {tr(
              "模板规定纪要要有哪几部分、每部分写什么，大模型照着写。上传录音、网页录音、重新生成纪要时都可以选。内置的四种不能改，可以照着它们写自己的。",
              "Templates define the sections and structure of generated minutes. Available when uploading, recording, or regenerating minutes. The four built-in templates are read-only — use them as inspiration for custom templates.",
            )}
          </p>
        </div>
        {creating ? (
          <TemplateForm
            onDone={() => setCreating(false)}
            submitLabel={tr("新建", "Create")}
            mutate={(body) => unwrap(api.POST("/api/templates", { body }))}
          />
        ) : (
          <div>
            <Button variant="primary" onClick={() => setCreating(true)}>
              {tr("新建模板", "New template")}
            </Button>
          </div>
        )}
      </Card>

      <Card className="flex flex-col divide-y divide-line">
        {items.map((t) => (
          <TemplateRow key={t.id} template={t} />
        ))}
      </Card>
    </div>
  );
}

function TemplateRow({ template: t }: { template: Template }) {
  useLang();
  const [editing, setEditing] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const remove = useMutation({
    mutationFn: () => unwrap(api.DELETE("/api/templates/{id}", { params: { path: { id: t.id } } })),
    onSuccess: refresh,
  });

  if (editing) {
    return (
      <div className="px-6 py-5">
        <TemplateForm
          initial={t}
          submitLabel={tr("保存", "Save")}
          onDone={() => setEditing(false)}
          mutate={(body) => unwrap(api.PUT("/api/templates/{id}", { params: { path: { id: t.id } }, body }))}
        />
      </div>
    );
  }
  return (
    <div className="flex flex-col gap-2.5 px-6 py-5">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <span className="flex items-center gap-2.5">
          <span className="font-semibold">{t.name}</span>
          {!t.custom && <Chip tone="mute">{tr("内置", "Built-in")}</Chip>}
        </span>
        {t.custom && !confirming && (
          <span className="flex gap-2">
            <Button size="sm" onClick={() => setEditing(true)}>
              {tr("修改", "Edit")}
            </Button>
            <Button size="sm" variant="ghost" onClick={() => setConfirming(true)}>
              {tr("删除", "Delete")}
            </Button>
          </span>
        )}
      </div>
      <pre className="whitespace-pre-wrap font-sans text-[13px] leading-relaxed text-ink-2">{t.guide}</pre>
      {confirming && (
        <div className="flex flex-wrap items-center justify-between gap-3 rounded-lg bg-bad-tint/60 px-4 py-3 text-sm">
          <span className="text-ink-2">
            {tr(
              "删除以后，用它写好的纪要不受影响；还选着它的会议以后重新生成时改用通用模板。",
              "Existing minutes generated with this template are unaffected; meetings still using it will fall back to the General template when regenerated.",
            )}
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

function TemplateForm({
  initial,
  submitLabel,
  onDone,
  mutate,
}: {
  initial?: Template;
  submitLabel: string;
  onDone: () => void;
  mutate: (body: { name: string; guide: string }) => Promise<Template>;
}) {
  useLang();
  const [name, setName] = useState(initial?.name ?? "");
  const [guide, setGuide] = useState(initial?.guide ?? "");
  const save = useMutation({
    mutationFn: () => mutate({ name, guide }),
    onSuccess: () => {
      refresh();
      onDone();
    },
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    save.mutate();
  };
  const id = initial?.id ?? "new";
  return (
    <form onSubmit={submit} className="flex flex-col gap-4">
      <Field label={tr("名字", "Name")} htmlFor={`tpl-name-${id}`}>
        <Input
          id={`tpl-name-${id}`}
          className="max-w-sm"
          maxLength={30}
          required
          value={name}
          placeholder={tr("比如：需求评审", "e.g. Spec Review")}
          onChange={(e) => setName(e.target.value)}
        />
      </Field>
      <Field
        label={tr("纪要要写哪几部分", "Sections to include")}
        htmlFor={`tpl-guide-${id}`}
        hint={tr(
          "一行一部分，写清楚这部分写什么。大模型还会照常标注时间点、列出待办的负责人和期限。",
          "One section per line explaining what to cover. The LLM will still include timestamps, action owners, and deadlines automatically.",
        )}
      >
        <textarea
          id={`tpl-guide-${id}`}
          className="min-h-44 w-full rounded-lg border border-line-strong bg-white p-3 text-sm leading-relaxed placeholder:text-muted/70 focus:outline-2 focus:-outline-offset-1 focus:outline-brand"
          maxLength={2000}
          required
          value={guide}
          placeholder={tr(GUIDE_EXAMPLE_ZH, GUIDE_EXAMPLE_EN)}
          onChange={(e) => setGuide(e.target.value)}
        />
      </Field>
      {save.isError && <Notice tone="bad">{errorMessage(save.error)}</Notice>}
      <span className="flex gap-2">
        <Button type="submit" variant="primary" disabled={save.isPending}>
          {submitLabel}
        </Button>
        <Button onClick={onDone}>{tr("取消", "Cancel")}</Button>
      </span>
    </form>
  );
}

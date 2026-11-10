import { useQuery } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import type { DragEvent, FormEvent } from "react";
import { useNavigate } from "react-router";
import { api, errorMessage, queryClient, unwrap } from "../api/client";
import { Button, Field, Icon, Input, Modal, Notice, ProgressBar, Select } from "../components/ui";
import { fileSize } from "../lib/format";
import { MAX_UPLOAD_BYTES, uploadFile } from "../lib/upload";

/** 新建会议时选谁能看。指定具体的人在建好以后的会议页面上设置。 */
export function VisibilityField({ id, value, onChange, disabled }: {
  id: string;
  value: "team" | "private";
  onChange: (v: "team" | "private") => void;
  disabled?: boolean;
}) {
  return (
    <Field label="谁能看" htmlFor={id} hint="建好以后可以在会议页面上改，也可以指定具体的人">
      <Select id={id} className="sm:w-72" value={value} onChange={(e) => onChange(e.target.value === "private" ? "private" : "team")} disabled={disabled}>
        <option value="team">团队里所有人</option>
        <option value="private">只有我（管理员也能看）</option>
      </Select>
    </Field>
  );
}

export function useTemplates() {
  return useQuery({
    queryKey: ["templates"],
    queryFn: () => unwrap(api.GET("/api/templates")),
    // 管理员随时可能加、改、删自定义模板
    staleTime: 60_000,
  });
}

/** 上传录音文件：选文件、填信息、切片上传，传完跳到会议页看处理进度。 */
export function UploadDialog({ onClose }: { onClose: () => void }) {
  const navigate = useNavigate();
  const templates = useTemplates();
  const picker = useRef<HTMLInputElement>(null);
  const abort = useRef<AbortController | null>(null);

  const [file, setFile] = useState<File | null>(null);
  const [title, setTitle] = useState("");
  const [template, setTemplate] = useState("general");
  const [hotwords, setHotwords] = useState("");
  const [visibility, setVisibility] = useState<"team" | "private">("team");
  const [dragging, setDragging] = useState(false);
  const [sent, setSent] = useState<number | null>(null);
  const [error, setError] = useState("");

  const busy = sent !== null;

  // 关掉弹窗时中止还在进行的上传
  useEffect(() => () => abort.current?.abort(), []);

  const choose = (f: File | undefined) => {
    if (!f) return;
    if (f.size > MAX_UPLOAD_BYTES) {
      setError("文件超过 4 GB，请压缩或拆分后再上传");
      return;
    }
    setError("");
    setFile(f);
    // 标题默认用文件名（去掉扩展名），用户改过就不覆盖
    if (!title) setTitle(f.name.replace(/\.[^.]+$/, ""));
  };

  const onDrop = (e: DragEvent) => {
    e.preventDefault();
    setDragging(false);
    choose(e.dataTransfer.files[0]);
  };

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!file || busy) return;
    setError("");
    setSent(0);
    const controller = new AbortController();
    abort.current = controller;
    try {
      const meeting = await unwrap(api.POST("/api/meetings", { body: { title, source: "upload", template, hotwords, visibility } }));
      const chunks = await uploadFile(meeting.id, file, setSent, controller.signal);
      await unwrap(api.POST("/api/meetings/{id}/complete", { params: { path: { id: meeting.id } }, body: { chunks } }));
      void queryClient.invalidateQueries({ queryKey: ["meetings"] });
      navigate(`/meetings/${meeting.id}`);
    } catch (err) {
      if (controller.signal.aborted) return;
      setError(errorMessage(err));
      setSent(null);
      // 建到一半的会议会留在列表里显示「上传未完成」
      void queryClient.invalidateQueries({ queryKey: ["meetings"] });
    }
  };

  return (
    <Modal title="上传录音文件" onClose={onClose}>
      <form onSubmit={submit} className="flex flex-col gap-4">
        <input ref={picker} type="file" accept="audio/*,video/*,.m4a,.mp3,.wav,.aac,.flac,.ogg,.opus,.wma,.amr,.mp4,.mov,.mkv,.webm" className="hidden" onChange={(e) => choose(e.target.files?.[0])} />

        {file ? (
          <div className="flex items-center gap-3 rounded-xl border border-line px-3.5 py-3">
            <span className="flex size-9 shrink-0 items-center justify-center rounded-lg bg-brand-tint text-brand">
              <Icon name="file" />
            </span>
            <span className="flex min-w-0 flex-1 flex-col gap-0.5">
              <b className="truncate text-sm font-semibold">{file.name}</b>
              <span className="text-xs text-muted">{fileSize(file.size)}</span>
            </span>
            {!busy && (
              <Button size="sm" onClick={() => picker.current?.click()}>
                换一个
              </Button>
            )}
          </div>
        ) : (
          <button
            type="button"
            onClick={() => picker.current?.click()}
            onDragOver={(e) => {
              e.preventDefault();
              setDragging(true);
            }}
            onDragLeave={() => setDragging(false)}
            onDrop={onDrop}
            className={`flex cursor-pointer flex-col items-center gap-2 rounded-xl border-[1.5px] border-dashed px-4 py-8 text-ink-2 ${
              dragging ? "border-brand bg-brand-tint" : "border-line-strong bg-canvas hover:border-muted"
            }`}
          >
            <span className="text-brand">
              <Icon name="upload" size={28} />
            </span>
            <span className="text-sm font-semibold">把文件拖到这里，或点击选择</span>
            <span className="text-xs text-muted">mp3、m4a、wav、mp4、mov 等常见音视频格式，视频只取声音。最大 4 GB</span>
          </button>
        )}

        <Field label="会议标题" htmlFor="up-title" hint="留空会用上传时间起一个，之后可以改">
          <Input id="up-title" value={title} onChange={(e) => setTitle(e.target.value)} maxLength={200} disabled={busy} />
        </Field>
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <Field label="纪要模板" htmlFor="up-template">
            <Select id="up-template" value={template} onChange={(e) => setTemplate(e.target.value)} disabled={busy}>
              {(templates.data?.items ?? []).map((t) => (
                <option key={t.id} value={t.id}>
                  {t.name}
                </option>
              ))}
            </Select>
          </Field>
          <Field label="本场热词" htmlFor="up-hotwords">
            <Input id="up-hotwords" value={hotwords} onChange={(e) => setHotwords(e.target.value)} maxLength={500} placeholder="人名、产品名，用逗号分开" disabled={busy} />
          </Field>
        </div>
        <p className="-mt-2 text-xs leading-relaxed text-muted">热词会交给大模型，纪要里这些词被识别成近音字时会纠正过来。</p>
        <VisibilityField id="up-visibility" value={visibility} onChange={setVisibility} disabled={busy} />

        {busy && file && (
          <div className="flex flex-col gap-2">
            <div className="flex justify-between text-sm">
              <b className="font-semibold">正在上传</b>
              <span className="tabular-nums">
                {fileSize(sent)} / {fileSize(file.size)}
              </span>
            </div>
            <ProgressBar value={file.size ? sent / file.size : 1} />
            <p className="text-xs text-muted">网络断了会自动重试。上传完成前请不要关闭这个页面。</p>
          </div>
        )}

        {error && <Notice tone="bad">{error}</Notice>}

        <div className="flex justify-end gap-2.5">
          <Button onClick={onClose}>{busy ? "取消上传" : "取消"}</Button>
          <Button type="submit" variant="primary" disabled={!file || busy}>
            上传并处理
          </Button>
        </div>
      </form>
    </Modal>
  );
}

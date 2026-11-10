import type { Meeting } from "../api/client";

/** 秒数写成 mm:ss，超过一小时写成 h:mm:ss。和后端转写里的时间点格式一致。 */
export function clock(sec: number): string {
  const s = Math.floor(sec);
  const mm = String(Math.floor((s % 3600) / 60)).padStart(2, "0");
  const ss = String(s % 60).padStart(2, "0");
  return s >= 3600 ? `${Math.floor(s / 3600)}:${mm}:${ss}` : `${mm}:${ss}`;
}

/** 把纪要里的 [mm:ss] 或 [h:mm:ss] 换算成秒。 */
export function parseClock(text: string): number {
  return text.split(":").reduce((total, part) => total * 60 + Number(part), 0);
}

export function duration(sec: number): string {
  if (sec < 60) return `${sec} 秒`;
  const minutes = Math.round(sec / 60);
  if (minutes < 60) return `${minutes} 分钟`;
  const rest = minutes % 60;
  return rest ? `${Math.floor(minutes / 60)} 小时 ${rest} 分` : `${Math.floor(minutes / 60)} 小时`;
}

export function dateTime(iso: string): string {
  const d = new Date(iso);
  const time = `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`;
  const year = d.getFullYear() === new Date().getFullYear() ? "" : `${d.getFullYear()}年`;
  return `${year}${d.getMonth() + 1}月${d.getDate()}日 ${time}`;
}

/** 会议列表的分组标题：今天、昨天、本周之外按月。 */
export function dayGroup(iso: string): string {
  const d = new Date(iso);
  const startOfToday = new Date();
  startOfToday.setHours(0, 0, 0, 0);
  const days = Math.floor((startOfToday.getTime() - new Date(d).setHours(0, 0, 0, 0)) / 86_400_000);
  if (days <= 0) return "今天";
  if (days === 1) return "昨天";
  if (days < 7) return "最近一周";
  const year = d.getFullYear() === startOfToday.getFullYear() ? "" : `${d.getFullYear()}年`;
  return `${year}${d.getMonth() + 1}月`;
}

export const SOURCE_LABEL: Record<Meeting["source"], string> = {
  upload: "上传文件",
  record: "网页录音",
  feishu: "飞书会议",
};

export const STAGE_LABEL: Record<Meeting["stage"], string> = {
  "": "准备中",
  models: "下载识别模型",
  assemble: "接收与转码",
  transcribe: "识别文字",
  diarize: "区分说话人",
  minutes: "生成纪要",
};

export const VERSION_SOURCE_LABEL = {
  auto: "自动生成",
  edit: "手动编辑",
  regenerate: "重新生成",
} as const;

export function speakerName(speaker: number): string {
  return speaker < 0 ? "说话人" : `说话人 ${speaker + 1}`;
}

// 蓝、橙、绿、紫……相邻的说话人在色相和明度上都拉开，色弱也分得出
const SPEAKER_COLORS = ["#2b59c3", "#b4540a", "#1f7a4d", "#7a3fb8", "#0f7285", "#a8324f", "#5c5c57"];

export function speakerColor(speaker: number): string {
  if (speaker < 0) return "#5c5c57";
  return SPEAKER_COLORS[speaker % SPEAKER_COLORS.length] ?? "#5c5c57";
}

export function fileSize(bytes: number): string {
  if (bytes < 1024 * 1024) return `${Math.max(1, Math.round(bytes / 1024))} KB`;
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
  return `${(bytes / 1024 / 1024 / 1024).toFixed(2)} GB`;
}

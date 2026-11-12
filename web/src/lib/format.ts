import type { Meeting } from "../api/client";

/** 秒数写成 mm:ss，超过一小时写成 h:mm:ss。和后端转写里的时间点格式一致。 */
export function clock(sec: number): string {
  const s = Math.floor(sec);
  const mm = String(Math.floor((s % 3600) / 60)).padStart(2, "0");
  const ss = String(s % 60).padStart(2, "0");
  return s >= 3600 ? `${Math.floor(s / 3600)}:${mm}:${ss}` : `${mm}:${ss}`;
}

/** 时:分:秒，录音的计时用（网页录音和桌面版录音都用）。 */
export function hms(sec: number): string {
  const s = Math.max(0, Math.floor(sec));
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${pad(Math.floor(s / 3600))}:${pad(Math.floor((s % 3600) / 60))}:${pad(s % 60)}`;
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
  desktop: "桌面版录音",
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

/** 说话人的名字：起过名字就用名字，没起过是「说话人 N」。names 的键是说话人编号（字符串）。 */
export function speakerName(speaker: number, names?: Record<string, string>): string {
  if (speaker < 0) return "说话人";
  return names?.[String(speaker)] || `说话人 ${speaker + 1}`;
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

/** 一段文字里的一截：mark 为 true 的是命中的词，要高亮。 */
export type Piece = { text: string; mark: boolean };

/**
 * 截取第一个命中的词前后的一小段，长句子不至于把命中的字挤到看不见的地方，截掉的地方用省略号；
 * 再把这一段切成高亮和不高亮的几截（matches 里的词出现的地方都高亮）。
 */
export function snippet(text: string, matches: string[], radius = 28): Piece[] {
  // 长的词先比，免得「Go 语言」里的「Go」先被单独切出去
  const words = matches.filter(Boolean).sort((a, b) => b.length - a.length);
  const positions = words.map((w) => text.indexOf(w)).filter((i) => i >= 0);
  let view = text;
  if (positions.length > 0) {
    const at = Math.min(...positions);
    const before = Array.from(text.slice(0, at));
    const after = Array.from(text.slice(at));
    view =
      (before.length > radius ? "…" + before.slice(-radius).join("") : before.join("")) +
      (after.length > radius * 3 ? after.slice(0, radius * 3).join("") + "…" : after.join(""));
  }
  return pieces(view, words);
}

/** 把 text 切成几截，words 里的词出现的地方标成高亮。 */
export function pieces(text: string, words: string[]): Piece[] {
  const out: Piece[] = [];
  let plain = "";
  for (let i = 0; i < text.length; ) {
    const word = words.find((w) => w && text.startsWith(w, i));
    if (word) {
      if (plain) out.push({ text: plain, mark: false });
      out.push({ text: word, mark: true });
      plain = "";
      i += word.length;
    } else {
      plain += text[i];
      i++;
    }
  }
  if (plain) out.push({ text: plain, mark: false });
  return out;
}

/** 两句接在一起时中间要不要空一格：英文的两句之间要，中文直接接上（和后端 transcript.joinText 一致）。 */
export function needsSpace(before: string, after: string): boolean {
  const last = before.at(-1) ?? "";
  const next = after[0] ?? "";
  return last !== "" && next !== "" && last < "\x80" && next < "\x80" && last !== " " && next !== " ";
}

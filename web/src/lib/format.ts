import type { Meeting } from "../api/client";
import { isEn, tr } from "../i18n/lang.ts";

const MONTHS_SHORT = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
const MONTHS_LONG = [
  "January",
  "February",
  "March",
  "April",
  "May",
  "June",
  "July",
  "August",
  "September",
  "October",
  "November",
  "December",
];

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
  if (sec < 60) return isEn() ? `${sec}s` : `${sec} 秒`;
  const minutes = Math.round(sec / 60);
  if (minutes < 60) return isEn() ? `${minutes} min` : `${minutes} 分钟`;
  const rest = minutes % 60;
  if (isEn()) return rest ? `${Math.floor(minutes / 60)}h ${rest}m` : `${Math.floor(minutes / 60)}h`;
  return rest ? `${Math.floor(minutes / 60)} 小时 ${rest} 分` : `${Math.floor(minutes / 60)} 小时`;
}

export function dateTime(iso: string): string {
  const d = new Date(iso);
  const time = `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`;
  if (isEn()) {
    const yr = d.getFullYear() === new Date().getFullYear() ? "" : `, ${d.getFullYear()}`;
    return `${MONTHS_SHORT[d.getMonth()]} ${d.getDate()}${yr} ${time}`;
  }
  const year = d.getFullYear() === new Date().getFullYear() ? "" : `${d.getFullYear()}年`;
  return `${year}${d.getMonth() + 1}月${d.getDate()}日 ${time}`;
}

/** 会议列表的分组标题：今天、昨天、本周之外按月。 */
export function dayGroup(iso: string): string {
  const d = new Date(iso);
  const startOfToday = new Date();
  startOfToday.setHours(0, 0, 0, 0);
  const days = Math.floor((startOfToday.getTime() - new Date(d).setHours(0, 0, 0, 0)) / 86_400_000);
  if (days <= 0) return tr("今天", "Today");
  if (days === 1) return tr("昨天", "Yesterday");
  if (days < 7) return tr("最近一周", "Past 7 days");
  if (isEn()) {
    const month = MONTHS_LONG[d.getMonth()] ?? "";
    return d.getFullYear() === startOfToday.getFullYear() ? month : `${month} ${d.getFullYear()}`;
  }
  const year = d.getFullYear() === startOfToday.getFullYear() ? "" : `${d.getFullYear()}年`;
  return `${year}${d.getMonth() + 1}月`;
}

export const SOURCE_LABEL: Record<Meeting["source"], string> = {
  get upload() {
    return tr("上传文件", "Uploaded file");
  },
  get record() {
    return tr("网页录音", "Web recording");
  },
  get feishu() {
    return tr("飞书会议", "Feishu meeting");
  },
  get desktop() {
    return tr("桌面版录音", "Desktop recording");
  },
};

export const STAGE_LABEL: Record<Meeting["stage"], string> = {
  get ""() {
    return tr("准备中", "Preparing");
  },
  get models() {
    return tr("下载识别模型", "Downloading ASR models");
  },
  get assemble() {
    return tr("接收与转码", "Receiving & transcoding");
  },
  get transcribe() {
    return tr("识别文字", "Transcribing");
  },
  get diarize() {
    return tr("区分说话人", "Speaker diarization");
  },
  get correct() {
    return tr("校对文字", "Proofreading transcript");
  },
  get minutes() {
    return tr("生成纪要", "Generating minutes");
  },
};

export const VERSION_SOURCE_LABEL: Record<"auto" | "edit" | "regenerate", string> = {
  get auto() {
    return tr("自动生成", "Auto-generated");
  },
  get edit() {
    return tr("手动编辑", "Edited");
  },
  get regenerate() {
    return tr("重新生成", "Regenerated");
  },
};

/** 说话人的名字：起过名字就用名字，没起过是「说话人 N」。names 的键是说话人编号（字符串）。 */
export function speakerName(speaker: number, names?: Record<string, string>): string {
  if (speaker < 0) return tr("说话人", "Speaker");
  return names?.[String(speaker)] || (isEn() ? `Speaker ${speaker + 1}` : `说话人 ${speaker + 1}`);
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

/** 语音识别支持的语言：auto 自动识别多语种与混说，其余固定单语种。单场会议留空表示跟随团队默认。 */
export type ASRLanguage = "auto" | "zh" | "en" | "yue" | "ja" | "ko";
export type MeetingASRLanguage = "" | ASRLanguage;

export const ASR_LANGUAGES: readonly ASRLanguage[] = ["auto", "zh", "en", "yue", "ja", "ko"];

export function asrLanguageLabel(lang: MeetingASRLanguage, short = false): string {
  switch (lang) {
    case "":
      return tr("跟随团队默认", "Follow team default");
    case "auto":
      return short ? tr("自动识别多语种", "Multilingual auto") : tr("自动识别（多语种与混合语音）", "Auto-detect (multilingual & mixed)");
    case "zh":
      return short ? tr("中文", "Chinese") : tr("中文（普通话，含中英夹杂）", "Chinese (Mandarin, incl. English terms)");
    case "en":
      return tr("英语", "English");
    case "yue":
      return short ? tr("粤语", "Cantonese") : tr("粤语（广东话）", "Cantonese");
    case "ja":
      return tr("日语", "Japanese");
    case "ko":
      return tr("韩语", "Korean");
  }
}

function escapeRegExp(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

/** 页内搜索：大小写不敏感地判断 text 里是否包含 keyword。 */
export function includesFold(text: string, keyword: string): boolean {
  if (!keyword) return true;
  return new RegExp(escapeRegExp(keyword), "i").test(text);
}

/** 页内搜索高亮：大小写不敏感切分，并保留原文里的大小写。 */
export function highlightPieces(text: string, keyword: string): Piece[] {
  if (!keyword) return text ? [{ text, mark: false }] : [];
  const re = new RegExp(escapeRegExp(keyword), "gi");
  const out: Piece[] = [];
  let last = 0;
  for (const m of text.matchAll(re)) {
    const at = m.index ?? 0;
    if (at > last) out.push({ text: text.slice(last, at), mark: false });
    out.push({ text: m[0], mark: true });
    last = at + m[0].length;
  }
  if (last < text.length) out.push({ text: text.slice(last), mark: false });
  return out;
}

const WORD_SPACE_RE = /[\x00-\x7f\uac00-\ud7a3\u1100-\u11ff\u3130-\u318f]/;

/** 两句接在一起时中间要不要空一格：英文、韩文的两句之间要，中文、日文直接接上（和后端 transcript.joinText 一致）。 */
export function needsSpace(before: string, after: string): boolean {
  const last = before.at(-1) ?? "";
  const next = after[0] ?? "";
  return last !== "" && next !== "" && last !== " " && next !== " " && WORD_SPACE_RE.test(last) && WORD_SPACE_RE.test(next);
}

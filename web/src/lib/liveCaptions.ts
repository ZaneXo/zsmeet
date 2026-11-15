import type { Segment } from "../api/client";
import { needsSpace } from "./format.ts";

export type CaptionLine = {
  start: number;
  end: number;
  text: string;
  /** true 表示靠近录音末尾、下一片传上来后还可能补全修正的暂定句 */
  provisional: boolean;
};

const MAX_LINE_CHARS = 140;

/**
 * 把实时字幕句子整理成适合阅读的段落行：
 * - 前 finalizedCount 句已定稿：相邻间隔不超过 0.8 秒且总字数不超过 140 字时合成一段；
 * - 末尾剩余的暂定句单独归在后面（provisional = true），不跟已定稿的句子混在同一行，刷新时前面已定稿的行不会跳动。
 */
export function groupCaptionLines(segments: readonly Pick<Segment, "start" | "end" | "text">[], finalizedCount: number): CaptionLine[] {
  const clamped = Math.max(0, Math.min(segments.length, finalizedCount));
  const out: CaptionLine[] = [];
  appendMerged(out, segments.slice(0, clamped), false, 0.8);
  appendMerged(out, segments.slice(clamped), true, 1.2);
  return out;
}

function appendMerged(
  out: CaptionLine[],
  items: readonly Pick<Segment, "start" | "end" | "text">[],
  provisional: boolean,
  maxGapSec: number,
) {
  for (const seg of items) {
    const text = seg.text.trim();
    if (!text) continue;
    const prev = out.at(-1);
    if (
      prev &&
      prev.provisional === provisional &&
      seg.start - prev.end <= maxGapSec &&
      prev.text.length + text.length <= MAX_LINE_CHARS
    ) {
      prev.text += (needsSpace(prev.text, text) ? " " : "") + text;
      prev.end = Math.max(prev.end, seg.end);
    } else {
      out.push({ start: seg.start, end: seg.end, text, provisional });
    }
  }
}

/** 滚动条是不是贴在底部附近（相差不超过 thresholdPx 像素时算贴底，新字幕进来自动往下滚）。 */
export function isNearBottom(scrollTop: number, clientHeight: number, scrollHeight: number, thresholdPx = 36): boolean {
  return scrollHeight - (scrollTop + clientHeight) <= thresholdPx;
}

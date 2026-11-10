// 会议页面网址里的 ?t=：从搜索结果、问会议的出处跳到会议里的某一句。
// 这里只放不依赖 React 的小函数，方便单独测试（jump.test.ts）。

/** 跳到的时间点。slack 是定位句子时的余量，见 parseJump。 */
export type Jump = { sec: number; slack: number };

/**
 * 时间点写进 ?t= 的样子。转写里的句子时间带一位小数（取整的话可能落在上一句里），写成「70.5」「70.0」；
 * 纪要里的时间点只到秒，写成「70」。搜索结果和问会议的出处都用它，同一个时间点只有一种写法。
 */
export function jumpParam(at: number, exact: boolean): string {
  return exact ? at.toFixed(1) : String(Math.floor(at));
}

/**
 * 网址里 ?t= 的秒数；没有或者不是合法的数字时返回 undefined。
 * slack 是定位句子时的余量：转写里的句子时间带小数，是准的；纪要里的时间点只到秒，
 * 那句话可能晚不到一秒才开始，所以多给一秒。
 */
export function parseJump(raw: string | null): Jump | undefined {
  if (raw === null || raw.trim() === "") return undefined;
  const sec = Number(raw);
  if (!Number.isFinite(sec) || sec < 0) return undefined;
  return { sec, slack: raw.includes(".") ? 0.05 : 0.99 };
}

/** 从那句前几秒开始播放，免得开头半句听不到 */
export const LEAD_IN = 3;

/**
 * 在会议页面上点了出处（时间点 at，exact 表示是转写里的句子）以后怎么做：
 * 总是直接从 seek 秒开始播放、把转写定位到 focus；param 是网址里该写的 ?t=，和现在的 current 一样时为 undefined。
 * moved 表示时间点（按数值比较，「70」和「70.0」算同一个）变了：改了网址以后，跟着网址跳转的那段逻辑会再跳一次，要让它跳过。
 */
export function citeJump(current: string | null, at: number, exact: boolean): { seek: number; focus: Jump; param?: string; moved: boolean } {
  const param = jumpParam(at, exact);
  const focus = parseJump(param) ?? { sec: Math.max(0, at), slack: 0.99 };
  return {
    seek: Math.max(0, focus.sec - LEAD_IN),
    focus,
    param: current === param ? undefined : param,
    moved: parseJump(current)?.sec !== focus.sec,
  };
}

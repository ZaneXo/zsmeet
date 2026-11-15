/**
 * 界面语言（简体中文 / English）。
 *
 * 默认跟随浏览器或系统语言（auto）：浏览器首选语言是英文时显示英文，其余情况默认简体中文；
 * 用户也可以在登录页或「设置 → 账号」里手动固定成简体中文或 English（存在 localStorage）。
 *
 * 注意：pnpm -C web test 用 Node 直接跑 *.test.ts，Node 21+ 自带 globalThis.navigator（在英文机器或
 * GitHub Actions 上 navigator.language 是 en-US）。为了不让运行环境的语言改变单测结果，Node 环境下 auto
 * 固定回退到 zh；单测要测英文时显式调 setLangSetting("en") 或 detectBrowserLang(["en-US"])。
 */

export type Lang = "zh" | "en";
export type LangSetting = "auto" | Lang;

const STORAGE_KEY = "zsmeet.lang";

const isNode =
  typeof process !== "undefined" &&
  Boolean((process as { versions?: { node?: string } }).versions?.node);

export function detectBrowserLang(langs?: readonly string[]): Lang {
  const list =
    langs ??
    (isNode || typeof navigator === "undefined"
      ? []
      : navigator.languages?.length
        ? navigator.languages
        : navigator.language
          ? [navigator.language]
          : []);
  for (const raw of list) {
    const tag = raw.trim().toLowerCase();
    if (!tag) continue;
    if (tag === "zh" || tag.startsWith("zh-") || tag.startsWith("zh_")) return "zh";
    if (tag === "en" || tag.startsWith("en-") || tag.startsWith("en_")) return "en";
  }
  return "zh";
}

function readStoredSetting(): LangSetting {
  try {
    if (typeof localStorage !== "undefined" && typeof localStorage.getItem === "function") {
      const v = localStorage.getItem(STORAGE_KEY);
      if (v === "auto" || v === "zh" || v === "en") return v;
    }
  } catch {
    // 隐私模式或测试环境禁用 localStorage 时忽略
  }
  return "auto";
}

let setting: LangSetting = readStoredSetting();
const listeners = new Set<() => void>();

function syncDocumentLang() {
  if (typeof document !== "undefined" && document.documentElement) {
    const en = getLang() === "en";
    document.documentElement.lang = en ? "en" : "zh-CN";
    if (typeof document.title === "string") {
      document.title = en ? "zsmeet Meeting Minutes" : "zsmeet 会议纪要";
    }
  }
}

function notify() {
  syncDocumentLang();
  for (const l of listeners) l();
}

export function getLangSetting(): LangSetting {
  return setting;
}

export function getLang(): Lang {
  return setting === "auto" ? detectBrowserLang() : setting;
}

export function isEn(): boolean {
  return getLang() === "en";
}

export function tr(zh: string, en: string): string {
  return getLang() === "en" ? en : zh;
}

export function acceptLanguageHeader(): string {
  return getLang() === "en" ? "en-US,en;q=0.9" : "zh-CN,zh;q=0.9";
}

export function setLangSetting(next: LangSetting) {
  if (setting === next) return;
  setting = next;
  try {
    if (typeof localStorage !== "undefined" && typeof localStorage.setItem === "function") {
      localStorage.setItem(STORAGE_KEY, next);
    }
  } catch {
    // 忽略存储异常
  }
  notify();
}

export function subscribeLang(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

syncDocumentLang();

if (!isNode && typeof window !== "undefined" && typeof window.addEventListener === "function") {
  window.addEventListener("languagechange", () => {
    if (setting === "auto") notify();
  });
}

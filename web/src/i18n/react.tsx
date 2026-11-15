import { useSyncExternalStore } from "react";
import { queryClient } from "../api/client";
import { Select } from "../components/ui";
import {
  getLang,
  getLangSetting,
  setLangSetting,
  subscribeLang,
  tr,
} from "./lang";
import type { Lang, LangSetting } from "./lang";

export { getLang, getLangSetting, setLangSetting, tr };
export type { Lang, LangSetting };

/** 订阅当前语言。任何直接调 tr(...) 或格式化函数的组件，在顶层调一次 useLang() 就能随语言实时刷新。 */
export function useLang(): Lang {
  return useSyncExternalStore(subscribeLang, getLang);
}

export function useLangSetting(): LangSetting {
  return useSyncExternalStore(subscribeLang, getLangSetting);
}

export function changeLanguage(next: LangSetting) {
  setLangSetting(next);
  // 重新拉取接口数据（内置模板名称、状态提示等由后端按 Accept-Language 返回）
  void queryClient.invalidateQueries();
}

export function LanguageSelect({
  id = "zsmeet-lang",
  compact = false,
  className = "",
}: {
  id?: string;
  compact?: boolean;
  className?: string;
}) {
  useLang();
  const setting = useLangSetting();
  if (compact) {
    return (
      <select
        id={id}
        aria-label={tr("界面语言", "Language")}
        value={setting}
        onChange={(e) => changeLanguage(e.target.value as LangSetting)}
        className={`h-8 cursor-pointer rounded-lg border border-line bg-white px-2.5 text-xs font-medium text-ink-2 hover:border-line-strong focus:outline-2 focus:-outline-offset-1 focus:outline-brand ${className}`}
      >
        <option value="auto">{tr("语言：跟随系统", "Language: System")}</option>
        <option value="zh">简体中文</option>
        <option value="en">English</option>
      </select>
    );
  }
  return (
    <Select
      id={id}
      value={setting}
      onChange={(e) => changeLanguage(e.target.value as LangSetting)}
      className={className}
    >
      <option value="auto">{tr("跟随浏览器 / 系统语言", "Follow browser / system language")}</option>
      <option value="zh">简体中文</option>
      <option value="en">English</option>
    </Select>
  );
}

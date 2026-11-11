import type { ReactNode } from "react";
import { saveExport } from "./desktop";
import { isDesktop } from "./env";

/**
 * 下载（导出）链接。网页版是普通的下载链接；桌面版的窗口里没有浏览器下载，
 * 由 App 取回文件、弹出保存对话框。
 */
export function DownloadLink({ href, className, children }: { href: string; className: string; children: ReactNode }) {
  if (isDesktop) {
    return (
      <button type="button" className={`cursor-pointer text-left ${className}`} onClick={() => void saveExport(href).catch(() => {})}>
        {children}
      </button>
    );
  }
  return (
    <a href={href} download className={className}>
      {children}
    </a>
  );
}

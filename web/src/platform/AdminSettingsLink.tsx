import { useMutation } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { errorMessage } from "../api/client";
import { AppLink } from "../components/AppLink";
import { openServerPage } from "./desktop";
import { isDesktop } from "./env";

type Props = { to: string; className: string; children: ReactNode };

/**
 * 指向管理员设置分区的链接（如 /settings/llm、/settings/voiceprints）。
 * 网页版直接在站内跳转；桌面版的团队设置只在网页里改，点它直接用系统浏览器打开服务器对应页面。
 */
export function AdminSettingsLink(props: Props) {
  if (isDesktop) return <DesktopAdminSettingsLink {...props} />;
  return (
    <AppLink to={props.to} className={props.className}>
      {props.children}
    </AppLink>
  );
}

function DesktopAdminSettingsLink({ to, className, children }: Props) {
  const open = useMutation({ mutationFn: () => openServerPage(to) });
  return (
    <>
      <button type="button" className={`cursor-pointer ${className}`} onClick={() => open.mutate()}>
        {children}
      </button>
      {open.isError && <span className="ml-1.5 text-xs text-bad">{errorMessage(open.error)}</span>}
    </>
  );
}

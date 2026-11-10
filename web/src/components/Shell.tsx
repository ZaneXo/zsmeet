import { useMutation } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { useLocation } from "react-router";
import { AppLink } from "./AppLink";
import { api, unwrap } from "../api/client";
import type { User } from "../api/client";
import { hms, session, useRecording } from "../lib/recording/session";
import { useTicker } from "../lib/useTicker";
import { AbandonedRecordingsBanner } from "./LocalRecordings";
import { Icon } from "./ui";

/** 登录后的整体框架：左边导航，右边内容。窄屏时导航排到上面。 */
export function Shell({ user, children }: { user: User; children: ReactNode }) {
  const logout = useMutation({
    mutationFn: () => unwrap(api.POST("/api/logout")),
    onSettled: () => {
      // 整页重新加载：换了人以后不能留着上一个人缓存的数据
      window.location.assign("/");
    },
  });

  // 会议详情页也算在「会议」下面
  const { pathname } = useLocation();
  const section = pathname.startsWith("/settings") ? "settings" : pathname.startsWith("/record") ? "record" : "meetings";
  // 会议详情页自己会显示这场会议没传完的录音，顶部不重复
  const meetingId = /^\/meetings\/([^/]+)/.exec(pathname)?.[1];
  const navClass = (active: boolean) =>
    `flex min-h-11 shrink-0 items-center gap-2.5 whitespace-nowrap rounded-lg px-3 text-sm font-medium ${
      active ? "bg-brand-tint text-brand-ink" : "text-ink-2 hover:bg-subtle"
    }`;

  return (
    <div className="flex min-h-full flex-col md:flex-row">
      <nav aria-label="主导航" className="flex shrink-0 flex-row items-center gap-1 border-b border-line bg-white px-3 py-2 md:w-56 md:flex-col md:items-stretch md:border-b-0 md:border-r md:px-3.5 md:py-5">
        <div className="flex items-center gap-2.5 px-3 md:pb-4">
          <div className="flex size-7 items-center justify-center rounded-lg bg-ink text-sm font-bold text-white">记</div>
          <span className="text-[15px] font-bold">zsmeet</span>
        </div>
        <AppLink to="/" className={navClass(section === "meetings")} aria-current={section === "meetings" ? "page" : undefined}>
          <Icon name="list" />
          会议
        </AppLink>
        <AppLink to="/record" className={navClass(section === "record")} aria-current={section === "record" ? "page" : undefined}>
          <Icon name="mic" />
          网页录音
          <RecordingBadge />
        </AppLink>
        <AppLink to="/settings" className={navClass(section === "settings")} aria-current={section === "settings" ? "page" : undefined}>
          <Icon name="settings" />
          设置
        </AppLink>
        <div className="flex-1" />
        <div className="flex items-center gap-2.5 px-3 md:border-t md:border-line md:pt-3">
          <div className="flex size-8 shrink-0 items-center justify-center rounded-full bg-brand-tint text-[13px] font-semibold text-brand-ink" aria-hidden="true">
            {user.name.slice(0, 1)}
          </div>
          <div className="hidden min-w-0 flex-1 md:block">
            <div className="truncate text-[13px] font-semibold">{user.name}</div>
            <div className="text-xs text-muted">{user.role === "admin" ? "管理员" : "成员"}</div>
          </div>
          <button
            type="button"
            title="退出登录"
            aria-label="退出登录"
            onClick={() => logout.mutate()}
            className="flex size-8 cursor-pointer items-center justify-center rounded-lg text-muted hover:bg-subtle hover:text-ink"
          >
            <Icon name="logout" />
          </button>
        </div>
      </nav>
      <main className="flex min-w-0 flex-1 flex-col gap-5 px-4 py-6 md:px-10 md:py-7">
        <div className="mx-auto w-full max-w-5xl empty:hidden">
          <AbandonedRecordingsBanner user={user} skip={meetingId} />
        </div>
        <div>{children}</div>
      </main>
    </div>
  );
}

/** 导航栏上的「正在录音」：红点加已录时长，在别的页面也看得到。 */
function RecordingBadge() {
  const rec = useRecording();
  const live = rec.phase === "recording" || rec.phase === "paused";
  useTicker(1000, live);
  if (!live && rec.phase !== "finishing") return null;
  return (
    <span className="ml-auto flex items-center gap-1.5 text-xs font-normal tabular-nums text-bad" title={live ? "正在录音" : "录音正在上传"}>
      <span className={`size-2 rounded-full ${rec.phase === "recording" ? "animate-pulse bg-bad" : "bg-muted"}`} aria-hidden="true" />
      {live ? hms(session.elapsed()) : "上传中"}
    </span>
  );
}

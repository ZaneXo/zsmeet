import { useMutation } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { useLocation, useNavigate } from "react-router";
import { AppLink } from "./AppLink";
import { api, errorMessage, unwrap } from "../api/client";
import type { User } from "../api/client";
import { hms } from "../lib/format";
import { session, useRecording } from "../lib/recording/session";
import { useTicker } from "../lib/useTicker";
import { isDesktop, useSigningOut } from "../platform";
import { logout as desktopLogout } from "../platform/desktop";
import { DesktopRecorderBanner, DesktopRecordingBadge } from "../platform/DesktopRecordings";
import { OfflineBanner } from "../platform/OfflineBanner";
import { recorderState } from "../platform/recorder";
import { signOutNote } from "../platform/recorderView";
import { AbandonedRecordingsBanner } from "./LocalRecordings";
import { Icon } from "./ui";

/** 登录后的整体框架：左边导航，右边内容。窄屏时导航排到上面。 */
export function Shell({ user, children }: { user: User; children: ReactNode }) {
  const logout = useMutation({
    // 桌面版：App 通知服务器作废这台电脑的令牌，再回到登录页（缓存的数据由 platform 清掉）
    mutationFn: isDesktop ? desktopLogout : () => unwrap(api.POST("/api/logout")),
    onSettled: () => {
      // 整页重新加载：换了人以后不能留着上一个人缓存的数据
      if (!isDesktop) window.location.assign("/");
    },
  });
  // 桌面版退出登录、切换服务器一次只做一个（platform/desktop.ts）：账号页上正在退出登录、切换服务器时，这里也变灰
  const signingOut = useSigningOut() || logout.isPending;

  const navigate = useNavigate();

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
        {/* 桌面版是 App 自己的录音（录电脑里会议软件的声音和麦克风），网页版是浏览器录音 */}
        <AppLink to="/record" className={navClass(section === "record")} aria-current={section === "record" ? "page" : undefined}>
          <Icon name="mic" />
          {isDesktop ? "录音" : "网页录音"}
          {isDesktop ? <DesktopRecordingBadge /> : <RecordingBadge />}
        </AppLink>
        <AppLink to="/settings" className={navClass(section === "settings")} aria-current={section === "settings" ? "page" : undefined}>
          <Icon name="settings" />
          设置
        </AppLink>
        <div className="flex-1" />
        <div className="md:border-t md:border-line md:pt-3">
          <div className="flex items-center gap-2.5 px-3">
            <div className="flex size-8 shrink-0 items-center justify-center rounded-full bg-brand-tint text-[13px] font-semibold text-brand-ink" aria-hidden="true">
              {user.name.slice(0, 1)}
            </div>
            <div className="hidden min-w-0 flex-1 md:block">
              <div className="truncate text-[13px] font-semibold">{user.name}</div>
              <div className="text-xs text-muted">{user.role === "admin" ? "管理员" : "成员"}</div>
            </div>
            <button
              type="button"
              title={signingOut ? "正在退出登录…" : "退出登录"}
              aria-label="退出登录"
              disabled={signingOut}
              onClick={() => {
                // 桌面版正在录音、有录音没传完：先到账号页，说清楚退出以后录音会怎样，再确认退出
                if (isDesktop && signOutNote(recorderState(), "退出登录")) navigate("/settings?logout=1");
                else logout.mutate();
              }}
              className="flex size-8 cursor-pointer items-center justify-center rounded-lg text-muted hover:bg-subtle hover:text-ink disabled:cursor-not-allowed disabled:opacity-45"
            >
              <Icon name="logout" />
            </button>
          </div>
          {/* 没能退出（比如还没来得及变灰就点到了，被正在做的退出登录、切换服务器拒绝）：说明原因，不然看起来点了没反应 */}
          {logout.isError && (
            <p role="alert" className="mt-1.5 px-3 text-xs text-bad">
              {errorMessage(logout.error)}
            </p>
          )}
        </div>
      </nav>
      <main className="flex min-w-0 flex-1 flex-col gap-5 px-4 py-6 md:px-10 md:py-7">
        <div className="mx-auto w-full max-w-5xl empty:hidden">
          <OfflineBanner />
        </div>
        <div className="mx-auto w-full max-w-5xl empty:hidden">
          {isDesktop ? <DesktopRecorderBanner skip={meetingId} /> : <AbandonedRecordingsBanner user={user} skip={meetingId} />}
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

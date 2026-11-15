import { useQuery } from "@tanstack/react-query";
import { Navigate, Route, Routes, useLocation } from "react-router";
import { api, errorMessage, unwrap } from "./api/client";
import { Shell } from "./components/Shell";
import { tr, useLang } from "./i18n/react";
import { isDesktop } from "./platform";
import { DesktopRecordPage } from "./platform/DesktopRecord";
import { DesktopNotInitialized, DesktopSessionExpired, SwitchServerButton } from "./platform/DesktopScreens";
import { Button, Notice, Spinner } from "./components/ui";
import { APP_AUTHORIZE_PATH, pendingAuthorize } from "./lib/appAuthorize";
import { AccountLinkPage } from "./pages/AccountLink";
import { AppAuthorizePage } from "./pages/AppAuthorize";
import { LoginPage, SetupPage } from "./pages/Auth";
import { MeetingDetailPage } from "./pages/MeetingDetail";
import { MeetingsPage } from "./pages/Meetings";
import { RecordPage } from "./pages/Record";
import { SettingsPage } from "./pages/Settings";

/** 系统状态：有没有初始化、当前是谁登录。整个界面显示什么由它决定。 */
export function useStatus() {
  return useQuery({
    queryKey: ["status"],
    queryFn: () => unwrap(api.GET("/api/status")),
  });
}

export function App() {
  useLang();
  const status = useStatus();
  const { pathname, hash } = useLocation();

  // 邀请和重置密码的链接：不管有没有登录、登录的是谁，都直接显示落地页
  if (pathname === "/invite" || pathname === "/reset") return <AccountLinkPage key={hash} token={hash.slice(1)} />;

  const data = status.data;
  if (!data || (status.isError && !data.user)) {
    if (status.isPending) return <Spinner label={tr("正在加载…", "Loading…")} />;
    return (
      <div className="mx-auto mt-24 flex max-w-md flex-col gap-4 px-4">
        <Notice tone="bad">{errorMessage(status.error)}</Notice>
        <Button onClick={() => status.refetch()}>{tr("重试", "Retry")}</Button>
        {isDesktop && <SwitchServerButton variant="ghost" />}
      </div>
    );
  }
  // App（桌面版）登录用的授权页：在浏览器里打开，不在平常的界面框架里，没登录时它自己先显示登录表单
  if (!isDesktop && pathname === APP_AUTHORIZE_PATH) return <AppAuthorizePage status={data} />;
  // 桌面版不在 App 里初始化、输密码：那些都在浏览器里做
  if (!data.initialized) return isDesktop ? <DesktopNotInitialized /> : <SetupPage />;
  if (!data.user) return isDesktop ? <DesktopSessionExpired /> : <LoginPage feishuLogin={data.feishu_login} />;
  // 从授权页去用飞书登录，登录完被带回了首页：接着打开授权页
  if (!isDesktop && pathname === "/") {
    const pending = pendingAuthorize();
    if (pending) return <Navigate to={APP_AUTHORIZE_PATH + pending} replace />;
  }

  return (
    <Shell user={data.user}>
      <Routes>
        <Route path="/" element={<MeetingsPage llmReady={data.llm_ready} />} />
        <Route
          path="/meetings/:id"
          element={
            <MeetingDetailPage
              user={data.user}
              llmReady={data.llm_ready}
              feishuReady={data.feishu_ready}
              voiceprints={data.voiceprints}
              isAdmin={data.user.role === "admin"}
            />
          }
        />
        {/* 桌面版用 App 自己的录音（录电脑里会议软件的声音和麦克风），不用网页录音 */}
        <Route path="/record" element={isDesktop ? <DesktopRecordPage /> : <RecordPage user={data.user} />} />
        <Route path="/settings/:section?" element={<SettingsPage user={data.user} />} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Routes>
    </Shell>
  );
}

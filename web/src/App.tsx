import { useQuery } from "@tanstack/react-query";
import { Navigate, Route, Routes, useLocation } from "react-router";
import { api, errorMessage, unwrap } from "./api/client";
import { Shell } from "./components/Shell";
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
  const status = useStatus();
  const { pathname, hash } = useLocation();

  // 邀请和重置密码的链接：不管有没有登录、登录的是谁，都直接显示落地页
  if (pathname === "/invite" || pathname === "/reset") return <AccountLinkPage key={hash} token={hash.slice(1)} />;

  // 一次都没取到状态时，整页等待或者整页报错。取到过以后再取失败的话（刚登录成功时会重取；
  // 电脑从睡眠里醒来、断网重连的那一下也会自动重取），要看现在登录了没有：
  // - 已经登录：界面保持原样，不能整个换成错误页。那样页面上还没复制的一次性链接就丢了，
  //   正在进行的上传也会被打断。各个页面自己的请求失败时会各自提示
  // - 还没登录（登录页、初始化页）：整页报错让人重试。否则刚登录成功、紧接着取状态失败时，
  //   页面上还是那个登录表单，什么也不说，用户只会以为没登录上
  const data = status.data;
  if (!data || (status.isError && !data.user)) {
    if (status.isPending) return <Spinner label="正在加载…" />;
    return (
      <div className="mx-auto mt-24 flex max-w-md flex-col gap-4 px-4">
        <Notice tone="bad">{errorMessage(status.error)}</Notice>
        <Button onClick={() => status.refetch()}>重试</Button>
      </div>
    );
  }
  // App（桌面版）登录的授权页：不在平常的界面框架里，没登录时它自己先显示登录表单
  if (pathname === APP_AUTHORIZE_PATH) return <AppAuthorizePage status={data} />;
  if (!data.initialized) return <SetupPage />;
  if (!data.user) return <LoginPage feishuLogin={data.feishu_login} />;
  // 从授权页去用飞书登录，登录完被带回了首页：接着打开授权页
  if (pathname === "/") {
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
        <Route path="/record" element={<RecordPage user={data.user} />} />
        <Route path="/settings/:section?" element={<SettingsPage user={data.user} />} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Routes>
    </Shell>
  );
}

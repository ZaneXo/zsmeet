import { QueryClientProvider } from "@tanstack/react-query";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { queryClient } from "./api/client";
import { App } from "./App";
import { isDesktop, Router } from "./platform";
import { onIdentityChange } from "./platform/desktop";
import { DesktopGate } from "./platform/DesktopScreens";
import "./index.css";

// 桌面版换了人、换了服务器、退出登录时，清掉上一个人缓存的数据（网页版是整页重新加载）
if (isDesktop) onIdentityChange(() => queryClient.clear());

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <Router>
        {isDesktop ? (
          <DesktopGate>
            <App />
          </DesktopGate>
        ) : (
          <App />
        )}
      </Router>
    </QueryClientProvider>
  </StrictMode>,
);

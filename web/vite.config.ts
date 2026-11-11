import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// 开发时网页由 Vite 提供，接口请求转给本机的 Go 服务。
// 地址可以用环境变量 ZSMEET_API 改，默认是 zsmeet serve 的默认端口。
//
// `--mode desktop` 构建桌面版的界面（打包进 desktop/ 的 App，见 docs/desktop.md）：
// VITE_PLATFORM 是 desktop，输出到 dist-desktop。也可以直接用环境变量 VITE_PLATFORM 指定。
export default defineConfig(({ mode }) => {
  const platform = process.env.VITE_PLATFORM ?? (mode === "desktop" ? "desktop" : "web");
  if (platform !== "web" && platform !== "desktop") throw new Error(`VITE_PLATFORM 只能是 web 或 desktop，现在是 ${platform}`);
  return {
    plugins: [react(), tailwindcss()],
    // 构建时换成常量：用不到的那一边（比如网页版里桌面版的代码）会被整个去掉
    define: { "import.meta.env.VITE_PLATFORM": JSON.stringify(platform) },
    server: {
      port: 5173,
      proxy: {
        "/api": {
          target: process.env.ZSMEET_API ?? "http://127.0.0.1:8080",
          // 进度推送是长连接，不能被代理缓冲或超时掐断
          timeout: 0,
        },
      },
    },
    build: {
      outDir: platform === "desktop" ? "dist-desktop" : "dist",
      emptyOutDir: true,
    },
  };
});

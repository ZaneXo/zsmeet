import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// 开发时网页由 Vite 提供，接口请求转给本机的 Go 服务。
// 地址可以用环境变量 ZSMEET_API 改，默认是 zsmeet serve 的默认端口。
export default defineConfig({
  plugins: [react(), tailwindcss()],
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
    outDir: "dist",
    emptyOutDir: true,
  },
});

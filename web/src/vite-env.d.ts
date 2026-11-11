/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** 网页版还是桌面版，构建时由 vite.config.ts 定下来（见 src/platform/） */
  readonly VITE_PLATFORM: "web" | "desktop";
}

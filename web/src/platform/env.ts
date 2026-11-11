/**
 * 网页版还是桌面版。构建时定下来的常量（`pnpm build --mode desktop` 是桌面版，见 vite.config.ts），
 * 打包时用不到的那一边会被整个去掉：网页版里没有桌面版的代码。
 */
export const isDesktop: boolean = import.meta.env.VITE_PLATFORM === "desktop";

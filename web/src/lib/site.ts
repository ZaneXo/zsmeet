// zsmeet 访问地址的小函数：「设置 → 飞书」和「设置 → 群机器人」里填的是同一个地址。
// 不依赖 React，方便单独测试（site.test.ts）。

/**
 * 只有这台电脑自己能打开的地址（localhost、127.0.0.0/8、0.0.0.0、::1 这些）。
 * 消息、卡片里的链接用它拼的话，同事点了打不开。看不懂的地址不算。
 */
export function localOnly(url: string): boolean {
  let host: string;
  try {
    // URL 会把主机名规范化：127.1 写成 127.0.0.1，[0:0::1] 写成 [::1]，[::ffff:127.0.0.1] 写成 [::ffff:7f00:1]
    host = new URL(url).hostname.toLowerCase().replace(/\.$/, "");
  } catch {
    return false;
  }
  return (
    host === "localhost" ||
    host.endsWith(".localhost") ||
    /^127\.\d+\.\d+\.\d+$/.test(host) ||
    host === "0.0.0.0" ||
    host === "[::1]" ||
    host === "[::]" ||
    /^\[::ffff:7f[0-9a-f]{2}:[0-9a-f]{1,4}\]$/.test(host)
  );
}

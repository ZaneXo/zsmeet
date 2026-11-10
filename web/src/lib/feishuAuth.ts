import { useEffect, useState } from "react";
import { useSearchParams } from "react-router";

// 用飞书登录、关联飞书完成后，服务器把浏览器跳回登录页（关联时是「设置 → 账号」），
// 地址里带着结果：feishu_error=<代号> 表示没成功，feishu=linked 表示关联好了。
// 代号换成给人看的说明见 feishuErrors.ts。

/**
 * 读出飞书跳回来时地址里带的结果，然后从地址栏里去掉（刷新页面、复制链接时不会再出现）。
 * 结果只在第一次渲染时读一次，记在组件里。
 */
export function useFeishuResult(): { error: string; linked: boolean } {
  const [params, setParams] = useSearchParams();
  const [result] = useState(() => ({ error: params.get("feishu_error") ?? "", linked: params.get("feishu") === "linked" }));
  useEffect(() => {
    if (!params.has("feishu_error") && !params.has("feishu")) return;
    setParams(
      (p) => {
        p.delete("feishu_error");
        p.delete("feishu");
        return p;
      },
      { replace: true },
    );
  }, [params, setParams]);
  return result;
}

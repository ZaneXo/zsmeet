// 用飞书登录、关联飞书没成功时，服务器跳回网页的地址里带着一个固定的错误代号（feishu_error），
// 代号是固定的几个（见 internal/service/feishu_login.go），这里换成给人看的说明。
// 地址里只放代号、不放文字：那样谁都能编一个带着任意说明的 zsmeet 链接发给别人。
// 这里不依赖 React，方便单独测试（feishuErrors.test.ts）。

/** 在哪个页面上显示：登录页，还是账号页上的「关联飞书」。说明里让人重新点的按钮不一样 */
type Where = "login" | "account";

const MESSAGES: Record<string, (action: string) => string> = {
  unavailable: () => "飞书登录现在用不了：管理员没有打开，或者还没配置好。请用邮箱和密码登录，或者联系管理员。",
  expired: (action) =>
    `这次飞书登录已经失效了（超过 10 分钟，或者不是在同一个浏览器里打开的），请重新点「${action}」。` +
    "一直这样的话，请用管理员在飞书设置里填的 zsmeet 访问地址打开 zsmeet 再试。",
  signed_out: () => "登录已失效。请重新登录，再到「设置 → 账号」里点「关联飞书」。",
  link_password: () => "关联飞书要先确认身份：请在「设置 → 账号」的飞书账号一栏输入当前密码，再点「关联飞书」。",
  password_changed: () =>
    "点「关联飞书」以后，这个账号的密码改过了，这次关联已经作废（改密码时会一起解除和飞书的关联）。" +
    "请在「设置 → 账号」的飞书账号一栏输入现在的密码，再点「关联飞书」。",
  denied: (action) => `你在飞书上没有同意授权。需要的话请重新点「${action}」，在飞书上点同意。`,
  account_disabled: () => "这个账号已被停用，请联系管理员。",
  user_linked_other: () =>
    "和你的企业邮箱对应的 zsmeet 账号已经关联了另一个飞书账号。如果那是你以前的飞书账号：请用邮箱和密码登录，" +
    "在「设置 → 账号」里解除关联，再点「关联飞书」；不知道密码的话，请管理员在「设置 → 成员」里解除这个账号和飞书的关联，再用飞书登录。",
  invite_unusable: () => "管理员发到你企业邮箱的邀请已经失效了。请管理员在「设置 → 成员」里重新生成邀请链接，之后再用飞书登录就行。",
  contact_email_member: () =>
    "你在飞书通讯录里的邮箱和一个 zsmeet 账号相同，但这个邮箱没有经过飞书验证，不能直接用来登录。" +
    "请先用邮箱和密码登录，再到「设置 → 账号」里点「关联飞书」，以后就能用飞书登录了。" +
    "不知道密码的话，请管理员在「设置 → 成员」里给你生成重置密码的链接。",
  contact_email_invite: () =>
    "管理员已经邀请了你在飞书通讯录里的邮箱。请打开管理员发给你的邀请链接创建账号（链接失效的话请管理员重新生成），" +
    "之后可以在「设置 → 账号」里关联飞书。",
  not_member: () =>
    "你的飞书账号还不是 zsmeet 的成员。请管理员在「设置 → 成员」里邀请你；用邀请链接创建账号以后，可以在「设置 → 账号」里关联飞书。",
  no_email: () =>
    "飞书没有给出你的邮箱，没法自动创建账号。请管理员在飞书开发者后台开通 contact:user.employee:readonly 和 contact:user.email:readonly 权限并发布新版本，" +
    "或者在「设置 → 成员」里邀请你。",
  linked_elsewhere: () =>
    "这个飞书账号已经关联了 zsmeet 的另一个账号。请登录那个账号，在「设置 → 账号」里解除关联；或者请管理员在「设置 → 成员」里解除。之后再来关联。",
  already_linked: () => "你的账号已经关联了另一个飞书账号。请先解除关联，再关联新的飞书账号。",
  busy: (action) => `刚才的飞书登录还没处理完，请重新点「${action}」。`,
  app_credentials: () => "飞书应用的 App ID 或 App Secret 不对，请管理员到「设置 → 飞书」里检查。",
  redirect_mismatch: () => "飞书应用里填的重定向 URL 和 zsmeet 的访问地址对不上，请管理员按「设置 → 飞书」里显示的地址重新填写。",
  out_of_scope: () => "你不在这个飞书应用的可用范围里，请管理员在飞书管理后台把你加进应用的可用范围。",
  retry: (action) => `这次飞书授权已经用过或者超时了，请重新点「${action}」。`,
};

/** 把地址里的错误代号换成说明。不认识的代号（比如服务器出错）给一句笼统的。 */
export function feishuErrorText(code: string, where: Where): string {
  const action = where === "login" ? "用飞书登录" : "关联飞书";
  // 代号来自地址栏，谁都能改：只认上面列出的，__proto__、toString 这类对象自带的属性不算
  const message = Object.hasOwn(MESSAGES, code) ? MESSAGES[code] : undefined;
  if (message) return message(action);
  return `${where === "login" ? "用飞书登录" : "关联飞书"}没有成功，请稍后再试。一直这样的话，请联系管理员。`;
}

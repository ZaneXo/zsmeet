// 用飞书登录、关联飞书没成功时，服务器跳回网页的地址里带着一个固定的错误代号（feishu_error），
// 代号是固定的几个（见 internal/service/feishu_login.go），这里换成给人看的说明。
// 地址里只放代号、不放文字：那样谁都能编一个带着任意说明的 zsmeet 链接发给别人。
// 这里不依赖 React，方便单独测试（feishuErrors.test.ts）。

import { isEn } from "../i18n/lang.ts";

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

const MESSAGES_EN: Record<string, (action: string) => string> = {
  unavailable: () => "Feishu sign-in is currently unavailable: it is disabled or not yet configured by an admin. Sign in with email and password, or contact an admin.",
  expired: (action) =>
    `This Feishu sign-in session expired (over 10 minutes or opened in a different browser). Click "${action}" again. ` +
    "If this keeps happening, open zsmeet using the public URL configured in Feishu settings.",
  signed_out: () => "Your session expired. Sign in again, then go to Settings → Account and click \"Link Feishu\".",
  link_password: () => "Identity verification is required to link Feishu: enter your current password under Feishu Account in Settings → Account, then click \"Link Feishu\".",
  password_changed: () =>
    "Your password was changed after starting the link flow, invalidating this request (changing your password also unlinks Feishu). " +
    "Enter your new password in Settings → Account and click \"Link Feishu\" again.",
  denied: (action) => `You declined authorization in Feishu. Click "${action}" again and approve in Feishu if needed.`,
  account_disabled: () => "This account has been deactivated. Please contact an admin.",
  user_linked_other: () =>
    "The zsmeet account matching your work email is already linked to another Feishu account. If that was your old Feishu account, sign in with email and password, " +
    "unlink it in Settings → Account, and click \"Link Feishu\"; otherwise ask an admin to unlink it in Settings → Members.",
  invite_unusable: () => "The invitation sent to your work email has expired. Ask an admin to regenerate an invite link in Settings → Members, then sign in with Feishu.",
  contact_email_member: () =>
    "Your email in Feishu Contacts matches an existing zsmeet account, but that email is unverified in Feishu and cannot sign in directly. " +
    "Sign in with email and password first, then click \"Link Feishu\" in Settings → Account.",
  contact_email_invite: () =>
    "An admin has invited the email in your Feishu Contacts. Open the invite link from your admin to create your account, " +
    "then link Feishu in Settings → Account.",
  not_member: () =>
    "Your Feishu account is not a zsmeet member yet. Ask an admin to invite you in Settings → Members, then link Feishu in Settings → Account after creating your account.",
  no_email: () =>
    "Feishu did not provide your email address, so an account could not be created automatically. Ask an admin to enable contact:user.employee:readonly and contact:user.email:readonly permissions in the Feishu developer console, or invite you in Settings → Members.",
  linked_elsewhere: () =>
    "This Feishu account is already linked to another zsmeet account. Sign in to that account and unlink it in Settings → Account, or ask an admin to unlink it in Settings → Members.",
  already_linked: () => "Your account is already linked to another Feishu account. Unlink it first before linking a new one.",
  busy: (action) => `The previous Feishu request is still being processed. Click "${action}" again.`,
  app_credentials: () => "Invalid Feishu App ID or App Secret. Ask an admin to check Settings → Feishu.",
  redirect_mismatch: () => "The redirect URL configured in the Feishu app does not match zsmeet's public URL. Ask an admin to update it as shown in Settings → Feishu.",
  out_of_scope: () => "You are outside the availability scope of this Feishu app. Ask an admin to add you in the Feishu admin console.",
  retry: (action) => `This Feishu authorization code was already used or timed out. Click "${action}" again.`,
};

/** 把地址里的错误代号换成说明。不认识的代号（比如服务器出错）给一句笼统的。 */
export function feishuErrorText(code: string, where: Where): string {
  if (isEn()) {
    const action = where === "login" ? "Sign in with Feishu" : "Link Feishu";
    const message = Object.hasOwn(MESSAGES_EN, code) ? MESSAGES_EN[code] : undefined;
    if (message) return message(action);
    return `${action} failed. Please try again later, or contact an admin if the problem persists.`;
  }
  const action = where === "login" ? "用飞书登录" : "关联飞书";
  // 代号来自地址栏，谁都能改：只认上面列出的，__proto__、toString 这类对象自带的属性不算
  const message = Object.hasOwn(MESSAGES, code) ? MESSAGES[code] : undefined;
  if (message) return message(action);
  return `${where === "login" ? "用飞书登录" : "关联飞书"}没有成功，请稍后再试。一直这样的话，请联系管理员。`;
}

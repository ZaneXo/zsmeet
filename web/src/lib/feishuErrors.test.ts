// 用 Node 自带的测试运行：pnpm -C web test
import assert from "node:assert/strict";
import { test } from "node:test";
import { feishuErrorText } from "./feishuErrors.ts";

test("认识的代号换成说明，按页面换要重新点的按钮", () => {
  assert.match(feishuErrorText("denied", "login"), /用飞书登录/);
  assert.match(feishuErrorText("denied", "account"), /关联飞书/);
  assert.match(feishuErrorText("password_changed", "account"), /密码改过了/);
});

test("地址栏里编出来的代号只给笼统的说明，不会让页面出错", () => {
  const fallback = "用飞书登录没有成功，请稍后再试。一直这样的话，请联系管理员。";
  for (const code of ["__proto__", "constructor", "toString", "hasOwnProperty", "valueOf", "", "no_such_code"]) {
    assert.equal(feishuErrorText(code, "login"), fallback, code);
  }
});

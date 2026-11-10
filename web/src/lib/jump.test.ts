// 用 Node 自带的测试运行：pnpm -C web test
import assert from "node:assert/strict";
import { test } from "node:test";
import { citeJump, jumpParam, parseJump, withoutJump } from "./jump.ts";

test("同一个时间点只有一种写法", () => {
  assert.equal(jumpParam(70, true), "70.0");
  assert.equal(jumpParam(70.54, true), "70.5");
  assert.equal(jumpParam(70.9, false), "70");
});

test("解析 ?t=", () => {
  assert.deepEqual(parseJump("70.5"), { sec: 70.5, slack: 0.05 });
  assert.deepEqual(parseJump("70"), { sec: 70, slack: 0.99 });
  assert.equal(parseJump(null), undefined);
  assert.equal(parseJump(" "), undefined);
  assert.equal(parseJump("abc"), undefined);
  assert.equal(parseJump("-1"), undefined);
});

test("出处和网址里的时间点数值一样、写法不同时，也直接播放并定位", () => {
  // 网址里是从纪要点过来的「70」，出处是转写里 70 秒那句
  const j = citeJump("70", 70, true);
  assert.equal(j.seek, 67);
  assert.deepEqual(j.focus, { sec: 70, slack: 0.05 });
  assert.equal(j.param, "70.0");
  assert.equal(j.moved, false);

  // 反过来：网址里是「70.0」，出处是纪要里的 01:10
  const k = citeJump("70.0", 70, false);
  assert.equal(k.seek, 67);
  assert.deepEqual(k.focus, { sec: 70, slack: 0.99 });
  assert.equal(k.param, "70");
  assert.equal(k.moved, false);
});

test("点同一个出处两次：网址不用改，照样播放", () => {
  const j = citeJump("70.5", 70.5, true);
  assert.equal(j.seek, 67.5);
  assert.equal(j.param, undefined);
  assert.equal(j.moved, false);
});

test("换了时间点：改网址，并标记已经跳过了", () => {
  const j = citeJump("70.5", 1.2, true);
  assert.equal(j.seek, 0);
  assert.equal(j.param, "1.2");
  assert.equal(j.moved, true);
  assert.equal(citeJump(null, 12, false).moved, true);
});

test("打开纪要时去掉网址里的 ?t=，其他参数不动", () => {
  const next = withoutJump(new URLSearchParams("t=70.5&x=1"));
  assert.equal(next?.toString(), "x=1");
  assert.equal(withoutJump(new URLSearchParams("x=1")), undefined);
  // 不改传进来的参数
  const params = new URLSearchParams("t=70");
  withoutJump(params);
  assert.equal(params.get("t"), "70");
});

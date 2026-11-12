import assert from "node:assert/strict";
import { test } from "node:test";
import { waitingForRecording } from "./meetingStatus.ts";

test("网页录音、桌面版录音的会议在等录音传完时要盯着（网页版里也盯着桌面版录的）", () => {
  assert.equal(waitingForRecording({ source: "record", status: "uploading" }), true);
  assert.equal(waitingForRecording({ source: "desktop", status: "uploading" }), true);
  // 传完开始处理以后，由「在处理」的刷新接手
  assert.equal(waitingForRecording({ source: "desktop", status: "queued" }), false);
  assert.equal(waitingForRecording({ source: "desktop", status: "done" }), false);
  // 上传文件、飞书会议不是边录边传的
  assert.equal(waitingForRecording({ source: "upload", status: "uploading" }), false);
  assert.equal(waitingForRecording({ source: "feishu", status: "uploading" }), false);
  assert.equal(waitingForRecording(undefined), false);
});

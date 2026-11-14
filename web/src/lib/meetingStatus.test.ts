import assert from "node:assert/strict";
import { test } from "node:test";
import { waitingForRecording, writingMinutesOnly } from "./meetingStatus.ts";

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

test("处理中只写纪要还是重新识别：重新转写时旧的转写还在，不能当成只写纪要", () => {
  // 重新生成纪要（有转写，没要求重新转写）：只有写纪要这一步
  assert.equal(writingMinutesOnly({ stage: "", retranscribing: false }, true), true);
  // 重新转写：有旧的转写，但接收转码、识别、区分说话人都要再做一遍
  assert.equal(writingMinutesOnly({ stage: "assemble", retranscribing: true }, true), false);
  assert.equal(writingMinutesOnly({ stage: "transcribe", retranscribing: true }, true), false);
  assert.equal(writingMinutesOnly({ stage: "", retranscribing: true }, true), false);
  // 走到写纪要这一步时都是只剩写纪要
  assert.equal(writingMinutesOnly({ stage: "minutes", retranscribing: true }, true), true);
  assert.equal(writingMinutesOnly({ stage: "minutes", retranscribing: false }, false), true);
  // 第一次处理（还没有转写）
  assert.equal(writingMinutesOnly({ stage: "transcribe", retranscribing: false }, false), false);
});

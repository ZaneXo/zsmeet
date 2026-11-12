import assert from "node:assert/strict";
import { test } from "node:test";
import { chunksFlowing, contiguous, DESKTOP_CHUNK_MS, RECORDING_STALE_MS, receivedSeconds } from "./unfinishedRecording.ts";

test("能用的分片只算从第 0 片开始连续的一段", () => {
  assert.equal(contiguous([]), 0);
  assert.equal(contiguous([0, 1, 2]), 3);
  // 服务器返回的顺序无所谓，重复的只算一次
  assert.equal(contiguous([2, 0, 1, 1]), 3);
  // 缺了第 2 片：后面的接不上
  assert.equal(contiguous([0, 1, 3, 4]), 2);
  // 没有第 0 片：一片也用不上
  assert.equal(contiguous([1, 2]), 0);
});

test("网页录音按网页的切片长度估算", () => {
  assert.equal(receivedSeconds("record", 0, 10_000), 0);
  assert.equal(receivedSeconds("record", 1, 10_000), 10);
  assert.equal(receivedSeconds("record", 6, 10_000), 60);
  // 跟着 SLICE_MS 变
  assert.equal(receivedSeconds("record", 6, 5_000), 30);
});

test("桌面版录音按录音程序的分片长度（每片约 10 秒）估算，和网页的切片长度无关", () => {
  assert.equal(DESKTOP_CHUNK_MS, 10_000);
  assert.equal(receivedSeconds("desktop", 0, 10_000), 0);
  assert.equal(receivedSeconds("desktop", 30, 10_000), 300);
  assert.equal(receivedSeconds("desktop", 30, 5_000), 300);
  // 录满 6 小时差不多是 2160 片
  assert.equal(receivedSeconds("desktop", 2160, 1_000), 6 * 3600);
});

test("最近 90 秒里收到过分片的，算录音还在进行（还在录，或者正在补传）", () => {
  assert.equal(RECORDING_STALE_MS, 90_000);
  const at = (iso: string) => Date.parse(iso);
  const meeting = { created_at: "2026-01-05T10:00:00Z", last_chunk_at: "2026-01-05T10:30:00Z" };
  assert.equal(chunksFlowing(meeting, at("2026-01-05T10:30:10Z")), true);
  assert.equal(chunksFlowing(meeting, at("2026-01-05T10:31:29Z")), true);
  // 90 秒没有新的分片：录音的网页关掉了、电脑断网了，或者暂停着
  assert.equal(chunksFlowing(meeting, at("2026-01-05T10:31:30Z")), false);
  assert.equal(chunksFlowing(meeting, at("2026-01-05T12:00:00Z")), false);
  // 还没收到分片：刚建好的会议先当作在录，过了 90 秒还没有就不算了
  assert.equal(chunksFlowing({ created_at: "2026-01-05T10:00:00Z" }, at("2026-01-05T10:01:00Z")), true);
  assert.equal(chunksFlowing({ created_at: "2026-01-05T10:00:00Z" }, at("2026-01-05T10:05:00Z")), false);
});

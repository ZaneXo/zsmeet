import assert from "node:assert/strict";
import { test } from "node:test";
import { dateTime } from "./format.ts";
import { chunksFlowing, contiguous, DESKTOP_CHUNK_MS, idleMs, lastReceived, RECORDING_STALE_MS, receivedSeconds } from "./unfinishedRecording.ts";

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
  const now = Date.parse("2026-01-05T10:32:00Z");
  const meeting = { created_at: "2026-01-05T10:00:00Z", last_chunk_at: "2026-01-05T10:30:00Z" };
  assert.equal(chunksFlowing({ ...meeting, upload_idle_sec: 10 }, now), true);
  assert.equal(chunksFlowing({ ...meeting, upload_idle_sec: 89 }, now), true);
  // 90 秒没有新的分片：录音的网页关掉了、电脑断网了，或者暂停着
  assert.equal(chunksFlowing({ ...meeting, upload_idle_sec: 90 }, now), false);
  assert.equal(chunksFlowing({ ...meeting, upload_idle_sec: 5400 }, now), false);
  // 还没收到分片：服务器从建会议算起，刚建好的会议先当作在录，过了 90 秒还没有就不算了
  assert.equal(chunksFlowing({ created_at: "2026-01-05T10:00:00Z", upload_idle_sec: 60 }, now), true);
  assert.equal(chunksFlowing({ created_at: "2026-01-05T10:00:00Z", upload_idle_sec: 300 }, now), false);
});

test("多久没收到分片按服务器算好的 upload_idle_sec，不拿这台电脑的时钟去减服务器记的时间", () => {
  // 服务器没有自动对时，比看会议的这台电脑慢 2 分钟：服务器在它的 10:30:00 收到一片，10 秒以后有人取会议，
  // 这时电脑上已经是 10:32:10。拿电脑的时钟去减 last_chunk_at 会算成 2 分多钟没收到，把还在录的会议当成断了
  const now = Date.parse("2026-01-05T10:32:10Z");
  const recording = { created_at: "2026-01-05T10:00:00Z", last_chunk_at: "2026-01-05T10:30:00Z", upload_idle_sec: 10 };
  assert.equal(idleMs(recording, now), 10_000);
  assert.equal(chunksFlowing(recording, now), true);
  assert.equal(lastReceived(recording, now), "最后一次收到录音：刚刚");
  // 反过来，电脑的时钟慢：服务器说已经 100 秒没收到了，就是不在传了，即使拿电脑的时钟去减才过了几秒
  const stalled = { ...recording, upload_idle_sec: 100 };
  const slow = Date.parse("2026-01-05T10:30:05Z");
  assert.equal(chunksFlowing(stalled, slow), false);
  assert.equal(lastReceived(stalled, slow), "最后一次收到录音：1 分钟前");
});

test("「最后一次收到录音」：一小时以内按分钟，一天以内按小时，再早的写日期和时间", () => {
  const meeting = { created_at: "2026-01-05T10:00:00Z", last_chunk_at: "2026-01-05T10:30:00Z" };
  const now = Date.parse("2026-01-08T00:00:00Z");
  assert.equal(lastReceived({ ...meeting, upload_idle_sec: 59 }, now), "最后一次收到录音：刚刚");
  assert.equal(lastReceived({ ...meeting, upload_idle_sec: 60 }, now), "最后一次收到录音：1 分钟前");
  assert.equal(lastReceived({ ...meeting, upload_idle_sec: 3599 }, now), "最后一次收到录音：59 分钟前");
  assert.equal(lastReceived({ ...meeting, upload_idle_sec: 3600 }, now), "最后一次收到录音：1 小时前");
  assert.equal(lastReceived({ ...meeting, upload_idle_sec: 86_399 }, now), "最后一次收到录音：23 小时前");
  assert.equal(lastReceived({ ...meeting, upload_idle_sec: 86_400 }, now), `最后一次收到录音：${dateTime(meeting.last_chunk_at)}`);
  // 还没收到过分片
  assert.equal(lastReceived({ created_at: "2026-01-05T10:00:00Z", upload_idle_sec: 300 }, now), "服务器还没有收到录音");
});

test("连着还没有 upload_idle_sec 的旧版本服务器（桌面版连着没升级的服务器）时，才用这台电脑的时钟算", () => {
  const at = (iso: string) => Date.parse(iso);
  const meeting = { created_at: "2026-01-05T10:00:00Z", last_chunk_at: "2026-01-05T10:30:00Z" };
  assert.equal(chunksFlowing(meeting, at("2026-01-05T10:30:10Z")), true);
  assert.equal(chunksFlowing(meeting, at("2026-01-05T10:31:29Z")), true);
  assert.equal(chunksFlowing(meeting, at("2026-01-05T10:31:30Z")), false);
  assert.equal(lastReceived(meeting, at("2026-01-05T10:33:00Z")), "最后一次收到录音：3 分钟前");
  // 还没收到分片：从建会议的时间算起
  assert.equal(chunksFlowing({ created_at: "2026-01-05T10:00:00Z" }, at("2026-01-05T10:01:00Z")), true);
  assert.equal(chunksFlowing({ created_at: "2026-01-05T10:00:00Z" }, at("2026-01-05T10:05:00Z")), false);
});

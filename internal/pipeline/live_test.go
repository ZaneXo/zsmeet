package pipeline

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/ZaneXo/zsmeet/internal/asr"
	"github.com/ZaneXo/zsmeet/internal/audio"
	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/models"
	"github.com/ZaneXo/zsmeet/internal/queue"
	"github.com/ZaneXo/zsmeet/internal/transcript"
)

func TestSplitLiveSegments(t *testing.T) {
	// 第一片 0..6s：第一句在 3.0s 结束（离 6.0s 超过 1.2s，定稿），第二句在 5.5s 结束（可能被分片截断，暂定）
	fin, all, finSec := splitLiveSegments(nil, 0, 0, 6.0, []transcript.Segment{
		{Start: 0.2, End: 3.0, Text: "第一句说完了。"},
		{Start: 4.0, End: 5.5, Text: "第二句还没"},
	})
	if len(fin) != 1 || fin[0].Text != "第一句说完了。" || finSec != 3.0 {
		t.Fatalf("第一轮定稿不对：fin=%+v finSec=%v", fin, finSec)
	}
	if len(all) != 2 || all[1].Text != "第二句还没" {
		t.Fatalf("第一轮可见句子不对：%+v", all)
	}

	// 第二片 0..12s，从 3.0s 起解出尾巴（相对 3.0s 的 1.0..4.8s，即绝对 4.0..7.8s）：完整替换刚才的半句并定稿
	fin, all, finSec = splitLiveSegments(fin, finSec, 3.0, 12.0, []transcript.Segment{
		{Start: 1.0, End: 4.8, Text: "第二句已经完整说完了。"},
	})
	if len(fin) != 2 || fin[1].Start != 4.0 || fin[1].End != 7.8 || fin[1].Text != "第二句已经完整说完了。" {
		t.Fatalf("第二轮定稿不对：%+v", fin)
	}
	if len(all) != 2 {
		t.Fatalf("第二轮可见句子不该重复：%+v", all)
	}
	// 7.8s 之后到 12.0s 有 4.2s 静音（> 4.0s），定稿位置顺带往前推到 12.0 - 4.0 = 8.0s
	if math.Abs(finSec-8.0) > 1e-6 {
		t.Fatalf("静音推进后的 finSec = %v, want 8.0", finSec)
	}
}

func TestLiveWorkerIncrementalAndCleanup(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()

	m, err := e.q.CreateMeeting(ctx, sqlc.CreateMeetingParams{
		OwnerID:    e.user.ID,
		Title:      "实时字幕测试",
		Source:     "record",
		Template:   "general",
		Hotwords:   "飞书",
		Visibility: "team",
		Language:   "zh",
	})
	if err != nil {
		t.Fatal(err)
	}

	putAACChunk := func(seq int, seconds int) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "chunk.aac")
		cmd := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error", "-y",
			"-f", "lavfi", "-i", "aevalsrc='0.05*sin(2*PI*440*t)':s=16000:d=6",
			"-ac", "1", "-c:a", "aac", "-b:a", "64k", "-f", "adts", path)
		if seconds != 6 {
			t.Fatalf("expected 6s chunk, got %d", seconds)
		}
		if msg, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg 生成分片失败：%v\n%s", err, msg)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		n, err := e.w.store.PutChunk(m.ID, seq, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		if err := e.q.AddChunk(ctx, sqlc.AddChunkParams{MeetingID: m.ID, Seq: int32(seq), Size: n}); err != nil {
			t.Fatal(err)
		}
	}

	lw := &liveWorker{w: e.w}
	runLive := func() {
		t.Helper()
		job := &river.Job[queue.LiveCaptionArgs]{
			JobRow: &rivertype.JobRow{ID: 1},
			Args:   queue.LiveCaptionArgs{MeetingID: m.ID},
		}
		if err := lw.Work(ctx, job); err != nil {
			t.Fatal(err)
		}
	}

	// 第 0 片（0..6s）：第一句定稿并做同音热词纠错（「非书」→「飞书」），第二句靠近末尾暂定
	putAACChunk(0, 6)
	e.engine.segs = []transcript.Segment{
		{Start: 0.2, End: 3.0, Text: "先在非书群里同步一下。"},
		{Start: 4.0, End: 5.6, Text: "后面这句还没"},
	}
	runLive()

	if got := e.engine.language.Load(); got == nil || *got != "zh" {
		t.Fatalf("engine language = %v, want zh", got)
	}
	lt, err := e.q.GetLiveTranscript(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lt.Chunks != 1 || lt.FinalizedSec != 3.0 {
		t.Fatalf("第一片后 chunks=%d finalized_sec=%v", lt.Chunks, lt.FinalizedSec)
	}
	var segs []transcript.Segment
	if err := json.Unmarshal(lt.Segments, &segs); err != nil {
		t.Fatal(err)
	}
	if len(segs) != 2 || segs[0].Text != "先在飞书群里同步一下。" || segs[1].Text != "后面这句还没" {
		t.Fatalf("第一片后实时字幕不对：%+v", segs)
	}

	// 第 1 片（6..12s）：只重算 3.0s 之后的 9s 尾巴，把跨分片的半句话补全定稿
	putAACChunk(1, 6)
	e.engine.segs = []transcript.Segment{
		{Start: 1.0, End: 4.5, Text: "后面这句话已经说完了。"},
	}
	runLive()

	// 尾巴时长应约为 9 秒（允许 ADTS 帧长 ±0.2s 误差），而不是整段 12 秒
	tailSec := float64(e.engine.samples.Load()) / audio.SampleRate
	if tailSec < 8.5 || tailSec > 9.5 {
		t.Fatalf("第二片应只解码约 9 秒尾巴，实际 %.2f 秒", tailSec)
	}
	lt, err = e.q.GetLiveTranscript(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lt.Chunks != 2 {
		t.Fatalf("第二片后 chunks=%d, want 2", lt.Chunks)
	}
	if err := json.Unmarshal(lt.Segments, &segs); err != nil {
		t.Fatal(err)
	}
	if len(segs) != 2 || segs[0].Text != "先在飞书群里同步一下。" || segs[1].Text != "后面这句话已经说完了。" || segs[1].Start != 4.0 {
		t.Fatalf("第二片后实时字幕不对：%+v", segs)
	}

	// 录音结束，正式转写完成后自动清掉 live_transcripts
	runID := uuid.New()
	if _, err := e.q.QueueMeeting(ctx, sqlc.QueueMeetingParams{
		ID: m.ID, RunID: &runID, RunBy: &e.user.ID, FromStatus: []string{"uploading"},
	}); err != nil {
		t.Fatal(err)
	}
	e.engine.segs = nil // 恢复默认整场句子
	procJob := &river.Job[queue.ProcessArgs]{
		JobRow: &rivertype.JobRow{ID: 2},
		Args:   queue.ProcessArgs{MeetingID: m.ID, RunID: runID},
	}
	if err := e.w.Work(ctx, procJob); err != nil {
		t.Fatal(err)
	}
	if _, err := e.q.GetLiveTranscript(ctx, m.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("正式转写完成后 live_transcripts 应该已清掉，实际 err=%v", err)
	}
	if _, err := e.q.GetTranscript(ctx, m.ID); err != nil {
		t.Fatalf("正式转写应该已存好：%v", err)
	}
}

func TestLiveWorkerSkipsUploadSource(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()

	m, err := e.q.CreateMeeting(ctx, sqlc.CreateMeetingParams{
		OwnerID:    e.user.ID,
		Title:      "上传音频",
		Source:     "upload",
		Template:   "general",
		Visibility: "team",
	})
	if err != nil {
		t.Fatal(err)
	}
	data := wav()
	n, err := e.w.store.PutChunk(m.ID, 0, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.q.AddChunk(ctx, sqlc.AddChunkParams{MeetingID: m.ID, Seq: 0, Size: n}); err != nil {
		t.Fatal(err)
	}

	lw := &liveWorker{w: e.w}
	job := &river.Job[queue.LiveCaptionArgs]{
		JobRow: &rivertype.JobRow{ID: 1},
		Args:   queue.LiveCaptionArgs{MeetingID: m.ID},
	}
	if err := lw.Work(ctx, job); err != nil {
		t.Fatal(err)
	}
	if e.engine.transcribed.Load() != 0 {
		t.Fatalf("上传文件的会议不该跑实时字幕，transcribed=%d", e.engine.transcribed.Load())
	}
}

// 真实 SenseVoice + 半截 WebM 字节流切片验证：模拟浏览器 MediaRecorder 把一段连续的 WebM/Opus 录音
// 从句子中间生生切成两片，验证第一片出半截字幕、第二片接上后自动把跨分片的那句话完整补齐。
func TestLiveWorkerRealWebMChunks(t *testing.T) {
	modelsDir := os.Getenv("ZSMEET_TEST_MODELS")
	if modelsDir == "" {
		t.Skip("没有设置 ZSMEET_TEST_MODELS")
	}
	if _, err := exec.LookPath("say"); err != nil {
		t.Skip("没有安装 macOS say")
	}
	e := newEnv(t)
	ctx := t.Context()

	realEngine, err := asr.Load(models.Paths{Dir: modelsDir}, 4, "auto")
	if err != nil {
		t.Fatal(err)
	}
	e.w.newEngine = func() (recognizer, error) { return realEngine, nil }

	dir := t.TempDir()
	aiff1 := filepath.Join(dir, "s1.aiff")
	aiff2 := filepath.Join(dir, "s2.aiff")
	if msg, err := exec.CommandContext(ctx, "say", "-v", "Tingting", "-o", aiff1, "今天我们先核对季度预算。").CombinedOutput(); err != nil {
		t.Skipf("say Tingting 不可用：%v (%s)", err, msg)
	}
	if msg, err := exec.CommandContext(ctx, "say", "-v", "Tingting", "-o", aiff2, "接下来讨论实时字幕的上线安排。").CombinedOutput(); err != nil {
		t.Fatalf("say 第二句失败：%v (%s)", err, msg)
	}
	webm := filepath.Join(dir, "live.webm")
	// 两句之间留 2.2 秒停顿，让切在字节中点时正好落在第二句中间
	ff := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-i", aiff1, "-i", aiff2,
		"-filter_complex", "[0:a]apad=pad_dur=2.2[a0];[a0][1:a]concat=n=2:v=0:a=1",
		"-ac", "1", "-ar", "48000", "-c:a", "libopus", "-b:a", "32k", webm)
	if msg, err := ff.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg 合成 WebM 失败：%v\n%s", err, msg)
	}
	raw, err := os.ReadFile(webm)
	if err != nil {
		t.Fatal(err)
	}
	mid := len(raw) * 2 / 3

	m, err := e.q.CreateMeeting(ctx, sqlc.CreateMeetingParams{
		OwnerID:    e.user.ID,
		Title:      "真实 WebM 实时字幕",
		Source:     "record",
		Template:   "general",
		Visibility: "team",
	})
	if err != nil {
		t.Fatal(err)
	}

	lw := &liveWorker{w: e.w}
	job := &river.Job[queue.LiveCaptionArgs]{
		JobRow: &rivertype.JobRow{ID: 1},
		Args:   queue.LiveCaptionArgs{MeetingID: m.ID},
	}

	// 上传前 2/3 字节（第二句被拦腰截断）
	n0, err := e.w.store.PutChunk(m.ID, 0, bytes.NewReader(raw[:mid]))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.q.AddChunk(ctx, sqlc.AddChunkParams{MeetingID: m.ID, Seq: 0, Size: n0}); err != nil {
		t.Fatal(err)
	}
	if err := lw.Work(ctx, job); err != nil {
		t.Fatal(err)
	}
	lt1, err := e.q.GetLiveTranscript(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("第 0 片后 live_transcripts: finalized_sec=%.2f segments=%s", lt1.FinalizedSec, lt1.Segments)

	// 上传剩余 1/3 字节（补齐第二句）
	n1, err := e.w.store.PutChunk(m.ID, 1, bytes.NewReader(raw[mid:]))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.q.AddChunk(ctx, sqlc.AddChunkParams{MeetingID: m.ID, Seq: 1, Size: n1}); err != nil {
		t.Fatal(err)
	}
	if err := lw.Work(ctx, job); err != nil {
		t.Fatal(err)
	}
	lt2, err := e.q.GetLiveTranscript(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("第 1 片后 live_transcripts: finalized_sec=%.2f segments=%s", lt2.FinalizedSec, lt2.Segments)

	var segs []transcript.Segment
	if err := json.Unmarshal(lt2.Segments, &segs); err != nil {
		t.Fatal(err)
	}
	var joined strings.Builder
	for _, s := range segs {
		joined.WriteString(s.Text)
	}
	got := joined.String()
	for _, kw := range []string{"预算", "实时字幕", "上线"} {
		if !strings.Contains(got, kw) {
			t.Errorf("补齐分片后的实时字幕缺少关键词 %q：%s", kw, got)
		}
	}
}

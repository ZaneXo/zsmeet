package pipeline

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/ZaneXo/zsmeet/internal/config"
	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/queue"
	"github.com/ZaneXo/zsmeet/internal/search"
	"github.com/ZaneXo/zsmeet/internal/secret"
	"github.com/ZaneXo/zsmeet/internal/settings"
	"github.com/ZaneXo/zsmeet/internal/storage"
	"github.com/ZaneXo/zsmeet/internal/testdb"
	"github.com/ZaneXo/zsmeet/internal/transcript"
)

// fakeEngine 代替真的识别引擎：不看音频内容，返回固定的两句话。
type fakeEngine struct {
	transcribed atomic.Int32
	samples     atomic.Int64
}

func (e *fakeEngine) Transcribe(_ context.Context, samples []float32, progress func(float64)) ([]transcript.Segment, error) {
	e.transcribed.Add(1)
	e.samples.Store(int64(len(samples)))
	progress(1)
	return []transcript.Segment{
		{Start: 0, End: 0.9, Speaker: -1, Text: "一期只做网页端。"},
		{Start: 1.1, End: 1.9, Speaker: -1, Text: "同意，录音器放到二期。"},
	}, nil
}

func (e *fakeEngine) Diarize([]float32) []transcript.Turn {
	return []transcript.Turn{{Start: 0, End: 1, Speaker: 4}, {Start: 1, End: 2, Speaker: 9}}
}

type env struct {
	t      *testing.T
	w      *Worker
	q      *sqlc.Queries
	pool   *pgxpool.Pool
	engine *fakeEngine
	user   sqlc.User
	// 最后一次发给假大模型的请求内容
	lastPrompt atomic.Pointer[string]
}

func newEnv(t *testing.T) *env {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("没有安装 ffmpeg")
	}
	pool := testdb.Open(t, "pipeline")
	store, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	box, err := secret.New("test-secret-key-0123456789")
	if err != nil {
		t.Fatal(err)
	}
	q := sqlc.New(pool)
	w := newWorker(pool, config.Config{}, store, settings.New(q, box))
	engine := &fakeEngine{}
	w.ensureModels = func(context.Context, func(done, total int64)) error { return nil }
	w.newEngine = func() (recognizer, error) { return engine, nil }

	user, err := q.CreateUser(t.Context(), sqlc.CreateUserParams{Email: "admin@example.com", Name: "张敏", PasswordHash: "x", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, w: w, q: q, pool: pool, engine: engine, user: user}
}

// wav 生成两秒钟的正弦波，格式是最普通的 16 位单声道 WAV。
func wav() []byte {
	const rate, seconds = 16000, 2
	data := new(bytes.Buffer)
	for i := range rate * seconds {
		binary.Write(data, binary.LittleEndian, int16(8000*math.Sin(2*math.Pi*440*float64(i)/rate)))
	}
	out := new(bytes.Buffer)
	out.WriteString("RIFF")
	binary.Write(out, binary.LittleEndian, uint32(36+data.Len()))
	out.WriteString("WAVEfmt ")
	binary.Write(out, binary.LittleEndian, uint32(16))     // fmt 块长度
	binary.Write(out, binary.LittleEndian, uint16(1))      // PCM
	binary.Write(out, binary.LittleEndian, uint16(1))      // 单声道
	binary.Write(out, binary.LittleEndian, uint32(rate))   // 采样率
	binary.Write(out, binary.LittleEndian, uint32(rate*2)) // 每秒字节数
	binary.Write(out, binary.LittleEndian, uint16(2))      // 每个采样的字节数
	binary.Write(out, binary.LittleEndian, uint16(16))     // 位深
	out.WriteString("data")
	binary.Write(out, binary.LittleEndian, uint32(data.Len()))
	out.Write(data.Bytes())
	return out.Bytes()
}

// upload 新建一场会议，把 content 切成两片存好，再入队。返回给 worker 的任务参数。
func (e *env) upload(content []byte) queue.ProcessArgs {
	e.t.Helper()
	ctx := e.t.Context()
	m, err := e.q.CreateMeeting(ctx, sqlc.CreateMeetingParams{
		OwnerID: e.user.ID, Title: "产品周会", Source: "upload", Template: "general", Hotwords: "River",
	})
	if err != nil {
		e.t.Fatal(err)
	}
	half := len(content) / 2
	for seq, part := range [][]byte{content[:half], content[half:]} {
		n, err := e.w.store.PutChunk(m.ID, seq, bytes.NewReader(part))
		if err != nil {
			e.t.Fatal(err)
		}
		if err := e.q.AddChunk(ctx, sqlc.AddChunkParams{MeetingID: m.ID, Seq: int32(seq), Size: n}); err != nil {
			e.t.Fatal(err)
		}
	}
	return e.enqueue(m.ID, "uploading")
}

func (e *env) enqueue(id uuid.UUID, from string) queue.ProcessArgs {
	e.t.Helper()
	runID := uuid.New()
	if _, err := e.q.QueueMeeting(e.t.Context(), sqlc.QueueMeetingParams{
		ID: id, RunID: &runID, RunBy: &e.user.ID, FromStatus: []string{from},
	}); err != nil {
		e.t.Fatalf("入队失败：%v", err)
	}
	return queue.ProcessArgs{MeetingID: id, RunID: runID}
}

func (e *env) work(args queue.ProcessArgs) error {
	return e.w.Work(e.t.Context(), &river.Job[queue.ProcessArgs]{Args: args})
}

func (e *env) meeting(id uuid.UUID) sqlc.Meeting {
	e.t.Helper()
	row, err := e.q.GetMeeting(e.t.Context(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return row.Meeting
}

// llm 起一个假的大模型接口并保存为团队配置。fail 为 true 时接口返回 401。
func (e *env) llm(fail *atomic.Bool) (calls *atomic.Int32) {
	e.t.Helper()
	calls = new(atomic.Int32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		prompt := string(body)
		e.lastPrompt.Store(&prompt)
		w.Header().Set("Content-Type", "application/json")
		if fail != nil && fail.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"message":"Incorrect API key provided","type":"invalid_request_error"}}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"id": "x", "object": "chat.completion", "model": "test",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "## 摘要\n[00:00] 一期只做网页端。"},
			}},
		})
	}))
	e.t.Cleanup(server.Close)
	if err := e.w.settings.SaveLLM(e.t.Context(), settings.LLM{BaseURL: server.URL + "/v1", Model: "test", APIKey: "sk-test"}); err != nil {
		e.t.Fatal(err)
	}
	return calls
}

func TestWorkTranscribesAndWritesMinutes(t *testing.T) {
	e := newEnv(t)
	calls := e.llm(nil)
	args := e.upload(wav())

	if err := e.work(args); err != nil {
		t.Fatal(err)
	}

	m := e.meeting(args.MeetingID)
	if m.Status != "done" || m.Error != "" {
		t.Fatalf("会议应该处理完成，实际状态 %s，错误 %q", m.Status, m.Error)
	}
	if m.DurationSec != 2 {
		t.Errorf("时长应该是 2 秒，得到 %d", m.DurationSec)
	}
	// 两片拼起来、转成 16k 以后应该正好是两秒的采样
	if got := e.engine.samples.Load(); got != 32000 {
		t.Errorf("交给识别引擎的采样数应该是 32000，得到 %d", got)
	}
	if info, err := os.Stat(e.w.store.Path(m.AudioKey)); err != nil || info.Size() == 0 {
		t.Errorf("回放用的录音没有存下来：%v", err)
	}
	if chunks, _ := e.q.ListChunks(t.Context(), m.ID); len(chunks) != 0 {
		t.Errorf("处理完应该删掉分片，还剩 %d 片", len(chunks))
	}

	raw, err := e.q.GetTranscript(t.Context(), m.ID)
	if err != nil {
		t.Fatal(err)
	}
	var segs []transcript.Segment
	if err := json.Unmarshal(raw, &segs); err != nil {
		t.Fatal(err)
	}
	if len(segs) != 2 || segs[0].Speaker != 0 || segs[1].Speaker != 1 {
		t.Errorf("转写应该是两段、两位说话人（按开口顺序编号），得到 %+v", segs)
	}

	if m.CurrentVersion != 1 || calls.Load() != 1 {
		t.Fatalf("应该生成第 1 版纪要并只调用一次大模型，版本 %d，调用 %d 次", m.CurrentVersion, calls.Load())
	}
	mv, err := e.q.GetMinutesVersion(t.Context(), sqlc.GetMinutesVersionParams{MeetingID: m.ID, Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	if mv.Source != "auto" || mv.AuthorID != nil || !strings.Contains(mv.Content, "一期只做网页端") {
		t.Errorf("第一版纪要应该是自动生成、没有作者：%+v", mv)
	}

	// 转写和纪要里的词都要能搜到
	for _, word := range []string{"录音器", "摘要", "周会"} {
		query := search.TSQuery([]string{word})
		rows, err := e.q.ListMeetings(t.Context(), sqlc.ListMeetingsParams{Query: &query, MaxRows: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Errorf("搜「%s」应该找到这场会议，找到 %d 场", word, len(rows))
		}
	}
}

func TestWorkWithoutLLMKeepsTranscriptOnly(t *testing.T) {
	e := newEnv(t)
	args := e.upload(wav())

	if err := e.work(args); err != nil {
		t.Fatal(err)
	}
	m := e.meeting(args.MeetingID)
	if m.Status != "done" || m.CurrentVersion != 0 {
		t.Fatalf("没配大模型时应该完成但没有纪要，状态 %s，版本 %d", m.Status, m.CurrentVersion)
	}
	if _, err := e.q.GetTranscript(t.Context(), m.ID); err != nil {
		t.Errorf("转写应该已经保存：%v", err)
	}
}

func TestLLMFailureThenRetryOnlyRewritesMinutes(t *testing.T) {
	e := newEnv(t)
	var fail atomic.Bool
	fail.Store(true)
	e.llm(&fail)
	args := e.upload(wav())

	if err := e.work(args); err == nil {
		t.Fatal("大模型报错时任务应该返回错误")
	}
	m := e.meeting(args.MeetingID)
	if m.Status != "failed" || m.Stage != StageMinutes || !strings.Contains(m.Error, "401") {
		t.Fatalf("应该失败在写纪要这一步并带上原因，状态 %s，步骤 %s，错误 %q", m.Status, m.Stage, m.Error)
	}

	// 修好大模型后重试：已经有转写了，不应该再识别一遍
	fail.Store(false)
	if err := e.work(e.enqueue(m.ID, "failed")); err != nil {
		t.Fatal(err)
	}
	m = e.meeting(m.ID)
	if m.Status != "done" || m.CurrentVersion != 1 {
		t.Fatalf("重试后应该完成并有第 1 版纪要，状态 %s，版本 %d", m.Status, m.CurrentVersion)
	}
	if n := e.engine.transcribed.Load(); n != 1 {
		t.Errorf("重试不应该重新识别，识别了 %d 次", n)
	}
}

func TestRegenerateAddsVersionCreditedToRequester(t *testing.T) {
	e := newEnv(t)
	e.llm(nil)
	args := e.upload(wav())
	if err := e.work(args); err != nil {
		t.Fatal(err)
	}

	if err := e.work(e.enqueue(args.MeetingID, "done")); err != nil {
		t.Fatal(err)
	}
	m := e.meeting(args.MeetingID)
	if m.CurrentVersion != 2 {
		t.Fatalf("重新生成后应该是第 2 版，得到 %d", m.CurrentVersion)
	}
	mv, err := e.q.GetMinutesVersion(t.Context(), sqlc.GetMinutesVersionParams{MeetingID: m.ID, Version: 2})
	if err != nil {
		t.Fatal(err)
	}
	if mv.Source != "regenerate" || mv.AuthorID == nil || *mv.AuthorID != e.user.ID {
		t.Errorf("第 2 版应该记为发起人重新生成的：%+v", mv)
	}
	if _, err := e.q.GetMinutesVersion(t.Context(), sqlc.GetMinutesVersionParams{MeetingID: m.ID, Version: 1}); err != nil {
		t.Errorf("旧版本应该还在：%v", err)
	}
}

func TestBrokenAudioFailsWithReadableReason(t *testing.T) {
	e := newEnv(t)
	args := e.upload([]byte("这不是音频文件，只是一段文字。"))

	if err := e.work(args); err == nil {
		t.Fatal("解不开的文件应该让任务失败")
	}
	m := e.meeting(args.MeetingID)
	if m.Status != "failed" || !strings.Contains(m.Error, "音频无法解码") {
		t.Fatalf("应该提示音频无法解码，状态 %s，错误 %q", m.Status, m.Error)
	}
	if strings.Contains(m.Error, "ffmpeg") {
		t.Errorf("给用户看的原因里不该出现 ffmpeg 的原始报错：%q", m.Error)
	}
	if e.engine.transcribed.Load() != 0 {
		t.Error("解码失败后不应该再去识别")
	}
}

// 会议被新一轮任务接手后，旧任务什么都不能改。
func TestSupersededRunChangesNothing(t *testing.T) {
	e := newEnv(t)
	e.llm(nil)
	old := e.upload(wav())

	// 模拟巡检把它重新入队：换了 run_id
	newRun := uuid.New()
	if _, err := e.pool.Exec(t.Context(), "UPDATE meetings SET run_id = $2 WHERE id = $1", old.MeetingID, newRun); err != nil {
		t.Fatal(err)
	}

	if err := e.work(old); err != nil {
		t.Fatalf("被顶替的任务应该安静地结束，得到 %v", err)
	}
	m := e.meeting(old.MeetingID)
	if m.Status != "queued" || *m.RunID != newRun {
		t.Fatalf("旧任务不应该改动会议，状态 %s", m.Status)
	}
	if e.engine.transcribed.Load() != 0 {
		t.Error("旧任务不应该开始识别")
	}
}

func TestReaperRequeuesStalledAndGivesUpEventually(t *testing.T) {
	e := newEnv(t)
	stalled := e.upload(wav())
	healthy := e.upload(wav())
	hopeless := e.upload(wav())
	ctx := t.Context()

	// 三场都在处理中：一场心跳停了很久，一场刚刚还有心跳，一场心跳停了而且已经重新入队过太多次
	set := func(id uuid.UUID, heartbeat time.Duration, requeues int) {
		if _, err := e.pool.Exec(ctx,
			"UPDATE meetings SET status = 'processing', heartbeat_at = now() - $2::interval, requeues = $3 WHERE id = $1",
			id, heartbeat.String(), requeues); err != nil {
			t.Fatal(err)
		}
	}
	set(stalled.MeetingID, 10*time.Minute, 0)
	set(healthy.MeetingID, 5*time.Second, 0)
	set(hopeless.MeetingID, 10*time.Minute, maxRequeues)

	var inserted []queue.ProcessArgs
	requeued, failed, err := requeueStalled(ctx, e.pool, func(_ context.Context, _ pgx.Tx, args queue.ProcessArgs) error {
		inserted = append(inserted, args)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if requeued != 1 || failed != 1 || len(inserted) != 1 {
		t.Fatalf("应该重新入队 1 场、判失败 1 场，得到 %d 和 %d，入队 %d 个任务", requeued, failed, len(inserted))
	}

	m := e.meeting(stalled.MeetingID)
	if m.Status != "queued" || m.Requeues != 1 || *m.RunID == stalled.RunID || inserted[0].RunID != *m.RunID {
		t.Errorf("卡住的会议应该换一个 run_id 重新排队：%+v，任务参数 %+v", m, inserted[0])
	}
	if m := e.meeting(healthy.MeetingID); m.Status != "processing" || *m.RunID != healthy.RunID {
		t.Errorf("心跳正常的会议不应该被动：%+v", m)
	}
	if m := e.meeting(hopeless.MeetingID); m.Status != "failed" || m.Error == "" {
		t.Errorf("反复挂掉的会议应该判为失败并写明原因：%+v", m)
	}

	// 原来那个任务如果其实还活着，它的写入现在都应该落空
	n, err := e.q.FinishMeeting(ctx, sqlc.FinishMeetingParams{ID: stalled.MeetingID, RunID: &stalled.RunID})
	if err != nil || n != 0 {
		t.Errorf("旧任务不应该还能把会议标记为完成：n=%d err=%v", n, err)
	}
}

// 录音时标记的重点要交给大模型。
func TestMarksReachMinutesPrompt(t *testing.T) {
	e := newEnv(t)
	e.llm(nil)
	args := e.upload(wav())
	if _, err := e.pool.Exec(t.Context(), "UPDATE meetings SET marks = '{1.5}' WHERE id = $1", args.MeetingID); err != nil {
		t.Fatal(err)
	}
	if err := e.work(args); err != nil {
		t.Fatal(err)
	}
	if m := e.meeting(args.MeetingID); m.Status != "done" {
		t.Fatalf("会议应该处理完成，实际 %s：%s", m.Status, m.Error)
	}
	prompt := e.lastPrompt.Load()
	if prompt == nil || !strings.Contains(*prompt, "【★ 00:01 标记为重点】") {
		t.Errorf("提示词里没有标记的重点：%v", prompt)
	}
}

// 重新转写：旧的转写在新的存好之前一直留着；识别失败时旧的转写和纪要都还在，标记也留着（重试时还会重新识别）。
func TestRetranscribeKeepsOldTranscriptUntilReplaced(t *testing.T) {
	e := newEnv(t)
	e.llm(nil)
	args := e.upload(wav())
	if err := e.work(args); err != nil {
		t.Fatal(err)
	}
	id := args.MeetingID
	ctx := t.Context()
	setOld := func() {
		t.Helper()
		if _, err := e.pool.Exec(ctx, `UPDATE transcripts SET segments = '[{"start":0,"end":1,"speaker":0,"text":"旧的转写。"}]' WHERE meeting_id = $1`, id); err != nil {
			t.Fatal(err)
		}
		if err := e.q.MarkRetranscribe(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	text := func() string {
		t.Helper()
		raw, err := e.q.GetTranscript(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}

	// 成功：重新识别，新的转写替换旧的，标记清掉
	setOld()
	if err := e.work(e.enqueue(id, "done")); err != nil {
		t.Fatal(err)
	}
	if m := e.meeting(id); m.Status != "done" || m.Retranscribe || e.engine.transcribed.Load() != 2 || !strings.Contains(text(), "一期只做网页端") {
		t.Fatalf("应该重新识别并替换旧的转写：状态 %s，标记 %v，识别 %d 次，转写 %s", m.Status, m.Retranscribe, e.engine.transcribed.Load(), text())
	}

	// 失败（录音文件没了）：旧的转写还在，标记也还在
	setOld()
	m := e.meeting(id)
	if err := os.Remove(e.w.store.Path(m.AudioKey)); err != nil {
		t.Fatal(err)
	}
	if err := e.work(e.enqueue(id, "done")); err == nil {
		t.Fatal("录音没了，重新识别应该失败")
	}
	if m := e.meeting(id); m.Status != "failed" || !m.Retranscribe || !strings.Contains(text(), "旧的转写") || m.CurrentVersion == 0 {
		t.Errorf("失败时旧的转写和纪要都要留着：状态 %s，标记 %v，版本 %d，转写 %s", m.Status, m.Retranscribe, m.CurrentVersion, text())
	}
}

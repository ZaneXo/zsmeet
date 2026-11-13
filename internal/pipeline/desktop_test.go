package pipeline

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ZaneXo/zsmeet/internal/audio"
	"github.com/ZaneXo/zsmeet/internal/audio/audiotest"
	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/queue"
	"github.com/ZaneXo/zsmeet/internal/service"
)

// 桌面版录音：录音程序把 AAC（ADTS）只在帧的边界上切成 10 秒一片，App 边录边按序号上传，
// 每标记一次重点就整份传一次，结束后带上全部标记「上传完成」。这里和接口一样经过业务层上传，再让 worker 处理：
// 分片接回去是一整段连续的录音，一帧不少，时长对得上；回放的录音前后顺序不乱；标记的重点交给大模型；能导出。
func TestDesktopRecordingADTS(t *testing.T) {
	e := newEnv(t)
	e.llm(nil)
	ctx := t.Context()
	svc := e.svc()

	// 三段各 8 秒、音高不同的声音，按 10 秒切片：分片的边界和换音高的地方错开
	tones := []float64{440, 660, 880}
	stream := audiotest.Tones(t, 8, tones...)
	chunks := audiotest.Split(t, stream, 10)
	if len(chunks) != 3 || !bytes.Equal(bytes.Join(chunks, nil), stream) {
		t.Fatalf("24 秒的录音应该在帧的边界上切成 3 片，得到 %d 片", len(chunks))
	}

	m, err := svc.CreateMeeting(ctx, e.user, service.NewMeeting{Title: "客户电话", Source: "desktop"})
	if err != nil {
		t.Fatal(err)
	}
	put := func(seq int) {
		t.Helper()
		if err := svc.PutChunk(ctx, e.user, m.ID, seq, bytes.NewReader(chunks[seq])); err != nil {
			t.Fatalf("第 %d 片传不上去：%v", seq+1, err)
		}
	}
	mark := func(marks ...float64) {
		t.Helper()
		if err := svc.SaveMarks(ctx, e.user, m.ID, marks); err != nil {
			t.Fatalf("标记存不下来：%v", err)
		}
	}
	put(0)
	mark(4.5)
	put(1)
	put(1) // 断网后重传同一片
	mark(4.5, 12.5)
	put(2)
	queued, err := svc.CompleteUpload(ctx, e.user, m.ID, len(chunks), []float64{4.5, 12.5, 20})
	if err != nil || queued.Status != "queued" {
		t.Fatalf("传完应该进入排队：%v，状态 %s", err, queued.Status)
	}
	e.workQueued()

	done := e.meeting(m.ID)
	if done.Status != "done" || done.Error != "" || done.Source != "desktop" {
		t.Fatalf("桌面版录音应该处理完成，状态 %s，来源 %s，错误 %q", done.Status, done.Source, done.Error)
	}
	want := audiotest.Seconds(t, stream)
	e.checkRecognized(want)
	if done.DurationSec != 24 {
		t.Errorf("时长应该是 24 秒，得到 %d", done.DurationSec)
	}
	if left, _ := e.q.ListChunks(ctx, m.ID); len(left) != 0 {
		t.Errorf("处理完应该删掉分片，还剩 %d 片", len(left))
	}

	// 回放的录音（网页和 App 播放的就是它）：一样长，三段声音按录的顺序。
	// ADTS 的帧各自独立，顺序乱了也照样能解码、时长也不变，只能听出来
	path, err := svc.AudioPath(ctx, e.user, m.ID)
	if err != nil {
		t.Fatalf("应该能回放：%v", err)
	}
	pcm := decode(t, path)
	if got := float64(len(pcm)) / audio.SampleRate; math.Abs(got-want) > 0.05 {
		t.Errorf("回放的录音应该是 %.2f 秒，得到 %.2f 秒", want, got)
	}
	for i, hz := range tones {
		at := 8*float64(i) + 4
		if got := toneHz(pcm, at-0.25, at+0.25); !sameTone(got, hz) {
			t.Errorf("第 %v 秒应该是第 %d 段的 %v Hz，听到的是 %.0f Hz", at, i+1, hz, got)
		}
	}

	// 标记的重点存下来了，写纪要时标在对应的时间点上
	if !slices.Equal(done.Marks, []float32{4.5, 12.5, 20}) {
		t.Errorf("标记应该是 [4.5 12.5 20]，得到 %v", done.Marks)
	}
	prompt := e.lastPrompt.Load()
	for _, tag := range []string{"【★ 00:04 标记为重点】", "【★ 00:12 标记为重点】", "【★ 00:20 标记为重点】"} {
		if prompt == nil || !strings.Contains(*prompt, tag) {
			t.Errorf("提示词里没有 %s", tag)
		}
	}

	// 导出的纪要里写着时长
	file, err := svc.Export(ctx, e.user, m.ID, "minutes", "md")
	if err != nil || !strings.Contains(string(file.Data), "时长：24 秒") {
		t.Errorf("导出的纪要应该写着时长：%v\n%s", err, file.Data)
	}
}

// 录到一半录音程序没了（App 被强制退出、电脑没电），下次打开接着录：录音程序重新开始编码，
// 分片序号接着上次的往下排。两次录的接起来照样是一整段录音，先录的在前，时长是两次相加。
func TestDesktopRecordingContinuedAfterInterruption(t *testing.T) {
	e := newEnv(t)
	before, after := audiotest.Tones(t, 12, 440), audiotest.Tones(t, 7, 880)
	done := e.processDesktop(append(audiotest.Split(t, before, 10), audiotest.Split(t, after, 10)...))
	if done.Status != "done" || done.DurationSec != 19 {
		t.Fatalf("接着录的会议应该处理完成、时长 19 秒，状态 %s，时长 %d，错误 %q", done.Status, done.DurationSec, done.Error)
	}
	e.checkRecognized(audiotest.Seconds(t, before) + audiotest.Seconds(t, after))
	pcm := decode(t, e.w.store.Path(done.AudioKey))
	if got := toneHz(pcm, 6, 6.5); !sameTone(got, 440) {
		t.Errorf("前面应该是第一次录的 440 Hz，听到的是 %.0f Hz", got)
	}
	if got := toneHz(pcm, 15.5, 16); !sameTone(got, 880) {
		t.Errorf("后面应该是接着录的 880 Hz，听到的是 %.0f Hz", got)
	}
}

// 不到 10 秒就结束的录音只有一片：worker 不拼接，直接拿这一片转码。分片的文件名没有扩展名，ffmpeg 要自己认出是 ADTS。
func TestDesktopRecordingSingleChunk(t *testing.T) {
	e := newEnv(t)
	stream := audiotest.Tones(t, 3, 440)
	done := e.processDesktop(audiotest.Split(t, stream, 10))
	if done.Status != "done" || done.DurationSec != 3 {
		t.Fatalf("只有一片的录音应该处理完成、时长 3 秒，状态 %s，时长 %d，错误 %q", done.Status, done.DurationSec, done.Error)
	}
	e.checkRecognized(audiotest.Seconds(t, stream))
}

// 没开自动增益的麦克风录的会（人离麦克风远，比正常说话小 20 dB）：交给识别的声音调到了正常说话的音量，
// 回放用的录音也调响了一些；重新转写时（从存好的回放录音解码）交给识别的声音也一样调好了。
func TestDesktopQuietRecordingIsRaised(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	stream := audiotest.Speech(t, -20)
	done := e.processDesktop(audiotest.Split(t, stream, 10))
	if done.Status != "done" {
		t.Fatalf("应该处理完成，状态 %s，错误 %q", done.Status, done.Error)
	}
	e.checkRecognized(audiotest.Seconds(t, stream))

	in := filepath.Join(t.TempDir(), "quiet.aac")
	if err := os.WriteFile(in, stream, 0o644); err != nil {
		t.Fatal(err)
	}
	level := func(samples []float32) float64 { return audiotest.SpeechLevel(samples, audio.SampleRate) }
	original, playback := level(decode(t, in)), level(decode(t, e.w.store.Path(done.AudioKey)))
	heard := level(*e.engine.heard.Load())
	t.Logf("说话的音量：录的 %.1f dBFS，交给识别的 %.1f dBFS，回放的 %.1f dBFS", original, heard, playback)
	if heard < -24 || heard > -12 {
		t.Errorf("交给识别的声音里说话的音量应该在 -24 到 -12 dBFS 之间，实际 %.1f dBFS（录的是 %.1f）", heard, original)
	}
	if playback < original+8 {
		t.Errorf("回放的录音应该调响至少 8 dB：录的 %.1f dBFS，回放的 %.1f dBFS", original, playback)
	}

	// 重新转写：从存好的回放录音解出来，照样调到正常说话的音量
	args := e.enqueue(done.ID, "done")
	if err := e.q.MarkRetranscribe(ctx, done.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.work(args); err != nil {
		t.Fatal(err)
	}
	if again := level(*e.engine.heard.Load()); again < -24 || again > -12 {
		t.Errorf("重新转写时交给识别的声音里说话的音量应该在 -24 到 -12 dBFS 之间，实际 %.1f dBFS", again)
	}
}

// svc 返回和接口用的同一套业务层：上传分片、标记、上传完成都经过它，和 App 调接口时一样。
func (e *env) svc() *service.Service {
	return service.New(e.pool, e.w.store, e.w.settings, e.w.queue)
}

// processDesktop 像桌面版那样建一场会议、按序号传完分片、「上传完成」，再执行放进队列的处理任务，返回处理后的会议。
func (e *env) processDesktop(chunks [][]byte) sqlc.Meeting {
	e.t.Helper()
	ctx, svc := e.t.Context(), e.svc()
	m, err := svc.CreateMeeting(ctx, e.user, service.NewMeeting{Title: "客户电话", Source: "desktop"})
	if err != nil {
		e.t.Fatal(err)
	}
	for seq, chunk := range chunks {
		if err := svc.PutChunk(ctx, e.user, m.ID, seq, bytes.NewReader(chunk)); err != nil {
			e.t.Fatalf("第 %d 片传不上去：%v", seq+1, err)
		}
	}
	if _, err := svc.CompleteUpload(ctx, e.user, m.ID, len(chunks), nil); err != nil {
		e.t.Fatal(err)
	}
	e.workQueued()
	return e.meeting(m.ID)
}

// workQueued 执行「上传完成」时放进队列的那个处理任务（测试里不跑 River，任务留在表里）。
func (e *env) workQueued() {
	e.t.Helper()
	jobs := e.jobs(queue.ProcessArgs{}.Kind())
	if len(jobs) != 1 {
		e.t.Fatalf("应该正好有一个处理任务，实际 %d 个", len(jobs))
	}
	var args queue.ProcessArgs
	if err := json.Unmarshal([]byte(jobs[0]), &args); err != nil {
		e.t.Fatal(err)
	}
	if err := e.work(args); err != nil {
		e.t.Fatal(err)
	}
}

// checkRecognized 确认交给识别的录音正好 want 秒长（ADTS 流解码出来的长度）：少一帧、多一帧都差 21 毫秒，
// 这里只容许 5 毫秒的误差。
func (e *env) checkRecognized(want float64) {
	e.t.Helper()
	if got := float64(e.engine.samples.Load()) / audio.SampleRate; math.Abs(got-want) > 0.005 {
		e.t.Errorf("交给识别的录音应该是 %.3f 秒，得到 %.3f 秒", want, got)
	}
}

// decode 把录音原样（不做送去识别之前的音量处理）解成 16k 单声道的 PCM，用来检查录音本身。
func decode(t *testing.T, path string) []float32 {
	t.Helper()
	pcm := filepath.Join(t.TempDir(), "playback.f32")
	if err := audio.DecodeWith(t.Context(), path, pcm, ""); err != nil {
		t.Fatalf("回放用的录音解不开：%v", err)
	}
	samples, err := audio.ReadPCM(pcm)
	if err != nil {
		t.Fatal(err)
	}
	return samples
}

// toneHz 数过零点，估计 from 到 to 秒之间那段声音的频率。只对单一音高的正弦波准。
func toneHz(pcm []float32, from, to float64) float64 {
	a, b := int(from*audio.SampleRate), min(int(to*audio.SampleRate), len(pcm))
	crossings := 0
	for i := a + 1; i < b; i++ {
		if (pcm[i-1] < 0) != (pcm[i] < 0) {
			crossings++
		}
	}
	return float64(crossings) / 2 / (to - from)
}

// sameTone 判断估计出来的频率是不是这个音高（差 2% 以内）。
func sameTone(got, hz float64) bool {
	return math.Abs(got-hz) <= hz*0.02
}

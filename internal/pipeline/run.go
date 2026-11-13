package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ZaneXo/zsmeet/internal/audio"
	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/llm"
	"github.com/ZaneXo/zsmeet/internal/minutes"
	"github.com/ZaneXo/zsmeet/internal/search"
	"github.com/ZaneXo/zsmeet/internal/service"
	"github.com/ZaneXo/zsmeet/internal/storage"
	"github.com/ZaneXo/zsmeet/internal/transcript"
)

// 处理阶段。网页按这个顺序显示进度。
const (
	StageModels     = "models"     // 下载识别模型（只有第一次）
	StageAssemble   = "assemble"   // 拼接分片、转码
	StageTranscribe = "transcribe" // 识别文字
	StageDiarize    = "diarize"    // 区分说话人
	StageMinutes    = "minutes"    // 写纪要
)

// run 是对一场会议的一轮处理。
type run struct {
	w      *Worker
	m      sqlc.Meeting
	runID  uuid.UUID
	cancel context.CancelCauseFunc

	lastStage  string
	lastReport time.Time
}

func (r *run) process(ctx context.Context) error {
	segs, err := r.loadTranscript(ctx)
	if err != nil {
		return err
	}
	// 已经有转写（重新生成纪要，或者上次是写纪要那一步失败的）就不再重新识别；
	// 要求重新转写时照样识别，旧的转写在新的存好之前一直留着
	if segs == nil || r.m.Retranscribe {
		if segs, err = r.transcribe(ctx, segs); err != nil {
			return err
		}
	}
	return r.writeMinutes(ctx, segs)
}

// report 更新阶段和进度，同一阶段内最多每秒写一次数据库。
func (r *run) report(ctx context.Context, stage string, progress float64) {
	if stage == r.lastStage && time.Since(r.lastReport) < time.Second {
		return
	}
	r.lastStage, r.lastReport = stage, time.Now()
	n, err := r.w.q.SetMeetingProgress(ctx, sqlc.SetMeetingProgressParams{
		ID: r.m.ID, RunID: &r.runID, Stage: stage, Progress: float32(progress),
	})
	if err == nil && n == 0 {
		r.cancel(errSuperseded)
	}
	// 进度写不进去不影响处理本身，不当成错误
}

// inTx 在事务里锁住会议这一行，确认它仍然属于这一轮任务，再执行 fn。
// 所有会改变会议内容的写入都走这里，被顶替的旧任务什么都写不进去。
func (r *run) inTx(ctx context.Context, fn func(tx pgx.Tx, q *sqlc.Queries, m sqlc.Meeting) error) error {
	tx, err := r.w.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	q := r.w.q.WithTx(tx)
	m, err := q.LockMeeting(ctx, r.m.ID)
	if err != nil {
		return err
	}
	if m.RunID == nil || *m.RunID != r.runID || m.Status != "processing" {
		return errSuperseded
	}
	if err := fn(tx, q, m); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *run) loadTranscript(ctx context.Context) ([]transcript.Segment, error) {
	raw, err := r.w.q.GetTranscript(ctx, r.m.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var segs []transcript.Segment
	if err := json.Unmarshal(raw, &segs); err != nil {
		return nil, err
	}
	return segs, nil
}

// transcribe 从上传的分片（或已存好的录音）得到带说话人的转写，并存进数据库。
// old 是重新转写之前的转写（第一次转写时为 nil），用来把以前给说话人起的名字对到新的编号上。
func (r *run) transcribe(ctx context.Context, old []transcript.Segment) ([]transcript.Segment, error) {
	w, id := r.w, r.m.ID

	r.report(ctx, StageModels, 0)
	err := w.ensureModels(ctx, func(done, total int64) {
		r.report(ctx, StageModels, float64(done)/float64(total))
	})
	if err != nil {
		return nil, err
	}

	r.report(ctx, StageAssemble, 0)
	// 每一轮用自己的临时目录：被顶替的旧任务清理时不会删掉新任务正在用的文件
	tmp, err := w.store.TempDir(id, r.runID)
	if err != nil {
		return nil, err
	}
	defer func() {
		os.RemoveAll(tmp)
		os.Remove(filepath.Dir(tmp)) // 这场会议没有别的任务在用了就把上一层也删掉
	}()
	pcm := filepath.Join(tmp, "audio.f32")

	audioKey := r.m.AudioKey
	// 新转好的回放用的录音，先放在临时目录里，登记之前才挪到正式的位置：中途失败、会议被删了，临时目录一清就干净了
	m4a := ""
	if audioKey != "" && exists(w.store.Path(audioKey)) {
		// 上次已经转好码了，直接用
		if err := audio.Decode(ctx, w.store.Path(audioKey), pcm); err != nil {
			return nil, err
		}
	} else {
		input, copied, err := r.concatChunks(ctx, filepath.Join(tmp, "input"))
		if err != nil {
			return nil, err
		}
		m4a = filepath.Join(tmp, "audio.m4a")
		if err := audio.Convert(ctx, input, pcm, m4a); err != nil {
			return nil, err
		}
		if copied {
			os.Remove(input)
		}
		audioKey = storage.AudioKey(id)
	}

	samples, err := audio.ReadPCM(pcm)
	if err != nil {
		return nil, err
	}
	os.Remove(pcm)
	duration := len(samples) / audio.SampleRate
	if duration < 1 {
		return nil, errors.New("录音太短，没有可以识别的内容")
	}
	if m4a != "" {
		if err := moveFile(m4a, w.store.Path(audioKey)); err != nil {
			return nil, err
		}
	}
	n, err := w.q.SetMeetingAudio(ctx, sqlc.SetMeetingAudioParams{
		ID: id, RunID: &r.runID, AudioKey: audioKey, DurationSec: int32(duration),
	})
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, errSuperseded
	}
	// 录音已经存好并登记，分片可以删了
	if err := w.store.RemoveChunks(id); err == nil {
		w.q.DeleteChunks(ctx, id)
	}

	r.report(ctx, StageTranscribe, 0)
	w.cpu.Lock()
	defer w.cpu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	engine, err := w.loadEngine()
	if err != nil {
		return nil, err
	}
	segs, err := engine.Transcribe(ctx, samples, func(p float64) { r.report(ctx, StageTranscribe, p) })
	if err != nil {
		return nil, err
	}
	if len(segs) == 0 {
		return nil, errors.New("没有识别到说话内容，请确认录音里有人声")
	}

	r.report(ctx, StageDiarize, 0)
	// 用户填了有几个人说话（会议上的说话人数）就按人数分，没填是 0：按声音自动区分
	turns, err := engine.Diarize(ctx, samples, int(r.m.SpeakerCount), func(p float64) { r.report(ctx, StageDiarize, p) })
	if err != nil {
		return nil, err
	}
	ids := transcript.AssignSpeakers(segs, turns)
	voices, err := r.voices(ctx, engine, samples, transcript.RenumberTurns(turns, ids))
	if err != nil {
		return nil, err
	}
	// 按句存：搜索命中、点击跳转、播放高亮都精确到句。页面上显示时再合成段落

	raw, err := json.Marshal(segs)
	if err != nil {
		return nil, err
	}
	err = r.inTx(ctx, func(_ pgx.Tx, q *sqlc.Queries, m sqlc.Meeting) error {
		if err := q.SaveTranscript(ctx, sqlc.SaveTranscriptParams{MeetingID: id, Segments: raw}); err != nil {
			return err
		}
		if err := q.ClearRetranscribe(ctx, id); err != nil {
			return err
		}
		// 重新转写以后说话人的编号可能变了：以前起的名字跟着人走，对不上的去掉；
		// 再按声音认出声纹库里的人。用锁住的这一行里的名字，转写期间有人改过名（包括全部去掉）也算上
		if err := r.nameSpeakers(ctx, q, m, old, segs, voices); err != nil {
			return err
		}
		return search.Reindex(ctx, q, id)
	})
	if err != nil {
		return nil, err
	}
	return segs, nil
}

// concatChunks 把分片按序号首尾相接写成一个文件，返回它的路径；copied 表示是不是新写的（用完可以删）。
// 网页录音的分片是同一路流切出来的，上传文件的分片是文件切出来的，两种都是直接拼字节。
// 只有一片时（比如从飞书下载的录制）直接用它，不再复制一份（可能有好几 GB）。
func (r *run) concatChunks(ctx context.Context, dest string) (path string, copied bool, err error) {
	chunks, err := r.w.q.ListChunks(ctx, r.m.ID)
	if err != nil {
		return "", false, err
	}
	if len(chunks) == 0 {
		return "", false, errors.New("找不到上传的录音，请重新上传")
	}
	if len(chunks) == 1 && chunks[0].Seq == 0 {
		if p := r.w.store.ChunkPath(r.m.ID, 0); exists(p) {
			return p, false, nil
		}
		return "", false, errors.New("录音不完整：第 1 片读不出来，请重新上传")
	}
	out, err := os.Create(dest)
	if err != nil {
		return "", false, err
	}
	defer out.Close()
	for i, c := range chunks {
		if int(c.Seq) != i {
			return "", false, fmt.Errorf("录音不完整：缺少第 %d 片，请重新上传", i+1)
		}
		in, err := os.Open(r.w.store.ChunkPath(r.m.ID, i))
		if err != nil {
			return "", false, fmt.Errorf("录音不完整：第 %d 片读不出来，请重新上传", i+1)
		}
		_, err = io.Copy(out, in)
		in.Close()
		if err != nil {
			return "", false, err
		}
	}
	return dest, true, out.Close()
}

// writeMinutes 写纪要并把会议标记为完成。没有配置大模型时只标记完成，纪要以后再生成。
func (r *run) writeMinutes(ctx context.Context, segs []transcript.Segment) error {
	w, id := r.w, r.m.ID

	cfg, err := w.settings.LLM(ctx)
	if err != nil {
		return err
	}
	if !cfg.Ready() {
		n, err := w.q.FinishMeeting(ctx, sqlc.FinishMeetingParams{ID: id, RunID: &r.runID})
		if err == nil && n == 0 {
			return errSuperseded
		}
		return err
	}

	r.report(ctx, StageMinutes, 0)
	// 选的模板被管理员删掉了的话用通用模板
	tpl, ok, err := service.FindTemplate(ctx, w.q, r.m.Template)
	if err != nil {
		return err
	}
	if !ok {
		tpl = minutes.Templates[0]
	}
	chat := func(ctx context.Context, system, user string) (string, error) {
		return llm.Chat(ctx, cfg, system, user)
	}
	result, err := minutes.Generate(ctx, chat, minutes.Input{
		Title:    r.m.Title,
		Date:     r.m.CreatedAt.Local(),
		Hotwords: r.m.Hotwords,
		Template: tpl,
		// 交给大模型时合成段落，省字数；不跨过录音时标记重点的时间点合并
		Segments: transcript.Merge(segs, 0.8, 160, marks(r.m.Marks)),
		Marks:    marks(r.m.Marks),
		Speakers: transcript.ParseNames(r.m.Speakers),
		// 每段放多少字按管理员设置的上下文长度算
		MaxChars: llm.TranscriptChars(cfg.Context()),
	}, func(done, total int) {
		r.report(ctx, StageMinutes, float64(done)/float64(total))
	})
	if err != nil {
		return err
	}

	feishuCfg, err := w.settings.Feishu(ctx)
	if err != nil {
		return err
	}
	return r.inTx(ctx, func(tx pgx.Tx, q *sqlc.Queries, m sqlc.Meeting) error {
		// 第一份是自动生成的；之后再生成的记在发起人名下
		source, author := "auto", (*uuid.UUID)(nil)
		if m.CurrentVersion > 0 {
			source, author = "regenerate", m.RunBy
		}
		// 写纪要期间有人给说话人改了名字：大模型用的还是开始时的名字，这里按最新的名字换一遍
		content := result.Content
		sent, now := transcript.ParseNames(r.m.Speakers), transcript.ParseNames(m.Speakers)
		if diff := sent.Diff(now); len(diff) > 0 {
			content = minutes.RenameSpeakers(content, sent, diff)
		}
		version, err := q.AddMinutesVersion(ctx, sqlc.AddMinutesVersionParams{
			MeetingID: id, Content: content, Source: source, Template: tpl.ID, AuthorID: author,
		})
		if err != nil {
			return err
		}
		// 第一份纪要生成后自动发到飞书群（管理员打开了的话）。重新生成的不自动发，免得刷屏。
		// 只发团队里所有人都能看的会议：指定了人、只有自己能看的会议不能自动发到群里
		if source == "auto" && m.Visibility == service.VisibilityTeam && feishuCfg.Ready() && feishuCfg.AutoPush && feishuCfg.ChatID != "" {
			if err := w.queueShare(ctx, tx, q, id, version, feishuCfg); err != nil {
				return err
			}
		}
		// 钉钉、企业微信、Slack 的群机器人也一样：只在第一份纪要生成后、只发团队里所有人都能看的会议
		if source == "auto" && m.Visibility == service.VisibilityTeam {
			if err := w.queueWebhookSends(ctx, tx, q, id, version); err != nil {
				return err
			}
		}
		// 术语表跟着最新一次生成走；手动编辑纪要不改它。大模型这次没给（或者写坏了）就留着原来的，
		// 免得一次没写好就把攒下的听错写法清掉
		if result.TermsFound {
			terms, err := json.Marshal(result.Terms)
			if err != nil {
				return err
			}
			if result.Terms == nil {
				terms = []byte("[]")
			}
			if err := q.SetMeetingTerms(ctx, sqlc.SetMeetingTermsParams{ID: id, Terms: terms}); err != nil {
				return err
			}
		}
		if err := search.Reindex(ctx, q, id); err != nil {
			return err
		}
		_, err = q.FinishMeeting(ctx, sqlc.FinishMeetingParams{ID: id, RunID: &r.runID})
		return err
	})
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// moveFile 把文件移到最终位置（同一个数据目录内，改名即可）。
func moveFile(from, to string) error {
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	return os.Rename(from, to)
}

// marks 把数据库里存的标记时间换成 float64。
func marks(in []float32) []float64 {
	out := make([]float64, len(in))
	for i, t := range in {
		out[i] = float64(t)
	}
	return out
}

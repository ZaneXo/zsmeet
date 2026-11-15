package pipeline

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/ZaneXo/zsmeet/internal/audio"
	"github.com/ZaneXo/zsmeet/internal/correct"
	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/queue"
	"github.com/ZaneXo/zsmeet/internal/secret"
	"github.com/ZaneXo/zsmeet/internal/transcript"
)

const (
	// 距离当前分片末尾至少留出这么久的余量才算「定稿」：语音检测已经在句末看到停顿，不是被分片拦腰切断的半句话
	liveSettleSec = 1.2
	// 长时间没人说话时，定稿位置最多留在末尾前这么久，避免静音越攒越长每次重扫
	liveSilenceKeepSec = 4.0
)

// liveWorker 在录音过程中把新传上来的连续分片增量识别成实时字幕。
// 只用本机 SenseVoice 做轻量识别（零外部调用开销，每次只算未定稿的最后几秒到十几秒）；
// 录音结束后仍由 Worker 走完整的转码、说话人区分、声纹匹配、校对和纪要生成。
type liveWorker struct {
	river.WorkerDefaults[queue.LiveCaptionArgs]
	w *Worker
}

func (l *liveWorker) Work(ctx context.Context, job *river.Job[queue.LiveCaptionArgs]) error {
	for {
		advanced, err := l.step(ctx, job.Args.MeetingID)
		if err != nil {
			if isLivePermanentError(err) {
				slog.Warn("实时字幕任务因不可重试错误取消", "meeting", job.Args.MeetingID, "err", err)
				return river.JobCancel(err)
			}
			return err
		}
		if !advanced {
			return nil
		}
	}
}

func isLivePermanentError(err error) bool {
	if errors.Is(err, audio.ErrTooLong) {
		return true
	}
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	return errors.As(err, &syntaxErr) || errors.As(err, &typeErr)
}

// step 往前识别一轮新传上来的连续分片；advanced 为 true 表示这一轮推进了分片进度，外层再看一眼跑的期间有没有又传上来新分片。
func (l *liveWorker) step(ctx context.Context, id uuid.UUID) (bool, error) {
	row, err := l.w.q.GetMeeting(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	m := row.Meeting
	if m.DeletedAt != nil || m.Status != "uploading" || (m.Source != "record" && m.Source != "desktop") {
		return false, nil
	}

	var (
		lt        sqlc.LiveTranscript
		finalized []transcript.Segment
	)
	lt, err = l.w.q.GetLiveTranscript(ctx, id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return false, err
	default:
		if len(lt.Finalized) > 0 {
			if err := json.Unmarshal(lt.Finalized, &finalized); err != nil {
				return false, err
			}
		}
	}

	chunks, err := l.w.q.ListChunks(ctx, id)
	if err != nil {
		return false, err
	}
	contiguous := 0
	for _, c := range chunks {
		if int(c.Seq) != contiguous || !exists(l.w.store.ChunkPath(id, contiguous)) {
			break
		}
		contiguous++
	}
	if contiguous == 0 || contiguous <= int(lt.Chunks) {
		return false, nil
	}

	if err := l.w.ensureModels(ctx, nil); err != nil {
		return false, err
	}

	tmp, err := l.w.store.TempDir(id, uuid.New())
	if err != nil {
		return false, err
	}
	defer func() {
		os.RemoveAll(tmp)
		os.Remove(filepath.Dir(tmp))
	}()

	input, err := l.concatPrefix(id, contiguous, filepath.Join(tmp, "prefix"))
	if err != nil {
		if l.noLongerUploading(ctx, id) {
			return false, nil
		}
		slog.Debug("实时字幕拼接分片跳过", "meeting", id, "chunks", contiguous, "err", err)
		return false, nil
	}

	tailSamples, actualFrom, err := audio.DecodeTail(ctx, input, filepath.Join(tmp, "tail.f32"), lt.FinalizedSec)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if l.noLongerUploading(ctx, id) || errors.Is(err, audio.ErrBadMedia) {
			slog.Debug("实时字幕解码分片跳过，等下一片再试", "meeting", id, "chunks", contiguous, "err", err)
			return false, nil
		}
		return false, err
	}

	var tailSegs []transcript.Segment
	if len(tailSamples) >= audio.SampleRate/2 {
		asrCfg, _, asrErr := l.w.settings.ASRWithTime(ctx)
		if asrErr != nil && !errors.Is(asrErr, secret.ErrDecrypt) {
			return false, asrErr
		}
		lang := cmp.Or(m.Language, asrCfg.EffectiveLanguage())

		tailSegs, err = func() ([]transcript.Segment, error) {
			l.w.cpu.Lock()
			defer l.w.cpu.Unlock()
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			engine, err := l.w.loadEngine()
			if err != nil {
				return nil, err
			}
			engine.SetLanguage(lang)
			return engine.Transcribe(ctx, tailSamples, func(float64) {})
		}()
		if err != nil {
			return false, err
		}
		correct.Homophones(tailSegs, correct.ParseHotwords(m.Hotwords))
	}

	totalEnd := actualFrom + float64(len(tailSamples))/audio.SampleRate
	nextFinal, allSegs, nextFinalSec := splitLiveSegments(finalized, lt.FinalizedSec, actualFrom, totalEnd, tailSegs)

	finalRaw, err := json.Marshal(nextFinal)
	if err != nil {
		return false, err
	}
	segsRaw, err := json.Marshal(allSegs)
	if err != nil {
		return false, err
	}

	tx, err := l.w.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	q := l.w.q.WithTx(tx)
	cur, err := q.ShareLockMeeting(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// 录音刚好在这几百毫秒里结束并入队处理了：不再写入，免得正式转写清理以后又冒出来
	if cur.DeletedAt != nil || cur.Status != "uploading" {
		return false, nil
	}
	if err := q.UpsertLiveTranscript(ctx, sqlc.UpsertLiveTranscriptParams{
		MeetingID:    id,
		Chunks:       int32(contiguous),
		FinalizedSec: nextFinalSec,
		Finalized:    finalRaw,
		Segments:     segsRaw,
	}); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func (l *liveWorker) concatPrefix(id uuid.UUID, n int, dest string) (string, error) {
	if n == 1 {
		p := l.w.store.ChunkPath(id, 0)
		if !exists(p) {
			return "", os.ErrNotExist
		}
		return p, nil
	}
	out, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	defer out.Close()
	for i := range n {
		in, err := os.Open(l.w.store.ChunkPath(id, i))
		if err != nil {
			return "", err
		}
		_, err = io.Copy(out, in)
		in.Close()
		if err != nil {
			return "", err
		}
	}
	return dest, out.Close()
}

func (l *liveWorker) noLongerUploading(ctx context.Context, id uuid.UUID) bool {
	bg := context.WithoutCancel(ctx)
	row, err := l.w.q.GetMeeting(bg, id)
	return errors.Is(err, pgx.ErrNoRows) || (err == nil && (row.Meeting.DeletedAt != nil || row.Meeting.Status != "uploading"))
}

// splitLiveSegments 把新解出的尾巴句子分成「已定稿」和「末尾暂定」，返回更新后的定稿列表、全部可见句子和新的定稿时间点。
func splitLiveSegments(finalized []transcript.Segment, finalizedSec, actualFrom, totalEnd float64, tailSegs []transcript.Segment) ([]transcript.Segment, []transcript.Segment, float64) {
	nextFinal := slices.Clone(finalized)
	if nextFinal == nil {
		nextFinal = []transcript.Segment{}
	}
	nextFinalSec := max(finalizedSec, actualFrom)
	var provisional []transcript.Segment
	for _, seg := range tailSegs {
		seg.Start += actualFrom
		seg.End += actualFrom
		if len(nextFinal) > 0 && seg.Start < nextFinal[len(nextFinal)-1].End {
			seg.Start = nextFinal[len(nextFinal)-1].End
		}
		if seg.End < seg.Start {
			seg.End = seg.Start
		}
		if seg.End <= totalEnd-liveSettleSec {
			nextFinal = append(nextFinal, seg)
			nextFinalSec = max(nextFinalSec, seg.End)
		} else {
			provisional = append(provisional, seg)
		}
	}
	if len(provisional) == 0 && totalEnd-nextFinalSec > liveSilenceKeepSec {
		nextFinalSec = totalEnd - liveSilenceKeepSec
	}
	all := make([]transcript.Segment, 0, len(nextFinal)+len(provisional))
	all = append(all, nextFinal...)
	all = append(all, provisional...)
	return nextFinal, all, nextFinalSec
}

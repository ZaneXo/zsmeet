// Package pipeline 是后台处理一场会议的全过程：
// 拼接上传的分片 → 转码 → 识别文字 → 区分说话人 → 写纪要。
package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/ZaneXo/zsmeet/internal/asr"
	"github.com/ZaneXo/zsmeet/internal/audio"
	"github.com/ZaneXo/zsmeet/internal/config"
	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/models"
	"github.com/ZaneXo/zsmeet/internal/queue"
	"github.com/ZaneXo/zsmeet/internal/settings"
	"github.com/ZaneXo/zsmeet/internal/storage"
	"github.com/ZaneXo/zsmeet/internal/transcript"
)

const (
	// 处理期间每隔这么久刷新一次心跳
	heartbeatEvery = 20 * time.Second
	// 心跳停了这么久，认为处理它的 worker 已经挂了
	staleAfter = 2 * time.Minute
	// 因为 worker 异常退出最多自动重新入队几次
	maxRequeues = 2
	// 一场会议最多处理多久
	jobTimeout = 8 * time.Hour
)

// errSuperseded 表示这场会议已经被新一轮任务接手，当前任务应该安静地退出。
var errSuperseded = errors.New("会议已被新的任务接手")

// recognizer 是处理流程需要识别引擎做的两件事。
// 测试里用假的替换，不用加载几百 MB 的模型。
type recognizer interface {
	Transcribe(ctx context.Context, samples []float32, progress func(float64)) ([]transcript.Segment, error)
	Diarize(samples []float32) []transcript.Turn
}

// Worker 处理 queue.ProcessArgs。
type Worker struct {
	river.WorkerDefaults[queue.ProcessArgs]

	pool     *pgxpool.Pool
	q        *sqlc.Queries
	store    *storage.Local
	settings *settings.Store

	// 确保识别模型在本地，缺了就下载
	ensureModels func(ctx context.Context, progress func(done, total int64)) error
	// 加载识别引擎
	newEngine func() (recognizer, error)

	// 识别很吃 CPU，同一时间只让一场会议做；写纪要主要在等大模型，不受这个限制
	cpu    sync.Mutex
	engine recognizer
}

func newWorker(pool *pgxpool.Pool, cfg config.Config, store *storage.Local, st *settings.Store) *Worker {
	return &Worker{
		pool:     pool,
		q:        sqlc.New(pool),
		store:    store,
		settings: st,
		ensureModels: func(ctx context.Context, progress func(done, total int64)) error {
			return models.Ensure(ctx, cfg.ModelsDir, cfg.ModelMirror, progress)
		},
		newEngine: func() (recognizer, error) {
			started := time.Now()
			engine, err := asr.Load(models.Paths{Dir: cfg.ModelsDir}, cfg.ASRThreads)
			if err != nil {
				return nil, err
			}
			slog.Info("识别模型已加载", "took", time.Since(started).Round(time.Millisecond), "threads", cfg.ASRThreads)
			return engine, nil
		},
	}
}

// Start 启动后台处理：消费任务队列，并定时巡检卡住的会议。返回的客户端用来在退出时停止。
func Start(ctx context.Context, pool *pgxpool.Pool, cfg config.Config, store *storage.Local, st *settings.Store) (*queue.Client, error) {
	w := newWorker(pool, cfg, store, st)

	workers := river.NewWorkers()
	river.AddWorker(workers, w)
	river.AddWorker(workers, &reaper{pool: pool})

	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			queue.QueueProcess:     {MaxWorkers: cfg.Concurrency},
			queue.QueueMaintenance: {MaxWorkers: 1},
		},
		Workers: workers,
		// River 认为超过这个时间还在跑的任务是卡死的，会重新执行，所以必须比最长的会议处理时间长
		JobTimeout: jobTimeout,
		PeriodicJobs: []*river.PeriodicJob{
			river.NewPeriodicJob(
				river.PeriodicInterval(time.Minute),
				func() (river.JobArgs, *river.InsertOpts) { return queue.ReapArgs{}, nil },
				&river.PeriodicJobOpts{RunOnStart: true},
			),
		},
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	if err != nil {
		return nil, err
	}
	if err := client.Start(ctx); err != nil {
		return nil, err
	}

	// 提前把模型备好，第一场会议就不用等下载
	go func() {
		if err := w.ensureModels(ctx, nil); err != nil && ctx.Err() == nil {
			slog.Error("识别模型没有准备好，处理会议时会再试", "err", err)
		}
	}()
	return client, nil
}

func (w *Worker) Work(ctx context.Context, job *river.Job[queue.ProcessArgs]) error {
	id, runID := job.Args.MeetingID, job.Args.RunID
	log := slog.With("meeting", id)

	m, err := w.q.ClaimMeeting(ctx, sqlc.ClaimMeetingParams{ID: id, RunID: &runID})
	if errors.Is(err, pgx.ErrNoRows) {
		log.Info("会议已被新的任务接手或已删除，跳过")
		return nil
	}
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stopHeartbeat := w.heartbeat(ctx, id, runID, cancel)
	defer stopHeartbeat()

	log.Info("开始处理会议", "title", m.Title)
	started := time.Now()
	r := &run{w: w, m: m, runID: runID, cancel: cancel}
	err = r.process(ctx)

	switch {
	case err == nil:
		log.Info("会议处理完成", "took", time.Since(started).Round(time.Second))
		return nil
	case errors.Is(err, errSuperseded), errors.Is(context.Cause(ctx), errSuperseded):
		log.Info("会议已被新的任务接手，当前任务退出")
		return nil
	case errors.Is(ctx.Err(), context.Canceled):
		// worker 被要求退出。把错误交还给 River，它会在 worker 重新起来后再执行这个任务
		log.Info("worker 正在退出，这场会议稍后继续")
		return ctx.Err()
	}

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = errors.New("处理超时")
	}
	log.Error("会议处理失败", "err", err)
	// 原来的 ctx 可能已经超时，换一个新的把失败原因写进去
	bg, done := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer done()
	if _, dbErr := w.q.FailMeeting(bg, sqlc.FailMeetingParams{ID: id, RunID: &runID, Error: userMessage(err)}); dbErr != nil {
		// 写不进去就让巡检来兜底：心跳停了以后它会重新入队
		log.Error("失败原因没能写入数据库", "err", dbErr)
	}
	// 这类失败重试也没用（文件坏了、大模型配置不对……），告诉 River 不要自动重试，由用户点「重试」
	return river.JobCancel(err)
}

// heartbeat 在处理期间定时刷新心跳。发现会议已被别的任务接手时取消当前任务。
func (w *Worker) heartbeat(ctx context.Context, id, runID uuid.UUID, cancel context.CancelCauseFunc) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		ticker := time.NewTicker(heartbeatEvery)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				n, err := w.q.Heartbeat(ctx, sqlc.HeartbeatParams{ID: id, RunID: &runID})
				if err == nil && n == 0 {
					cancel(errSuperseded)
					return
				}
				// 数据库暂时连不上就下次再试。断得太久巡检会把会议交给新任务，我们之后的写入都会落空
			}
		}
	})
	return func() {
		close(done)
		wg.Wait()
	}
}

// loadEngine 第一次用到时加载识别模型，之后一直复用。调用前必须拿着 w.cpu。
func (w *Worker) loadEngine() (recognizer, error) {
	if w.engine != nil {
		return w.engine, nil
	}
	engine, err := w.newEngine()
	if err != nil {
		return nil, err
	}
	w.engine = engine
	return engine, nil
}

// userMessage 把错误变成写给用户看的失败原因。
func userMessage(err error) string {
	// ffmpeg 的原始报错只进日志，用户看到的是能照着处理的那句话
	if errors.Is(err, audio.ErrBadMedia) {
		return audio.ErrBadMedia.Error()
	}
	return err.Error()
}

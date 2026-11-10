package pipeline

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/queue"
	"github.com/ZaneXo/zsmeet/internal/service"
	"github.com/ZaneXo/zsmeet/internal/settings"
	"github.com/ZaneXo/zsmeet/internal/storage"
)

// reaper 每分钟巡检一次：会议显示「处理中」但心跳已经停了，说明处理它的 worker 挂了
// （被杀掉、内存不够、机器重启）。这种会议换一个 run_id 重新入队；反复挂掉的判为失败。
// 同时彻底删除回收站里放满 30 天的会议、删掉过了保留期的录音。
type reaper struct {
	river.WorkerDefaults[queue.ReapArgs]
	pool     *pgxpool.Pool
	store    *storage.Local
	settings *settings.Store
}

func (*reaper) Timeout(*river.Job[queue.ReapArgs]) time.Duration { return time.Minute }

func (p *reaper) Work(ctx context.Context, _ *river.Job[queue.ReapArgs]) error {
	client := river.ClientFromContext[pgx.Tx](ctx)
	requeued, failed, err := requeueStalled(ctx, p.pool, func(ctx context.Context, tx pgx.Tx, args queue.ProcessArgs) error {
		_, err := client.InsertTx(ctx, tx, args, nil)
		return err
	})
	if err != nil {
		return err
	}
	if failed > 0 || requeued > 0 {
		slog.Warn("巡检发现处理中断的会议", "requeued", requeued, "failed", failed)
	}
	// 顺手清理回收站里放满 30 天的会议（连同录音文件）
	q := sqlc.New(p.pool)
	purged, err := service.PurgeExpired(ctx, q, p.store)
	if purged > 0 {
		slog.Info("彻底删除了回收站里过期的会议", "count", purged)
	}
	if err != nil {
		return err
	}
	// 删掉过了保留期的录音（转写和纪要留着）
	retention, err := p.settings.Retention(ctx)
	if err != nil {
		return err
	}
	expired, err := service.ExpireAudio(ctx, q, p.store, retention.AudioDays)
	if expired > 0 {
		slog.Info("删除了过了保留期的录音", "count", expired, "days", retention.AudioDays)
	}
	return err
}

// requeueStalled 处理心跳停了的会议：重新入队过太多次的判为失败，其余的换一个 run_id 重新入队。
// insert 负责把任务放进队列，和状态修改在同一个事务里，不会出现改了状态却没有任务的情况。
func requeueStalled(
	ctx context.Context,
	pool *pgxpool.Pool,
	insert func(ctx context.Context, tx pgx.Tx, args queue.ProcessArgs) error,
) (requeued int, failed int64, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	q := sqlc.New(tx)

	stale := int32(staleAfter / time.Second)
	failed, err = q.FailStalled(ctx, sqlc.FailStalledParams{
		Error:       "处理程序多次异常退出，已停止自动重试。常见原因是服务器内存不足，可以点「重试」再试一次",
		StaleSecs:   stale,
		MaxRequeues: maxRequeues,
	})
	if err != nil {
		return 0, 0, err
	}
	rows, err := q.RequeueStalled(ctx, sqlc.RequeueStalledParams{StaleSecs: stale, MaxRequeues: maxRequeues})
	if err != nil {
		return 0, 0, err
	}
	for _, row := range rows {
		if err := insert(ctx, tx, queue.ProcessArgs{MeetingID: row.ID, RunID: *row.RunID}); err != nil {
			return 0, 0, err
		}
	}
	return len(rows), failed, tx.Commit(ctx)
}

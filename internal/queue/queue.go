// Package queue 定义后台任务。队列用 River，任务就存在 Postgres 里，
// 所以「改会议状态」和「任务入队」能放进同一个事务。
package queue

import (
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

const (
	// 处理会议的队列，并发数由部署配置决定
	QueueProcess = river.QueueDefault
	// 巡检等维护任务单独一个队列，不会被长时间的会议处理堵住
	QueueMaintenance = "maintenance"
)

// ProcessArgs 处理一场会议：有转写就只写纪要，没有就从音频开始。
type ProcessArgs struct {
	MeetingID uuid.UUID `json:"meeting_id"`
	// 对应 meetings.run_id。会议被重新入队后，旧任务拿着旧 run_id 什么都改不了
	RunID uuid.UUID `json:"run_id"`
}

func (ProcessArgs) Kind() string { return "process_meeting" }

func (ProcessArgs) InsertOpts() river.InsertOpts {
	// 只有 worker 被要求退出时才会让 River 重试，次数不用多
	return river.InsertOpts{Queue: QueueProcess, MaxAttempts: 5}
}

// ReapArgs 巡检：把 worker 挂掉后卡在「处理中」的会议重新入队。
type ReapArgs struct{}

func (ReapArgs) Kind() string { return "reap_stalled" }

func (ReapArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueMaintenance, MaxAttempts: 1}
}

// Client 是 River 客户端的简称。
type Client = river.Client[pgx.Tx]

// NewInserter 返回只负责入队的客户端，给 API 进程用。
func NewInserter(pool *pgxpool.Pool) (*Client, error) {
	return river.NewClient(riverpgxv5.New(pool), &river.Config{})
}

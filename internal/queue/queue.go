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
	// 发到飞书（推群、导入文档）。飞书云空间同一个文件夹不支持并发操作，一个一个来
	QueueFeishu = "feishu"
	// 导入飞书会议的录制：要等飞书生成好、再下载，单独一个队列，不耽误发群
	QueueFeishuImport = "feishu_import"
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

// FeishuShareArgs 把一场会议的纪要发到飞书：推到群里、导入成飞书文档（照着 feishu_shares 表里的一条做）。
type FeishuShareArgs struct {
	ShareID uuid.UUID `json:"share_id"`
}

func (FeishuShareArgs) Kind() string { return "feishu_share" }

func (FeishuShareArgs) InsertOpts() river.InsertOpts {
	// 网络问题、限流时重试几次，间隔越来越长
	return river.InsertOpts{Queue: QueueFeishu, MaxAttempts: 6}
}

// FeishuRecordingArgs 导入一场结束了的飞书会议的云录制。字段来自飞书的「会议结束」事件。
type FeishuRecordingArgs struct {
	// 飞书的会议 ID。同一场会议只有一个任务（飞书可能重复推送事件）
	MeetingID   string `json:"meeting_id" river:"unique"`
	Topic       string `json:"topic"`
	Start       int64  `json:"start"` // unix 秒
	End         int64  `json:"end"`
	OwnerOpenID string `json:"owner_open_id"`
}

func (FeishuRecordingArgs) Kind() string { return "feishu_recording" }

func (FeishuRecordingArgs) InsertOpts() river.InsertOpts {
	// 等录制生成用的是「过一会儿再来」（不算次数），这里的次数只给网络问题之类的错误用
	return river.InsertOpts{Queue: QueueFeishuImport, MaxAttempts: 8, UniqueOpts: river.UniqueOpts{ByArgs: true}}
}

// Client 是 River 客户端的简称。
type Client = river.Client[pgx.Tx]

// NewInserter 返回只负责入队的客户端，给 API 进程用。
func NewInserter(pool *pgxpool.Pool) (*Client, error) {
	return river.NewClient(riverpgxv5.New(pool), &river.Config{})
}

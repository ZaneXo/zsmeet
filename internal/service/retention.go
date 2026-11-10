package service

import (
	"context"
	"log/slog"

	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/settings"
	"github.com/ZaneXo/zsmeet/internal/storage"
)

// 录音保留期最短、最长可以设多少天（0 表示一直保留）
const (
	minAudioDays = 7
	maxAudioDays = 3650
)

// Retention 返回录音保留多久。
func (s *Service) Retention(ctx context.Context) (settings.Retention, error) {
	return s.settings.Retention(ctx)
}

// SaveRetention 保存录音保留多少天，0 表示一直保留。缩短以后，后台巡检时会删掉已经过期的录音。
func (s *Service) SaveRetention(ctx context.Context, days int) (settings.Retention, error) {
	if days != 0 && (days < minAudioDays || days > maxAudioDays) {
		return settings.Retention{}, invalid("录音保留期要在 %d 到 %d 天之间，或者选「一直保留」", minAudioDays, maxAudioDays)
	}
	r := settings.Retention{AudioDays: days}
	return r, s.settings.SaveRetention(ctx, r)
}

// ExpireAudio 删掉过了保留期的录音文件（转写和纪要留着），返回删了几个。days 为 0 时什么也不做。后台巡检时调用。
// 先在数据库里记下录音已删除，再删文件：文件删不掉只记日志，网页上已经不会再放它了。
func ExpireAudio(ctx context.Context, q *sqlc.Queries, store *storage.Local, days int) (int, error) {
	if days <= 0 {
		return 0, nil
	}
	rows, err := q.ExpiredAudio(ctx, int32(days))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		marked, err := q.MarkAudioDeleted(ctx, r.ID)
		if err != nil {
			return n, err
		}
		if marked == 0 {
			continue // 这时又开始处理了（重新转写），下次再看
		}
		if err := store.Remove(r.AudioKey); err != nil {
			slog.Warn("删除过期的录音文件失败", "meeting", r.ID, "err", err)
		}
		n++
	}
	return n, nil
}

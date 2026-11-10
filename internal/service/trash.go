package service

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/storage"
)

// TrashDays 是会议在回收站里放多少天，过了就由后台彻底删除（连同录音文件）。
const TrashDays = 30

// TrashedMeeting 是回收站里的一场会议。
type TrashedMeeting struct {
	Meeting
	DeletedByName string
}

// PurgeAt 是这场会议会被彻底删除的时间。
func (t TrashedMeeting) PurgeAt() time.Time {
	return t.DeletedAt.Add(TrashDays * 24 * time.Hour)
}

var errNotInTrash = notFound("回收站里没有这场会议，可能已经被恢复或者彻底删除了")

// DeleteMeeting 把会议放进回收站。创建者和管理员可以删。正在处理的会议也可以删，后台处理完的结果照样保存，恢复以后能看到。
func (s *Service) DeleteMeeting(ctx context.Context, user sqlc.User, id uuid.UUID) error {
	if _, err := s.modifiable(ctx, user, id); err != nil {
		return err
	}
	_, err := s.q.TrashMeeting(ctx, sqlc.TrashMeetingParams{ID: id, DeletedBy: &user.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return errNoMeeting
	}
	return err
}

// ListTrash 列出回收站。管理员看到全部；其他人只看到自己创建的，也只能处理这些。
func (s *Service) ListTrash(ctx context.Context, user sqlc.User) ([]TrashedMeeting, error) {
	var owner *uuid.UUID
	if user.Role != "admin" {
		owner = &user.ID
	}
	rows, err := s.q.ListTrash(ctx, owner)
	if err != nil {
		return nil, err
	}
	out := make([]TrashedMeeting, len(rows))
	for i, r := range rows {
		out[i] = TrashedMeeting{Meeting: Meeting{Meeting: r.Meeting, OwnerName: r.OwnerName}, DeletedByName: r.DeletedByName}
	}
	return out, nil
}

// trashed 取出回收站里的一场会议，并确认 user 能处理它。
func (s *Service) trashed(ctx context.Context, user sqlc.User, id uuid.UUID) error {
	row, err := s.q.GetMeeting(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotInTrash
	}
	if err != nil {
		return err
	}
	if row.Meeting.DeletedAt == nil {
		return errNotInTrash
	}
	if !CanModify(user, row.Meeting) {
		return errNotAllowed
	}
	return nil
}

// RestoreMeeting 把会议从回收站恢复。
func (s *Service) RestoreMeeting(ctx context.Context, user sqlc.User, id uuid.UUID) (Meeting, error) {
	if err := s.trashed(ctx, user, id); err != nil {
		return Meeting{}, err
	}
	if _, err := s.q.RestoreMeeting(ctx, id); errors.Is(err, pgx.ErrNoRows) {
		return Meeting{}, errNotInTrash
	} else if err != nil {
		return Meeting{}, err
	}
	return s.GetMeeting(ctx, id)
}

// PurgeMeeting 立即彻底删除回收站里的一场会议，不能恢复。
func (s *Service) PurgeMeeting(ctx context.Context, user sqlc.User, id uuid.UUID) error {
	if err := s.trashed(ctx, user, id); err != nil {
		return err
	}
	purged, err := Purge(ctx, s.q, s.store, id)
	if err != nil {
		return err
	}
	if !purged {
		return errNotInTrash
	}
	return nil
}

// Purge 彻底删除回收站里的一场会议：先删数据库里的记录（转写、纪要、分片记录跟着级联删掉），
// 再删录音和分片文件。会议不在回收站里（被恢复了，或者已经删掉了）时什么也不做，返回 false。
// 文件删不掉只记日志：记录已经没了，留下的文件不会再被用到。
func Purge(ctx context.Context, q *sqlc.Queries, store *storage.Local, id uuid.UUID) (bool, error) {
	key, err := q.PurgeMeeting(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if key != "" {
		if err := store.Remove(key); err != nil {
			slog.Warn("删除录音文件失败", "meeting", id, "err", err)
		}
	}
	if err := store.RemoveChunks(id); err != nil {
		slog.Warn("删除上传分片失败", "meeting", id, "err", err)
	}
	return true, nil
}

// PurgeExpired 彻底删除在回收站里放满 TrashDays 天的会议，返回删了几场。后台巡检时调用。
func PurgeExpired(ctx context.Context, q *sqlc.Queries, store *storage.Local) (int, error) {
	ids, err := q.ExpiredTrash(ctx, TrashDays)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		purged, err := Purge(ctx, q, store, id)
		if err != nil {
			return n, err
		}
		if purged {
			n++
		}
	}
	return n, nil
}

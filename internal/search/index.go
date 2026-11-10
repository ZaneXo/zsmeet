package search

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/transcript"
)

// Reindex 用会议的标题、当前纪要和转写重建搜索索引。
// 标题、纪要、转写任何一个变了都要调用，和那次修改放在同一个事务里。
func Reindex(ctx context.Context, q *sqlc.Queries, id uuid.UUID) error {
	row, err := q.GetMeeting(ctx, id)
	if err != nil {
		return err
	}
	texts := []string{row.Meeting.Title}

	if v := row.Meeting.CurrentVersion; v > 0 {
		mv, err := q.GetMinutesVersion(ctx, sqlc.GetMinutesVersionParams{MeetingID: id, Version: v})
		if err != nil {
			return err
		}
		texts = append(texts, mv.Content)
	}

	raw, err := q.GetTranscript(ctx, id)
	switch {
	case err == nil:
		var segs []transcript.Segment
		if err := json.Unmarshal(raw, &segs); err != nil {
			return err
		}
		texts = append(texts, transcript.PlainText(segs))
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}

	terms := DocTerms(texts...)
	if terms == nil {
		terms = []string{}
	}
	return q.SetMeetingSearch(ctx, sqlc.SetMeetingSearchParams{MeetingID: id, Terms: terms})
}

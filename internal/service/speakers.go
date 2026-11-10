package service

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/minutes"
	"github.com/ZaneXo/zsmeet/internal/search"
	"github.com/ZaneXo/zsmeet/internal/transcript"
)

// 名字最长多少个字
const maxSpeakerName = 20

// RenameSpeakers 给说话人起名字、改名，或者去掉名字（给空字符串）。names 的键是说话人编号（从 0 开始）。
// 转写和搜索结果马上显示新名字；当前纪要里的「说话人 N」也换成名字，作为一个新版本保存（旧版本保留）。
// 以后重新生成纪要时，大模型直接看到名字。
func (s *Service) RenameSpeakers(ctx context.Context, user sqlc.User, id uuid.UUID, names map[int]string) (Meeting, error) {
	clean := make(map[int]string, len(names))
	for idx, name := range names {
		name = strings.Join(strings.Fields(name), " ")
		if idx < 0 || idx >= 100 {
			return Meeting{}, invalid("说话人编号不对：%d", idx)
		}
		if utf8.RuneCountInString(name) > maxSpeakerName {
			return Meeting{}, invalid("名字最长 %d 个字", maxSpeakerName)
		}
		clean[idx] = name
	}
	err := s.tx(ctx, func(_ pgx.Tx, q *sqlc.Queries) error {
		m, err := q.LockMeeting(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && m.DeletedAt != nil) {
			return errNoMeeting
		}
		if err != nil {
			return err
		}
		if !CanModify(user, m) {
			return errNotAllowed
		}
		current := transcript.ParseNames(m.Speakers)
		renamed := map[int][2]string{}
		for idx, name := range clean {
			if current[idx] == name {
				continue
			}
			renamed[idx] = [2]string{current[idx], name}
			if name == "" {
				delete(current, idx)
			} else {
				current[idx] = name
			}
		}
		if len(renamed) == 0 {
			return nil
		}
		stored := make(map[string]string, len(current))
		for idx, name := range current {
			stored[strconv.Itoa(idx)] = name
		}
		raw, err := json.Marshal(stored)
		if err != nil {
			return err
		}
		if err := q.SetMeetingSpeakers(ctx, sqlc.SetMeetingSpeakersParams{ID: id, Speakers: raw}); err != nil {
			return err
		}
		// 当前纪要里的称呼跟着改。正在处理的会议不改：新生成的纪要会直接用名字
		if m.CurrentVersion > 0 && m.Status != "queued" && m.Status != "processing" {
			mv, err := q.GetMinutesVersion(ctx, sqlc.GetMinutesVersionParams{MeetingID: id, Version: m.CurrentVersion})
			if err != nil {
				return err
			}
			if content := minutes.RenameSpeakers(mv.Content, renamed); content != mv.Content {
				if _, err := q.AddMinutesVersion(ctx, sqlc.AddMinutesVersionParams{
					MeetingID: id, Content: content, Source: "edit", Template: "", AuthorID: &user.ID,
				}); err != nil {
					return err
				}
				return search.Reindex(ctx, q, id)
			}
		}
		return nil
	})
	if err != nil {
		return Meeting{}, err
	}
	return s.GetMeeting(ctx, id)
}

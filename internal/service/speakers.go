package service

import (
	"context"
	"errors"
	"maps"
	"slices"
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
// 声纹打开时，起的名字连同这位说话人的声音特征记到声纹库里，之后的会议按声音认出他（见 learnVoices）；
// 按声音自动起的名字被人改过（或者照原样保存过）以后，就算人起的名字了。
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
		access, err := accessIn(ctx, q, user, m)
		if err != nil {
			return err
		}
		if !access.View {
			return errNoMeeting
		}
		if !access.Edit {
			return errNotAllowed
		}
		current := transcript.ParseNames(m.Speakers)
		before := maps.Clone(current)
		auto := slices.Clone(m.AutoSpeakers)
		renamed := map[int][2]string{}
		// 有人亲手起的名字（包括确认了按声音认出的名字），声纹库要跟着学
		learned := map[int]string{}
		for idx, name := range clean {
			wasAuto := slices.Contains(auto, int32(idx))
			auto = slices.DeleteFunc(auto, func(a int32) bool { return a == int32(idx) })
			if current[idx] == name {
				// 按声音认出的名字，有人照原样保存一次就算确认了：以后算作人起的名字
				if name != "" && wasAuto {
					learned[idx] = name
				}
				continue
			}
			renamed[idx] = [2]string{current[idx], name}
			learned[idx] = name
			if name == "" {
				delete(current, idx)
			} else {
				current[idx] = name
			}
		}
		if len(learned) == 0 {
			return nil
		}
		if len(auto) != len(m.AutoSpeakers) {
			if err := q.SetAutoSpeakers(ctx, sqlc.SetAutoSpeakersParams{ID: id, AutoSpeakers: append([]int32{}, auto...)}); err != nil {
				return err
			}
		}
		if err := s.learnVoices(ctx, q, id, learned); err != nil {
			return err
		}
		if len(renamed) == 0 {
			return nil
		}
		raw, err := current.Marshal()
		if err != nil {
			return err
		}
		if err := q.SetMeetingSpeakers(ctx, sqlc.SetMeetingSpeakersParams{ID: id, Speakers: raw}); err != nil {
			return err
		}
		// 当前纪要里的称呼跟着改。正在处理的会议也改：后台这一轮如果写出了新的纪要，
		// 写完时会对照最新的名字再换一遍；没写出来（失败了、没配大模型）的话，现在这一版就是对的
		if m.CurrentVersion > 0 {
			mv, err := q.GetMinutesVersion(ctx, sqlc.GetMinutesVersionParams{MeetingID: id, Version: m.CurrentVersion})
			if err != nil {
				return err
			}
			if content := minutes.RenameSpeakers(mv.Content, before, renamed); content != mv.Content {
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
	return s.GetMeeting(ctx, user, id)
}

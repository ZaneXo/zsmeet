package pipeline

import (
	"context"
	"log/slog"
	"slices"

	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/service"
	"github.com/ZaneXo/zsmeet/internal/transcript"
	"github.com/ZaneXo/zsmeet/internal/voiceprint"
)

// voices 在管理员打开了声纹时，给每位说话人算声音特征；关着时什么都不算。
// turns 已经换成转写里的说话人编号。算不出来不影响转写和纪要，只是这场会议认不出人、也学不了。
func (r *run) voices(ctx context.Context, engine recognizer, samples []float32, turns []transcript.Turn) (map[int]voiceprint.Voice, error) {
	cfg, err := r.w.settings.Voiceprints(ctx)
	if err != nil || !cfg.Enabled {
		return nil, err
	}
	voices, err := engine.Embed(samples, turns)
	if err != nil {
		slog.Warn("算说话人的声音特征失败，这场会议不按声音认人", "meeting", r.m.ID, "err", err)
		return nil, nil
	}
	return voices, nil
}

// nameSpeakers 在新的转写存进去时整理说话人的名字，m 是锁住的会议，old 是重新转写之前的转写（第一次转写时为 nil）：
// 先拿声纹库的锁（会议已经锁着，顺序和改名一样：会议 → 声纹库），然后：
//  1. 换掉这场会议存的声音特征（声纹关着时删掉，不存）；
//  2. 重新转写的话，以前起的名字、按声音认出的标记、记到声纹库里的样本都挪到新的编号上，对不上的去掉；
//  3. 按声音给还没有名字的说话人配上声纹库里的人，记下这些是自动起的。
//
// 名字在写纪要之前就定下来，大模型看到的是真名。
func (r *run) nameSpeakers(ctx context.Context, q *sqlc.Queries, m sqlc.Meeting, old, segs []transcript.Segment, voices map[int]voiceprint.Voice) error {
	id := m.ID
	if err := service.LockVoiceprints(ctx, q); err != nil {
		return err
	}
	// 算的时候管理员可能刚把声纹关掉了：关掉以后一点都不存。关掉也要拿声纹库的锁（见 service.SaveVoiceprintSettings），
	// 所以在锁里、同一个事务里读到的设置，到这个事务提交都不会变
	if len(voices) > 0 {
		cfg, err := r.w.settings.WithTx(q).Voiceprints(ctx)
		if err != nil {
			return err
		}
		if !cfg.Enabled {
			voices = nil
		}
	}
	if err := service.SaveSpeakerVoices(ctx, q, id, voices); err != nil {
		return err
	}

	names := transcript.ParseNames(m.Speakers)
	auto := slices.Clone(m.AutoSpeakers)
	if old != nil {
		ids := transcript.SpeakerMap(old, segs)
		var moved []int32
		for _, a := range auto {
			if b, ok := ids[int(a)]; ok && names[int(a)] != "" {
				moved = append(moved, int32(b))
			}
		}
		names, auto = transcript.RemapNames(old, segs, names), moved
		if err := service.RemapVoiceprintSamples(ctx, q, id, ids, voices); err != nil {
			return err
		}
	}
	matched, err := service.MatchVoiceprints(ctx, q, voices, names)
	if err != nil {
		return err
	}
	for idx, name := range matched {
		names[idx] = name
		auto = append(auto, int32(idx))
	}
	slices.Sort(auto)

	r.m.Speakers = m.Speakers
	if len(transcript.ParseNames(m.Speakers).Diff(names)) > 0 {
		stored, err := names.Marshal()
		if err != nil {
			return err
		}
		if err := q.SetMeetingSpeakers(ctx, sqlc.SetMeetingSpeakersParams{ID: id, Speakers: stored}); err != nil {
			return err
		}
		r.m.Speakers = stored
	}
	if !slices.Equal(auto, m.AutoSpeakers) {
		if err := q.SetAutoSpeakers(ctx, sqlc.SetAutoSpeakersParams{ID: id, AutoSpeakers: append([]int32{}, auto...)}); err != nil {
			return err
		}
	}
	return nil
}

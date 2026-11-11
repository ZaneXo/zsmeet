package service

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/settings"
	"github.com/ZaneXo/zsmeet/internal/voiceprint"
)

// 记住说话人（声纹）：有人在会议里给说话人改了名字，就把这位说话人的声音特征记在这个名字下面；
// 之后的会议转写完，按声音认出声纹库里的人，自动用他的名字。只有管理员打开了才收集和比对。

// 声纹库的锁，见 LockVoiceprints
const voiceprintsLock = 7_368_824

// LockVoiceprints 在当前事务里拿声纹库的锁（一把全局的事务级 advisory lock），事务提交或回滚时放开。
// 改声纹库（voiceprints、voiceprint_samples）和 speaker_embeddings 的事务都要先拿它，所以这些事务一个一个来：
//
//   - 清理空声纹时，不会有另一个事务正往这个声纹下加样本却还没提交（否则清理照样删掉声纹，级联删掉刚加的样本）；
//     拿到锁以后的语句看得到前面拿过锁的事务提交的全部改动（READ COMMITTED 每条语句重新取快照）；
//   - 两次改名朝相反方向挪样本、同名的人同时新建，不会互相卡死。
//
// 要锁会议那一行的事务（改名、转写完存结果），顺序固定是：会议 → 声纹库的锁 → 声纹库和声音特征的行。
// 删声纹不锁会议，只拿声纹库的锁，所以不会互相卡死。同一个事务里拿几次都行。
func LockVoiceprints(ctx context.Context, q *sqlc.Queries) error {
	return q.LockVoiceprints(ctx, voiceprintsLock)
}

// VoiceprintSettings 返回声纹是否打开。
func (s *Service) VoiceprintSettings(ctx context.Context) (settings.Voiceprints, error) {
	return s.settings.Voiceprints(ctx)
}

// SaveVoiceprintSettings 打开或关掉声纹。关掉以后不再收集、不再比对，已经记下的声纹留着，要删的话用 DeleteAllVoiceprints。
// 拿着声纹库的锁改：正在存声音特征、比对的事务（转写完、改名）都是拿着锁、在锁里读的设置，
// 所以关掉的请求返回时，它们要么已经做完，要么之后读到的是关着，不会在关掉以后还存或者比对。
func (s *Service) SaveVoiceprintSettings(ctx context.Context, enabled bool) (settings.Voiceprints, error) {
	v := settings.Voiceprints{Enabled: enabled}
	return v, s.tx(ctx, func(_ pgx.Tx, q *sqlc.Queries) error {
		if err := LockVoiceprints(ctx, q); err != nil {
			return err
		}
		return s.settings.WithTx(q).SaveVoiceprints(ctx, v)
	})
}

// Voiceprint 是声纹库里的一个人。只给管理员看：不说是哪几场会议，只说有几场。
type Voiceprint struct {
	ID   uuid.UUID
	Name string
	// 从几场会议里学到的
	Meetings int
	// 样本一共多少秒的说话
	Seconds   float64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ListVoiceprints 列出声纹库里的人，最近更新的在前。
func (s *Service) ListVoiceprints(ctx context.Context) ([]Voiceprint, error) {
	rows, err := s.q.ListVoiceprints(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Voiceprint, len(rows))
	for i, r := range rows {
		out[i] = Voiceprint{
			ID: r.ID, Name: r.Name, Meetings: int(r.Meetings), Seconds: r.Seconds, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		}
	}
	return out, nil
}

// DeleteVoiceprint 删掉一个人的声纹（连同全部样本）。会议里已经起好的名字不变；
// 以后有人再把说话人改成这个名字，会重新开始学。
func (s *Service) DeleteVoiceprint(ctx context.Context, id uuid.UUID) error {
	return s.tx(ctx, func(_ pgx.Tx, q *sqlc.Queries) error {
		if err := LockVoiceprints(ctx, q); err != nil {
			return err
		}
		n, err := q.DeleteVoiceprint(ctx, id)
		if err != nil {
			return err
		}
		if n == 0 {
			return notFound("没有这个声纹，可能已经删掉了")
		}
		return nil
	})
}

// DeleteAllVoiceprints 删掉声纹库，以及每场会议里存的说话人声音特征，服务器上不再留任何声音特征。
// 会议里已经起好的名字不变。
func (s *Service) DeleteAllVoiceprints(ctx context.Context) error {
	return s.tx(ctx, func(_ pgx.Tx, q *sqlc.Queries) error {
		if err := LockVoiceprints(ctx, q); err != nil {
			return err
		}
		if err := q.DeleteAllVoiceprints(ctx); err != nil {
			return err
		}
		return q.DeleteAllSpeakerEmbeddings(ctx)
	})
}

// learnVoices 在给说话人改名以后更新声纹库，names 是改了名字的说话人 → 新名字（空字符串表示去掉名字）。调用方锁着会议。
//   - 起了名字：这位说话人的声音特征记到这个名字下面（没有这个人就新建），以前记在别人名下的挪过来；
//   - 去掉名字：删掉这位说话人的样本；
//   - 声纹关着：不记新的，只删掉这些说话人以前的样本（名字改了，旧样本记的人就不对了）。
func (s *Service) learnVoices(ctx context.Context, q *sqlc.Queries, meetingID uuid.UUID, names map[int]string) error {
	if err := LockVoiceprints(ctx, q); err != nil {
		return err
	}
	// 在锁里、同一个事务里读设置（见 SaveVoiceprintSettings）
	cfg, err := s.settings.WithTx(q).Voiceprints(ctx)
	if err != nil {
		return err
	}
	// 去掉、挪走过样本的声纹，最后看看是不是空了
	var touched []uuid.UUID
	for idx, name := range names {
		key := int32(idx)
		if !cfg.Enabled || name == "" {
			removed, err := q.DeleteVoiceprintSample(ctx, sqlc.DeleteVoiceprintSampleParams{MeetingID: meetingID, Speaker: key})
			if err != nil {
				return err
			}
			for _, vp := range removed {
				if err := q.TouchVoiceprint(ctx, vp); err != nil {
					return err
				}
			}
			touched = append(touched, removed...)
			continue
		}

		old, err := q.GetVoiceprintSample(ctx, sqlc.GetVoiceprintSampleParams{MeetingID: meetingID, Speaker: key})
		hadSample := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		embedding, seconds := old.Embedding, old.Seconds
		voice, err := q.GetSpeakerEmbedding(ctx, sqlc.GetSpeakerEmbeddingParams{MeetingID: meetingID, Speaker: key})
		switch {
		case err == nil:
			embedding, seconds = voice.Embedding, voice.Seconds
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		case !hadSample:
			// 这位说话人没有声音特征（声纹打开之前处理的会议，或者他说话太少），学不了
			continue
		}

		vp, err := q.UpsertVoiceprint(ctx, name)
		if err != nil {
			return err
		}
		if err := q.SaveVoiceprintSample(ctx, sqlc.SaveVoiceprintSampleParams{
			MeetingID: meetingID, Speaker: key, VoiceprintID: vp, Embedding: embedding, Seconds: seconds,
		}); err != nil {
			return err
		}
		if hadSample && old.VoiceprintID != vp {
			if err := q.TouchVoiceprint(ctx, old.VoiceprintID); err != nil {
				return err
			}
			touched = append(touched, old.VoiceprintID)
		}
	}
	return deleteEmptyVoiceprints(ctx, q, touched)
}

// deleteEmptyVoiceprints 删掉 ids 里一个样本都不剩的声纹（没有用了）。调用方拿着声纹库的锁。
func deleteEmptyVoiceprints(ctx context.Context, q *sqlc.Queries, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	return q.DeleteEmptyVoiceprints(ctx, ids)
}

// SaveSpeakerVoices 换掉一场会议里存的说话人声音特征。转写（包括重新转写）完调用，调用方拿着声纹库的锁（见 LockVoiceprints）；
// 声纹关着时 voices 为空，以前存的也删掉。
func SaveSpeakerVoices(ctx context.Context, q *sqlc.Queries, meetingID uuid.UUID, voices map[int]voiceprint.Voice) error {
	if err := q.DeleteSpeakerEmbeddings(ctx, meetingID); err != nil {
		return err
	}
	for idx, v := range voices {
		if err := q.AddSpeakerEmbedding(ctx, sqlc.AddSpeakerEmbeddingParams{
			MeetingID: meetingID, Speaker: int32(idx), Embedding: v.Embedding, Seconds: float32(v.Seconds),
		}); err != nil {
			return err
		}
	}
	return nil
}

// RemapVoiceprintSamples 在重新转写以后，把这场会议记到声纹库里的样本挪到新的说话人编号上（ids 是旧编号 → 新编号，
// 和名字的挪法一样，见 transcript.SpeakerMap）。新的转写里有这个人的声音特征就换成新的；对不上的样本删掉。
func RemapVoiceprintSamples(ctx context.Context, q *sqlc.Queries, meetingID uuid.UUID, ids map[int]int, voices map[int]voiceprint.Voice) error {
	if err := LockVoiceprints(ctx, q); err != nil {
		return err
	}
	samples, err := q.MeetingVoiceprintSamples(ctx, meetingID)
	if err != nil || len(samples) == 0 {
		return err
	}
	if err := q.DeleteMeetingVoiceprintSamples(ctx, meetingID); err != nil {
		return err
	}
	// 这些样本全删了再按新编号加回去（两个旧编号挪到同一个新编号时只留后一个），它们原来的声纹都算动过
	touched := make([]uuid.UUID, 0, len(samples))
	for _, sample := range samples {
		touched = append(touched, sample.VoiceprintID)
		to, ok := ids[int(sample.Speaker)]
		if !ok {
			if err := q.TouchVoiceprint(ctx, sample.VoiceprintID); err != nil {
				return err
			}
			continue
		}
		embedding, seconds := sample.Embedding, sample.Seconds
		if v, ok := voices[to]; ok {
			embedding, seconds = v.Embedding, float32(v.Seconds)
		}
		if err := q.SaveVoiceprintSample(ctx, sqlc.SaveVoiceprintSampleParams{
			MeetingID: meetingID, Speaker: int32(to), VoiceprintID: sample.VoiceprintID, Embedding: embedding, Seconds: seconds,
		}); err != nil {
			return err
		}
	}
	return deleteEmptyVoiceprints(ctx, q, touched)
}

// MatchVoiceprints 按声音给还没有名字的说话人配上声纹库里的人，返回 说话人编号 → 名字（规则见 voiceprint.Match）。
// named 是会议里已经有的名字。
func MatchVoiceprints(ctx context.Context, q *sqlc.Queries, voices map[int]voiceprint.Voice, named map[int]string) (map[int]string, error) {
	if len(voices) == 0 {
		return nil, nil
	}
	rows, err := q.AllVoiceprintSamples(ctx)
	if err != nil {
		return nil, err
	}
	var prints []voiceprint.Print
	for i := 0; i < len(rows); {
		j := i
		var samples [][]float32
		for ; j < len(rows) && rows[j].ID == rows[i].ID; j++ {
			samples = append(samples, rows[j].Embedding)
		}
		if v := voiceprint.Mean(samples); v != nil {
			prints = append(prints, voiceprint.Print{Name: rows[i].Name, Vector: v})
		}
		i = j
	}
	vectors := make(map[int][]float32, len(voices))
	for idx, v := range voices {
		vectors[idx] = v.Embedding
	}
	return voiceprint.Match(vectors, prints, named), nil
}

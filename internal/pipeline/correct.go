package pipeline

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ZaneXo/zsmeet/internal/correct"
	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/llm"
	"github.com/ZaneXo/zsmeet/internal/minutes"
	"github.com/ZaneXo/zsmeet/internal/search"
	"github.com/ZaneXo/zsmeet/internal/transcript"
)

// hotwords 是纠错时用的专有名词：建会议时填的本场热词，加上这场会议以前写纪要时整理出的术语表里的正确写法
// （重新转写时才有）。
func (r *run) hotwords() []string {
	words := correct.ParseHotwords(r.m.Hotwords)
	var terms []minutes.Term
	if len(r.m.Terms) > 0 && json.Unmarshal(r.m.Terms, &terms) == nil {
		for _, t := range terms {
			if w := strings.TrimSpace(t.Term); w != "" && !slices.ContainsFunc(words, func(x string) bool { return strings.EqualFold(x, w) }) {
				words = append(words, w)
			}
		}
	}
	return words
}

// correct 让大模型逐句校对刚识别出的转写，改过的存回数据库。没有配置大模型时什么都不做。
//
// 校对是锦上添花：某几批请求失败了只记日志，用没改的句子接着写纪要，不让整场会议失败。
// 只有任务被取消、被顶替（或者存不进数据库）时才返回错误。
func (r *run) correct(ctx context.Context, segs []transcript.Segment) ([]transcript.Segment, error) {
	cfg, err := r.w.settings.LLM(ctx)
	if err != nil {
		return nil, err
	}
	if !cfg.Ready() {
		return segs, nil
	}
	r.report(ctx, StageCorrect, 0)

	// 说话人的名字用最新的：转写刚存好时按声纹认出了一些人
	names := transcript.ParseNames(r.m.Speakers)
	if row, err := r.w.q.GetMeeting(ctx, r.m.ID); err == nil {
		names = transcript.ParseNames(row.Meeting.Speakers)
	}
	chat := func(ctx context.Context, system, user string) (string, error) {
		return llm.Chat(ctx, cfg, system, user)
	}
	fixed := slices.Clone(segs)
	stats, err := correct.LLM(ctx, chat, fixed, correct.Input{
		Title:    r.m.Title,
		Hotwords: r.hotwords(),
		Speakers: names,
	}, func(done, total int) {
		r.report(ctx, StageCorrect, float64(done)/float64(total))
	})
	if err != nil {
		return nil, err
	}
	log := slog.With("meeting", r.m.ID, "batches", stats.Batches, "changed", stats.Changed, "rejected", stats.Rejected)
	if stats.Err != nil {
		log.Warn("大模型校对转写有几批没做成，这几批保持原样", "failed", stats.Failed, "err", stats.Err)
	} else {
		log.Info("大模型校对了转写")
	}
	if stats.Changed == 0 {
		return segs, nil
	}

	raw, err := json.Marshal(fixed)
	if err != nil {
		return nil, err
	}
	err = r.inTx(ctx, func(_ pgx.Tx, q *sqlc.Queries, _ sqlc.Meeting) error {
		if err := q.SaveTranscript(ctx, sqlc.SaveTranscriptParams{MeetingID: r.m.ID, Segments: raw}); err != nil {
			return err
		}
		return search.Reindex(ctx, q, r.m.ID)
	})
	if err != nil {
		return nil, err
	}
	return fixed, nil
}

package service

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ZaneXo/zsmeet/internal/ask"
	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/llm"
	"github.com/ZaneXo/zsmeet/internal/search"
	"github.com/ZaneXo/zsmeet/internal/transcript"
)

const (
	// 问题最长多少个字
	maxQuestionChars = 300
	// 交给大模型的材料大约多少个字。照顾上下文只有 32K 的本地模型，也让回答快一些
	askBudget = 12_000
	// 材料最多来自几场会议
	maxAskMeetings = 6
	// 每个搜索词最多找多少场会议，再从里面挑最相关的
	askPerKeyword = 20
	// 搜索命中的那句话前后各带几句，让大模型看得懂上下文
	askNeighbours = 1
	// 只问一场会议、整场放不下时，命中的那句话前后各带几句
	askNeighboursOne = 2
	// 一次提问最长等多久（两次大模型调用）
	askTimeout = 3 * time.Minute
	// 第一次调用（把问题变成搜索词）最长等多久。超时就从问题里拆几个词去搜，把剩下的时间留给回答
	askPlanTimeout = 45 * time.Second
	// 每个人每分钟最多问几次
	askPerMinute = 6
)

// AskInput 是一次提问。
type AskInput struct {
	Question string
	// 只问这一场会议；为 nil 时在提问的人能看的所有会议里找
	MeetingID *uuid.UUID
}

// Answer 是大模型的回答。
type Answer struct {
	// 回答，引用的地方写成 [1]、[2]，对应 Citations 里的 N
	Text      string
	Citations []ask.Citation
	// 材料来自几场会议
	Meetings int
	// 实际用来搜索的词（大模型从问题里提取的，大模型没给出时从问题里拆的）；只问一场会议、整场都交给了大模型时为空
	Keywords []string
	// 问题限定的日期范围（含首尾），没限定时是零值
	From, To time.Time
}

// 没找到任何相关内容时的回答，不用再问大模型
const askNothingFound = "在你能看到的会议里没有找到和这个问题相关的内容。可以换个说法，或者用会上可能说过的词（人名、产品名、项目名）再问一次。"

// Ask 回答关于会议的问题：先让大模型从问题里提取搜索词和时间范围，在提问的人能看的会议里搜，
// 把搜到的纪要和转写片段编上号交给大模型回答，回答里引用的编号换回会议和时间点。
// 只问一场会议时，整场放得下就整场交给大模型，放不下才按搜索词挑片段。
// 看不到的会议不会出现在材料、回答和出处里。
func (s *Service) Ask(ctx context.Context, user sqlc.User, in AskInput) (Answer, error) {
	question := strings.Join(strings.Fields(in.Question), " ")
	if question == "" {
		return Answer{}, invalid("请输入要问的问题")
	}
	if utf8.RuneCountInString(question) > maxQuestionChars {
		return Answer{}, invalid("问题太长了，最多 %d 个字", maxQuestionChars)
	}
	cfg, err := s.settings.LLM(ctx)
	if err != nil {
		return Answer{}, err
	}
	if !cfg.Ready() {
		return Answer{}, conflict("还没有配置大模型，请管理员先到「设置」里填写")
	}
	var one *Meeting
	if in.MeetingID != nil {
		m, err := s.GetMeeting(ctx, user, *in.MeetingID)
		if err != nil {
			return Answer{}, err
		}
		one = &m
	}
	if wait := s.asks.take(user.ID, time.Now()); wait > 0 {
		return Answer{}, &Error{TooManyRequests, fmt.Sprintf("提问太频繁了，每分钟最多问 %d 次，请 %d 秒后再试", askPerMinute, int(wait.Seconds())+1)}
	}

	ctx, cancel := context.WithTimeout(ctx, askTimeout)
	defer cancel()
	now := time.Now()
	chat := func(ctx context.Context, system, prompt string) (string, error) {
		return llm.Chat(ctx, cfg, system, prompt)
	}

	var sources []ask.Source
	var plan ask.Plan
	if one != nil {
		sources, plan, err = s.askOne(ctx, user, *one, question, now, chat)
	} else {
		sources, plan, err = s.askAll(ctx, user, question, now, chat)
	}
	if err != nil {
		return Answer{}, askFailed(ctx, user, err)
	}
	out := Answer{Keywords: plan.Keywords, From: plan.From, To: plan.To, Citations: []ask.Citation{}}
	material := ask.Build(sources, askBudget)
	if material.Meetings == 0 {
		out.Text = askNothingFound
		return out, nil
	}
	system, prompt := ask.AnswerPrompt(question, material, now)
	reply, err := chat(ctx, system, prompt)
	if err != nil {
		return Answer{}, askFailed(ctx, user, llmError{err})
	}
	out.Text, out.Citations = ask.Cite(reply, material.Snippets)
	out.Meetings = material.Meetings
	return out, nil
}

// llmError 是调用大模型失败，和查数据库失败区分开。
type llmError struct{ err error }

func (e llmError) Error() string { return e.err.Error() }

// askFailed 把提问时的错误换成给用户看的话。大模型的报错原文只告诉管理员（可能带着接口地址），
// 其他人看到的是让他稍后再试、或者找管理员。
func askFailed(ctx context.Context, user sqlc.User, err error) error {
	var le llmError
	if !errors.As(err, &le) {
		return err
	}
	slog.Warn("问会议时调用大模型失败", "err", le.err)
	if ctx.Err() != nil {
		return &Error{Unavailable, "大模型太久没有回答，请稍后再试，或者把问题问得具体一些"}
	}
	if user.Role == "admin" {
		return &Error{Unavailable, "大模型没有答出来：" + le.err.Error()}
	}
	return &Error{Unavailable, "大模型暂时没有答出来，请稍后再试。一直不行的话，请管理员到「设置」里测试一下大模型的连接"}
}

// plan 让大模型从问题里提取搜索词和时间范围。超时、没给出搜索词时从问题里拆几个短词去搜（见 ask.ParsePlan）。
func (s *Service) plan(ctx context.Context, question string, now time.Time, chat func(context.Context, string, string) (string, error)) (ask.Plan, error) {
	planCtx, cancel := context.WithTimeout(ctx, askPlanTimeout)
	defer cancel()
	system, prompt := ask.PlanPrompt(question, now)
	out, err := chat(planCtx, system, prompt)
	if err != nil {
		if planCtx.Err() != nil && ctx.Err() == nil {
			slog.Warn("问会议时提取搜索词超时，从问题里拆词搜索")
			return ask.ParsePlan("", question, now), nil
		}
		return ask.Plan{}, llmError{err}
	}
	return ask.ParsePlan(out, question, now), nil
}

// askOne 准备只问一场会议时的材料：纪要和整场转写；放不下时纪要加上按搜索词找到的转写片段。
func (s *Service) askOne(ctx context.Context, user sqlc.User, m Meeting, question string, now time.Time, chat func(context.Context, string, string) (string, error)) ([]ask.Source, ask.Plan, error) {
	segs, minutesOf, err := s.searchTexts(ctx, []uuid.UUID{m.ID})
	if err != nil {
		return nil, ask.Plan{}, err
	}
	lines := ask.TranscriptLines(segs[m.ID], transcript.ParseNames(m.Speakers))
	src := ask.Source{MeetingID: m.ID, Title: m.Title, Date: m.CreatedAt.Local(), Minutes: ask.MinutesLines(minutesOf[m.ID])}
	if len(lines) == 0 && len(src.Minutes) == 0 {
		return nil, ask.Plan{}, conflict("这场会议还没有转写和纪要，处理完以后再问")
	}
	if len(lines) > 0 {
		src.Transcript = [][]ask.Line{lines}
	}
	if ask.Size(src) <= askBudget {
		return []ask.Source{src}, ask.Plan{}, nil
	}

	plan, err := s.plan(ctx, question, now, chat)
	if err != nil {
		return nil, ask.Plan{}, err
	}
	// 只问一场会议时不按时间筛选
	plan.From, plan.To = time.Time{}, time.Time{}
	var hits []int
	for _, kw := range plan.Keywords {
		spellings, err := s.spellings(ctx, user, kw)
		if err != nil {
			return nil, ask.Plan{}, err
		}
		res := search.Find("", "", segs[m.ID], spellings)
		hits = append(hits, sentenceIndexes(segs[m.ID], res.Hits)...)
	}
	if len(hits) == 0 {
		// 一处也没搜到：给纪要和转写的开头，纪要一般已经概括了全场
		src.Transcript = [][]ask.Line{lines}
	} else {
		src.Transcript = windows(lines, hits, askNeighboursOne)
	}
	return []ask.Source{src}, plan, nil
}

// askAll 准备在所有能看的会议里找时的材料：按搜索词搜，挑出最相关的几场，放进命中的纪要行和转写片段。
func (s *Service) askAll(ctx context.Context, user sqlc.User, question string, now time.Time, chat func(context.Context, string, string) (string, error)) ([]ask.Source, ask.Plan, error) {
	plan, err := s.plan(ctx, question, now, chat)
	if err != nil {
		return nil, ask.Plan{}, err
	}
	var within period
	if !plan.From.IsZero() {
		within.from = &plan.From
	}
	if !plan.To.IsZero() {
		end := plan.To.AddDate(0, 0, 1)
		within.to = &end
	}

	found := map[uuid.UUID]*askCandidate{}
	if len(plan.Keywords) == 0 {
		// 只限定了时间（「上周开了哪些会」）：列出这段时间的会议
		params := sqlc.ListMeetingsParams{Viewer: user.ID, IsAdmin: user.Role == "admin", MaxRows: maxAskMeetings * 4}
		if within.to != nil {
			params.BeforeAt, params.BeforeID = within.to, &uuid.Nil
		}
		rows, err := s.q.ListMeetings(ctx, params)
		if err != nil {
			return nil, plan, err
		}
		for _, r := range rows {
			if within.from != nil && r.Meeting.CreatedAt.Before(*within.from) {
				break
			}
			found[r.Meeting.ID] = &askCandidate{m: listed(user, r)}
		}
	}
	for _, kw := range plan.Keywords {
		list, err := s.searchMeetings(ctx, user, kw, nil, askPerKeyword, within)
		if err != nil {
			return nil, plan, err
		}
		for _, m := range list.Items {
			c := found[m.ID]
			if c == nil {
				c = &askCandidate{m: m}
				c.m.Hits = nil
				found[m.ID] = c
			}
			c.add(m.Hits)
		}
	}

	ranked := make([]*askCandidate, 0, len(found))
	for _, c := range found {
		ranked = append(ranked, c)
	}
	slices.SortFunc(ranked, func(a, b *askCandidate) int {
		if d := cmp.Compare(b.score(now), a.score(now)); d != 0 {
			return d
		}
		return b.m.CreatedAt.Compare(a.m.CreatedAt)
	})
	ranked = ranked[:min(len(ranked), maxAskMeetings)]

	ids := make([]uuid.UUID, len(ranked))
	for i, c := range ranked {
		ids[i] = c.m.ID
	}
	segs, minutesOf, err := s.searchTexts(ctx, ids)
	if err != nil {
		return nil, plan, err
	}
	sources := make([]ask.Source, 0, len(ranked))
	for _, c := range ranked {
		m := c.m
		src := ask.Source{MeetingID: m.ID, Title: m.Title, Date: m.CreatedAt.Local()}
		lines := ask.TranscriptLines(segs[m.ID], transcript.ParseNames(m.Speakers))
		for _, h := range c.hits {
			if h.Source == "minutes" {
				src.Minutes = append(src.Minutes, ask.Line{Start: h.Start, Text: h.Text, Minutes: true})
			}
		}
		src.Transcript = windows(lines, sentenceIndexes(segs[m.ID], c.hits), askNeighbours)
		if len(src.Minutes) == 0 && len(src.Transcript) == 0 {
			// 只是标题对上了，或者只限定了时间：给纪要的开头（摘要），没有纪要就给转写的开头
			src.Minutes = ask.MinutesLines(minutesOf[m.ID])
			src.Minutes = src.Minutes[:min(len(src.Minutes), 15)]
			if len(src.Minutes) == 0 && len(lines) > 0 {
				src.Transcript = [][]ask.Line{lines[:min(len(lines), 20)]}
			}
		}
		sources = append(sources, src)
	}
	return sources, plan, nil
}

// askCandidate 是一场搜到的会议，和各个搜索词在它里面的命中。
type askCandidate struct {
	m    Meeting
	hits []search.Hit
	// 有几个搜索词搜到了它（不算只错了一个字的）、几个只是错了一个字
	exact, fuzzy int
}

func (c *askCandidate) add(hits []search.Hit) {
	isFuzzy := len(hits) > 0
	for _, h := range hits {
		isFuzzy = isFuzzy && h.Fuzzy
		dup := slices.ContainsFunc(c.hits, func(x search.Hit) bool {
			return x.Source == h.Source && x.Start == h.Start && x.Text == h.Text
		})
		if !dup {
			c.hits = append(c.hits, h)
		}
	}
	if isFuzzy {
		c.fuzzy++
	} else {
		c.exact++
	}
}

// score 是会议和问题的相关程度：搜到它的词越多越相关，命中的句子多一些更好，近期的会议优先。
func (c *askCandidate) score(now time.Time) float64 {
	score := 10*float64(c.exact) + 4*float64(c.fuzzy) + float64(min(len(c.hits), 10))
	switch age := now.Sub(c.m.CreatedAt); {
	case age < 7*24*time.Hour:
		score += 3
	case age < 30*24*time.Hour:
		score += 2
	case age < 90*24*time.Hour:
		score += 1
	}
	return score
}

// sentenceIndexes 找出转写里命中的那几句是第几句。
func sentenceIndexes(segs []transcript.Segment, hits []search.Hit) []int {
	var out []int
	for _, h := range hits {
		if h.Source != "transcript" {
			continue
		}
		if i := slices.IndexFunc(segs, func(s transcript.Segment) bool { return s.Start == h.Start && s.Text == h.Text }); i >= 0 {
			out = append(out, i)
		}
	}
	return out
}

// windows 取出命中的句子和前后各 n 句，按时间排好，挨着或重叠的并成一段。
func windows(lines []ask.Line, idx []int, n int) [][]ask.Line {
	if len(idx) == 0 {
		return nil
	}
	idx = slices.Clone(idx)
	slices.Sort(idx)
	idx = slices.Compact(idx)
	var out [][]ask.Line
	from, to := -1, -1
	flush := func() {
		if from >= 0 {
			out = append(out, lines[from:to+1])
		}
	}
	for _, i := range idx {
		a, b := max(0, i-n), min(len(lines)-1, i+n)
		if from >= 0 && a <= to+1 {
			to = max(to, b)
			continue
		}
		flush()
		from, to = a, b
	}
	flush()
	return out
}

// rateLimit 是内存里的简单限流：每个人在 window 里最多 n 次。服务重启就清零，只用来挡住连着猛点和脚本刷接口。
type rateLimit struct {
	mu     sync.Mutex
	n      int
	window time.Duration
	hits   map[uuid.UUID][]time.Time
}

func newRateLimit(n int, window time.Duration) *rateLimit {
	return &rateLimit{n: n, window: window, hits: map[uuid.UUID][]time.Time{}}
}

// take 记一次。超过限制时不记，返回还要等多久；没超过返回 0。
func (r *rateLimit) take(id uuid.UUID, now time.Time) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	// 顺便清掉所有人过期的记录，map 不会越积越多
	for k, ts := range r.hits {
		ts = slices.DeleteFunc(ts, func(t time.Time) bool { return now.Sub(t) >= r.window })
		if len(ts) == 0 {
			delete(r.hits, k)
		} else {
			r.hits[k] = ts
		}
	}
	ts := r.hits[id]
	if len(ts) >= r.n {
		return r.window - now.Sub(ts[0])
	}
	r.hits[id] = append(ts, now)
	return 0
}

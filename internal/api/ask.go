package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/ZaneXo/zsmeet/internal/service"
)

type AskInput struct {
	Body struct {
		// 长度在 service 里按字数检查（最多 300 个字），这里只挡住特别长的请求
		Question  string `json:"question" maxLength:"2000" doc:"要问的问题，最多 300 个字，例如「上周关于报价定了什么？」"`
		MeetingID string `json:"meeting_id,omitempty" doc:"只问这一场会议（会议的 ID）；不填表示在自己能看的所有会议里找"`
	}
}

type Citation struct {
	N         int       `json:"n" doc:"回答里写成 [n] 的序号，从 1 开始"`
	MeetingID string    `json:"meeting_id" format:"uuid"`
	Title     string    `json:"title" doc:"会议标题"`
	Date      time.Time `json:"date" doc:"会议时间"`
	At        float64   `json:"at" doc:"引用的那句话从录音开头算的秒数；-1 表示没有时间点（纪要里没标时间的行）"`
	Source    string    `json:"source" enum:"transcript,minutes" doc:"引用的是转写里的一句，还是纪要里的一行。纪要里的时间点只精确到秒"`
	Quote     string    `json:"quote" doc:"引用的原文（太长的截断）"`
}

type AskOutput struct {
	Body struct {
		Answer    string     `json:"answer" doc:"大模型的回答，纯文本；[n] 对应 citations 里的出处。会议记录里没找到时也是一段说明"`
		Citations []Citation `json:"citations" nullable:"false" doc:"回答引用的出处，按 n 排列。回答里引用了材料里没有的编号时直接去掉，不会出现在这里"`
		Meetings  int        `json:"meetings_searched" doc:"交给大模型的材料来自几场会议（按问题找到的最相关的几场，最多 6 场）"`
		Keywords  []string   `json:"keywords" nullable:"false" doc:"实际拿去搜索的词：大模型从问题里提取的，大模型没给出时从问题里拆出来的。只问一场会议、整场都交给了大模型时为空"`
		From      string     `json:"from,omitempty" format:"date" doc:"问题限定的开始日期（含），没限定时没有这一项"`
		To        string     `json:"to,omitempty" format:"date" doc:"问题限定的结束日期（含），没限定时没有这一项"`
	}
}

var askOp = huma.Operation{
	OperationID: "ask",
	Method:      http.MethodPost,
	Path:        "/api/ask",
	Summary:     "问会议",
	Description: "用自然语言提问，管理员配置的大模型根据会议的纪要和转写回答，并给出能点的出处。" +
		"不填 meeting_id 时在自己能看的所有会议里找，填了只问那一场（看不到的会议返回 404）。" +
		"没配置大模型时返回 409；每人每分钟最多问 6 次，超过返回 429；大模型出错或超时返回 502。",
	Tags:     []string{"会议"},
	Security: loggedIn,
}

func (h *Handlers) Ask(ctx context.Context, in *AskInput) (*AskOutput, error) {
	q := service.AskInput{Question: in.Body.Question}
	if in.Body.MeetingID != "" {
		id, err := uuid.Parse(in.Body.MeetingID)
		if err != nil {
			return nil, huma.Error404NotFound("会议不存在，可能已被删除")
		}
		q.MeetingID = &id
	}
	a, err := h.svc.Ask(ctx, currentUser(ctx), q)
	if err != nil {
		return nil, toHTTP(err)
	}
	out := &AskOutput{}
	out.Body.Answer = a.Text
	out.Body.Meetings = a.Meetings
	out.Body.Keywords = a.Keywords
	if out.Body.Keywords == nil {
		out.Body.Keywords = []string{}
	}
	if !a.From.IsZero() {
		out.Body.From = a.From.Format(time.DateOnly)
	}
	if !a.To.IsZero() {
		out.Body.To = a.To.Format(time.DateOnly)
	}
	out.Body.Citations = make([]Citation, len(a.Citations))
	for i, c := range a.Citations {
		source := "transcript"
		if c.Minutes {
			source = "minutes"
		}
		out.Body.Citations[i] = Citation{
			N: c.N, MeetingID: c.MeetingID.String(), Title: c.Title, Date: c.Date,
			At: c.Start, Source: source, Quote: c.Text,
		}
	}
	return out, nil
}

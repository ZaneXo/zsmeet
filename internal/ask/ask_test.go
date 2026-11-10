package ask

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ZaneXo/zsmeet/internal/transcript"
)

var now = time.Date(2026, 11, 10, 15, 0, 0, 0, time.Local)

func date(s string) time.Time {
	t, err := time.ParseInLocation(time.DateOnly, s, time.Local)
	if err != nil {
		panic(err)
	}
	return t
}

func TestPlanPromptHasToday(t *testing.T) {
	_, user := PlanPrompt("上周关于报价定了什么？", now)
	if !strings.Contains(user, "2026-11-10，星期二") || !strings.Contains(user, "上周关于报价定了什么？") {
		t.Errorf("要告诉大模型今天的日期和星期，才能换算「上周」：%s", user)
	}
}

func TestParsePlan(t *testing.T) {
	q := "上周关于报价定了什么？"
	cases := []struct {
		name, out string
		keywords  string
		from, to  string
	}{
		{"正常", `{"keywords": ["报价", "合同"], "from": "2026-11-02", "to": "2026-11-08"}`, "[报价 合同]", "2026-11-02", "2026-11-08"},
		{"包在代码块和废话里", "好的：\n```json\n{\"keywords\": [\"报价\"], \"from\": \"\", \"to\": \"\"}\n```", "[报价]", "", ""},
		{"JSON 后面还有带花括号的话", `{"keywords": ["报价"], "from": "", "to": ""}` + "\n说明：{报价} 是按 {问题} 挑的。", "[报价]", "", ""},
		{"代码块后面还有花括号", "```json\n{\"keywords\": [\"合同\"]}\n```\n注意 {不要} 漏了 }", "[合同]", "", ""},
		{"前面的花括号不是 JSON", `我理解的是 {上周} 的会：{"keywords": ["定价"], "from": "2026-11-02", "to": "2026-11-08"}`, "[定价]", "2026-11-02", "2026-11-08"},
		{"写坏了就从问题里拆词", `{"keywords": ["报价"`, "[报价]", "", ""},
		{"根本不是 JSON", "抱歉，我不明白", "[报价]", "", ""},
		{"格式不对的不算", `{"keywords": "报价合同"}`, "[报价]", "", ""},
		{"去掉单字、太长、没有字、重复的词", `{"keywords": ["价", "  报价 ", "报价", "2026", "` + strings.Repeat("长", 21) + `", "ABC Pro"]}`, "[报价 ABC Pro]", "", ""},
		{"最多五个", `{"keywords": ["甲方", "乙方", "丙方", "丁方", "戊方", "己方"]}`, "[甲方 乙方 丙方 丁方 戊方]", "", ""},
		{"只限定了时间", `{"keywords": [], "from": "2026-11-02", "to": "2026-11-08"}`, "[]", "2026-11-02", "2026-11-08"},
		{"只给了开始就算到今天", `{"keywords": ["报价"], "from": "2026-10-01"}`, "[报价]", "2026-10-01", "2026-11-10"},
		{"只给了结束", `{"keywords": ["报价"], "to": "2026-10-31"}`, "[报价]", "", "2026-10-31"},
		{"前后颠倒的日期不要", `{"keywords": ["报价"], "from": "2026-11-08", "to": "2026-11-02"}`, "[报价]", "", ""},
		{"日期写错不要", `{"keywords": ["报价"], "from": "上周一", "to": "上周日"}`, "[报价]", "", ""},
		{"跨度太大不要", `{"keywords": ["报价"], "from": "2000-01-01", "to": "2026-11-08"}`, "[报价]", "", ""},
	}
	for _, c := range cases {
		p := ParsePlan(c.out, q, now)
		kw := fmt.Sprint(p.Keywords)
		if p.Keywords == nil {
			kw = "[]"
		}
		from, to := "", ""
		if !p.From.IsZero() {
			from = p.From.Format(time.DateOnly)
		}
		if !p.To.IsZero() {
			to = p.To.Format(time.DateOnly)
		}
		if kw != c.keywords || from != c.from || to != c.to {
			t.Errorf("%s：得到 %s %q～%q，应该是 %s %q～%q", c.name, kw, from, to, c.keywords, c.from, c.to)
		}
	}
}

// 大模型没给出搜索词时从问题里拆几个短词：去掉问法、时间这些词，不超过搜索的长度和个数限制。
func TestQuestionTerms(t *testing.T) {
	cases := []struct{ q, want string }{
		{"上周关于客户报价定了什么？", "[客户报价]"},
		{"网页录音什么时候上线？", "[网页录音 上线]"},
		{"我们和 ABC Pro 的合同签了吗", "[ABC Pro 合同签]"},
		{"李航负责的事情有哪些", "[李航负责]"},
		{"What did we decide about pricing?", "[decide pricing]"},
		{"最近开了哪些会？", "[]"},
		{"？？？", "[]"},
		{"", "[]"},
	}
	for _, c := range cases {
		if got := fmt.Sprint(questionTerms(c.q)); got != c.want {
			t.Errorf("%q：拆出 %s，应该是 %s", c.q, got, c.want)
		}
	}
}

func TestQuestionTermsLimits(t *testing.T) {
	// 300 个字的问题：词不超过五个，每个不超过搜索词的长度限制；很长的一串汉字切成几段
	q := "关于" + strings.Repeat("甲乙丙丁戊己庚辛壬癸", 20) + "，" + strings.Repeat("x", 60) + "，客户报价是多少？" + strings.Repeat("嗯", 60)
	if n := utf8.RuneCountInString(q); n < 280 {
		t.Fatalf("问题要够长：%d", n)
	}
	terms := ParsePlan("我不知道", q, now).Keywords
	if len(terms) == 0 || len(terms) > maxKeywords {
		t.Fatalf("最多 %d 个词：%v", maxKeywords, terms)
	}
	for _, k := range terms {
		if n := utf8.RuneCountInString(k); n < 2 || n > maxKeywordLen {
			t.Errorf("每个词 2 到 %d 个字：%q", maxKeywordLen, k)
		}
	}
	if terms[0] != "甲乙丙丁" {
		t.Errorf("太长的一串汉字切成几段：%v", terms)
	}
	// 前面的词不多时，后面的英文词截断到长度限制
	terms = questionTerms("报价 " + strings.Repeat("x", 60))
	if fmt.Sprint(terms) != "[报价 "+strings.Repeat("x", maxKeywordLen)+"]" {
		t.Errorf("太长的词截断：%v", terms)
	}
}

func TestBuildLabelsEveryLineAndKeepsBudget(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	sources := []Source{
		{
			MeetingID: a, Title: "产品周会", Date: date("2026-11-03"),
			Minutes: []Line{{Start: 75, Text: "报价定为每年 12 万。", Minutes: true, Section: "结论"}, {Start: -1, Text: "聊了报价。", Minutes: true}},
			Transcript: [][]Line{
				{{Start: 70, Speaker: "张敏", Text: "我们先说报价。"}, {Start: 75.5, Speaker: "李航", Text: "就按 12 万。"}},
				{{Start: 300, Speaker: "张敏", Text: "合同下周签。"}},
			},
		},
		{MeetingID: b, Title: "技术评审", Date: date("2026-11-04"), Transcript: [][]Line{{{Start: 5, Speaker: "说话人 1", Text: "报价系统要重构。"}}}},
	}
	m := Build(sources, 10_000)
	if m.Meetings != 2 || len(m.Snippets) != 6 {
		t.Fatalf("两场会议、六行都要放进去：%d 场，%v", m.Meetings, m.Snippets)
	}
	for _, want := range []string{
		"## 会议 M1：产品周会（2026-11-03 00:00）",
		"[M1-1] 纪要·结论（01:15）：报价定为每年 12 万。",
		"[M1-2] 纪要：聊了报价。",
		"[M1-4] 01:15 李航：就按 12 万。",
		"……\n[M1-5] 05:00 张敏：合同下周签。",
		"## 会议 M2：技术评审",
		"[M2-1] 00:05 说话人 1：报价系统要重构。",
	} {
		if !strings.Contains(m.Text, want) {
			t.Errorf("材料里应该有 %q：\n%s", want, m.Text)
		}
	}
	if sn := m.Snippets["M1-4"]; sn.MeetingID != a || sn.Start != 75.5 || sn.Title != "产品周会" || sn.Minutes {
		t.Errorf("M1-4 对应产品周会 75.5 秒那句：%+v", sn)
	}

	// 放不下时每场分到一份，第一场用不完的留给后面
	long := make([]Line, 200)
	for i := range long {
		long[i] = Line{Start: float64(i), Speaker: "张敏", Text: strings.Repeat("报价", 20)}
	}
	sources = []Source{
		{MeetingID: a, Title: "短会", Date: now, Transcript: [][]Line{{{Start: 1, Speaker: "张敏", Text: "好。"}}}},
		{MeetingID: b, Title: "长会", Date: now, Transcript: [][]Line{long}},
	}
	m = Build(sources, 3000)
	if n := len([]rune(m.Text)); n > 3000 || n < 2500 {
		t.Errorf("材料应该接近但不超过 3000 字，实际 %d 字", n)
	}
	if _, ok := m.Snippets["M1-1"]; !ok || m.Meetings != 2 {
		t.Errorf("两场都要有：%d", m.Meetings)
	}

	// 一行都放不下的会议整场不放，编号也不占
	m = Build([]Source{
		{MeetingID: a, Title: "长会", Date: now, Transcript: [][]Line{long}},
		{MeetingID: b, Title: "短会", Date: now, Transcript: [][]Line{{{Start: 1, Speaker: "张敏", Text: "好。"}}}},
	}, 80)
	if m.Meetings != 1 || m.Snippets["M1-1"].MeetingID != b || strings.Contains(m.Text, "长会") {
		t.Errorf("放不下的会议不要：%s %v", m.Text, m.Snippets)
	}
}

func TestCite(t *testing.T) {
	a := uuid.New()
	snippets := map[string]Snippet{
		"M1-1": {ID: "M1-1", MeetingID: a, Title: "产品周会", Start: 75, Text: "报价定为每年 12 万。"},
		"M1-4": {ID: "M1-4", MeetingID: a, Title: "产品周会", Start: 75.5, Text: strings.Repeat("长", 200)},
		"M2-1": {ID: "M2-1", MeetingID: uuid.New(), Title: "技术评审", Start: 5, Text: "报价系统要重构。"},
	}
	text, cites := Cite("报价定为每年 12 万 [M1-1][M1-4]。合同下周签【M9-9】。报价系统要重构（M2-1，M1-1）。再说一次 [M1-1][M1-1]，编的 [M3-2]。", snippets)
	want := "报价定为每年 12 万[1][2]。合同下周签。报价系统要重构[3][1]。再说一次[1]，编的。"
	if text != want {
		t.Errorf("编号要换成序号，编造的去掉：\n得到 %s\n应该 %s", text, want)
	}
	if len(cites) != 3 || cites[0].N != 1 || cites[0].ID != "M1-1" || cites[1].ID != "M1-4" || cites[2].ID != "M2-1" || cites[2].N != 3 {
		t.Fatalf("出处按第一次出现的先后编号：%+v", cites)
	}
	if n := len([]rune(cites[1].Text)); n > maxQuoteChars+1 {
		t.Errorf("引用的原文太长要截断：%d 个字", n)
	}
	if _, cites := Cite("没有找到。", snippets); len(cites) != 0 {
		t.Errorf("没有引用就没有出处：%v", cites)
	}
}

func TestMinutesAndTranscriptLines(t *testing.T) {
	lines := MinutesLines("开头没有标题\n## 结论\n\n- [12:34] **报价**定为 12 万，[13:00] 再议\n---\n## 待办\n- [1:02:03] 下周签合同")
	if len(lines) != 3 || lines[0].Start != -1 || lines[0].Section != "" || lines[1].Start != 754 || lines[2].Start != 3723 || !lines[1].Minutes {
		t.Fatalf("纪要按行拆，取每行第一个时间点，标题、分隔线和空行不单独成行：%+v", lines)
	}
	if lines[1].Text != "报价定为 12 万，[13:00] 再议" || lines[1].Section != "结论" || lines[2].Text != "下周签合同" || lines[2].Section != "待办" {
		t.Errorf("去掉列表符号、加粗和行首的时间点，记下所在的小节：%+v", lines)
	}
	tl := TranscriptLines([]transcript.Segment{{Start: 1, Speaker: 0, Text: "好"}, {Start: 2, Speaker: 1, Text: "行"}}, transcript.Names{0: "张敏"})
	if tl[0].Speaker != "张敏" || tl[1].Speaker != "说话人 2" {
		t.Errorf("说话人用起过的名字：%+v", tl)
	}
}

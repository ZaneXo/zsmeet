package search

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ZaneXo/zsmeet/internal/transcript"
)

func TestTSQuery(t *testing.T) {
	if got := TSQuery([]string{"飞书", "非书"}); got != "('飞书') | ('非书')" {
		t.Errorf("几种写法任何一种命中都算：%s", got)
	}
	if got := TSQuery([]string{"网页录音"}); got != "('网页' & '页录' & '录音')" {
		t.Errorf("粗筛只要完全命中的写法，错一个字的另外查：%s", got)
	}
	if got := TSQuery([]string{"it's"}); got != "('it' & 's')" {
		t.Errorf("英文按单词：%s", got)
	}
	if got := TSQuery([]string{"！？"}); got != "" {
		t.Errorf("全是标点时没有能查的词：%q", got)
	}
}

// 错一个字：第 k 个字错了，剩下的「两字词」和没被它们盖住的单字都要在。没有只剩一个「两字词」的组，免得粗筛出太多会议。
func TestFuzzyTSQuery(t *testing.T) {
	got := FuzzyTSQuery("网页录音")
	want := "('页录' & '录音') | ('录音' & '网') | ('网页' & '音') | ('网页' & '页录')"
	if got != want {
		t.Errorf("错一个字的写法不对：\n得到 %s\n想要 %s", got, want)
	}
	for _, q := range []string{"飞书", "网页 录音", "Go语言框架", "ABCD"} {
		if got := FuzzyTSQuery(q); got != "" {
			t.Errorf("「%s」不该放宽（太短、中间有空格或者不全是中文）：%s", q, got)
		}
	}
}

// hits 把命中写成一行，方便比较。
func hits(r Result) string {
	var b strings.Builder
	for _, h := range r.Hits {
		fuzzy := ""
		if h.Fuzzy {
			fuzzy = "~"
		}
		fmt.Fprintf(&b, "%s %.0f %q %q%s|", h.Source, h.Start, h.Text, h.Matches, fuzzy)
	}
	return b.String()
}

func TestFind(t *testing.T) {
	segs := []transcript.Segment{
		{Start: 1, Speaker: 0, Text: "今天开产品周会。"},
		{Start: 5, Speaker: 1, Text: "非书集成放到下一个版本。"},
		{Start: 9, Speaker: 0, Text: "网页录因的功能做完了。"}, // 识别错了一个字
		{Start: 12, Speaker: 1, Text: "网页录音下周三发布。"},
	}
	minutes := "## 结论\n- [00:12] 网页录音下周三之前发布。\n- 网页录音还要补测试。\n- 其他事项"

	r := Find("周会", minutes, segs, []string{"网页录音"})
	want := `transcript 12 "网页录音下周三发布。" ["网页录音"]|` + // 完全命中的排前面
		`transcript 9 "网页录因的功能做完了。" ["网页录因"]~|` + // 再列错一个字的
		`minutes 12 "网页录音下周三之前发布。" ["网页录音"]|` + // 纪要里带时间点的行能跳过去，开头的时间点不重复显示
		`minutes -1 "网页录音还要补测试。" ["网页录音"]|` // 没有时间点的行时间是 -1
	if got := hits(r); got != want || !r.Matched || r.Fuzzy {
		t.Errorf("命中不对：\n得到 %s %v\n想要 %s", got, r, want)
	}

	// 术语表扩展出来的写法也能命中；不分大小写，高亮用原文
	r = Find("", "", []transcript.Segment{{Start: 3, Text: "Docker 部署。"}, {Start: 5, Text: "非书集成。"}}, []string{"飞书", "非书", "docker"})
	if got := hits(r); got != `transcript 3 "Docker 部署。" ["Docker"]|transcript 5 "非书集成。" ["非书"]|` || !r.Matched {
		t.Errorf("扩展写法和大小写：%s", got)
	}
	// 短词不放宽：错一个字的不算
	if r := Find("", "", []transcript.Segment{{Text: "非书集成。"}}, []string{"飞书"}); len(r.Hits) != 0 || r.Matched || r.Fuzzy {
		t.Errorf("两个字的词错一个字就是别的词了，不该命中：%+v", r)
	}
	// 只在标题里出现也算搜到
	if r := Find("网页录音评审", "", nil, []string{"网页录音"}); !r.Matched || len(r.Hits) != 0 {
		t.Errorf("标题命中：%+v", r)
	}
	// 只有错一个字的：算近似
	if r := Find("", "", segs[2:3], []string{"网页录音"}); r.Matched || !r.Fuzzy || len(r.Hits) != 1 {
		t.Errorf("只有错一个字的命中时是近似：%+v", r)
	}
}

// 中文和英文之间有没有空格都一样：SenseVoice 写「Go语言」，用户和术语表常写「Go 语言」。
func TestFindIgnoresSpacesAroundChinese(t *testing.T) {
	segs := []transcript.Segment{
		{Start: 1, Text: "我们用Go语言写后端。"},
		{Start: 4, Text: "勾语言的并发很好用。"},
		{Start: 8, Text: "API文档也更新了。"},
	}
	r := Find("", "", segs, []string{"Go 语言", "勾语言"})
	if got := hits(r); got != `transcript 1 "我们用Go语言写后端。" ["Go语言"]|transcript 4 "勾语言的并发很好用。" ["勾语言"]|` {
		t.Errorf("「Go 语言」要能找到「Go语言」：%s", got)
	}
	r = Find("", "## 结论\n- [00:26] 后端用 Go 语言", segs, []string{"Go语言"})
	if got := hits(r); got != `transcript 1 "我们用Go语言写后端。" ["Go语言"]|minutes 26 "后端用 Go 语言" ["Go 语言"]|` {
		t.Errorf("「Go语言」也要能找到「Go 语言」：%s", got)
	}
	if r := Find("", "", segs, []string{"API 文档"}); !r.Matched || len(r.Hits) != 1 || r.Hits[0].Start != 8 {
		t.Errorf("「API 文档」：%+v", r)
	}
	// 英文单词之间的空格不能去掉
	if r := Find("", "", []transcript.Segment{{Text: "看一下 api 的文档。"}}, []string{"a pi"}); r.Matched {
		t.Errorf("「a pi」不能等于「api」：%+v", r)
	}
}

// 用空格隔开的几个词：每个词都出现过就算搜到，可以在不同的句子里；同一句里都有的排前面。
func TestFindSeveralWords(t *testing.T) {
	segs := []transcript.Segment{
		{Start: 1, Text: "明天部署。"},
		{Start: 3, Text: "周五上线。"},
		{Start: 5, Text: "部署完周五上线。"},
	}
	r := Find("", "", segs, []string{"部署 上线"})
	if got := hits(r); got != `transcript 5 "部署完周五上线。" ["部署" "上线"]|` || !r.Matched {
		t.Errorf("两个词都在的那句排前面，只有一个词的不列：%s", got)
	}
	r = Find("", "", segs[:2], []string{"部署 上线"})
	if got := hits(r); got != `transcript 1 "明天部署。" ["部署"]|transcript 3 "周五上线。" ["上线"]|` || !r.Matched {
		t.Errorf("两个词在不同的句子里也算搜到：%s", got)
	}
	if r := Find("", "", segs[:1], []string{"部署 上线"}); r.Matched {
		t.Errorf("少一个词就不算搜到：%+v", r)
	}
}

// 英文要整个单词对上。
func TestFindWholeWords(t *testing.T) {
	segs := []transcript.Segment{
		{Start: 1, Text: "我发了一封 email。"},
		{Start: 2, Text: "2025年的预算。"},
		{Start: 3, Text: "用AI写纪要，大概20个人用。"},
	}
	if got := hits(Find("", "", segs, []string{"AI"})); got != `transcript 3 "用AI写纪要，大概20个人用。" ["AI"]|` {
		t.Errorf("搜「AI」不算 email：%s", got)
	}
	if got := hits(Find("", "", segs, []string{"20"})); got != `transcript 3 "用AI写纪要，大概20个人用。" ["20"]|` {
		t.Errorf("搜「20」不算 2025：%s", got)
	}
}

// 错一个字只算中文字：标点、英文不算听错的字。
func TestFuzzyOnlyChinese(t *testing.T) {
	segs := []transcript.Segment{
		{Start: 1, Text: "在数据中，我们发现了问题。"},
		{Start: 2, Text: "数据中x的东西。"},
		{Start: 3, Text: "数据中的异常值要去掉。"},
	}
	r := Find("", "", segs, []string{"数据中心"})
	if got := hits(r); got != `transcript 3 "数据中的异常值要去掉。" ["数据中的"]~|` || !r.Fuzzy || r.Matched {
		t.Errorf("只有中文字不一样的才算错一个字：%s", got)
	}
}

// 纪要里一行有好几个时间点时，跳到命中的词前面最近的那个。
func TestMinutesHitUsesNearestClock(t *testing.T) {
	minutes := "- [00:26] 后端用 Go 语言；[00:33] 识别模型用处理器就可以。"
	r := Find("", minutes, nil, []string{"识别模型"})
	if len(r.Hits) != 1 || r.Hits[0].Start != 33 || r.Hits[0].Text != "后端用 Go 语言；[00:33] 识别模型用处理器就可以。" {
		t.Errorf("应该跳到 [00:33]，正文去掉开头的时间点：%+v", r.Hits)
	}
	r = Find("", minutes, nil, []string{"Go 语言"})
	if len(r.Hits) != 1 || r.Hits[0].Start != 26 {
		t.Errorf("命中的词在第一个时间点后面，就跳到第一个：%+v", r.Hits)
	}
}

// 标题里中文之间有空格时，索引里也有跨过空格的词，搜连在一起的写法能粗筛到。
func TestDocTermsAcrossSpaces(t *testing.T) {
	doc := DocTerms("产品 周会")
	for _, term := range QueryTerms("产品周会") {
		if !slices.Contains(doc, term) {
			t.Errorf("「产品周会」的词 %q 不在「产品 周会」的索引里：%q", term, doc)
		}
	}
	if !Find("产品 周会", "", nil, []string{"产品周会"}).Matched {
		t.Error("「产品周会」应该能搜到标题「产品 周会」")
	}
	// 换行隔开的不连：转写一句一行
	if slices.Contains(DocTerms("网页\n录音"), "页录") {
		t.Error("跨行的两个字不该连成词")
	}
}

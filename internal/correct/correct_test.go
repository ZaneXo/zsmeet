package correct

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ZaneXo/zsmeet/internal/transcript"
)

func TestParseHotwords(t *testing.T) {
	got := ParseHotwords(" 飞书，zsmeet, 张敏、SenseVoice;Claude Code；\n飞书, ZSMEET ,, ")
	want := []string{"飞书", "zsmeet", "张敏", "SenseVoice", "Claude Code"}
	if !slices.Equal(got, want) {
		t.Fatalf("ParseHotwords = %q, want %q", got, want)
	}
}

func TestTokens(t *testing.T) {
	got := Tokens("我们用 SenseVoice-Small，识别率 95%！ＡＢＣ don't")
	want := []string{"我", "们", "用", "sensevoice", "small", "识", "别", "率", "95", "abc", "don't"}
	if !slices.Equal(got, want) {
		t.Fatalf("Tokens = %q, want %q", got, want)
	}
}

func TestDistance(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"飞书", "非书", 1},
		{"今天开会", "今天不开会", 1},
		{"这是米特很好", "zsmeet很好", 4},
	} {
		if got := Distance(Tokens(c.a), Tokens(c.b)); got != c.want {
			t.Errorf("Distance(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func segs(texts ...string) []transcript.Segment {
	out := make([]transcript.Segment, len(texts))
	for i, s := range texts {
		out[i] = transcript.Segment{Start: float64(i), End: float64(i) + 1, Speaker: i % 2, Text: s}
	}
	return out
}

func texts(segs []transcript.Segment) []string {
	out := make([]string, len(segs))
	for i, s := range segs {
		out[i] = s.Text
	}
	return out
}

func TestHomophones(t *testing.T) {
	in := segs(
		"我们把纪要发到非书群里。",     // 非书 → 飞书
		"章敏你来说一下进度。",       // 章敏 → 张敏（声调不同也算）
		"飞书已经配好了。",         // 已经是热词，不动
		"这个不用改。",           // 没有对得上的
		"长沙的张敏和长敏都在。",      // 「长」是多音字（zhang），长敏 → 张敏；长沙的「长沙」对不上「张敏」
		"智能体平台叫知能体平台。",     // 最长的热词优先：智能体平台 已经对，知能体平台 → 智能体平台
		"Feishu 和 zsmeet。", // 没有汉字，不动
	)
	n := Homophones(in, []string{"飞书", "张敏", "智能体平台", "智能", "zsmeet", "敏"})
	want := []string{
		"我们把纪要发到飞书群里。",
		"张敏你来说一下进度。",
		"飞书已经配好了。",
		"这个不用改。",
		"长沙的张敏和张敏都在。",
		"智能体平台叫智能体平台。",
		"Feishu 和 zsmeet。",
	}
	if got := texts(in); !slices.Equal(got, want) {
		t.Fatalf("Homophones:\n got %q\nwant %q", got, want)
	}
	if n != 4 {
		t.Errorf("改了 %d 句，want 4", n)
	}
	if in[0].Orig != "我们把纪要发到非书群里。" || in[2].Orig != "" {
		t.Errorf("Orig = %q, %q", in[0].Orig, in[2].Orig)
	}
	// 再做一遍不会再改，原文还是最早识别出的
	if n := Homophones(in, []string{"飞书"}); n != 0 || in[0].Orig != "我们把纪要发到非书群里。" {
		t.Errorf("第二遍改了 %d 句，Orig = %q", n, in[0].Orig)
	}
}

func TestAcceptable(t *testing.T) {
	for _, c := range []struct {
		orig, fixed string
		want        bool
	}{
		{"我们把纪要发到非书群里。", "我们把纪要发到飞书群里。", true},
		{"这个是米特的新版本今天发布", "这个是 zsmeet 的新版本今天发布", true},
		{"嗯", "嗯，对", true}, // 短句至少允许改 3 个
		{"我们把纪要发到非书群里。", "", false},
		{"我们把纪要发到非书群里。", "我们把纪要\n发到飞书群里。", false},
		// 润色、改写
		{"然后呢就是说这个事情我们下周再看一下吧", "这件事下周再讨论", false},
		{"今天主要讨论三个问题", "今天主要讨论三个问题，第一个是上线时间，第二个是预算，第三个是人员安排", false},
	} {
		if got := acceptable(c.orig, c.fixed); got != c.want {
			t.Errorf("acceptable(%q, %q) = %v, want %v", c.orig, c.fixed, got, c.want)
		}
	}
}

func TestSplit(t *testing.T) {
	in := segs("第一句话", "", "第二句话", "第三句话")
	names := transcript.Names{0: "张敏"}
	got := split(in, names, 30, false)
	if len(got) != 2 {
		t.Fatalf("分成了 %d 批：%+v", len(got), got)
	}
	if got[0].from != 0 || got[0].to != 3 || got[0].text != "[0] 张敏：第一句话\n[2] 张敏：第二句话\n" {
		t.Errorf("第一批 = %+v", got[0])
	}
	if got[1].from != 3 || got[1].to != 4 || got[1].text != "[3] 说话人 2：第三句话\n" {
		t.Errorf("第二批 = %+v", got[1])
	}
}

func TestSplitWithPinyin(t *testing.T) {
	got := split(segs("用飞书开会。", "OK"), transcript.Names{}, 100, true)
	want := "[0] 说话人 1：用飞书开会。\n    拼音：yong fei shu kai hui\n[1] 说话人 2：OK\n"
	if len(got) != 1 || got[0].text != want {
		t.Errorf("split = %+v", got)
	}
}

func TestPinyinOf(t *testing.T) {
	for in, want := range map[string]string{
		"今天用飞书开会。":   "jin tian yong fei shu kai hui",
		"上线 v2.0 版本": "shang xian v2 0 ban ben",
		"OK, sure":   "",
		"":           "",
	} {
		if got := pinyinOf(in); got != want {
			t.Errorf("pinyinOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// DeepSeek 有时把编号写成字符串
func TestParseFixesStringIDs(t *testing.T) {
	got, err := parseFixes(`{"fixes": [{"id": "2", "text": "飞书"}, {"id": 3, "text": "星璨"}]}`, 0, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[2] != "飞书" || got[3] != "星璨" {
		t.Errorf("parseFixes = %v", got)
	}
}

func TestParseFixes(t *testing.T) {
	out := "好的：\n```json\n{\"fixes\": [{\"id\": 3, \"text\": \" 飞书 \"}, {\"id\": 9, \"text\": \"越界\"}]}\n```"
	got, err := parseFixes(out, 0, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[3] != "飞书" {
		t.Errorf("parseFixes = %v", got)
	}
	if _, err := parseFixes("没有要改的", 0, 5); err == nil {
		t.Error("没有 JSON 时应该报错")
	}
	if _, err := parseFixes(`{"fixes": [{"id": "x"}]}`, 0, 5); err == nil {
		t.Error("JSON 写坏了应该报错")
	}
}

func TestLLM(t *testing.T) {
	in := segs(
		"我们把纪要发到非书群里。",
		"然后呢就是说这个事情我们下周再看一下吧",
		"章敏你来说一下进度。",
		"这句没错。",
	)
	var calls atomic.Int32
	chat := func(_ context.Context, system, user string) (string, error) {
		calls.Add(1)
		if !strings.Contains(system, "飞书、张敏") || !strings.Contains(system, "会议标题：产品周会") || !strings.Contains(system, "参会的人：李雷") {
			t.Errorf("系统提示里缺会议信息：%s", system)
		}
		answers := map[int]string{
			0: "我们把纪要发到飞书群里。",
			1: "这件事下周再讨论",         // 改写，不采用
			2: "说话人 1：张敏你来说一下进度。", // 把说话人也抄回来了
			3: "这句没错。",
		}
		var fixes []string
		for id, text := range answers {
			if strings.Contains(user, fmt.Sprintf("[%d] ", id)) {
				fixes = append(fixes, fmt.Sprintf(`{"id": %d, "text": %q}`, id, text))
			}
		}
		return fmt.Sprintf(`{"fixes": [%s]}`, strings.Join(fixes, ",")), nil
	}
	var done []int
	stats, err := LLM(context.Background(), chat, in, Input{
		Title: "产品周会", Hotwords: []string{"飞书", "张敏"}, Speakers: transcript.Names{1: "李雷"}, BatchChars: 40,
	}, func(d, total int) { done = append(done, d) })
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"我们把纪要发到飞书群里。", "然后呢就是说这个事情我们下周再看一下吧", "张敏你来说一下进度。", "这句没错。"}
	if got := texts(in); !slices.Equal(got, want) {
		t.Fatalf("LLM:\n got %q\nwant %q", got, want)
	}
	if stats.Changed != 2 || stats.Rejected != 1 || stats.Failed != 0 || stats.Batches != int(calls.Load()) || stats.Batches < 2 {
		t.Errorf("stats = %+v, calls = %d", stats, calls.Load())
	}
	if in[0].Orig != "我们把纪要发到非书群里。" || in[1].Orig != "" || in[3].Orig != "" {
		t.Errorf("Orig 不对：%+v", in)
	}
	if len(done) != stats.Batches || done[len(done)-1] != stats.Batches {
		t.Errorf("progress = %v", done)
	}
}

func TestLLMBatchFails(t *testing.T) {
	in := segs("非书一", "非书二", "非书三")
	boom := errors.New("429")
	chat := func(_ context.Context, _, user string) (string, error) {
		if strings.Contains(user, "[1] ") {
			return "", boom
		}
		if strings.Contains(user, "[2] ") {
			return "不是 JSON", nil
		}
		return `{"fixes": [{"id": 0, "text": "飞书一"}]}`, nil
	}
	stats, err := LLM(context.Background(), chat, in, Input{BatchChars: 12, Parallel: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Batches != 3 || stats.Failed != 2 || stats.Changed != 1 || stats.Err == nil {
		t.Errorf("stats = %+v", stats)
	}
	if got := texts(in); !slices.Equal(got, []string{"飞书一", "非书二", "非书三"}) {
		t.Errorf("texts = %q", got)
	}
}

func TestLLMCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	chat := func(ctx context.Context, _, _ string) (string, error) {
		cancel()
		return "", ctx.Err()
	}
	if _, err := LLM(ctx, chat, segs("一", "二", "三"), Input{BatchChars: 8, Parallel: 1}, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

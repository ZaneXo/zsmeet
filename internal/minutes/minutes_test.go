package minutes

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ZaneXo/zsmeet/internal/llm"
	"github.com/ZaneXo/zsmeet/internal/transcript"
)

func input(n int) Input {
	segs := make([]transcript.Segment, n)
	for i := range segs {
		segs[i] = transcript.Segment{Start: float64(i * 10), End: float64(i*10 + 8), Speaker: i % 2, Text: strings.Repeat("讨论一期范围。", 20)}
	}
	tpl, _ := Find("general")
	return Input{Title: "产品周会", Date: time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC), Hotwords: "River, sqlc", Template: tpl, Segments: segs}
}

func TestGenerateSinglePass(t *testing.T) {
	var systems, users []string
	chat := func(_ context.Context, system, user string) (string, error) {
		systems, users = append(systems, system), append(users, user)
		return "```markdown\n## 摘要\n定了一期范围。\n```", nil
	}
	var calls [][2]int
	out, err := Generate(t.Context(), chat, input(3), func(done, total int) { calls = append(calls, [2]int{done, total}) })
	if err != nil {
		t.Fatal(err)
	}
	if out.Content != "## 摘要\n定了一期范围。" || len(out.Terms) != 0 {
		t.Errorf("没有去掉外层代码块：%+v", out)
	}
	if len(users) != 1 {
		t.Fatalf("短会议应该只调用一次大模型，实际 %d 次", len(users))
	}
	for _, want := range []string{"产品周会", "2026-11-09 10:00", "[00:10] 说话人 2：", "待办"} {
		if !strings.Contains(users[0], want) {
			t.Errorf("提示词里缺少 %q", want)
		}
	}
	if !strings.Contains(systems[0], "River, sqlc") {
		t.Error("热词没有进提示词")
	}
	if len(calls) != 1 || calls[0] != [2]int{1, 1} {
		t.Errorf("进度回调不对：%v", calls)
	}
}

func TestGenerateLongMeetingSplits(t *testing.T) {
	n := 0
	chat := func(_ context.Context, system, user string) (string, error) {
		n++
		if strings.Contains(system, "长会议转写的第") {
			return "- [00:10] 要点", nil
		}
		if !strings.Contains(user, "### 第 1 部分") || strings.Contains(user, "会议转写：") {
			t.Error("汇总时应该只给分段要点，不再给原始转写")
		}
		return "## 摘要\n汇总。", nil
	}
	// 每句约 150 字，400 句约 6 万字，应该分成 4 段再汇总
	out, err := Generate(t.Context(), chat, input(400), nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("应该调用 5 次（4 段 + 汇总），实际 %d 次", n)
	}
	if out.Content != "## 摘要\n汇总。" {
		t.Errorf("输出不对：%+v", out)
	}
}

// 每段放多少字跟着 Input.MaxChars 走：同一场会，上下文 32K（每段 2 万字）时一次写完，8K（每段 3000 字）时要分段
func TestGenerateSplitsByMaxChars(t *testing.T) {
	calls := func(maxChars int) (n int, longest int) {
		chat := func(_ context.Context, system, user string) (string, error) {
			n++
			if strings.Contains(system, "长会议转写的第") {
				longest = max(longest, len([]rune(user)))
			}
			return "## 摘要\n定了一期范围。", nil
		}
		in := input(60) // 每句约 150 字，60 句约 9000 字
		in.MaxChars = maxChars
		if _, err := Generate(t.Context(), chat, in, nil); err != nil {
			t.Fatal(err)
		}
		return n, longest
	}
	if n, _ := calls(0); n != 1 {
		t.Errorf("没给 MaxChars 时按每段 2 万字，9000 字的会一次写完，实际调用 %d 次", n)
	}
	if n, _ := calls(20_000); n != 1 {
		t.Errorf("每段 2 万字时一次写完，实际调用 %d 次", n)
	}
	n, longest := calls(3_000)
	if n != 5 {
		t.Errorf("每段 3000 字时应该分成 4 段再汇总，共 5 次，实际 %d 次", n)
	}
	if longest == 0 || longest > 3_000 {
		t.Errorf("每段的转写不能超过 3000 字，最长的一段 %d 字", longest)
	}
}

// 没给 MaxChars 时和没设置上下文长度（按 32K）时算出来的一样
func TestDefaultMaxCharsMatchesDefaultContext(t *testing.T) {
	if got := llm.TranscriptChars(0); got != defaultMaxChars {
		t.Errorf("默认每段 %d 字，按默认上下文算出来是 %d 字", defaultMaxChars, got)
	}
}

func TestGeneratePropagatesError(t *testing.T) {
	boom := errors.New("大模型返回 401")
	_, err := Generate(t.Context(), func(context.Context, string, string) (string, error) { return "", boom }, input(1), nil)
	if !errors.Is(err, boom) {
		t.Fatalf("应该原样返回大模型的错误，得到 %v", err)
	}
}

func TestUnwrapFenceKeepsInnerCodeBlocks(t *testing.T) {
	// 正文里本来就有代码块时不能动
	s := "## 结论\n```go\nfmt.Println()\n```\n\n```sh\nmake\n```"
	if got := unwrapFence(s); got != s {
		t.Errorf("误删了正文里的代码块：%q", got)
	}
}

func TestGenerateWithMarks(t *testing.T) {
	var system, user string
	chat := func(_ context.Context, s, u string) (string, error) {
		system, user = s, u
		return "## 摘要\n好", nil
	}
	in := input(3) // 三句话，分别从 0、10、20 秒开始
	in.Marks = []float64{12.5, 95}
	if _, err := Generate(t.Context(), chat, in, nil); err != nil {
		t.Fatal(err)
	}
	// 12.5 秒的标记标在 10 秒开始的那一段末尾；95 秒的标在最后一段末尾
	for _, want := range []string{"讨论一期范围。【★ 00:12 标记为重点】\n[00:20] 说话人 1：", "讨论一期范围。【★ 01:35 标记为重点】"} {
		if !strings.Contains(user, want) {
			t.Errorf("标记的重点没有标在对应那一段的末尾，缺少 %q：\n%s", want, user)
		}
	}
	if !strings.Contains(system, "6. 转写里带「【★ 时间 标记为重点】」") || !strings.Contains(system, "7. 用 Markdown 输出") {
		t.Errorf("有标记时要多一条要求，编号接着排：\n%s", system)
	}

	in.Marks = nil
	if _, err := Generate(t.Context(), chat, in, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(user, "标记为重点") || strings.Contains(system, "标记") || !strings.Contains(system, "6. 用 Markdown 输出") {
		t.Errorf("没有标记时不该提到标记：\n%s\n%s", system, user)
	}
}

// 长会议分段处理时，最后汇总那一次看不到转写里的标记，要把标记的时间点单独列给它。
func TestGenerateLongMeetingWithMarks(t *testing.T) {
	var partWithMark bool
	var finalSystem, finalUser string
	chat := func(_ context.Context, system, user string) (string, error) {
		if strings.Contains(system, "长会议转写的第") {
			if strings.Contains(user, "【★ 00:15 标记为重点】") {
				partWithMark = true
			}
			return "- 要点", nil
		}
		finalSystem, finalUser = system, user
		return "## 摘要\n好", nil
	}
	in := input(400)
	in.Marks = []float64{15, 3000}
	if _, err := Generate(t.Context(), chat, in, nil); err != nil {
		t.Fatal(err)
	}
	if !partWithMark {
		t.Error("分段提取要点时，转写里要带着标记")
	}
	if !strings.Contains(finalUser, "开会的人特意标记为重点的时间点：[00:15]、[50:00]") {
		t.Errorf("汇总时要列出标记的时间点：\n%s", finalUser)
	}
	if !strings.Contains(finalSystem, "6. 材料最后列出了") || !strings.Contains(finalSystem, "7. 用 Markdown 输出") || strings.Contains(finalSystem, "转写里带") {
		t.Errorf("汇总时的要求应该指向列出的时间点，而不是转写里的标记行：\n%s", finalSystem)
	}
}

// 大模型在纪要末尾附的术语表要取出来单独存，不能留在纪要正文里。
func TestTermsAreSplitOff(t *testing.T) {
	var system string
	chat := func(_ context.Context, s, _ string) (string, error) {
		system = s
		return "## 摘要\n飞书集成放到下一版。\n\n```terms\n" +
			`{"terms": [{"term": "飞书", "variants": ["非书", "飞书", " "]}, {"term": "王悦", "variants": []}, {"term": "飞书"}, {"term": ""}]}` +
			"\n```\n", nil
	}
	out, err := Generate(t.Context(), chat, input(3), nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Content != "## 摘要\n飞书集成放到下一版。" {
		t.Errorf("术语表要从纪要正文里去掉：%q", out.Content)
	}
	got, _ := json.Marshal(out.Terms)
	if string(got) != `[{"term":"飞书","variants":["非书"]},{"term":"王悦","variants":[]}]` {
		t.Errorf("术语表要去重、去掉空的和与正确写法相同的听错写法：%s", got)
	}
	if !strings.Contains(system, "terms 代码块") {
		t.Error("要让大模型输出术语表")
	}

	// 写坏了的术语表（不是 JSON、被截断）不影响纪要本身
	for _, tail := range []string{"```terms\n不是 JSON\n```", "```terms\n{\"terms\": [{\"term\": \"飞"} {
		chat := func(context.Context, string, string) (string, error) { return "## 摘要\n好。\n\n" + tail, nil }
		out, err := Generate(t.Context(), chat, input(3), nil)
		if err != nil || out.Content != "## 摘要\n好。" || len(out.Terms) != 0 || out.TermsFound {
			t.Errorf("坏掉的术语表要丢掉，纪要照常，并且说明没拿到术语表：%+v %v", out, err)
		}
	}
	// 明确说没有专有名词也算拿到了术语表（调用方可以放心地清空旧的）
	chat = func(context.Context, string, string) (string, error) {
		return "## 摘要\n好。\n\n```terms\n{\"terms\": []}\n```", nil
	}
	if out, err := Generate(t.Context(), chat, input(3), nil); err != nil || !out.TermsFound || len(out.Terms) != 0 {
		t.Errorf("空的术语表：%+v %v", out, err)
	}
}

// 只有一个字的写法、没有字的写法不进术语表：「爱」会让搜「AI」命中几乎所有会议。
func TestTermsDropUselessSpellings(t *testing.T) {
	chat := func(context.Context, string, string) (string, error) {
		return "## 摘要\n好。\n\n```terms\n" +
			`{"terms": [{"term": "AI", "variants": ["爱", "-", "A I", "埃爱"]}, {"term": "勾", "variants": ["沟"]}, {"term": "Go 语言", "variants": ["go语言", "勾语言", "用Go语言"]}]}` +
			"\n```", nil
	}
	out, err := Generate(t.Context(), chat, input(3), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(out.Terms)
	if string(got) != `[{"term":"AI","variants":["埃爱"]},{"term":"Go 语言","variants":["勾语言"]}]` {
		t.Errorf("一个字的、没有字的、只差空格大小写的、包含正确写法的写法都不要：%s", got)
	}
}

// 长会议分段处理时，术语表从每一部分要（只有那时看得到转写里的听错写法），合在一起。
func TestLongMeetingCollectsTermsFromParts(t *testing.T) {
	part := 0
	var finalSystem, finalUser string
	chat := func(_ context.Context, system, user string) (string, error) {
		if strings.Contains(system, "长会议转写的第") {
			if !strings.Contains(system, "terms 代码块") {
				t.Error("每一部分都要列术语表")
			}
			part++
			switch part {
			case 1:
				return "- 要点一\n\n```terms\n{\"terms\": [{\"term\": \"飞书\", \"variants\": [\"非书\"]}]}\n```", nil
			case 2:
				return "- 要点二\n\n```terms\n{\"terms\": [{\"term\": \"飞书\", \"variants\": [\"飞叔\", \"非书\"]}, {\"term\": \"Go 语言\", \"variants\": [\"勾语言\"]}]}\n```", nil
			}
			return "- 要点", nil
		}
		finalSystem, finalUser = system, user
		return "## 摘要\n汇总。", nil
	}
	out, err := Generate(t.Context(), chat, input(400), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(out.Terms)
	if string(got) != `[{"term":"飞书","variants":["非书","飞叔"]},{"term":"Go 语言","variants":["勾语言"]}]` || !out.TermsFound {
		t.Errorf("各部分的术语表要合在一起：%s %v", got, out.TermsFound)
	}
	if strings.Contains(finalSystem, "terms 代码块") || strings.Contains(finalUser, "```terms") || !strings.Contains(finalUser, "- 要点二") {
		t.Errorf("汇总时不再要术语表，各部分的术语表也不该混进要点里：\n%s\n%s", finalSystem, finalUser)
	}
}

// 整篇包在 ```markdown 里、结尾的 ``` 被 terms 代码块吃掉了，也要把开头那行去掉。
func TestUnwrapFenceWithoutClosingLine(t *testing.T) {
	chat := func(context.Context, string, string) (string, error) {
		return "```markdown\n## 摘要\n内容。\n\n```terms\n{\"terms\": []}\n```", nil
	}
	out, err := Generate(t.Context(), chat, input(3), nil)
	if err != nil || out.Content != "## 摘要\n内容。" {
		t.Errorf("开头的 ```markdown 要去掉：%q %v", out.Content, err)
	}
}

func TestRenameSpeakers(t *testing.T) {
	none := transcript.Names{}
	content := "- 说话人 1 提出一期只做网页端；说话人 2（王悦）同意。说话人 12 没来。说话人1也提到预算。"
	got := RenameSpeakers(content, none, map[int][2]string{0: {"", "张敏"}, 1: {"", "王悦"}})
	want := "- 张敏 提出一期只做网页端；王悦同意。说话人 12 没来。张敏也提到预算。"
	if got != want {
		t.Errorf("第一次起名字：\n得到 %s\n想要 %s", got, want)
	}
	// 再改一次名字：纪要里的旧名字也换掉；只有一个字的旧名字不换
	if got := RenameSpeakers("张敏说好。", transcript.Names{0: "张敏"}, map[int][2]string{0: {"张敏", "张敏敏"}}); got != "张敏敏说好。" {
		t.Errorf("旧名字要换成新名字：%s", got)
	}
	if got := RenameSpeakers("王悦说好。", transcript.Names{1: "王"}, map[int][2]string{1: {"王", "王悦悦"}}); got != "王悦说好。" {
		t.Errorf("一个字的旧名字不换，免得误伤正文：%s", got)
	}
	// 去掉名字：纪要不动
	if got := RenameSpeakers("张敏说好。", transcript.Names{0: "张敏"}, map[int][2]string{0: {"张敏", ""}}); got != "张敏说好。" {
		t.Errorf("去掉名字时纪要不动：%s", got)
	}
}

// 互换、重名、英文名：不能把两个人改成一个人，也不能改到别的词里去。
func TestRenameSpeakersAmbiguous(t *testing.T) {
	before := transcript.Names{0: "张敏", 1: "王悦"}
	content := "张敏提出一期只做网页端，王悦同意。"
	// 一次互换两个人：一次换完，不串
	if got := RenameSpeakers(content, before, map[int][2]string{0: {"张敏", "王悦"}, 1: {"王悦", "张敏"}}); got != "王悦提出一期只做网页端，张敏同意。" {
		t.Errorf("一次互换两个人的名字：%s", got)
	}
	// 一个一个改：第一步改完和别人重名了，不动正文，留给人手动改
	if got := RenameSpeakers(content, before, map[int][2]string{0: {"张敏", "王悦"}}); got != content {
		t.Errorf("新名字和别人重名时不改正文：%s", got)
	}
	// 同一个人被分成了两个说话人、都叫张敏：改其中一个时不知道正文里的张敏是哪个，不改
	if got := RenameSpeakers(content, transcript.Names{0: "张敏", 2: "张敏"}, map[int][2]string{2: {"张敏", "李雷"}}); got != content {
		t.Errorf("旧名字不止一个人用时不改正文：%s", got)
	}
	// 「说话人 N」换成的名字不会再被当成旧名字换一遍：0 号原来叫错成张敏，一次改成「0 是王悦、1 是张敏」
	if got := RenameSpeakers("张敏说A。说话人 2说B。", transcript.Names{0: "张敏"}, map[int][2]string{0: {"张敏", "王悦"}, 1: {"", "张敏"}}); got != "王悦说A。张敏说B。" {
		t.Errorf("一次改完，不能换两遍：%s", got)
	}
	if got := RenameSpeakers("张敏说A。", transcript.Names{0: "张敏"}, map[int][2]string{0: {"张敏", "张敏华"}}); got != "张敏华说A。" {
		t.Errorf("新名字里包含旧名字：%s", got)
	}
	// 英文名要整个单词对上
	if got := RenameSpeakers("Li 说 Linux 要升级，Li。", transcript.Names{0: "Li"}, map[int][2]string{0: {"Li", "Li Wei"}}); got != "Li Wei 说 Linux 要升级，Li Wei。" {
		t.Errorf("英文名：%s", got)
	}
}

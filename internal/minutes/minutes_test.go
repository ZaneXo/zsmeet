package minutes

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"zsmeet/internal/transcript"
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
	if out != "## 摘要\n定了一期范围。" {
		t.Errorf("没有去掉外层代码块：%q", out)
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
	if out != "## 摘要\n汇总。" {
		t.Errorf("输出不对：%q", out)
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

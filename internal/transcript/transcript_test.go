package transcript

import "testing"

func TestAssignSpeakers(t *testing.T) {
	segs := []Segment{
		{Start: 0, End: 4, Text: "甲先说。"},
		{Start: 5, End: 9, Text: "乙接着说。"},
		{Start: 9.2, End: 10, Text: "乙补充。"},
		{Start: 30, End: 31, Text: "没人认领。"},
	}
	// 说话人区分给的编号是 7 和 3，结果应该按开口顺序变成 0 和 1
	turns := []Turn{
		{Start: 0, End: 4.5, Speaker: 7},
		{Start: 4.5, End: 9, Speaker: 3},
		{Start: 9.6, End: 12, Speaker: 3},
	}
	AssignSpeakers(segs, turns)
	want := []int{0, 1, 1, -1}
	for i, w := range want {
		if segs[i].Speaker != w {
			t.Errorf("第 %d 句的说话人是 %d，应该是 %d", i, segs[i].Speaker, w)
		}
	}
}

func TestAssignSpeakersNearest(t *testing.T) {
	// 这句和任何时间段都不重叠，但离说话人 2 只有 0.3 秒
	segs := []Segment{{Start: 10, End: 11}}
	AssignSpeakers(segs, []Turn{{Start: 0, End: 5, Speaker: 1}, {Start: 11.3, End: 15, Speaker: 2}})
	if segs[0].Speaker != 0 {
		t.Fatalf("应该归给最近的说话人（重新编号后是 0），得到 %d", segs[0].Speaker)
	}
}

func TestMerge(t *testing.T) {
	segs := []Segment{
		{Start: 0, End: 2, Speaker: 0, Text: "第一句。"},
		{Start: 2.3, End: 4, Speaker: 0, Text: "第二句。"},
		{Start: 4.2, End: 6, Speaker: 1, Text: "换人了。"},
		{Start: 20, End: 22, Speaker: 1, Text: "停了很久。"},
	}
	got := Merge(segs, 1.0, 100, nil)
	if len(got) != 3 {
		t.Fatalf("应该并成 3 段，得到 %d 段：%+v", len(got), got)
	}
	if got[0].Text != "第一句。第二句。" || got[0].End != 4 {
		t.Errorf("前两句没并对：%+v", got[0])
	}
	if got := Merge(segs, 1.0, 5, nil); len(got) != 4 {
		t.Errorf("超过字数上限时不该合并，得到 %d 段", len(got))
	}
}

func TestClock(t *testing.T) {
	cases := map[float64]string{0: "00:00", 75.9: "01:15", 3725: "1:02:05"}
	for sec, want := range cases {
		if got := Clock(sec); got != want {
			t.Errorf("Clock(%v) = %q, want %q", sec, got, want)
		}
	}
}

// 标记重点的时间点不被合并跨过：标记时正在说的那句是一段的结尾。
func TestMergeBreaksAtMarks(t *testing.T) {
	segs := []Segment{
		{Start: 0, End: 3, Text: "第一件事。"},
		{Start: 3.2, End: 8, Text: "第二件事，"},
		{Start: 8.1, End: 12, Text: "王悦负责。"},
		{Start: 12.3, End: 16, Text: "第三件事。"},
	}
	got := Merge(segs, 1.0, 100, []float64{9.5})
	if len(got) != 2 || got[0].Text != "第一件事。第二件事，王悦负责。" || got[0].End != 12 || got[1].Text != "第三件事。" {
		t.Errorf("应该在 9.5 秒正在说的那句后面断开：%+v", got)
	}
}

func TestMergeJoinsEnglishWithSpace(t *testing.T) {
	segs := []Segment{{Start: 0, End: 1, Text: "Hello."}, {Start: 1.1, End: 2, Text: "How are you?"}, {Start: 2.1, End: 3, Text: "好的。"}}
	if got := Merge(segs, 1, 100, nil); len(got) != 1 || got[0].Text != "Hello. How are you?好的。" {
		t.Errorf("英文句子之间要有空格，中文直接接上：%+v", got)
	}
}

// 重新识别以后编号变了：名字跟着人走；对不上的不要。
func TestRemapNames(t *testing.T) {
	old := []Segment{
		{Start: 0, End: 10, Speaker: 0},  // 张敏
		{Start: 10, End: 20, Speaker: 1}, // 王悦
		{Start: 20, End: 30, Speaker: 0},
	}
	// 新的转写里先说话的是王悦，编号换了；还多出一个新的人
	segs := []Segment{
		{Start: 0, End: 2, Speaker: 1},
		{Start: 2, End: 10, Speaker: 0},
		{Start: 10, End: 20, Speaker: 1},
		{Start: 20, End: 28, Speaker: 0},
		{Start: 28, End: 30, Speaker: 2},
	}
	// 新的 0 和旧的 0 重叠 16 秒，新的 1 和旧的 1 重叠 10 秒（和旧的 0 只有 2 秒）
	got := RemapNames(old, segs, Names{0: "张敏", 1: "王悦"})
	if len(got) != 2 || got[0] != "张敏" || got[1] != "王悦" {
		t.Errorf("名字要跟着人走：%v", got)
	}
	swapped := []Segment{{Start: 0, End: 10, Speaker: 1}, {Start: 10, End: 20, Speaker: 0}, {Start: 20, End: 30, Speaker: 1}}
	if got := RemapNames(old, swapped, Names{0: "张敏", 1: "王悦"}); got[1] != "张敏" || got[0] != "王悦" {
		t.Errorf("编号互换了：%v", got)
	}
	// 两个旧的人和同一个新的人重叠一样多：对不上，不要
	merged := []Segment{{Start: 5, End: 15, Speaker: 0}}
	if got := RemapNames(old, merged, Names{0: "张敏", 1: "王悦"}); len(got) != 0 {
		t.Errorf("对不上的名字不要：%v", got)
	}
}

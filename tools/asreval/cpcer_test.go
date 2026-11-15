package main

import (
	"testing"

	"github.com/ZaneXo/zsmeet/internal/transcript"
)

func TestCPScorePermutesAndPenalizesMergedSpeakers(t *testing.T) {
	ref := []transcript.Segment{
		{Start: 0, End: 2, Speaker: 0, Text: "今天先过排期。"},
		{Start: 3, End: 5, Speaker: 1, Text: "同意下周上线。"},
	}
	// 说话人区分给出的编号反过来也没关系（9 对 0，4 对 1）
	turns := []transcript.Turn{
		{Start: 0, End: 2.2, Speaker: 9},
		{Start: 2.8, End: 5.2, Speaker: 4},
	}

	// 按句切好、时间对齐时 cpCER 为 0
	aligned := []transcript.Segment{
		{Start: 0.1, End: 1.9, Speaker: -1, Text: "今天先过排期。"},
		{Start: 3.1, End: 4.9, Speaker: -1, Text: "同意下周上线。"},
	}
	if errs, words := cpScore(ref, aligned, turns); errs != 0 || words != 12 {
		t.Fatalf("时间对齐的两句 cpScore = %d/%d, want 0/12", errs, words)
	}

	// 整段 5 秒合成一句时，两个人的话全算到一个人名下，另一人全漏（删 6 + 插 6 = 12 个错字）
	merged := []transcript.Segment{
		{Start: 0.1, End: 4.9, Speaker: -1, Text: "今天先过排期。同意下周上线。"},
	}
	if errs, words := cpScore(ref, merged, turns); errs != 12 || words != 12 {
		t.Fatalf("未对齐合并成一句时 cpScore = %d/%d, want 12/12", errs, words)
	}
}

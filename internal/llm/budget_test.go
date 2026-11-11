package llm

import (
	"testing"

	"github.com/ZaneXo/zsmeet/internal/settings"
)

// 32K（也是没设置过时）正好是以前写死的值：写纪要每段 2 万字，问会议 1.2 万字
func TestBudgetAt32KMatchesOldValues(t *testing.T) {
	for _, tokens := range []int{0, settings.DefaultContextTokens} {
		if got := TranscriptChars(tokens); got != 20_000 {
			t.Errorf("%d：写纪要每段应该是 20000 字，得到 %d", tokens, got)
		}
		if got := AskChars(tokens); got != 12_000 {
			t.Errorf("%d：问会议的材料应该是 12000 字，得到 %d", tokens, got)
		}
	}
}

func TestBudgetGrowsWithContextAndIsClamped(t *testing.T) {
	prevT, prevA := 0, 0
	for _, tokens := range append([]int{1, 1_024, 4_096}, append(settings.ContextChoices, 1_000_000)...) {
		tc, ac := TranscriptChars(tokens), AskChars(tokens)
		if tc < prevT || ac < prevA {
			t.Errorf("%d：上下文越长，能放的字不能越少（%d→%d，%d→%d）", tokens, prevT, tc, prevA, ac)
		}
		if tc < minTranscriptChars || tc > maxTranscriptChars || ac < minAskChars || ac > maxAskChars {
			t.Errorf("%d：超出范围：写纪要 %d，问会议 %d", tokens, tc, ac)
		}
		if ac > tc {
			t.Errorf("%d：问会议的材料不该比写纪要的一段还多：%d > %d", tokens, ac, tc)
		}
		// 按每个字 1.1 个 token（比实测最费的分词器还多）算，加上提示词，也不能超出上下文
		if tokens >= settings.ContextChoices[0] && tc*11/10+promptReserve+minOutput > tokens {
			t.Errorf("%d：%d 字的转写放不进上下文", tokens, tc)
		}
		prevT, prevA = tc, ac
	}

	if got := TranscriptChars(8_192); got >= 20_000 || got < minTranscriptChars {
		t.Errorf("8K 时每段应该比 32K 少：%d", got)
	}
	if got := TranscriptChars(1); got != minTranscriptChars {
		t.Errorf("再小的设置也至少 %d 字，得到 %d", minTranscriptChars, got)
	}
	if TranscriptChars(1_000_000) != maxTranscriptChars || AskChars(1_000_000) != maxAskChars {
		t.Error("上下文再长，每次放的字也有上限")
	}
	if TranscriptChars(65_536) <= 20_000 || TranscriptChars(65_536) >= maxTranscriptChars {
		t.Errorf("64K 时每段比 32K 多、还没到上限：%d", TranscriptChars(65_536))
	}
}

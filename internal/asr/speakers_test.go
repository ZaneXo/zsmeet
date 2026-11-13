package asr

import (
	"slices"
	"testing"

	"github.com/ZaneXo/zsmeet/internal/transcript"
	"github.com/ZaneXo/zsmeet/internal/voiceprint"
)

// 自动区分把 3 个人（A、B、C）拆成了 8 个：0、1 是 A，2 是 B，3、4 是 C；
// 5 是一段 4 秒的笑声，和谁都不太像（稍微像 C）；6 是 B 说的 1 秒（声纹不可靠，不参与合并）；
// 7 太短，连声纹都算不出来。
var (
	splitTurns = []transcript.Turn{
		{Start: 0, End: 30, Speaker: 0},
		{Start: 30, End: 42, Speaker: 1},
		{Start: 42, End: 62, Speaker: 2},
		{Start: 62, End: 77, Speaker: 3},
		{Start: 77, End: 88, Speaker: 4},
		{Start: 88, End: 88.3, Speaker: 7},
		{Start: 90, End: 94, Speaker: 5},
		{Start: 95, End: 96, Speaker: 6},
	}
	splitVoices = map[int]voiceprint.Voice{
		0: {Embedding: voiceprint.Normalize([]float32{1, 0.1, 0, 0}), Seconds: 30},
		1: {Embedding: voiceprint.Normalize([]float32{0.95, 0.05, 0.1, 0}), Seconds: 12},
		2: {Embedding: voiceprint.Normalize([]float32{0, 1, 0, 0}), Seconds: 20},
		3: {Embedding: voiceprint.Normalize([]float32{0.1, 0, 1, 0}), Seconds: 15},
		4: {Embedding: voiceprint.Normalize([]float32{0, 0.1, 1, 0}), Seconds: 11},
		5: {Embedding: voiceprint.Normalize([]float32{0.1, 0, 0.2, 1}), Seconds: 4},
		6: {Embedding: voiceprint.Normalize([]float32{0.1, 1, 0, 0}), Seconds: 1},
	}
)

func speakerAt(turns []transcript.Turn, at float64) int {
	for _, t := range turns {
		if t.Start <= at && at < t.End {
			return t.Speaker
		}
	}
	return -1
}

// 按 3 个人分：声音像的并成一个人；笑声不占人数，归到声音最像的人；声纹不可靠的按声纹归，
// 算不出声纹的归到挨着的那段话的人；同一个人连着的时间段接起来。
func TestMergeSpeakers(t *testing.T) {
	got := mergeSpeakers(slices.Clone(splitTurns), splitVoices, 3)
	want := []transcript.Turn{
		{Start: 0, End: 42, Speaker: 0},
		{Start: 42, End: 62, Speaker: 1},
		{Start: 62, End: 88.3, Speaker: 2},
		{Start: 90, End: 94, Speaker: 2},
		{Start: 95, End: 96, Speaker: 1},
	}
	if !slices.Equal(got, want) {
		t.Errorf("按 3 个人分成了 %v，应该是 %v", got, want)
	}

	// 只有一个人：全并成一个，连着的接成一段
	got = mergeSpeakers(slices.Clone(splitTurns), splitVoices, 1)
	want = []transcript.Turn{{Start: 0, End: 88.3, Speaker: 0}, {Start: 90, End: 94, Speaker: 0}, {Start: 95, End: 96, Speaker: 0}}
	if !slices.Equal(got, want) {
		t.Errorf("按 1 个人分成了 %v，应该是 %v", got, want)
	}

	// 要求的人比实际的多：拆开声音最不像的同一个人，而不是让笑声单独算一个人
	got = mergeSpeakers(slices.Clone(splitTurns), splitVoices, 4)
	if n := speakerCount(got); n != 4 {
		t.Errorf("按 4 个人分应该分出 4 个，分出了 %d 个：%v", n, got)
	}
	if speakerAt(got, 91) != speakerAt(got, 70) {
		t.Errorf("笑声应该归到声音最像的说话人 3：%v", got)
	}

	// 说够 10 秒的凑不齐要求的人数：说得最多的零碎也算一个人
	got = mergeSpeakers(slices.Clone(splitTurns), splitVoices, 6)
	if n := speakerCount(got); n != 6 {
		t.Errorf("按 6 个人分应该分出 6 个，分出了 %d 个：%v", n, got)
	}
	laugh := speakerAt(got, 91)
	for _, at := range []float64{10, 35, 50, 70, 80, 95.5} {
		if speakerAt(got, at) == laugh {
			t.Errorf("笑声应该单独算一个人：%v", got)
		}
	}
}

// 分出的人本来就不比要求的多、或者一个声纹都没算出来时，不动。
func TestMergeSpeakersKeepsTurns(t *testing.T) {
	for _, c := range []struct {
		name   string
		voices map[int]voiceprint.Voice
		n      int
	}{
		{"人数刚好", splitVoices, 8},
		{"要求的人更多", splitVoices, 10},
		{"没有声纹", nil, 3},
	} {
		if got := mergeSpeakers(slices.Clone(splitTurns), c.voices, c.n); !slices.Equal(got, splitTurns) {
			t.Errorf("%s：不该改，结果是 %v", c.name, got)
		}
	}
}

// 录音很短、谁都没说够 10 秒时，按声音并到要求的人数。
func TestMergeSpeakersShortRecording(t *testing.T) {
	turns := []transcript.Turn{
		{Start: 0, End: 3, Speaker: 0},
		{Start: 3, End: 6, Speaker: 1},
		{Start: 6, End: 9, Speaker: 2},
	}
	voices := map[int]voiceprint.Voice{
		0: {Embedding: voiceprint.Normalize([]float32{1, 0}), Seconds: 3},
		1: {Embedding: voiceprint.Normalize([]float32{0, 1}), Seconds: 3},
		2: {Embedding: voiceprint.Normalize([]float32{0.9, 0.1}), Seconds: 3},
	}
	got := mergeSpeakers(turns, voices, 2)
	want := []transcript.Turn{{Start: 0, End: 3, Speaker: 0}, {Start: 3, End: 6, Speaker: 1}, {Start: 6, End: 9, Speaker: 0}}
	if !slices.Equal(got, want) {
		t.Errorf("分成了 %v，应该是 %v", got, want)
	}
}

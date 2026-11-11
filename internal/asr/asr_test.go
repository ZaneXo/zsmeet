package asr

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"github.com/ZaneXo/zsmeet/internal/audio"
	"github.com/ZaneXo/zsmeet/internal/models"
	"github.com/ZaneXo/zsmeet/internal/transcript"
	"github.com/ZaneXo/zsmeet/internal/voiceprint"
)

// 这个测试要用到约 560 MB 的模型和一段真实录音，默认跳过。运行方法：
//
//	ZSMEET_TEST_MODELS=模型目录 ZSMEET_TEST_AUDIO=录音文件 go test ./internal/asr/ -v
func TestEngineOnRealAudio(t *testing.T) {
	dir, file := os.Getenv("ZSMEET_TEST_MODELS"), os.Getenv("ZSMEET_TEST_AUDIO")
	if dir == "" || file == "" {
		t.Skip("没有设置 ZSMEET_TEST_MODELS 和 ZSMEET_TEST_AUDIO")
	}
	pcm := filepath.Join(t.TempDir(), "audio.f32")
	if err := audio.Decode(t.Context(), file, pcm); err != nil {
		t.Fatal(err)
	}
	samples, err := audio.ReadPCM(pcm)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := Load(models.Paths{Dir: dir}, 4, "zh")
	if err != nil {
		t.Fatal(err)
	}

	segs, err := engine.Transcribe(t.Context(), samples, func(float64) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) == 0 {
		t.Fatal("一句话都没识别出来")
	}
	for _, s := range segs {
		if s.End <= s.Start || strings.TrimSpace(s.Text) == "" {
			t.Errorf("片段不对：%+v", s)
		}
	}

	// 声纹：每位说话人一个。不同的人之间的相似度应该明显低于 voiceprint.Threshold，调阈值时看
	turns := engine.Diarize(samples)
	copied := append([]transcript.Segment(nil), segs...)
	ids := transcript.AssignSpeakers(copied, turns)
	voices, err := engine.Embed(samples, transcript.RenumberTurns(turns, ids))
	if err != nil {
		t.Fatal(err)
	}
	for a, va := range voices {
		for b, vb := range voices {
			if a < b {
				t.Logf("说话人 %d（%.0f 秒）和说话人 %d（%.0f 秒）的声纹相似度 %.2f", a+1, va.Seconds, b+1, vb.Seconds, voiceprint.Cosine(va.Embedding, vb.Embedding))
			}
		}
	}

	// 看聚类阈值对说话人数量的影响，调参时用
	for _, threshold := range []float32{0.4, 0.5, 0.6, 0.7, 0.8, 0.9} {
		cfg := sherpa.OfflineSpeakerDiarizationConfig{}
		cfg.Clustering.NumClusters = -1
		cfg.Clustering.Threshold = threshold
		engine.diarizer.SetConfig(&cfg)
		turns := engine.Diarize(samples)
		copied := append([]transcript.Segment(nil), segs...)
		transcript.AssignSpeakers(copied, turns)
		var who []string
		speakers := map[int]bool{}
		for _, s := range copied {
			who = append(who, transcript.SpeakerName(s.Speaker)[len("说话人"):])
			speakers[s.Speaker] = true
		}
		t.Logf("阈值 %.1f：%d 个时间段，%d 位说话人，各句依次是%s", threshold, len(turns), len(speakers), strings.Join(who, ""))
	}
}

// 算声纹用的片段：每人从最长的一段挑起，最多 30 秒；说话不到 2 秒的人不要；超出录音的部分截掉。
func TestPickTurns(t *testing.T) {
	const rate = audio.SampleRate
	turns := []transcript.Turn{
		{Start: 0, End: 5, Speaker: 0},
		{Start: 5, End: 25, Speaker: 0},
		{Start: 25, End: 26.5, Speaker: 1}, // 说话人 1 一共 1.5 秒，不够
		{Start: 30, End: 45, Speaker: 0},
		{Start: 50, End: 51, Speaker: 2},
		{Start: 59, End: 70, Speaker: 2}, // 录音只有 60 秒，这段只剩 1 秒；加上前面一共 2 秒，刚好够
		{Start: 10, End: 20, Speaker: -1},
	}
	got := pickTurns(turns, 60*rate)
	want := map[int][][2]int{
		0: {{5 * rate, 25 * rate}, {30 * rate, 40 * rate}},
		2: {{50 * rate, 51 * rate}, {59 * rate, 60 * rate}},
	}
	if len(got) != len(want) {
		t.Fatalf("应该只有说话人 0 和 2：%v", got)
	}
	for speaker, spans := range want {
		if !slices.Equal(got[speaker], spans) {
			t.Errorf("说话人 %d 挑的片段是 %v，应该是 %v", speaker, got[speaker], spans)
		}
	}
}

package asr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"github.com/ZaneXo/zsmeet/internal/audio"
	"github.com/ZaneXo/zsmeet/internal/models"
	"github.com/ZaneXo/zsmeet/internal/transcript"
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

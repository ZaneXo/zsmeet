package asr

import (
	"context"
	"fmt"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"github.com/ZaneXo/zsmeet/internal/audio"
	"github.com/ZaneXo/zsmeet/internal/diarize"
	"github.com/ZaneXo/zsmeet/internal/transcript"
	"github.com/ZaneXo/zsmeet/internal/voiceprint"
)

// Diarize 找出每个人说话的时间段，照着 3D-Speaker 的做法（算法见 diarize 包）：有人说话的地方切成 1.5 秒的小段，
// 每段算一个声纹（3D-Speaker 的 CAM++ 模型），谱聚类分成几个人。
// speakers 大于 0 时分成这么多人（用户在会议上选了说话人数），0 是按声音自动估计有几个人。progress 的参数是 0 到 1。
// 不能同时调用（后台处理时拿着锁）。
func (e *Engine) Diarize(ctx context.Context, samples []float32, speakers int, progress func(float64)) ([]transcript.Turn, error) {
	var speech []diarize.Span
	// 语音检测很快，算进度的前 5%；算声纹占大头
	err := e.detectSpeech(ctx, samples, func(p float64) { progress(0.05 * p) }, func(start int, s []float32) {
		from := float64(start) / audio.SampleRate
		speech = append(speech, diarize.Span{Start: from, End: from + float64(len(s))/audio.SampleRate})
	})
	if err != nil {
		return nil, err
	}
	all := diarize.Windows(speech)
	windows := make([]diarize.Span, 0, len(all))
	voices := make([][]float32, 0, len(all))
	for i, w := range all {
		if i%100 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			progress(0.05 + 0.9*float64(i)/float64(len(all)))
		}
		if v := e.windowVoice(samples, w); v != nil {
			windows = append(windows, w)
			voices = append(voices, v)
		}
	}
	turns := diarize.Turns(speech, windows, diarize.Cluster(voices, speakers))
	progress(1)
	return turns, nil
}

// windowVoice 算一个小段的声纹（已归一化）。不够 1.5 秒的小段把自己的声音循环接到 1.5 秒再算（和 3D-Speaker 一样）。
// 算不出来时返回 nil。
func (e *Engine) windowVoice(samples []float32, w diarize.Span) []float32 {
	from := min(max(int(w.Start*audio.SampleRate), 0), len(samples))
	to := min(max(int(w.End*audio.SampleRate), from), len(samples))
	part := samples[from:to]
	if len(part) == 0 {
		return nil
	}
	if want := int(diarize.WindowSeconds * audio.SampleRate); len(part) < want {
		padded := make([]float32, want)
		for i := range padded {
			padded[i] = part[i%len(part)]
		}
		part = padded
	}
	stream := e.diarizer.CreateStream()
	defer sherpa.DeleteOnlineStream(stream)
	stream.AcceptWaveform(audio.SampleRate, part)
	stream.InputFinished()
	if !e.diarizer.IsReady(stream) {
		return nil
	}
	return voiceprint.Normalize(e.diarizer.Compute(stream))
}

// SetSpeakerModel 换区分说话人用的声纹模型文件，zsmeet transcribe 比较模型用。
func (e *Engine) SetSpeakerModel(path string) error {
	cfg := e.diarizerModel
	cfg.Model = path
	x := sherpa.NewSpeakerEmbeddingExtractor(&cfg)
	if x == nil {
		return fmt.Errorf("区分说话人的模型 %s 加载失败：请确认文件存在、可以读取，而且是 sherpa-onnx 能用的说话人声纹模型（.onnx）", path)
	}
	sherpa.DeleteSpeakerEmbeddingExtractor(e.diarizer)
	e.diarizer, e.diarizerModel = x, cfg
	return nil
}

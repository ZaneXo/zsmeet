// Package asr 用 sherpa-onnx 做语音识别：切出有人说话的片段、识别成文字、加标点、区分说话人。
// 全部在本机 CPU 上跑，不调用外部服务。
package asr

import (
	"context"
	"errors"
	"fmt"
	"strings"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"zsmeet/internal/audio"
	"zsmeet/internal/models"
	"zsmeet/internal/transcript"
)

// Engine 持有加载好的模型。加载一次要几秒、占几百 MB 内存，所以整个进程共用一个。
// 它的方法不能并发调用，由调用方排队。
type Engine struct {
	recognizer *sherpa.OfflineRecognizer
	punct      *sherpa.OfflinePunctuation
	diarizer   *sherpa.OfflineSpeakerDiarization
	vad        sherpa.VadModelConfig
}

// Load 加载模型。threads 是识别用的 CPU 线程数。
func Load(p models.Paths, threads int) (*Engine, error) {
	e := &Engine{}

	rc := sherpa.OfflineRecognizerConfig{}
	rc.FeatConfig.SampleRate = audio.SampleRate
	rc.FeatConfig.FeatureDim = 80
	rc.ModelConfig.Paraformer.Model = p.Paraformer()
	rc.ModelConfig.Tokens = p.Tokens()
	rc.ModelConfig.NumThreads = threads
	rc.ModelConfig.Provider = "cpu"
	rc.DecodingMethod = "greedy_search"
	if e.recognizer = sherpa.NewOfflineRecognizer(&rc); e.recognizer == nil {
		return nil, errors.New("识别模型加载失败")
	}

	pc := sherpa.OfflinePunctuationConfig{}
	pc.Model.CtTransformer = p.Punct()
	pc.Model.NumThreads = min(threads, 2)
	pc.Model.Provider = "cpu"
	if e.punct = sherpa.NewOfflinePunctuation(&pc); e.punct == nil {
		return nil, errors.New("标点模型加载失败")
	}

	dc := sherpa.OfflineSpeakerDiarizationConfig{}
	dc.Segmentation.Pyannote.Model = p.Segmentation()
	dc.Segmentation.NumThreads = threads
	dc.Segmentation.Provider = "cpu"
	dc.Embedding.Model = p.Embedding()
	dc.Embedding.NumThreads = threads
	dc.Embedding.Provider = "cpu"
	// 不知道有几个人，按声音相似度的阈值自动聚类
	dc.Clustering.NumClusters = -1
	// 阈值越小分出的人越多。库的默认值 0.5 会把同一个人拆成两个，0.6 是在测试录音上刚好不拆的值；
	// 还需要用真实的多人会议再校准
	dc.Clustering.Threshold = 0.6
	dc.MinDurationOn = 0.3
	dc.MinDurationOff = 0.5
	if e.diarizer = sherpa.NewOfflineSpeakerDiarization(&dc); e.diarizer == nil {
		return nil, errors.New("说话人模型加载失败")
	}
	if rate := e.diarizer.SampleRate(); rate != audio.SampleRate {
		return nil, fmt.Errorf("说话人模型要求采样率 %d，和识别用的 %d 不一致", rate, audio.SampleRate)
	}

	e.vad.SileroVad.Model = p.VAD()
	e.vad.SileroVad.Threshold = 0.5
	e.vad.SileroVad.MinSilenceDuration = 0.3
	e.vad.SileroVad.MinSpeechDuration = 0.25
	e.vad.SileroVad.WindowSize = vadWindow
	// 一句话最长 10 秒，再长就强行切开，识别模型在短句上更准
	e.vad.SileroVad.MaxSpeechDuration = 10
	e.vad.SampleRate = audio.SampleRate
	e.vad.NumThreads = 1
	e.vad.Provider = "cpu"
	return e, nil
}

// Silero VAD 在 16k 采样率下固定每次吃 512 个采样
const vadWindow = 512

// Transcribe 把整段录音识别成带时间的句子。progress 的参数是 0 到 1。
func (e *Engine) Transcribe(ctx context.Context, samples []float32, progress func(float64)) ([]transcript.Segment, error) {
	vad := sherpa.NewVoiceActivityDetector(&e.vad, 30)
	if vad == nil {
		return nil, errors.New("语音检测模型加载失败")
	}
	defer sherpa.DeleteVoiceActivityDetector(vad)

	var segs []transcript.Segment
	drain := func() {
		for !vad.IsEmpty() {
			speech := vad.Front()
			vad.Pop()
			if text := e.recognize(speech.Samples); text != "" {
				start := float64(speech.Start) / audio.SampleRate
				segs = append(segs, transcript.Segment{
					Start:   start,
					End:     start + float64(len(speech.Samples))/audio.SampleRate,
					Speaker: -1,
					Text:    text,
				})
			}
		}
	}

	for i := 0; i+vadWindow <= len(samples); i += vadWindow {
		// 大约每 3 秒音频检查一次是否被取消，并报一次进度
		if i%(vadWindow*100) == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			progress(float64(i) / float64(len(samples)))
		}
		vad.AcceptWaveform(samples[i : i+vadWindow])
		drain()
	}
	vad.Flush()
	drain()
	progress(1)
	return segs, nil
}

// recognize 识别一个片段并加标点。
func (e *Engine) recognize(samples []float32) string {
	stream := sherpa.NewOfflineStream(e.recognizer)
	defer sherpa.DeleteOfflineStream(stream)
	stream.AcceptWaveform(audio.SampleRate, samples)
	e.recognizer.Decode(stream)
	text := strings.TrimSpace(stream.GetResult().Text)
	if text == "" {
		return ""
	}
	if withPunct := strings.TrimSpace(e.punct.AddPunct(text)); withPunct != "" {
		return withPunct
	}
	return text
}

// Diarize 找出每个人说话的时间段。这一步是一次不能中断的长计算。
func (e *Engine) Diarize(samples []float32) []transcript.Turn {
	if len(samples) == 0 {
		return nil
	}
	result := e.diarizer.Process(samples)
	turns := make([]transcript.Turn, len(result))
	for i, r := range result {
		turns[i] = transcript.Turn{Start: float64(r.Start), End: float64(r.End), Speaker: r.Speaker}
	}
	return turns
}

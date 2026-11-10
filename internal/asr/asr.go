// Package asr 用 sherpa-onnx 做语音识别：切出有人说话的片段、识别成带标点和时间的句子、区分说话人。
// 全部在本机 CPU 上跑，不调用外部服务。
package asr

import (
	"context"
	"errors"
	"fmt"
	"strings"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"github.com/ZaneXo/zsmeet/internal/audio"
	"github.com/ZaneXo/zsmeet/internal/models"
	"github.com/ZaneXo/zsmeet/internal/transcript"
)

// Engine 持有加载好的模型。加载一次要几秒、占几百 MB 内存，所以整个进程共用一个。
// 它的方法不能并发调用，由调用方排队。
type Engine struct {
	recognizer *sherpa.OfflineRecognizer
	diarizer   *sherpa.OfflineSpeakerDiarization
	vad        sherpa.VadModelConfig
}

// Load 加载模型。threads 是识别用的 CPU 线程数，language 是识别的语言（见 config.Config.ASRLanguage）。
func Load(p models.Paths, threads int, language string) (*Engine, error) {
	e := &Engine{}

	rc := sherpa.OfflineRecognizerConfig{}
	rc.FeatConfig.SampleRate = audio.SampleRate
	rc.FeatConfig.FeatureDim = 80
	// SenseVoice 自己会加标点、把数字写成阿拉伯数字（use ITN），并给出每个字的时间，
	// 所以不再需要单独的标点模型，句子也能按标点切得很准
	rc.ModelConfig.SenseVoice.Model = p.SenseVoice()
	// 固定成中文时，夹在中文里的英文照样认得出来；auto 会把很短的「嗯」「哦」认成日文
	rc.ModelConfig.SenseVoice.Language = language
	rc.ModelConfig.SenseVoice.UseInverseTextNormalization = 1
	rc.ModelConfig.Tokens = p.Tokens()
	rc.ModelConfig.NumThreads = threads
	rc.ModelConfig.Provider = "cpu"
	rc.DecodingMethod = "greedy_search"
	if e.recognizer = sherpa.NewOfflineRecognizer(&rc); e.recognizer == nil {
		return nil, errors.New("识别模型加载失败")
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
			start := float64(speech.Start) / audio.SampleRate
			segs = append(segs, e.recognize(speech.Samples, start, start+float64(len(speech.Samples))/audio.SampleRate)...)
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

// recognize 识别一小段录音（start 到 end 秒），按标点切成句子，每句带开始和结束时间。
// 模型没给出每个字的时间时（正常不会发生），整段算一句。
func (e *Engine) recognize(samples []float32, start, end float64) []transcript.Segment {
	stream := sherpa.NewOfflineStream(e.recognizer)
	defer sherpa.DeleteOfflineStream(stream)
	stream.AcceptWaveform(audio.SampleRate, samples)
	e.recognizer.Decode(stream)
	result := stream.GetResult()
	if len(result.Tokens) > 0 && len(result.Timestamps) == len(result.Tokens) {
		tokens := make([]transcript.Token, len(result.Tokens))
		for i, text := range result.Tokens {
			tokens[i] = transcript.Token{Text: text, Start: float64(result.Timestamps[i])}
		}
		return transcript.Sentences(tokens, start, end)
	}
	if text := strings.TrimSpace(result.Text); text != "" {
		return []transcript.Segment{{Start: start, End: end, Speaker: -1, Text: text}}
	}
	return nil
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

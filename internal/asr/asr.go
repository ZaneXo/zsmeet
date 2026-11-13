// Package asr 用 sherpa-onnx 做语音识别：切出有人说话的片段、识别成带标点和时间的句子、区分说话人。
// 全部在本机 CPU 上跑，不调用外部服务。
package asr

import (
	"cmp"
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"github.com/ZaneXo/zsmeet/internal/audio"
	"github.com/ZaneXo/zsmeet/internal/models"
	"github.com/ZaneXo/zsmeet/internal/transcript"
	"github.com/ZaneXo/zsmeet/internal/voiceprint"
)

// Engine 持有加载好的模型。加载一次要几秒、占几百 MB 内存，所以整个进程共用一个。
// 它的方法不能并发调用，由调用方排队。
type Engine struct {
	recognizer *sherpa.OfflineRecognizer
	vad        sherpa.VadModelConfig

	// 区分说话人时给每个小段算声纹的模型（见 Diarize）
	diarizerModel sherpa.SpeakerEmbeddingExtractorConfig
	diarizer      *sherpa.SpeakerEmbeddingExtractor

	// 声纹库（按声音认出改过名的人）用的模型。管理员打开了声纹才用到，第一次用时才加载。
	// 和区分说话人用的不是同一个：存下来的声纹是这个模型算的，换了模型就对不上了
	embedding sherpa.SpeakerEmbeddingExtractorConfig
	extractor *sherpa.SpeakerEmbeddingExtractor
}

// Params 是语音检测的参数。后台处理用 DefaultParams，zsmeet transcribe 可以换别的值来比较。
type Params struct {
	// 每一小段（32 毫秒）是人声的概率超过 VADThreshold 算有人在说话；
	// 安静 MinSilence 秒算这句话说完了，短于 MinSpeech 秒的不要，长于 MaxSpeech 秒的强行切开（单位都是秒）
	VADThreshold, MinSilence, MinSpeech, MaxSpeech float32
}

// DefaultParams 是后台处理用的参数。
func DefaultParams() Params {
	return Params{
		VADThreshold: 0.5,
		MinSilence:   0.3,
		MinSpeech:    0.25,
		// 一句话最长 10 秒，再长就强行切开，识别模型在短句上更准
		MaxSpeech: 10,
	}
}

// Load 加载模型。threads 是识别用的 CPU 线程数，language 是识别的语言（见 config.Config.ASRLanguage）。
func Load(p models.Paths, threads int, language string) (*Engine, error) {
	return LoadWith(p, threads, language, DefaultParams())
}

// LoadWith 和 Load 一样，只是语音检测用指定的参数。
func LoadWith(p models.Paths, threads int, language string, params Params) (*Engine, error) {
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
		return nil, modelError("识别模型", p.SenseVoice())
	}

	e.diarizerModel = sherpa.SpeakerEmbeddingExtractorConfig{Model: p.Diarization(), NumThreads: threads, Provider: "cpu"}
	if e.diarizer = sherpa.NewSpeakerEmbeddingExtractor(&e.diarizerModel); e.diarizer == nil {
		return nil, modelError("区分说话人的模型", p.Diarization())
	}
	e.embedding = sherpa.SpeakerEmbeddingExtractorConfig{Model: p.Embedding(), NumThreads: threads, Provider: "cpu"}

	e.vad.SileroVad.Model = p.VAD()
	e.vad.SileroVad.Threshold = params.VADThreshold
	e.vad.SileroVad.MinSilenceDuration = params.MinSilence
	e.vad.SileroVad.MinSpeechDuration = params.MinSpeech
	e.vad.SileroVad.WindowSize = vadWindow
	e.vad.SileroVad.MaxSpeechDuration = params.MaxSpeech
	e.vad.SampleRate = audio.SampleRate
	e.vad.NumThreads = 1
	e.vad.Provider = "cpu"
	return e, nil
}

// modelError 是模型加载失败时给用户看的说明。模型下载时校验过，加载失败多半是文件后来坏了；
// 检查模型时只看文件在不在、大小对不对，所以要删掉它才会重新下载。
func modelError(what, path string) error {
	return fmt.Errorf("%s加载失败，模型文件 %s 可能坏了。请管理员删掉模型目录（ZSMEET_MODELS_DIR，"+
		"docker compose 部署的是 models 数据卷）里的这个文件，再重启后台处理（docker compose restart worker），它会自动重新下载",
		what, filepath.Base(path))
}

// Silero VAD 在 16k 采样率下固定每次吃 512 个采样
const vadWindow = 512

// Transcribe 把整段录音识别成带时间的句子。progress 的参数是 0 到 1。
func (e *Engine) Transcribe(ctx context.Context, samples []float32, progress func(float64)) ([]transcript.Segment, error) {
	var segs []transcript.Segment
	err := e.detectSpeech(ctx, samples, progress, func(start int, speech []float32) {
		from := float64(start) / audio.SampleRate
		segs = append(segs, e.recognize(speech, from, from+float64(len(speech))/audio.SampleRate)...)
	})
	if err != nil {
		return nil, err
	}
	return segs, nil
}

// detectSpeech 用语音检测找出有人说话的片段，按先后交给 each（片段开头的采样下标和片段的声音）。
// progress 的参数是 0 到 1。
func (e *Engine) detectSpeech(ctx context.Context, samples []float32, progress func(float64), each func(start int, speech []float32)) error {
	vad := sherpa.NewVoiceActivityDetector(&e.vad, 30)
	if vad == nil {
		return modelError("语音检测模型", e.vad.SileroVad.Model)
	}
	defer sherpa.DeleteVoiceActivityDetector(vad)

	drain := func() {
		for !vad.IsEmpty() {
			speech := vad.Front()
			vad.Pop()
			each(speech.Start, speech.Samples)
		}
	}
	for i := 0; i+vadWindow <= len(samples); i += vadWindow {
		// 大约每 3 秒音频检查一次是否被取消，并报一次进度
		if i%(vadWindow*100) == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			progress(float64(i) / float64(len(samples)))
		}
		vad.AcceptWaveform(samples[i : i+vadWindow])
		drain()
	}
	vad.Flush()
	drain()
	progress(1)
	return nil
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

// 算一个人的声纹时用他最长的几段话，加起来最多这么多秒：再多也不会更准，只是更慢
const maxVoiceSeconds = 30

// 一场会议里说话加起来不到这么多秒的人不算声纹：太短的话算出来的不可靠，容易认错
const minVoiceSeconds = 2

// Embed 给每位说话人算一个声音特征（声纹），用来和声纹库比对，turns 的编号就是返回值的键。
// 说话太少的人没有。算出来的向量已经归一化。
func (e *Engine) Embed(samples []float32, turns []transcript.Turn) (map[int]voiceprint.Voice, error) {
	picked := pickTurns(turns, len(samples))
	if len(picked) == 0 {
		return nil, nil
	}
	if e.extractor == nil {
		if e.extractor = sherpa.NewSpeakerEmbeddingExtractor(&e.embedding); e.extractor == nil {
			return nil, modelError("声纹模型", e.embedding.Model)
		}
	}
	out := make(map[int]voiceprint.Voice, len(picked))
	for speaker, spans := range picked {
		stream := e.extractor.CreateStream()
		used := 0
		for _, sp := range spans {
			stream.AcceptWaveform(audio.SampleRate, samples[sp[0]:sp[1]])
			used += sp[1] - sp[0]
		}
		stream.InputFinished()
		var v []float32
		if e.extractor.IsReady(stream) {
			v = voiceprint.Normalize(e.extractor.Compute(stream))
		}
		sherpa.DeleteOnlineStream(stream)
		if v != nil {
			out[speaker] = voiceprint.Voice{Embedding: v, Seconds: float64(used) / audio.SampleRate}
		}
	}
	return out, nil
}

// pickTurns 给每位说话人挑出用来算声纹的几段（采样下标的起止，不含止）：从最长的一段开始挑，
// 加起来到 maxVoiceSeconds 秒为止，最后一段截短。说话加起来不到 minVoiceSeconds 秒的人不要。
// total 是录音的采样数，超出录音的部分截掉。
func pickTurns(turns []transcript.Turn, total int) map[int][][2]int {
	bySpeaker := map[int][][2]int{}
	for _, t := range turns {
		if t.Speaker < 0 {
			continue
		}
		from := min(max(int(t.Start*audio.SampleRate), 0), total)
		to := min(max(int(t.End*audio.SampleRate), from), total)
		if to > from {
			bySpeaker[t.Speaker] = append(bySpeaker[t.Speaker], [2]int{from, to})
		}
	}
	out := map[int][][2]int{}
	for speaker, spans := range bySpeaker {
		sum := 0
		for _, sp := range spans {
			sum += sp[1] - sp[0]
		}
		if sum < minVoiceSeconds*audio.SampleRate {
			continue
		}
		// 最长的在前；一样长的按时间先后，保证结果稳定
		slices.SortFunc(spans, func(a, b [2]int) int {
			return cmp.Or(cmp.Compare(b[1]-b[0], a[1]-a[0]), cmp.Compare(a[0], b[0]))
		})
		left := maxVoiceSeconds * audio.SampleRate
		var chosen [][2]int
		for _, sp := range spans {
			if left <= 0 {
				break
			}
			n := min(sp[1]-sp[0], left)
			chosen = append(chosen, [2]int{sp[0], sp[0] + n})
			left -= n
		}
		out[speaker] = chosen
	}
	return out
}

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
	"unicode"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"github.com/ZaneXo/zsmeet/internal/audio"
	"github.com/ZaneXo/zsmeet/internal/models"
	"github.com/ZaneXo/zsmeet/internal/transcript"
	"github.com/ZaneXo/zsmeet/internal/voiceprint"
)

// Engine 持有加载好的模型。加载一次要几秒、占几百 MB 内存，所以整个进程共用一个。
// 它的方法不能并发调用，由调用方排队。
type Engine struct {
	// 语音检测和识别文字（SenseVoice）
	*Transcriber

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
	t, err := NewTranscriberWith(SenseVoice(p, threads, language), p.VAD(), params)
	if err != nil {
		return nil, err
	}
	e.Transcriber = t

	e.diarizerModel = sherpa.SpeakerEmbeddingExtractorConfig{Model: p.Diarization(), NumThreads: threads, Provider: "cpu"}
	if e.diarizer = sherpa.NewSpeakerEmbeddingExtractor(&e.diarizerModel); e.diarizer == nil {
		return nil, modelError("区分说话人的模型", p.Diarization())
	}
	e.embedding = sherpa.SpeakerEmbeddingExtractorConfig{Model: p.Embedding(), NumThreads: threads, Provider: "cpu"}
	return e, nil
}

// Transcriber 是识别文字这一步：Silero VAD 切出有人说话的片段，逐段识别，按标点切成带时间的句子。
// Engine 用它跑 SenseVoice；评测工具（tools/asreval）用它在同样的切段方式下比较别的识别模型。
// 它的方法不能并发调用。
type Transcriber struct {
	recognizer *sherpa.OfflineRecognizer
	config     sherpa.OfflineRecognizerConfig
	vad        sherpa.VadModelConfig
}

// NewTranscriber 加载识别模型 m，语音检测用 DefaultParams。vadModel 是 Silero VAD 模型文件的路径。
func NewTranscriber(m Model, vadModel string) (*Transcriber, error) {
	return NewTranscriberWith(m, vadModel, DefaultParams())
}

// NewTranscriberWith 和 NewTranscriber 一样，只是语音检测用指定的参数。
func NewTranscriberWith(m Model, vadModel string, params Params) (*Transcriber, error) {
	t := &Transcriber{config: m.config}
	if t.recognizer = sherpa.NewOfflineRecognizer(&t.config); t.recognizer == nil {
		return nil, modelError("识别模型", m.file())
	}
	t.vad.SileroVad.Model = vadModel
	t.vad.SileroVad.Threshold = params.VADThreshold
	t.vad.SileroVad.MinSilenceDuration = params.MinSilence
	t.vad.SileroVad.MinSpeechDuration = params.MinSpeech
	t.vad.SileroVad.WindowSize = vadWindow
	t.vad.SileroVad.MaxSpeechDuration = params.MaxSpeech
	t.vad.SampleRate = audio.SampleRate
	t.vad.NumThreads = 1
	t.vad.Provider = "cpu"
	return t, nil
}

func (t *Transcriber) isSenseVoice() bool {
	return t.config.ModelConfig.SenseVoice.Model != ""
}

// SetLanguage 切换 SenseVoice 的识别语言（"auto"、"zh"、"en"、"yue"、"ja"、"ko"）。
// sherpa-onnx 直接更新识别器内部的语言选项（耗时几微秒），不重新加载模型权重。
func (t *Transcriber) SetLanguage(lang string) {
	if !t.isSenseVoice() {
		return
	}
	lang = cmp.Or(strings.TrimSpace(lang), "auto")
	if t.config.ModelConfig.SenseVoice.Language == lang {
		return
	}
	t.config.ModelConfig.SenseVoice.Language = lang
	t.recognizer.SetConfig(&t.config)
}

// Close 释放识别模型。进程里一直用着的 Engine 不用调。
func (t *Transcriber) Close() {
	sherpa.DeleteOfflineRecognizer(t.recognizer)
	t.recognizer = nil
}

// Close 释放识别、说话人区分和声纹模型。
func (e *Engine) Close() {
	if e.Transcriber != nil {
		e.Transcriber.Close()
		e.Transcriber = nil
	}
	if e.diarizer != nil {
		sherpa.DeleteSpeakerEmbeddingExtractor(e.diarizer)
		e.diarizer = nil
	}
	if e.extractor != nil {
		sherpa.DeleteSpeakerEmbeddingExtractor(e.extractor)
		e.extractor = nil
	}
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
func (t *Transcriber) Transcribe(ctx context.Context, samples []float32, progress func(float64)) ([]transcript.Segment, error) {
	speeches, err := t.TranscribeDetailed(ctx, samples, progress)
	if err != nil {
		return nil, err
	}
	var segs []transcript.Segment
	for _, sp := range speeches {
		segs = append(segs, sp.Segments...)
	}
	return segs, nil
}

// TranscribeDetailed 找出有人说话的区间，逐段识别成带时间的字和句子。progress 的参数是 0 到 1。
// 在线识别用它的区间来切录音、用它每个字的时间给远端返回的文字定时间（见 internal/cloudasr）。
func (t *Transcriber) TranscribeDetailed(ctx context.Context, samples []float32, progress func(float64)) ([]transcript.Speech, error) {
	type chunkMeta struct {
		from, end float64
		samples   []float32
		lang      string
	}
	useAuto := t.isSenseVoice() && t.config.ModelConfig.SenseVoice.Language == "auto"
	var (
		out    []transcript.Speech
		chunks []chunkMeta
	)
	err := t.detectSpeech(ctx, samples, progress, func(start int, speech []float32) {
		from := float64(start) / audio.SampleRate
		end := from + float64(len(speech))/audio.SampleRate
		tokens, segs, lang := t.recognize(speech, from, end)
		out = append(out, transcript.Speech{Start: from, End: end, Tokens: tokens, Segments: segs})
		if useAuto {
			var raw []float32
			if start >= 0 && start+len(speech) <= len(samples) {
				raw = samples[start : start+len(speech)]
			} else {
				raw = slices.Clone(speech)
			}
			chunks = append(chunks, chunkMeta{from: from, end: end, samples: raw, lang: lang})
		}
	})
	if err != nil {
		return nil, err
	}
	if useAuto && len(out) > 0 {
		items := make([]autoSegment, len(out))
		for i, sp := range out {
			items[i] = autoSegment{
				dur:  chunks[i].end - chunks[i].from,
				lang: chunks[i].lang,
				text: joinSegmentText(sp.Segments),
			}
		}
		if fallback, indices := planAutoRedecode(items); len(indices) > 0 {
			cfg := t.config
			cfg.ModelConfig.SenseVoice.Language = fallback
			t.recognizer.SetConfig(&cfg)
			defer t.recognizer.SetConfig(&t.config)
			for _, idx := range indices {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				ch := chunks[idx]
				tokens, segs, _ := t.recognize(ch.samples, ch.from, ch.end)
				out[idx] = transcript.Speech{Start: ch.from, End: ch.end, Tokens: tokens, Segments: segs}
			}
		}
	}
	return out, nil
}

// detectSpeech 用语音检测找出有人说话的片段，按先后交给 each（片段开头的采样下标和片段的声音）。
// progress 的参数是 0 到 1。
func (t *Transcriber) detectSpeech(ctx context.Context, samples []float32, progress func(float64), each func(start int, speech []float32)) error {
	vad := sherpa.NewVoiceActivityDetector(&t.vad, 30)
	if vad == nil {
		return modelError("语音检测模型", t.vad.SileroVad.Model)
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

// recognize 识别一小段录音（start 到 end 秒），返回每个字的时间（从这一小段开头算）、按标点切出的句子和识别出的语言标签。
// 模型没给出每个字的时间时（正常不会发生），整段算一句，tokens 为 nil。
func (t *Transcriber) recognize(samples []float32, start, end float64) ([]transcript.Token, []transcript.Segment, string) {
	stream := sherpa.NewOfflineStream(t.recognizer)
	defer sherpa.DeleteOfflineStream(stream)
	stream.AcceptWaveform(audio.SampleRate, samples)
	t.recognizer.Decode(stream)
	result := stream.GetResult()
	lang := normalizeLangTag(result.Lang)
	if len(result.Tokens) > 0 && len(result.Timestamps) == len(result.Tokens) {
		tokens := make([]transcript.Token, len(result.Tokens))
		for i, text := range result.Tokens {
			tokens[i] = transcript.Token{Text: text, Start: float64(result.Timestamps[i])}
		}
		return tokens, transcript.Sentences(tokens, start, end), lang
	}
	if text := strings.TrimSpace(result.Text); text != "" {
		return nil, []transcript.Segment{{Start: start, End: end, Speaker: -1, Text: text}}, lang
	}
	return nil, nil, lang
}

func normalizeLangTag(tag string) string {
	return strings.ToLower(strings.Trim(tag, "<|> \t"))
}

func joinSegmentText(segs []transcript.Segment) string {
	if len(segs) == 0 {
		return ""
	}
	if len(segs) == 1 {
		return segs[0].Text
	}
	var b strings.Builder
	for _, s := range segs {
		b.WriteString(s.Text)
	}
	return b.String()
}

// autoSegment 是 SenseVoice 在 auto 模式下第一遍识别出一小段语音的摘要，用来判断哪些片段属于误判、需要按会议主语言重算。
type autoSegment struct {
	dur  float64
	lang string
	text string
}

var langOrder = []string{"zh", "en", "yue", "ja", "ko"}

// planAutoRedecode 根据整场录音各语种的分布，挑出在 SenseVoice auto 模式下被误判成外语的片段下标，以及重算时用的主语言。
//
// SenseVoice 的 auto 对正常长度的普通话、粤语、英语、日语、韩语和中英混合都很准，但遇到孤立的短语气词（「嗯」「哦」「是」）
// 或底噪切段时，容易把它们判成日文（「うん。」「四？」）、韩文（「분.」「하늘튀 왜种这种。」）或单个英文词（「O.」「The.」）：
//   - 整场里没有成句的日文时，所有判成 ja 或带假名的片段都按主语言重算（包括没有假名的日文汉字幻觉如「四？」）；
//   - 整场里没有成句的韩文时，所有判成 ko 或带谚文的片段都按主语言重算（包括夹着汉字的噪声段）；
//   - 整场主语言明确（长段占比 ≥ 70%）时，不到 1.5 秒且该语种在长段里几乎没出现过的超短异语种语气词也按主语言重算，
//     而 ≥ 1.5 秒的独立外语整句和句内中英夹杂原样保留。
func planAutoRedecode(items []autoSegment) (fallback string, indices []int) {
	var (
		totalDur, totalLongDur   float64
		jaCleanDur, koCleanDur   float64
		jaCleanSegs, koCleanSegs int
		totalKana, totalHangul   int
	)
	totalByLang := map[string]float64{}
	longByLang := map[string]float64{}

	for _, it := range items {
		if strings.TrimSpace(it.text) == "" {
			continue
		}
		totalDur += it.dur
		kana, hangul, han := countScripts(it.text)
		totalKana += kana
		totalHangul += hangul

		// 韩文识别正常只出谚文和字母数字，混着汉字（如「하늘튀 왜种这种。」）或没有谚文的 ko 标签属于噪声幻觉；
		// 日文成句必然带假名（助词、词尾），短段判成 ja 却一个假名都没有（如「四？」）也是语气词误判
		koHalluc := (it.lang == "ko" && hangul == 0) || (hangul > 0 && han > 0)
		jaHalluc := it.lang == "ja" && kana == 0 && it.dur < 3.0
		if !koHalluc && !jaHalluc && slices.Contains(langOrder, it.lang) {
			totalByLang[it.lang] += it.dur
			if it.dur >= 1.8 {
				longByLang[it.lang] += it.dur
				totalLongDur += it.dur
			}
		}
		if it.lang == "ja" && kana >= 2 && it.dur >= 1.8 {
			jaCleanDur += it.dur
			jaCleanSegs++
		}
		if it.lang == "ko" && hangul >= 3 && han == 0 && it.dur >= 1.8 {
			koCleanDur += it.dur
			koCleanSegs++
		}
	}

	dom := dominantLang(longByLang)
	if dom == "" {
		dom = dominantLang(totalByLang)
		if totalDur < 1.5 {
			if (dom == "ja" && totalKana <= 2) || (dom == "ko" && totalHangul <= 1) {
				dom = "zh"
			}
		}
	}
	if dom == "" {
		dom = "zh"
	}

	jaActive := dom == "ja" || (jaCleanDur >= 4.0 && (jaCleanDur >= 0.03*totalDur || jaCleanSegs >= 3))
	koActive := dom == "ko" || (koCleanDur >= 4.0 && (koCleanDur >= 0.03*totalDur || koCleanSegs >= 3))
	if (dom == "ja" && !jaActive) || (dom == "ko" && !koActive) {
		dom = "zh"
	}
	fallback = dom

	domStrong := totalLongDur >= 5.0 && longByLang[dom] >= 0.70*totalLongDur
	for i, it := range items {
		if strings.TrimSpace(it.text) == "" {
			continue
		}
		kana, hangul, _ := countScripts(it.text)
		switch {
		case !jaActive && (it.lang == "ja" || kana > 0):
			indices = append(indices, i)
		case !koActive && (it.lang == "ko" || hangul > 0):
			indices = append(indices, i)
		case domStrong && it.dur < 1.5 && it.lang != "" && it.lang != dom:
			if longByLang[it.lang] < 4.0 && longByLang[it.lang] < 0.05*totalLongDur {
				indices = append(indices, i)
			}
		}
	}
	return fallback, indices
}

func dominantLang(byLang map[string]float64) string {
	best := ""
	bestDur := 0.0
	for _, l := range langOrder {
		if d := byLang[l]; d > bestDur {
			best, bestDur = l, d
		}
	}
	return best
}

func countScripts(s string) (kana, hangul, han int) {
	for _, r := range s {
		switch {
		case unicode.In(r, unicode.Hiragana, unicode.Katakana):
			kana++
		case unicode.Is(unicode.Hangul, r):
			hangul++
		case unicode.Is(unicode.Han, r):
			han++
		}
	}
	return kana, hangul, han
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

package asr

import (
	"cmp"
	"os"
	"path/filepath"
	"strings"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"github.com/ZaneXo/zsmeet/internal/audio"
	"github.com/ZaneXo/zsmeet/internal/models"
)

// Model 是一种识别模型和它的设置，用来建 Transcriber。线上只用 SenseVoice，
// 另外两种给评测工具（tools/asreval）比较用。
type Model struct {
	config sherpa.OfflineRecognizerConfig
}

func newModel(threads int) Model {
	var m Model
	m.config.FeatConfig.SampleRate = audio.SampleRate
	m.config.FeatConfig.FeatureDim = 80
	m.config.ModelConfig.NumThreads = threads
	m.config.ModelConfig.Provider = "cpu"
	m.config.DecodingMethod = "greedy_search"
	return m
}

// WithProvider 换成在别的设备上跑："cpu"（默认）或 "cuda"（NVIDIA 显卡，要用带 CUDA 的 sherpa-onnx 库）。
// 只换识别模型，语音检测（VAD）很轻，一直在 CPU 上跑。
func (m Model) WithProvider(provider string) Model {
	m.config.ModelConfig.Provider = provider
	return m
}

// SenseVoice 是线上识别文字用的模型。threads 是 CPU 线程数，language 是识别的语言（见 config.Config.ASRLanguage）。
func SenseVoice(p models.Paths, threads int, language string) Model {
	return SenseVoiceFile(p.SenseVoice(), p.Tokens(), threads, language)
}

// SenseVoiceFile 和 SenseVoice 一样，但指定模型文件：评测时在 GPU 上用没有量化的 model.onnx
// （int8 量化的模型有不少算子 GPU 上没有，会退回 CPU 算，反而更慢）。
func SenseVoiceFile(model, tokens string, threads int, language string) Model {
	m := newModel(threads)
	sv := &m.config.ModelConfig.SenseVoice
	// SenseVoice 自己会加标点、把数字写成阿拉伯数字（use ITN），并给出每个字的时间，
	// 所以不再需要单独的标点模型，句子也能按标点切得很准
	sv.Model = model
	// 固定成中文时，夹在中文里的英文照样认得出来；auto 会把很短的「嗯」「哦」认成日文
	sv.Language = language
	sv.UseInverseTextNormalization = 1
	m.config.ModelConfig.Tokens = tokens
	return m
}

// file 是加载失败时告诉用户的模型文件。
func (m Model) file() string {
	c := m.config.ModelConfig
	return cmp.Or(c.SenseVoice.Model, c.FunAsrNano.EncoderAdaptor, c.Qwen3ASR.Encoder)
}

// pick 返回 dir 里第一个存在的文件，都不存在时返回第一个（加载时会报找不到）。
func pick(dir string, names ...string) string {
	for _, n := range names {
		if p := filepath.Join(dir, n); exists(p) {
			return p
		}
	}
	return filepath.Join(dir, names[0])
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// FunASRNano 是 Fun-ASR-Nano，dir 是解压出的目录：int8 量化版（sherpa-onnx-funasr-nano-int8-2025-12-30）、
// fp16 版或没有量化的版本（sherpa-onnx-funasr-nano-2025-12-30，GPU 上用它），按目录里有的文件选。
// 支持热词：识别时会偏向这些词。生成参数用 sherpa-onnx 文档里的默认值，温度接近 0，结果是确定的。
func FunASRNano(dir string, threads int, hotwords []string) Model {
	m := newModel(threads)
	f := &m.config.ModelConfig.FunAsrNano
	f.EncoderAdaptor = pick(dir, "encoder_adaptor.int8.onnx", "encoder_adaptor.onnx")
	f.LLM = pick(dir, "llm.int8.onnx", "llm.fp16.onnx", "llm.fp32.onnx", "llm.onnx")
	f.Embedding = pick(dir, "embedding.int8.onnx", "embedding.onnx")
	f.Tokenizer = filepath.Join(dir, "Qwen3-0.6B")
	f.SystemPrompt = "You are a helpful assistant."
	f.UserPrompt = "语音转写："
	f.MaxNewTokens = 512
	f.Temperature = 1e-6
	f.TopP = 0.8
	f.Seed = 42
	f.UseInverseTextNormalization = 1
	f.Hotwords = strings.Join(hotwords, ",")
	return m
}

// Qwen3ASR 是 Qwen3-ASR 0.6B（sherpa-onnx-qwen3-asr-0.6B-int8-2026-03-25，dir 是解压出的目录）。
// 支持热词。生成参数用 sherpa-onnx 文档里的默认值。
func Qwen3ASR(dir string, threads int, hotwords []string) Model {
	m := newModel(threads)
	q := &m.config.ModelConfig.Qwen3ASR
	q.ConvFrontend = filepath.Join(dir, "conv_frontend.onnx")
	q.Encoder = filepath.Join(dir, "encoder.int8.onnx")
	q.Decoder = filepath.Join(dir, "decoder.int8.onnx")
	q.Tokenizer = filepath.Join(dir, "tokenizer")
	q.MaxTotalLen = 512
	q.MaxNewTokens = 512
	q.Temperature = 1e-6
	q.TopP = 0.8
	q.Seed = 42
	q.Hotwords = strings.Join(hotwords, ",")
	return m
}

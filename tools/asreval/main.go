// asreval 在同一批录音上比较几种识别方案的字错误率、带说话人错字率（cpCER）和速度：
// 要不要开在线识别、按热词纠错和大模型校对有没有用，拿自己的录音量一量再决定。
//
//	go run ./tools/asreval -data 录音目录 [-models 模型目录] \
//	    [-deepseek-asr https://api.deepseek.com/v1/tools/asr] \
//	    [-funasr-nano 模型目录] [-qwen3-asr 模型目录] [-hotwords "飞书,张敏"] \
//	    [-llm-url https://.../v1 -llm-model 模型名] [-out 结果目录]
//
// 录音目录里每段录音（wav、m4a、mp3 等 ffmpeg 认得的格式）配一个同名的 .txt，是人工校对过的标准文字，
// 标点、空格、换行不影响结果；阿拉伯数字和汉字数字会自动统一成汉字读法再比。
// 可以再配一个同名的 .hotwords.txt（这段录音的热词），以及同名的 .segments.json（asrdata textgrid 生成的说话人标注，
// 有它时会跑一次线上的说话人区分，算出「带说话人错字率」cpCER，检验时间对齐准不准）。
//
// 比较的方案（后面几种在给了对应的参数、有热词时才有）：
//
//	sensevoice                现在默认用的本机识别模型
//	sensevoice+拼音            再按热词的读音替换同音字（internal/correct.Homophones）
//	sensevoice+拼音+大模型      再让大模型逐句校对（internal/correct.LLM），和线上处理会议的步骤一样
//	…+大模型(附拼音)            校对时每句附上拼音（correct.Input.Pinyin），看值不值得多花一倍的请求长度
//	deepseek-asr(按句)         DeepSeek 在线识别（-deepseek-asr），按 VAD 一句一个请求（不对齐）
//	deepseek-asr(合到N秒)      相邻句子合到最长 N 秒再发（不对齐回单句，用来做对照）
//	deepseek-asr(线上)         线上真实流程：VAD 合到 100 秒发给 DeepSeek，再对齐回 SenseVoice 字级时间分句（对不上兜底）
//	deepseek-asr(线上)+拼音…   在线识别对齐后再跑拼音替换和大模型校对
//	funasr-nano(+热词)         Fun-ASR-Nano，模型目录是解压出的 sherpa-onnx-funasr-nano-int8-2025-12-30
//	qwen3-asr(+热词)           Qwen3-ASR 0.6B，模型目录是解压出的 sherpa-onnx-qwen3-asr-0.6B-int8-2026-03-25
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/ZaneXo/zsmeet/internal/asr"
	"github.com/ZaneXo/zsmeet/internal/audio"
	"github.com/ZaneXo/zsmeet/internal/cloudasr"
	"github.com/ZaneXo/zsmeet/internal/correct"
	"github.com/ZaneXo/zsmeet/internal/llm"
	"github.com/ZaneXo/zsmeet/internal/models"
	"github.com/ZaneXo/zsmeet/internal/settings"
	"github.com/ZaneXo/zsmeet/internal/transcript"
)

var audioExts = []string{".wav", ".m4a", ".mp3", ".flac", ".ogg", ".opus", ".aac", ".webm", ".mp4", ".aiff", ".aif"}

type flags struct {
	data, models, mirror, funasrNano, qwen3ASR string
	senseVoice, filter                         string
	hotwords, language, out, provider          string
	llmURL, llmModel, llmKey                   string
	llmConcurrency                             int
	llmPinyin                                  bool
	deepseekURL, deepseekKey, deepseekReplay   string
	deepseekConcurrency                        int
	deepseekChunk                              float64
	deepseekBySentence                         bool
	threads, workers                           int
	concurrent, diarize                        bool
}

func main() {
	log.SetFlags(0)
	var f flags
	flag.StringVar(&f.data, "data", "", "录音目录：每段录音配一个同名的 .txt 标准文字（必填）")
	flag.StringVar(&f.models, "models", cmp.Or(os.Getenv("ZSMEET_MODELS_DIR"), "./models"), "线上用的模型目录（SenseVoice、VAD、说话人区分），缺了会下载")
	flag.StringVar(&f.mirror, "mirror", cmp.Or(os.Getenv("ZSMEET_MODEL_MIRROR"), "https://huggingface.co"), "下载线上模型用的 Hugging Face 地址")
	flag.StringVar(&f.senseVoice, "sensevoice", "", "换一个 SenseVoice 模型文件（比如 GPU 上用没有量化的 model.onnx），不给就用 -models 里线上用的")
	flag.StringVar(&f.filter, "filter", "", "读录音时用这个 ffmpeg 音频滤镜代替线上的音量处理（audio.SpeechFilter），none 是不处理，比如 loudnorm=I=-20")
	flag.StringVar(&f.funasrNano, "funasr-nano", "", "Fun-ASR-Nano 的模型目录，不给就不比")
	flag.StringVar(&f.qwen3ASR, "qwen3-asr", "", "Qwen3-ASR 的模型目录，不给就不比")
	flag.StringVar(&f.hotwords, "hotwords", "", "所有录音共用的热词，逗号隔开")
	flag.StringVar(&f.language, "language", cmp.Or(os.Getenv("ZSMEET_ASR_LANGUAGE"), "zh"), "SenseVoice 的识别语言，和线上一致")
	flag.IntVar(&f.threads, "threads", min(runtime.NumCPU(), 4), "每个识别模型用几个 CPU 线程")
	flag.IntVar(&f.workers, "workers", 1, "每种识别模型同时加载几份、同时识别几段录音（CPU 多的机器上调大，占用的 CPU 是 threads×workers）")
	flag.StringVar(&f.provider, "provider", "cpu", "识别模型在哪跑：cpu，或者 cuda（NVIDIA 显卡，要用带 CUDA 的 sherpa-onnx 库，见 LD_LIBRARY_PATH）")
	flag.BoolVar(&f.concurrent, "concurrent", false, "几种识别模型同时跑（CPU 够多时用；CPU 不够时互相抢，测出的速度不准）")
	flag.BoolVar(&f.diarize, "diarize", true, "录音带 .segments.json 说话人标注时，跑线上的说话人区分并算带说话人错字率（cpCER）")
	flag.StringVar(&f.llmURL, "llm-url", "", "大模型接口地址（OpenAI 兼容，一般以 /v1 结尾），给了才比大模型校对")
	flag.StringVar(&f.llmModel, "llm-model", "", "大模型名")
	flag.IntVar(&f.llmConcurrency, "llm-concurrency", 3, "最多同时发几个大模型请求（中转服务一般限制每个账号的并发数）")
	flag.StringVar(&f.llmKey, "llm-key", os.Getenv("ZSMEET_EVAL_LLM_KEY"), "大模型的 API Key，也可以用环境变量 ZSMEET_EVAL_LLM_KEY")
	flag.BoolVar(&f.llmPinyin, "llm-pinyin", false, "大模型校对时多比一组每句附拼音的方案")
	flag.StringVar(&f.deepseekURL, "deepseek-asr", "", "DeepSeek 在线识别的接口地址（https://api.deepseek.com/v1/tools/asr），给了才比")
	flag.StringVar(&f.deepseekKey, "deepseek-key", os.Getenv("ZSMEET_EVAL_DEEPSEEK_KEY"), "DeepSeek 的 API Key，也可以用环境变量 ZSMEET_EVAL_DEEPSEEK_KEY")
	flag.StringVar(&f.deepseekReplay, "deepseek-replay", "", "复用之前保存的 deepseek-asr(合到N秒) 目录，离线重跑时间对齐、cpCER 和纠错")
	flag.IntVar(&f.deepseekConcurrency, "deepseek-concurrency", 8, "最多同时发几个 DeepSeek 识别请求（接口限制每个账号 50 个）")
	flag.Float64Var(&f.deepseekChunk, "deepseek-chunk", 100, "合并识别时一个请求最长几秒（接口最多收 120 秒）")
	flag.BoolVar(&f.deepseekBySentence, "deepseek-by-sentence", false, "同时跑一句一个请求的 deepseek-asr(按句) 对照")
	flag.StringVar(&f.out, "out", "", "把每种方案识别出的文字和报告写到这个目录，方便逐句对比")
	flag.Parse()
	if f.data == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(context.Background(), f); err != nil {
		log.Fatal(err)
	}
}

// clip 是一段要评测的录音。
type clip struct {
	name     string
	samples  []float32
	ref      string
	hotwords []string
	// 有同名 .segments.json 时是标准说话人分段；跑完 Diarize 后 turns 不为空
	refSegs []transcript.Segment
	turns   []transcript.Turn
}

func (c clip) seconds() float64 { return float64(len(c.samples)) / audio.SampleRate }

// result 是一种方案在所有录音上的结果。
type result struct {
	name string
	// 和 clips 一一对应，每段录音识别出的句子
	segs [][]transcript.Segment
	// 识别（或者纠错）每段录音的耗时加起来是多久、加载模型一共花了多久；
	// 同时跑几份时，从开始到全部识别完实际过了多久
	took, load, wall time.Duration
	// 这种方案的耗时只是纠错这一步，不是识别
	postOnly bool
}

func run(ctx context.Context, f flags) error {
	clips, err := loadClips(ctx, f.data, correct.ParseHotwords(f.hotwords), f.filter)
	if err != nil {
		return err
	}
	var total float64
	anyHotwords := false
	anyRefSegs := false
	for _, c := range clips {
		total += c.seconds()
		anyHotwords = anyHotwords || len(c.hotwords) > 0
		anyRefSegs = anyRefSegs || len(c.refSegs) > 0
	}
	log.Printf("%d 段录音，共 %.0f 秒", len(clips), total)

	if err := models.Ensure(ctx, f.models, f.mirror, nil); err != nil {
		return fmt.Errorf("准备线上模型：%w", err)
	}
	p := models.Paths{Dir: f.models}

	showCP := f.diarize && anyRefSegs
	if showCP {
		if err := diarizeClips(ctx, p, f.threads, max(f.workers, 1), f.language, clips); err != nil {
			return err
		}
	}

	rn := runner{clips: clips, vad: p.VAD(), workers: max(f.workers, 1)}
	var jobs []func() ([]result, error)
	// SenseVoice、纠错方案和借用 SenseVoice 时间轴的 DeepSeek 在线识别
	jobs = append(jobs, func() ([]result, error) {
		sv := asr.SenseVoiceFile(cmp.Or(f.senseVoice, p.SenseVoice()), p.Tokens(), f.threads, f.language).WithProvider(f.provider)
		base, speeches, err := rn.recognizeDetailed(ctx, "sensevoice", func([]string) asr.Model { return sv }, false)
		if err != nil {
			return nil, err
		}
		results := []result{base}
		if anyHotwords {
			results = append(results, homophoneCorrect(clips, base))
		}
		if f.llmURL != "" && f.llmModel != "" {
			from := results[len(results)-1]
			pinyins := []bool{false}
			if f.llmPinyin {
				pinyins = append(pinyins, true)
			}
			for _, withPinyin := range pinyins {
				r, err := llmCorrect(ctx, f, clips, from, withPinyin)
				if err != nil {
					return nil, err
				}
				results = append(results, r)
			}
		}
		if f.deepseekURL != "" || f.deepseekReplay != "" {
			var unaligned, online result
			if f.deepseekReplay != "" {
				unaligned, online, err = deepseekReplay(f, clips, base, speeches)
			} else {
				if f.deepseekBySentence {
					r, err := deepseekASR(ctx, f, clips, base, 0)
					if err != nil {
						return nil, err
					}
					results = append(results, r)
				}
				if f.deepseekChunk > 0 && f.deepseekChunk != cloudasr.MaxChunkSec {
					r, err := deepseekASR(ctx, f, clips, base, f.deepseekChunk)
					if err != nil {
						return nil, err
					}
					results = append(results, r)
				}
				unaligned, online, err = deepseekOnlineASR(ctx, f, clips, speeches)
			}
			if err != nil {
				return nil, err
			}
			results = append(results, unaligned, online)
			fromOnline := online
			if anyHotwords {
				fromOnline = homophoneCorrect(clips, online)
				results = append(results, fromOnline)
			}
			if f.llmURL != "" && f.llmModel != "" {
				r, err := llmCorrect(ctx, f, clips, fromOnline, false)
				if err != nil {
					return nil, err
				}
				results = append(results, r)
			}
		}
		return results, nil
	})
	if f.funasrNano != "" {
		jobs = append(jobs, func() ([]result, error) {
			return rn.compare(ctx, "funasr-nano", anyHotwords, func(hotwords []string) asr.Model {
				return asr.FunASRNano(f.funasrNano, f.threads, hotwords).WithProvider(f.provider)
			})
		})
	}
	if f.qwen3ASR != "" {
		jobs = append(jobs, func() ([]result, error) {
			return rn.compare(ctx, "qwen3-asr", anyHotwords, func(hotwords []string) asr.Model {
				return asr.Qwen3ASR(f.qwen3ASR, f.threads, hotwords).WithProvider(f.provider)
			})
		})
	}

	outs := make([][]result, len(jobs))
	errs := make([]error, len(jobs))
	var wg sync.WaitGroup
	for i, job := range jobs {
		if !f.concurrent {
			outs[i], errs[i] = job()
			continue
		}
		wg.Go(func() { outs[i], errs[i] = job() })
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}
	results := slices.Concat(outs...)

	report := render(clips, results, total, showCP)
	fmt.Print(report)
	if f.out != "" {
		return save(f.out, clips, results, report)
	}
	return nil
}

func diarizeClips(ctx context.Context, p models.Paths, threads, workers int, language string, clips []clip) error {
	var todo []int
	for i := range clips {
		if len(clips[i].refSegs) > 0 {
			todo = append(todo, i)
		}
	}
	if len(todo) == 0 {
		return nil
	}
	n := min(max(workers, 1), len(todo))
	work := make(chan int, len(todo))
	for _, idx := range todo {
		work <- idx
	}
	close(work)

	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
	)
	started := time.Now()
	for range n {
		wg.Go(func() {
			engine, err := asr.Load(p, threads, language)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			defer engine.Close()
			for idx := range work {
				if err := ctx.Err(); err != nil {
					return
				}
				t0 := time.Now()
				turns, err := engine.Diarize(ctx, clips[idx].samples, 0, func(float64) {})
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("说话人区分 %s：%w", clips[idx].name, err)
					}
					mu.Unlock()
					return
				}
				clips[idx].turns = turns
				log.Printf("diarize：%s（%.0f 秒录音用了 %.1f 秒）", clips[idx].name, clips[idx].seconds(), time.Since(t0).Seconds())
			}
		})
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	log.Printf("diarize：%d 段录音全部完成，实际用时 %.1f 秒", len(todo), time.Since(started).Seconds())
	return nil
}

func homophoneCorrect(clips []clip, from result) result {
	started := time.Now()
	r := result{name: from.name + "+拼音", segs: cloneAll(from.segs), postOnly: true}
	for i, c := range clips {
		correct.Homophones(r.segs[i], c.hotwords)
	}
	r.took = time.Since(started)
	r.wall = r.took
	return r
}

// llmCorrect 在 from 的结果上让大模型逐句校对，和线上处理会议的步骤一样。
// withPinyin 时每句附上拼音（correct.Input.Pinyin）。所有录音同时校对，不然推理模型一批要等一两分钟。
func llmCorrect(ctx context.Context, f flags, clips []clip, from result, withPinyin bool) (result, error) {
	cfg := settings.LLM{BaseURL: f.llmURL, Model: f.llmModel, APIKey: f.llmKey}
	// 所有录音的请求加起来最多同时发 llmConcurrency 个：中转服务一般限制每个账号的并发数，超了直接报错
	limit := make(chan struct{}, max(f.llmConcurrency, 1))
	chat := func(ctx context.Context, system, user string) (string, error) {
		select {
		case limit <- struct{}{}:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		defer func() { <-limit }()
		return llm.Chat(ctx, cfg, system, user)
	}
	name := from.name + "+大模型"
	if withPinyin {
		name += "(附拼音)"
	}
	log.Printf("%s：校对中…", name)
	started := time.Now()
	r := result{name: name, segs: cloneAll(from.segs), postOnly: true}
	var (
		mu                     sync.Mutex
		batches, failed, fixed int
		wg                     sync.WaitGroup
	)
	for i, c := range clips {
		wg.Go(func() {
			stats, err := correct.LLM(ctx, chat, r.segs[i], correct.Input{Hotwords: c.hotwords, Pinyin: withPinyin}, nil)
			mu.Lock()
			defer mu.Unlock()
			batches, failed, fixed = batches+stats.Batches, failed+stats.Failed, fixed+stats.Changed
			if err != nil {
				return
			}
			if stats.Err != nil {
				log.Printf("  %s：%d 批里有 %d 批失败：%v", c.name, stats.Batches, stats.Failed, stats.Err)
			}
			log.Printf("  %s：改了 %d 句，改动太大没采用 %d 句", c.name, stats.Changed, stats.Rejected)
		})
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return r, err
	}
	log.Printf("%s：一共 %d 批，失败 %d 批，改了 %d 句", name, batches, failed, fixed)
	r.took = time.Since(started)
	r.wall = r.took
	return r, nil
}

// runner 在所有录音上跑识别模型。
type runner struct {
	clips   []clip
	vad     string
	workers int
}

// compare 跑一种识别模型：不带热词跑一遍；有热词时再带上热词跑一遍。
func (rn runner) compare(ctx context.Context, name string, anyHotwords bool, config func(hotwords []string) asr.Model) ([]result, error) {
	plain, _, err := rn.recognizeDetailed(ctx, name, func([]string) asr.Model { return config(nil) }, false)
	if err != nil {
		return nil, err
	}
	if !anyHotwords {
		return []result{plain}, nil
	}
	biased, _, err := rn.recognizeDetailed(ctx, name+"+热词", config, true)
	if err != nil {
		return nil, err
	}
	return []result{plain, biased}, nil
}

// recognizeDetailed 用一种识别模型识别所有录音：同时加载 workers 份模型，各自从队列里取录音识别，同时返回详细的 VAD/Token 结果。
func (rn runner) recognizeDetailed(ctx context.Context, name string, config func(hotwords []string) asr.Model, perHotwords bool) (result, [][]transcript.Speech, error) {
	clips := rn.clips
	r := result{name: name, segs: make([][]transcript.Segment, len(clips))}
	allSpeeches := make([][]transcript.Speech, len(clips))
	type done struct {
		i          int
		speeches   []transcript.Speech
		segs       []transcript.Segment
		took, load time.Duration
		err        error
	}
	queue := make(chan int)
	results := make(chan done)
	for range min(rn.workers, len(clips)) {
		go func() {
			var t *asr.Transcriber
			key := ""
			defer func() {
				if t != nil {
					t.Close()
				}
			}()
			for i := range queue {
				c := clips[i]
				var hotwords []string
				if perHotwords {
					hotwords = c.hotwords
				}
				k := strings.Join(hotwords, ",")
				var load time.Duration
				if t == nil || k != key {
					if t != nil {
						t.Close()
					}
					started := time.Now()
					var err error
					if t, err = asr.NewTranscriber(config(hotwords), rn.vad); err != nil {
						t = nil
						results <- done{i: i, err: fmt.Errorf("%s：%w", name, err)}
						continue
					}
					key, load = k, time.Since(started)
				}
				started := time.Now()
				speeches, err := t.TranscribeDetailed(ctx, c.samples, func(float64) {})
				var segs []transcript.Segment
				for _, sp := range speeches {
					segs = append(segs, sp.Segments...)
				}
				results <- done{i: i, speeches: speeches, segs: segs, took: time.Since(started), load: load, err: err}
			}
		}()
	}
	go func() {
		for i := range clips {
			queue <- i
		}
		close(queue)
	}()

	started := time.Now()
	var errs []error
	for range clips {
		d := <-results
		if d.err != nil {
			errs = append(errs, d.err)
			continue
		}
		allSpeeches[d.i] = d.speeches
		r.segs[d.i], r.took, r.load = d.segs, r.took+d.took, r.load+d.load
		log.Printf("%s：%s（%.0f 秒的录音用了 %.1f 秒）", name, clips[d.i].name, clips[d.i].seconds(), d.took.Seconds())
	}
	r.wall = time.Since(started)
	return r, allSpeeches, errors.Join(errs...)
}

// loadClips 读出目录里所有有标准文字的录音，解码成 16k 单声道。filter 见 decode。
func loadClips(ctx context.Context, dir string, shared []string, filter string) ([]clip, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp("", "asreval-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	var clips []clip
	for _, e := range entries {
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if e.IsDir() || !slices.Contains(audioExts, ext) {
			continue
		}
		base := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		ref, err := os.ReadFile(filepath.Join(dir, base+".txt"))
		if errors.Is(err, os.ErrNotExist) {
			log.Printf("跳过 %s：没有标准文字 %s.txt", e.Name(), base)
			continue
		}
		if err != nil {
			return nil, err
		}
		hotwords := slices.Clone(shared)
		if raw, err := os.ReadFile(filepath.Join(dir, base+".hotwords.txt")); err == nil {
			for _, w := range correct.ParseHotwords(string(raw)) {
				if !slices.ContainsFunc(hotwords, func(x string) bool { return strings.EqualFold(x, w) }) {
					hotwords = append(hotwords, w)
				}
			}
		}
		var refSegs []transcript.Segment
		if raw, err := os.ReadFile(filepath.Join(dir, base+".segments.json")); err == nil {
			if err := json.Unmarshal(raw, &refSegs); err != nil {
				return nil, fmt.Errorf("%s.segments.json：%w", base, err)
			}
		}
		pcm := filepath.Join(tmp, base+".f32")
		if err := decode(ctx, filepath.Join(dir, e.Name()), pcm, filter); err != nil {
			return nil, fmt.Errorf("%s：%w", e.Name(), err)
		}
		samples, err := audio.ReadPCM(pcm)
		if err != nil {
			return nil, err
		}
		clips = append(clips, clip{name: e.Name(), samples: samples, ref: string(ref), hotwords: hotwords, refSegs: refSegs})
	}
	if len(clips) == 0 {
		return nil, fmt.Errorf("%s 里没有配了标准文字（同名 .txt）的录音", dir)
	}
	return clips, nil
}

// decode 和线上一样解成 16k 单声道 float32（先按 audio.SpeechFilter 调匀音量）。
// filter 不为空时用它代替线上的音量处理，none 是不处理（和 zsmeet transcribe 的 -filter 一样）。
func decode(ctx context.Context, in, pcm, filter string) error {
	switch filter {
	case "":
		return audio.Decode(ctx, in, pcm)
	case "none":
		return audio.DecodeWith(ctx, in, pcm, "")
	default:
		return audio.DecodeWith(ctx, in, pcm, filter)
	}
}

func cloneAll(in [][]transcript.Segment) [][]transcript.Segment {
	out := make([][]transcript.Segment, len(in))
	for i, s := range in {
		out[i] = slices.Clone(s)
	}
	return out
}

// text 是识别出的整段文字，一句一行。
func text(segs []transcript.Segment) string {
	lines := make([]string, len(segs))
	for i, s := range segs {
		lines[i] = s.Text
	}
	return strings.Join(lines, "\n")
}

// score 是一段（或几段加起来）的错字数和标准文字的字数，以及热词识别数和带说话人错字数（cpCER）。
type score struct {
	errors, words   int
	hotRef, hotHit  int
	cpErrs, cpWords int
}

func (s score) cer() float64 {
	if s.words == 0 {
		return 0
	}
	return float64(s.errors) / float64(s.words)
}

func (s score) cpCER() (float64, bool) {
	if s.cpWords == 0 {
		return 0, false
	}
	return float64(s.cpErrs) / float64(s.cpWords), true
}

func (s *score) add(o score) {
	s.errors += o.errors
	s.words += o.words
	s.hotRef += o.hotRef
	s.hotHit += o.hotHit
	s.cpErrs += o.cpErrs
	s.cpWords += o.cpWords
}

func scoreOf(c clip, segs []transcript.Segment) score {
	hyp := text(segs)
	// 数字统一换成汉字读法再比（见 cloudasr.SpokenNumbers）
	ref, got := correct.Tokens(cloudasr.SpokenNumbers(c.ref)), correct.Tokens(cloudasr.SpokenNumbers(hyp))
	s := score{errors: correct.Distance(ref, got), words: len(ref)}
	// 热词按完整的单位对：前后加上分隔符，「AI」不会算到「said」里
	join := func(tokens []string) string { return "|" + strings.Join(tokens, "|") + "|" }
	r, h := join(ref), join(got)
	for _, w := range c.hotwords {
		key := join(correct.Tokens(cloudasr.SpokenNumbers(w)))
		if key == "||" {
			continue
		}
		n := strings.Count(r, key)
		s.hotRef += n
		s.hotHit += min(n, strings.Count(h, key))
	}
	if len(c.refSegs) > 0 && len(c.turns) > 0 {
		s.cpErrs, s.cpWords = cpScore(c.refSegs, segs, c.turns)
	}
	return s
}

// render 排出报告：每种方案的总体结果，再是每段录音的字错误率。
func render(clips []clip, results []result, total float64, showCP bool) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	if showCP {
		fmt.Fprintln(w, "方案\t字错误率\t带说话人错字率\t错字/总字数\t热词识别对\t耗时/录音时长\t实际用时\t加载模型")
	} else {
		fmt.Fprintln(w, "方案\t字错误率\t错字/总字数\t热词识别对\t耗时/录音时长\t实际用时\t加载模型")
	}
	for _, r := range results {
		var sum score
		for i, c := range clips {
			sum.add(scoreOf(c, r.segs[i]))
		}
		hot := "-"
		if sum.hotRef > 0 {
			hot = fmt.Sprintf("%d/%d", sum.hotHit, sum.hotRef)
		}
		rtf := fmt.Sprintf("%.3f", r.took.Seconds()/total)
		if r.postOnly {
			rtf += "（只算纠错）"
		}
		if showCP {
			cpStr := "-"
			if v, ok := sum.cpCER(); ok {
				cpStr = fmt.Sprintf("%.2f%%", v*100)
			}
			fmt.Fprintf(w, "%s\t%.2f%%\t%s\t%d/%d\t%s\t%s\t%.0fs\t%.0fs\n",
				r.name, sum.cer()*100, cpStr, sum.errors, sum.words, hot, rtf, r.wall.Seconds(), r.load.Seconds())
		} else {
			fmt.Fprintf(w, "%s\t%.2f%%\t%d/%d\t%s\t%s\t%.0fs\t%.0fs\n",
				r.name, sum.cer()*100, sum.errors, sum.words, hot, rtf, r.wall.Seconds(), r.load.Seconds())
		}
	}
	w.Flush()

	if len(clips) > 1 {
		b.WriteString("\n每段录音的字错误率：\n")
		w = tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		header := []string{"录音", "时长"}
		for _, r := range results {
			header = append(header, r.name)
		}
		fmt.Fprintln(w, strings.Join(header, "\t"))
		for i, c := range clips {
			row := []string{c.name, fmt.Sprintf("%.0fs", c.seconds())}
			for _, r := range results {
				row = append(row, fmt.Sprintf("%.2f%%", scoreOf(c, r.segs[i]).cer()*100))
			}
			fmt.Fprintln(w, strings.Join(row, "\t"))
		}
		w.Flush()
	}
	return b.String()
}

// save 把报告和每种方案识别出的文字写到 dir：dir/report.txt、dir/方案/录音名.txt（有说话人结果时另存 .speakers.txt）。
func save(dir string, clips []clip, results []result, report string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "report.txt"), []byte(report), 0o644); err != nil {
		return err
	}
	for _, r := range results {
		sub := filepath.Join(dir, r.name)
		if err := os.MkdirAll(sub, 0o755); err != nil {
			return err
		}
		for i, c := range clips {
			stem := strings.TrimSuffix(c.name, filepath.Ext(c.name))
			if err := os.WriteFile(filepath.Join(sub, stem+".txt"), []byte(text(r.segs[i])+"\n"), 0o644); err != nil {
				return err
			}
			if len(c.turns) > 0 {
				assigned := slices.Clone(r.segs[i])
				transcript.AssignSpeakers(assigned, c.turns)
				var lines []string
				for _, s := range assigned {
					lines = append(lines, fmt.Sprintf("[%.2f-%.2f 说话人 %d] %s", s.Start, s.End, s.Speaker+1, s.Text))
				}
				if err := os.WriteFile(filepath.Join(sub, stem+".speakers.txt"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
					return err
				}
			}
		}
	}
	log.Printf("识别出的文字和报告写在 %s", dir)
	return nil
}

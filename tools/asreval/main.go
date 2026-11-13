// asreval 在同一批录音上比较几种识别方案的字错误率和速度：要不要换识别模型、按热词纠错和大模型校对有没有用，
// 拿自己的录音量一量再决定。
//
//	go run ./tools/asreval -data 录音目录 [-models 模型目录] \
//	    [-funasr-nano 模型目录] [-qwen3-asr 模型目录] [-hotwords "飞书,张敏"] \
//	    [-llm-url https://.../v1 -llm-model 模型名] [-out 结果目录]
//
// 录音目录里每段录音（wav、m4a、mp3 等 ffmpeg 认得的格式）配一个同名的 .txt，是人工校对过的标准文字，
// 标点、空格、换行不影响结果；数字的写法要和识别结果一致（都写阿拉伯数字），不然会算成错字。
// 可以再配一个同名的 .hotwords.txt，是这段录音的热词（逗号或换行隔开），和 -hotwords 给的合在一起用。
//
// 比较的方案（后面几种在给了对应的参数、有热词时才有）：
//
//	sensevoice              现在线上用的识别模型
//	sensevoice+拼音          再按热词的读音替换同音字（internal/correct.Homophones）
//	sensevoice+拼音+大模型    再让大模型逐句校对（internal/correct.LLM），和线上处理会议的步骤一样
//	…+大模型(附拼音)          校对时每句附上拼音（correct.Input.Pinyin），看值不值得多花一倍的请求长度
//	funasr-nano(+热词)       Fun-ASR-Nano，模型目录是解压出的 sherpa-onnx-funasr-nano-int8-2025-12-30
//	qwen3-asr(+热词)         Qwen3-ASR 0.6B，模型目录是解压出的 sherpa-onnx-qwen3-asr-0.6B-int8-2026-03-25
//
// 解码（包括响度归一化）和切段方式（Silero VAD，一段最长 10 秒）都和线上一样，只换识别模型，
// 所以比较的就是识别模型本身。-filter 换掉线上的处理：-filter anull 看不做归一化的效果。
// 字错误率按「一个汉字算一个、一个英文单词算一个」算编辑距离。
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/ZaneXo/zsmeet/internal/asr"
	"github.com/ZaneXo/zsmeet/internal/audio"
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
	threads, workers                           int
	concurrent                                 bool
}

func main() {
	log.SetFlags(0)
	var f flags
	flag.StringVar(&f.data, "data", "", "录音目录：每段录音配一个同名的 .txt 标准文字（必填）")
	flag.StringVar(&f.models, "models", cmp.Or(os.Getenv("ZSMEET_MODELS_DIR"), "./models"), "线上用的模型目录（SenseVoice、VAD），缺了会下载")
	flag.StringVar(&f.mirror, "mirror", cmp.Or(os.Getenv("ZSMEET_MODEL_MIRROR"), "https://huggingface.co"), "下载线上模型用的 Hugging Face 地址")
	flag.StringVar(&f.senseVoice, "sensevoice", "", "换一个 SenseVoice 模型文件（比如 GPU 上用没有量化的 model.onnx），不给就用 -models 里线上用的")
	flag.StringVar(&f.filter, "filter", "", "读录音时用这个 ffmpeg 音频滤镜代替线上的响度归一化，比如 anull（不处理）、dynaudnorm")
	flag.StringVar(&f.funasrNano, "funasr-nano", "", "Fun-ASR-Nano 的模型目录，不给就不比")
	flag.StringVar(&f.qwen3ASR, "qwen3-asr", "", "Qwen3-ASR 的模型目录，不给就不比")
	flag.StringVar(&f.hotwords, "hotwords", "", "所有录音共用的热词，逗号隔开")
	flag.StringVar(&f.language, "language", cmp.Or(os.Getenv("ZSMEET_ASR_LANGUAGE"), "zh"), "SenseVoice 的识别语言，和线上一致")
	flag.IntVar(&f.threads, "threads", min(runtime.NumCPU(), 4), "每个识别模型用几个 CPU 线程")
	flag.IntVar(&f.workers, "workers", 1, "每种识别模型同时加载几份、同时识别几段录音（CPU 多的机器上调大，占用的 CPU 是 threads×workers）")
	flag.StringVar(&f.provider, "provider", "cpu", "识别模型在哪跑：cpu，或者 cuda（NVIDIA 显卡，要用带 CUDA 的 sherpa-onnx 库，见 LD_LIBRARY_PATH）")
	flag.BoolVar(&f.concurrent, "concurrent", false, "几种识别模型同时跑（CPU 够多时用；CPU 不够时互相抢，测出的速度不准）")
	flag.StringVar(&f.llmURL, "llm-url", "", "大模型接口地址（OpenAI 兼容，一般以 /v1 结尾），给了才比大模型校对")
	flag.StringVar(&f.llmModel, "llm-model", "", "大模型名")
	flag.StringVar(&f.llmKey, "llm-key", os.Getenv("ZSMEET_EVAL_LLM_KEY"), "大模型的 API Key，也可以用环境变量 ZSMEET_EVAL_LLM_KEY")
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
	for _, c := range clips {
		total += c.seconds()
		anyHotwords = anyHotwords || len(c.hotwords) > 0
	}
	log.Printf("%d 段录音，共 %.0f 秒", len(clips), total)

	if err := models.Ensure(ctx, f.models, f.mirror, nil); err != nil {
		return fmt.Errorf("准备线上模型：%w", err)
	}
	vad := models.Paths{Dir: f.models}.VAD()

	rn := runner{clips: clips, vad: vad, workers: max(f.workers, 1)}
	var jobs []func() ([]result, error)
	// SenseVoice 和在它的结果上纠错的几种方案
	jobs = append(jobs, func() ([]result, error) {
		p := models.Paths{Dir: f.models}
		sv := asr.SenseVoiceFile(cmp.Or(f.senseVoice, p.SenseVoice()), p.Tokens(), f.threads, f.language).WithProvider(f.provider)
		base, err := rn.recognize(ctx, "sensevoice", func([]string) asr.Model { return sv }, false)
		if err != nil {
			return nil, err
		}
		results := []result{base}
		if anyHotwords {
			started := time.Now()
			r := result{name: "sensevoice+拼音", segs: cloneAll(base.segs), postOnly: true}
			for i, c := range clips {
				correct.Homophones(r.segs[i], c.hotwords)
			}
			r.took = time.Since(started)
			r.wall = r.took
			results = append(results, r)
		}
		if f.llmURL != "" && f.llmModel != "" {
			from := results[len(results)-1]
			for _, withPinyin := range []bool{false, true} {
				r, err := llmCorrect(ctx, f, clips, from, withPinyin)
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

	report := render(clips, results, total)
	fmt.Print(report)
	if f.out != "" {
		return save(f.out, clips, results, report)
	}
	return nil
}

// llmCorrect 在 from 的结果上让大模型逐句校对，和线上处理会议的步骤一样。
// withPinyin 时每句附上拼音（correct.Input.Pinyin）。几段录音同时校对，不然推理模型一段要等一两分钟。
func llmCorrect(ctx context.Context, f flags, clips []clip, from result, withPinyin bool) (result, error) {
	cfg := settings.LLM{BaseURL: f.llmURL, Model: f.llmModel, APIKey: f.llmKey}
	chat := func(ctx context.Context, system, user string) (string, error) {
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
	sem := make(chan struct{}, 4)
	for i, c := range clips {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
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
	plain, err := rn.recognize(ctx, name, func([]string) asr.Model { return config(nil) }, false)
	if err != nil {
		return nil, err
	}
	if !anyHotwords {
		return []result{plain}, nil
	}
	biased, err := rn.recognize(ctx, name+"+热词", config, true)
	if err != nil {
		return nil, err
	}
	return []result{plain, biased}, nil
}

// recognize 用一种识别模型识别所有录音：同时加载 workers 份模型，各自从队列里取录音识别。
// perHotwords 为 true 时热词写在模型配置里，换到热词不一样的录音时要重新加载。
func (rn runner) recognize(ctx context.Context, name string, config func(hotwords []string) asr.Model, perHotwords bool) (result, error) {
	clips := rn.clips
	r := result{name: name, segs: make([][]transcript.Segment, len(clips))}
	type done struct {
		i          int
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
				segs, err := t.Transcribe(ctx, c.samples, func(float64) {})
				results <- done{i: i, segs: segs, took: time.Since(started), load: load, err: err}
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
		r.segs[d.i], r.took, r.load = d.segs, r.took+d.took, r.load+d.load
		log.Printf("%s：%s（%.0f 秒的录音用了 %.1f 秒）", name, clips[d.i].name, clips[d.i].seconds(), d.took.Seconds())
	}
	r.wall = time.Since(started)
	return r, errors.Join(errs...)
}

// loadClips 读出目录里所有有标准文字的录音，解码成 16k 单声道。filter 不为空时用它代替线上的响度归一化。
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
		pcm := filepath.Join(tmp, base+".f32")
		if err := decode(ctx, filepath.Join(dir, e.Name()), pcm, filter); err != nil {
			return nil, fmt.Errorf("%s：%w", e.Name(), err)
		}
		samples, err := audio.ReadPCM(pcm)
		if err != nil {
			return nil, err
		}
		clips = append(clips, clip{name: e.Name(), samples: samples, ref: string(ref), hotwords: hotwords})
	}
	if len(clips) == 0 {
		return nil, fmt.Errorf("%s 里没有配了标准文字（同名 .txt）的录音", dir)
	}
	return clips, nil
}

// decode 和线上一样解成 16k 单声道 float32；给了滤镜时用它代替线上的响度归一化。
func decode(ctx context.Context, in, pcm, filter string) error {
	if filter == "" {
		return audio.Decode(ctx, in, pcm)
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-loglevel", "error", "-y", "-i", in,
		"-map", "0:a:0", "-vn", "-af", filter, "-ac", "1", "-ar", fmt.Sprint(audio.SampleRate), "-f", "f32le", pcm)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg：%v：%s", err, strings.TrimSpace(string(out)))
	}
	return nil
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

// score 是一段（或几段加起来）的错字数和标准文字的字数，以及热词在标准文字里出现了几次、识别对了几次。
type score struct {
	errors, words  int
	hotRef, hotHit int
}

func (s score) cer() float64 {
	if s.words == 0 {
		return 0
	}
	return float64(s.errors) / float64(s.words)
}

func (s *score) add(o score) {
	s.errors += o.errors
	s.words += o.words
	s.hotRef += o.hotRef
	s.hotHit += o.hotHit
}

func scoreOf(c clip, hyp string) score {
	// 数字统一换成汉字读法再比（见 spokenNumbers）
	ref, got := correct.Tokens(spokenNumbers(c.ref)), correct.Tokens(spokenNumbers(hyp))
	s := score{errors: correct.Distance(ref, got), words: len(ref)}
	// 热词按完整的单位对：前后加上分隔符，「AI」不会算到「said」里
	join := func(tokens []string) string { return "|" + strings.Join(tokens, "|") + "|" }
	r, h := join(ref), join(got)
	for _, w := range c.hotwords {
		key := join(correct.Tokens(spokenNumbers(w)))
		if key == "||" {
			continue
		}
		n := strings.Count(r, key)
		s.hotRef += n
		s.hotHit += min(n, strings.Count(h, key))
	}
	return s
}

// render 排出报告：每种方案的总体结果，再是每段录音的字错误率。
func render(clips []clip, results []result, total float64) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "方案\t字错误率\t错字/总字数\t热词识别对\t耗时/录音时长\t实际用时\t加载模型")
	for _, r := range results {
		var sum score
		for i, c := range clips {
			sum.add(scoreOf(c, text(r.segs[i])))
		}
		hot := "-"
		if sum.hotRef > 0 {
			hot = fmt.Sprintf("%d/%d", sum.hotHit, sum.hotRef)
		}
		rtf := fmt.Sprintf("%.3f", r.took.Seconds()/total)
		if r.postOnly {
			rtf += "（只算纠错）"
		}
		fmt.Fprintf(w, "%s\t%.2f%%\t%d/%d\t%s\t%s\t%.0fs\t%.0fs\n", r.name, sum.cer()*100, sum.errors, sum.words, hot, rtf, r.wall.Seconds(), r.load.Seconds())
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
				row = append(row, fmt.Sprintf("%.2f%%", scoreOf(c, text(r.segs[i])).cer()*100))
			}
			fmt.Fprintln(w, strings.Join(row, "\t"))
		}
		w.Flush()
	}
	return b.String()
}

// save 把报告和每种方案识别出的文字写到 dir：dir/report.txt、dir/方案/录音名.txt。
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
			name := strings.TrimSuffix(c.name, filepath.Ext(c.name)) + ".txt"
			if err := os.WriteFile(filepath.Join(sub, name), []byte(text(r.segs[i])+"\n"), 0o644); err != nil {
				return err
			}
		}
	}
	log.Printf("识别出的文字和报告写在 %s", dir)
	return nil
}

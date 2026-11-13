package main

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ZaneXo/zsmeet/internal/asr"
	"github.com/ZaneXo/zsmeet/internal/audio"
	"github.com/ZaneXo/zsmeet/internal/config"
	"github.com/ZaneXo/zsmeet/internal/models"
	"github.com/ZaneXo/zsmeet/internal/transcript"
)

const transcribeUsage = `用法：zsmeet transcribe 录音文件 [选项]

给开发者调参数用：拿一个录音文件，走一遍和后台处理完全一样的转码、语音检测、识别、区分说话人，
打印每一句（带说话人和这一句原来的音量）和统计：几句、有字的时长、多长的空档里没有字、分出几位说话人。
不连数据库，不改任何东西。选项的默认值就是后台处理用的值；模型目录、线程数、识别语言和 worker 一样
读 ZSMEET_MODELS_DIR、ZSMEET_ASR_THREADS、ZSMEET_ASR_LANGUAGE。

用 docker compose 部署的服务器上（录音在 data 数据卷的 audio/ 下，文件名是会议的 ID）：

  docker compose run --rm --no-deps worker transcribe /data/audio/<会议 ID>.m4a -filter none

存在那里的是回放用的录音：加了音量处理以后才处理的会议，它已经按回放的处理（-playback-filter）调过音量，
-filter none 时送去识别的也不是原来的音量。

选项（写在文件名前面、后面都行）：
`

// transcribeFile 是 zsmeet transcribe：见 transcribeUsage。
func transcribeFile(ctx context.Context, args []string) error {
	cfg, err := config.LoadASR()
	if err != nil {
		return err
	}
	params := asr.DefaultParams()
	fs := flag.NewFlagSet("transcribe", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), transcribeUsage)
		fs.PrintDefaults()
	}
	speech := fs.String("filter", "production", "送去识别之前对声音做的处理：production 是后台处理用的，none 是不处理，\n别的当成 ffmpeg 的`滤镜`（-af 的写法），在转成 16k 单声道以后做")
	playback := fs.String("playback-filter", "production", "回放用的录音编码之前对声音做的处理，`滤镜`的写法同 -filter")
	m4aOut := fs.String("m4a", "", "把转好的回放用的录音存到这个`路径`，用来试听（默认不存）")
	fs.Var(float32Flag{&params.VADThreshold}, "vad-threshold", "语音检测的阈值：每一小段是人声的`概率`超过它算有人在说话")
	fs.Var(float32Flag{&params.MinSilence}, "min-silence", "安静多少`秒`算一句话说完了")
	fs.Var(float32Flag{&params.MinSpeech}, "min-speech", "短于多少`秒`的声音不算说话")
	fs.Var(float32Flag{&params.MaxSpeech}, "max-speech", "一句话最长多少`秒`，再长就切开")
	cluster := fs.String("cluster", strconv.FormatFloat(float64(params.ClusterThreshold), 'g', -1, 32),
		"区分说话人的聚类`阈值`，越小分出的人越多。用逗号隔开写几个时每个都分一次，比较分出几位说话人\n（打印的每一句按第一个算）；none 是不区分说话人")
	minHole := fs.Float64("hole", 10, "多少`秒`以上没有字算一个空档")
	statsOnly := fs.Bool("stats-only", false, "只打印统计，不打印每一句")

	file, err := parseFileArgs(fs, args)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	if params.VADThreshold <= 0 || params.VADThreshold >= 1 {
		return errors.New("-vad-threshold 要在 0 到 1 之间")
	}
	if params.MinSilence <= 0 || params.MinSpeech <= 0 || params.MaxSpeech <= params.MinSpeech {
		return errors.New("-min-silence、-min-speech 要大于 0，-max-speech 要大于 -min-speech")
	}
	thresholds, err := parseThresholds(*cluster)
	if err != nil {
		return err
	}
	speechFilter, playbackFilter := pickFilter(*speech, audio.SpeechFilter), pickFilter(*playback, audio.PlaybackFilter)

	if err := models.Ensure(ctx, cfg.ModelsDir, cfg.ModelMirror, logDownload()); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "zsmeet-transcribe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	// 和后台处理一样：一次转出识别用的 PCM 和回放用的录音
	started := time.Now()
	pcm := filepath.Join(tmp, "speech.f32")
	m4a := cmp.Or(*m4aOut, filepath.Join(tmp, "playback.m4a"))
	if err := audio.ConvertWith(ctx, file, pcm, m4a, speechFilter, playbackFilter); err != nil {
		return err
	}
	samples, err := audio.ReadPCM(pcm)
	if err != nil {
		return err
	}
	if len(samples) < audio.SampleRate {
		return errors.New("录音不到 1 秒")
	}
	// 原始的声音（不做处理）：看处理前后的音量，以及每一句原来有多响（原来很轻的地方多出来的字可能是把噪音认成了字）
	original := samples
	if speechFilter != "" {
		raw := filepath.Join(tmp, "original.f32")
		if err := audio.DecodeWith(ctx, file, raw, ""); err != nil {
			return err
		}
		if original, err = audio.ReadPCM(raw); err != nil {
			return err
		}
	}
	convertTook := time.Since(started)

	started = time.Now()
	engine, err := asr.LoadWith(models.Paths{Dir: cfg.ModelsDir}, cfg.Threads, cfg.Language, params)
	if err != nil {
		return err
	}
	loadTook := time.Since(started)

	started = time.Now()
	segs, err := engine.Transcribe(ctx, samples, func(float64) {})
	if err != nil {
		return err
	}
	transcribeTook := time.Since(started)

	type diarized struct {
		threshold          float32
		clusters, speakers int
	}
	var speakers []diarized
	started = time.Now()
	for i, threshold := range thresholds {
		if err := ctx.Err(); err != nil {
			return err
		}
		engine.SetClusterThreshold(threshold)
		turns := engine.Diarize(samples)
		assigned := slices.Clone(segs)
		ids := transcript.AssignSpeakers(assigned, turns)
		clusters := map[int]bool{}
		for _, t := range turns {
			clusters[t.Speaker] = true
		}
		speakers = append(speakers, diarized{threshold, len(clusters), len(ids)})
		if i == 0 {
			segs = assigned
		}
	}
	diarizeTook := time.Since(started)

	duration := float64(len(samples)) / audio.SampleRate
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	fmt.Fprintf(out, "录音：%s，%.1f 秒\n", file, duration)
	fmt.Fprintf(out, "识别前的处理：%s\n", cmp.Or(speechFilter, "不处理"))
	fmt.Fprintf(out, "回放用的录音的处理：%s\n", cmp.Or(playbackFilter, "不处理"))
	fmt.Fprintf(out, "语音检测：阈值 %g，安静 %g 秒算一句说完，短于 %g 秒的不要，长于 %g 秒的切开\n\n",
		params.VADThreshold, params.MinSilence, params.MinSpeech, params.MaxSpeech)
	if !*statsOnly {
		for _, s := range segs {
			fmt.Fprintf(out, "[%s-%s] %s（原来 %.0f dB）：%s\n", transcript.Clock(s.Start), transcript.Clock(s.End),
				transcript.SpeakerName(s.Speaker), level(original, s.Start, s.End), s.Text)
		}
		fmt.Fprintln(out)
	}

	c := cover(segs, duration, *minHole)
	holes := make([]string, len(c.holes))
	holeSum := 0.0
	for i, h := range c.holes {
		holes[i] = fmt.Sprintf("%s-%s", transcript.Clock(h[0]), transcript.Clock(h[1]))
		holeSum += h[1] - h[0]
	}
	fmt.Fprintln(out, "统计")
	fmt.Fprintf(out, "  句子：%d\n", len(segs))
	fmt.Fprintf(out, "  有字的时长：%.1f 秒（%.0f%%）\n", c.covered, 100*c.covered/duration)
	fmt.Fprintf(out, "  超过 %g 秒没有字的空档：%d 个，共 %.1f 秒 %s\n", *minHole, len(c.holes), holeSum, strings.Join(holes, " "))
	for _, d := range speakers {
		fmt.Fprintf(out, "  说话人（聚类阈值 %g）：%d 位（区分说话人分出 %d 个）\n", d.threshold, d.speakers, d.clusters)
	}
	a, b := measure(original, segs), measure(samples, segs)
	fmt.Fprintf(out, "  音量（原来 → 送去识别的，dBFS）：整体 %.1f → %.1f，说话的部分 %.1f → %.1f，底噪 %.1f → %.1f\n",
		a.overall, b.overall, a.speech, b.speech, a.floor, b.floor)
	fmt.Fprintf(out, "  用时：转码 %.1f 秒，加载模型 %.1f 秒，识别 %.1f 秒，区分说话人 %.1f 秒\n",
		convertTook.Seconds(), loadTook.Seconds(), transcribeTook.Seconds(), diarizeTook.Seconds())
	return nil
}

// parseFileArgs 读出录音文件和选项。选项写在文件名前面、后面都行（flag 包遇到第一个不是选项的参数就停了）。
func parseFileArgs(fs *flag.FlagSet, args []string) (string, error) {
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return "", errors.New("没有指定录音文件")
	}
	file := fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return "", err
	}
	if fs.NArg() > 0 {
		return "", fmt.Errorf("一次只能处理一个录音文件，多出来的参数：%s", strings.Join(fs.Args(), " "))
	}
	return file, nil
}

// pickFilter 把 -filter 的值换成真正的滤镜：production 是后台处理用的，none 是不处理。
func pickFilter(value, production string) string {
	switch value {
	case "production":
		return production
	case "none":
		return ""
	}
	return value
}

// parseThresholds 读 -cluster：逗号隔开的几个阈值，none 是不区分说话人。
func parseThresholds(s string) ([]float32, error) {
	if s == "none" {
		return nil, nil
	}
	var out []float32
	for f := range strings.SplitSeq(s, ",") {
		v, err := strconv.ParseFloat(strings.TrimSpace(f), 32)
		if err != nil || v <= 0 {
			return nil, fmt.Errorf("-cluster 里的 %q 不是大于 0 的数", f)
		}
		out = append(out, float32(v))
	}
	return out, nil
}

// float32Flag 让 flag 包能读 float32 的选项（语音检测的参数都是 float32）。
type float32Flag struct{ p *float32 }

func (f float32Flag) String() string {
	if f.p == nil {
		return ""
	}
	return strconv.FormatFloat(float64(*f.p), 'g', -1, 32)
}

func (f float32Flag) Set(s string) error {
	v, err := strconv.ParseFloat(s, 32)
	if err != nil {
		return err
	}
	*f.p = float32(v)
	return nil
}

// coverage 是转写覆盖了录音的多少。
type coverage struct {
	// 有字的总时长（几句重叠的部分只算一次），单位秒
	covered float64
	// 比 minHole 长的、一句话都没有的空档（开始、结束的秒数），包括开头和结尾的
	holes [][2]float64
}

func cover(segs []transcript.Segment, duration, minHole float64) coverage {
	spans := make([][2]float64, len(segs))
	for i, s := range segs {
		spans[i] = [2]float64{s.Start, s.End}
	}
	slices.SortFunc(spans, func(a, b [2]float64) int { return cmp.Compare(a[0], b[0]) })
	var c coverage
	// reached：从 0 到这里，有字的地方都已经算进去了
	reached := 0.0
	for _, sp := range spans {
		if sp[0]-reached > minHole {
			c.holes = append(c.holes, [2]float64{reached, sp[0]})
		}
		if sp[1] > reached {
			c.covered += sp[1] - max(sp[0], reached)
			reached = sp[1]
		}
	}
	if duration-reached > minHole {
		c.holes = append(c.holes, [2]float64{reached, duration})
	}
	return c
}

// levels 是一段录音的几个音量，单位 dBFS。
type levels struct {
	// 整段的均方根
	overall float64
	// 转写里各句所在的时间段合起来的均方根
	speech float64
	// 底噪：每 0.1 秒算一个均方根，从小到大排在 10% 处的那个
	floor float64
}

func measure(samples []float32, segs []transcript.Segment) levels {
	var speech, n float64
	for _, s := range segs {
		from, to := span(len(samples), s.Start, s.End)
		speech += sumSquares(samples[from:to])
		n += float64(to - from)
	}
	const frame = audio.SampleRate / 10
	var frames []float64
	for i := 0; i+frame <= len(samples); i += frame {
		frames = append(frames, sumSquares(samples[i:i+frame])/frame)
	}
	slices.Sort(frames)
	l := levels{overall: dB(sumSquares(samples) / float64(max(len(samples), 1))), speech: dB(speech / max(n, 1)), floor: dB(0)}
	if len(frames) > 0 {
		l.floor = dB(frames[len(frames)/10])
	}
	return l
}

// level 是 from 到 to 秒之间的均方根，单位 dBFS。
func level(samples []float32, from, to float64) float64 {
	a, b := span(len(samples), from, to)
	return dB(sumSquares(samples[a:b]) / float64(max(b-a, 1)))
}

// span 把秒换成采样的下标（不含 to），超出录音的部分截掉。
func span(n int, from, to float64) (int, int) {
	a := min(max(int(from*audio.SampleRate), 0), n)
	return a, min(max(int(to*audio.SampleRate), a), n)
}

func sumSquares(samples []float32) float64 {
	var sum float64
	for _, v := range samples {
		sum += float64(v) * float64(v)
	}
	return sum
}

// dB 把均方（不是均方根）换成 dBFS，最小 -120。
func dB(meanSquare float64) float64 {
	return max(10*math.Log10(meanSquare), -120)
}

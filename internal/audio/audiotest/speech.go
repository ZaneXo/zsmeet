package audiotest

import (
	"encoding/binary"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// SpeechText 是 Speech 里说的话（用 say 合成时），句与句之间停 1.5 秒。
var SpeechText = []string{
	"大家好，我们开始今天的站会。",
	"昨天我把录音的上传接口改完了，今天准备修一下转写太慢的问题。",
	"前端这边，会议列表的分页已经做好了，还差搜索框的样式。",
	"好的，那我们下午三点再对一下发布的时间。",
}

// NoiseLevel 是 Speech 垫的底噪的均方根（dBFS），和录音的音量无关：
// 没开自动增益的麦克风录到的底噪本来就在这一带，人离得远、说话轻，只是人声小了。
const NoiseLevel = -66

// Speech 生成一段桌面版录音格式的 AAC（整段 ADTS 流）：开头静 1 秒，然后是 SpeechText 那几句中文，
// 句间停 1.5 秒，最后静 1 秒，全程垫着 NoiseLevel 的底噪。gain 是人声的增益（dB）：0 是正常说话的音量
// （说话时均方根约 -20 到 -18 dBFS），-20 像离麦克风远、又没有自动增益的录音。
//
// 用 macOS 的 say（婷婷）合成；没有 say 或者没有婷婷这个声音时（比如在 Linux 上跑 CI），
// 用合成的元音代替（音量、停顿和说话差不多，用来测音量处理够了，识别不出字）。
func Speech(t testing.TB, gain float64) []byte {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("没有安装 ffmpeg")
	}
	dir := t.TempDir()
	voice := tts(t, dir)
	if voice == nil {
		voice = vowels()
	}
	pad := make([]float64, SampleRate)
	samples := slices.Concat(pad, voice, pad)

	// 底噪：固定种子的白噪声，每次生成的一样
	rng := rand.New(rand.NewPCG(1, 2))
	scale := math.Pow(10, gain/20)
	noise := math.Pow(10, NoiseLevel/20.0)
	pcm := make([]byte, 0, 4*len(samples))
	for _, v := range samples {
		pcm = binary.LittleEndian.AppendUint32(pcm, math.Float32bits(float32(v*scale+noise*rng.NormFloat64())))
	}
	raw, out := filepath.Join(dir, "speech.f32"), filepath.Join(dir, "speech.aac")
	if err := os.WriteFile(raw, pcm, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "f32le", "-ar", "48000", "-ac", "1", "-i", raw,
		"-c:a", "aac", "-b:a", "64k", "-f", "adts", out)
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg 编码 AAC 失败：%v\n%s", err, msg)
	}
	stream, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return stream
}

// CanSpeak 报告这台机器能不能用 say 合成中文（macOS，装着婷婷这个声音）。
// 不能的话 Speech 用合成的元音代替，识别不出字，要看识别结果的测试就该跳过。
func CanSpeak() bool {
	if _, err := exec.LookPath("say"); err != nil {
		return false
	}
	voices, err := exec.Command("say", "-v", "?").Output()
	return err == nil && strings.Contains(string(voices), "Tingting")
}

// tts 用 say 合成 SpeechText，返回 48 kHz 单声道的采样（正常说话的音量）。合成不了时返回 nil。
func tts(t testing.TB, dir string) []float64 {
	t.Helper()
	if !CanSpeak() {
		return nil
	}
	aiff := filepath.Join(dir, "tts.aiff")
	// [[slnc 毫秒]] 是 say 的停顿命令
	text := strings.Join(SpeechText, "[[slnc 1500]]")
	if err := exec.CommandContext(t.Context(), "say", "-v", "Tingting", "-o", aiff, text).Run(); err != nil {
		return nil
	}
	raw := filepath.Join(dir, "tts.f32")
	cmd := exec.CommandContext(t.Context(), "ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-i", aiff, "-ac", "1", "-ar", "48000", "-f", "f32le", raw)
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg 解不开 say 合成的声音：%v\n%s", err, msg)
	}
	data, err := os.ReadFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]float64, len(data)/4)
	for i := range out {
		out[i] = float64(math.Float32frombits(binary.LittleEndian.Uint32(data[4*i:])))
	}
	return out
}

// vowels 代替说话声：4 句，每句 12 个 0.2 秒的「音节」，音节之间空 0.05 秒，句间停 1.5 秒。
// 每个音节是基频 110 到 220 Hz 的一串脉冲（12 个谐波，越高越弱），前后 25 毫秒淡入淡出，
// 轻重交替（最轻的比最重的小 8 dB）。峰值和均方根之比和真人说话差不多，说话时的均方根约 -20 dBFS。
func vowels() []float64 {
	const syllable, ramp, gap, pause = 0.2 * SampleRate, 0.025 * SampleRate, 0.05 * SampleRate, 1.5 * SampleRate
	stress := []float64{1, 0.5, 0.75, 0.4, 0.65, 0.55}
	var out []float64
	for s := range 4 {
		if s > 0 {
			out = append(out, make([]float64, pause)...)
		}
		for k := range 12 {
			n := s*12 + k
			f0 := 110 + 110*math.Abs(math.Sin(float64(n)))
			for i := range int(syllable) {
				env := 1.0
				if edge := min(float64(i), syllable-1-float64(i)); edge < ramp {
					env = 0.5 - 0.5*math.Cos(math.Pi*edge/ramp)
				}
				v := 0.0
				for h := 1.0; h <= 12; h++ {
					v += math.Cos(2*math.Pi*f0*h*float64(i)/SampleRate) / math.Pow(h, 0.75)
				}
				out = append(out, 0.17*stress[n%len(stress)]*env*v)
			}
			out = append(out, make([]float64, gap)...)
		}
	}
	return out
}

// SpeechLevel 是一段声音里说话的部分有多响（dBFS）：每 0.1 秒算一个均方根，
// 比最响的一帧小不到 20 dB 的帧算说话（底噪、停顿不算），取它们的平均功率。rate 是采样率。
func SpeechLevel(samples []float32, rate int) float64 {
	n := rate / 10
	var levels []float64
	for i := 0; i+n <= len(samples); i += n {
		sum := 0.0
		for _, v := range samples[i : i+n] {
			sum += float64(v) * float64(v)
		}
		levels = append(levels, max(sum/float64(n), 1e-12))
	}
	if len(levels) == 0 {
		return math.Inf(-1)
	}
	loudest := slices.Max(levels)
	sum, count := 0.0, 0
	for _, l := range levels {
		if l > loudest/100 {
			sum += l
			count++
		}
	}
	return 10 * math.Log10(sum/float64(count))
}

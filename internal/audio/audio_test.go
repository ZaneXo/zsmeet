package audio

import (
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ZaneXo/zsmeet/internal/audio/audiotest"
)

// 没开自动增益、离麦克风远的录音（比正常说话小 20 dB）：送去识别的声音调到正常说话的音量。
func TestConvertRaisesQuietSpeech(t *testing.T) {
	in := writeFile(t, "quiet.aac", audiotest.Speech(t, -20))
	speech, original := convert(t, in), decodeOriginal(t, in)
	before, after := audiotest.SpeechLevel(original, SampleRate), audiotest.SpeechLevel(speech, SampleRate)
	t.Logf("说话的音量 %.1f → %.1f dBFS，底噪 %.1f → %.1f dBFS", before, after, floorLevel(original), floorLevel(speech))
	if before > -34 {
		t.Fatalf("测试用的录音应该很轻（-38 dBFS 左右），实际 %.1f dBFS", before)
	}
	if after < -24 || after > -12 {
		t.Errorf("送去识别时说话的音量应该在 -24 到 -12 dBFS 之间，实际 %.1f dBFS", after)
	}
}

// 正常音量的录音（网页录音有浏览器的自动增益，上传的录音大多也是正常音量）：送去识别的声音差不多不变。
func TestConvertKeepsNormalSpeech(t *testing.T) {
	in := writeFile(t, "normal.aac", audiotest.Speech(t, 0))
	speech, original := convert(t, in), decodeOriginal(t, in)
	before, after := audiotest.SpeechLevel(original, SampleRate), audiotest.SpeechLevel(speech, SampleRate)
	t.Logf("说话的音量 %.1f → %.1f dBFS，峰值 %.2f → %.2f", before, after, peak(original), peak(speech))
	if math.Abs(after-before) > 6 {
		t.Errorf("正常音量的录音不该大动：说话的音量从 %.1f 变成了 %.1f dBFS", before, after)
	}
	if p := peak(speech); p > 1 {
		t.Errorf("处理后的峰值 %.2f 超过了满幅", p)
	}
}

// 回放用的录音：很轻的录音调响一些（听的时候不用把音量开到最大），正常音量的录音基本不动。
func TestConvertPlayback(t *testing.T) {
	for _, c := range []struct {
		name string
		gain float64
		// 回放的录音里说话的音量比原来大多少（dB）
		min, max float64
	}{
		{"很轻的录音", -20, 8, 22},
		{"正常音量的录音", 0, -2, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := writeFile(t, "speech.aac", audiotest.Speech(t, c.gain))
			dir := t.TempDir()
			m4a := filepath.Join(dir, "playback.m4a")
			if err := Convert(t.Context(), in, filepath.Join(dir, "speech.f32"), m4a); err != nil {
				t.Fatal(err)
			}
			original, playback := decodeOriginal(t, in), decodeOriginal(t, m4a)
			before, after := audiotest.SpeechLevel(original, SampleRate), audiotest.SpeechLevel(playback, SampleRate)
			t.Logf("说话的音量 %.1f → %.1f dBFS，峰值 %.2f → %.2f", before, after, peak(original), peak(playback))
			if d := after - before; d < c.min || d > c.max {
				t.Errorf("回放的录音里说话的音量应该比原来大 %v 到 %v dB，实际从 %.1f 变成了 %.1f dBFS", c.min, c.max, before, after)
			}
			if p := peak(playback); p > 1 {
				t.Errorf("回放的录音削波了：峰值 %.2f", p)
			}
			if len(playback) != len(original) {
				t.Errorf("回放的录音有 %d 个采样，原来有 %d 个", len(playback), len(original))
			}
		})
	}
}

// 音量处理不能挪动声音：送去识别的和原来的一样长，一个采样都不差，原来在第 5 秒的声音还在第 5 秒。
// 转写里每句的时间就是按这个算的，差一点点点击跳转就对不上了。
func TestConvertKeepsTiming(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("没有安装 ffmpeg")
	}
	// 12 秒很轻的 300 Hz（像远处说话），第 5 秒有一下很短的「咔哒」
	in := filepath.Join(t.TempDir(), "click.wav")
	cmd := exec.CommandContext(t.Context(), "ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "aevalsrc='0.002*sin(2*PI*300*t)+if(between(t,5,5.001),0.05,0)':s=48000:d=12", "-ac", "1", in)
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg 生成测试音频失败：%v\n%s", err, msg)
	}
	speech, original := convert(t, in), decodeOriginal(t, in)
	if len(speech) != len(original) {
		t.Fatalf("处理后有 %d 个采样，原来有 %d 个", len(speech), len(original))
	}
	if got, want := loudest(speech), loudest(original); got != want {
		t.Errorf("「咔哒」原来在第 %d 个采样，处理后在第 %d 个", want, got)
	}
}

// writeFile 把测试用的录音写到临时目录里。
func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// convert 和后台处理一样转一遍，返回送去识别的声音。
func convert(t *testing.T, in string) []float32 {
	t.Helper()
	dir := t.TempDir()
	pcm := filepath.Join(dir, "speech.f32")
	if err := Convert(t.Context(), in, pcm, filepath.Join(dir, "playback.m4a")); err != nil {
		t.Fatal(err)
	}
	return readPCM(t, pcm)
}

// decodeOriginal 解出不做任何处理的声音。
func decodeOriginal(t *testing.T, in string) []float32 {
	t.Helper()
	pcm := filepath.Join(t.TempDir(), "original.f32")
	if err := DecodeWith(t.Context(), in, pcm, ""); err != nil {
		t.Fatal(err)
	}
	return readPCM(t, pcm)
}

func readPCM(t *testing.T, path string) []float32 {
	t.Helper()
	samples, err := ReadPCM(path)
	if err != nil {
		t.Fatal(err)
	}
	return samples
}

// floorLevel 是底噪（dBFS）：每 0.1 秒算一个均方根，从小到大排在 10% 处的那个。
func floorLevel(samples []float32) float64 {
	const n = SampleRate / 10
	var levels []float64
	for i := 0; i+n <= len(samples); i += n {
		sum := 0.0
		for _, v := range samples[i : i+n] {
			sum += float64(v) * float64(v)
		}
		levels = append(levels, 10*math.Log10(max(sum/n, 1e-12)))
	}
	slices.Sort(levels)
	return levels[len(levels)/10]
}

func peak(samples []float32) float64 {
	p := 0.0
	for _, v := range samples {
		p = max(p, math.Abs(float64(v)))
	}
	return p
}

// loudest 是绝对值最大的那个采样的下标。
func loudest(samples []float32) int {
	at, p := 0, float32(0)
	for i, v := range samples {
		if v = max(v, -v); v > p {
			at, p = i, v
		}
	}
	return at
}

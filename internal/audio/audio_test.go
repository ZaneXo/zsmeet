package audio

import (
	"math"
	"os/exec"
	"path/filepath"
	"testing"
)

// 很小声的录音（-45 dBFS 的正弦波）转出来给识别用的 PCM 应该被放大到正常音量
func TestQuietAudioIsNormalized(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("没有安装 ffmpeg")
	}
	dir := t.TempDir()
	in := filepath.Join(dir, "quiet.wav")
	gen := exec.CommandContext(t.Context(), "ffmpeg", "-nostdin", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=5", "-af", "volume=-45dB", in)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("生成测试录音：%v\n%s", err, out)
	}

	pcm, m4a := filepath.Join(dir, "a.f32"), filepath.Join(dir, "a.m4a")
	if err := Convert(t.Context(), in, pcm, m4a); err != nil {
		t.Fatal(err)
	}
	checkLoud(t, "Convert", pcm)

	// 重试时从存好的回放录音重新解码，也要归一化
	again := filepath.Join(dir, "b.f32")
	if err := Decode(t.Context(), m4a, again); err != nil {
		t.Fatal(err)
	}
	checkLoud(t, "Decode", again)
}

func checkLoud(t *testing.T, name, pcm string) {
	t.Helper()
	samples, err := ReadPCM(pcm)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(samples) / SampleRate; got < 4 || got > 6 {
		t.Errorf("%s：转出来 %d 秒，应该是 5 秒左右", name, got)
	}
	var sum float64
	for _, s := range samples {
		sum += float64(s) * float64(s)
	}
	// 原来的有效值约 0.004（-48 dBFS），归一化到 -20 LUFS 以后应该在 0.05 以上
	if rms := math.Sqrt(sum / float64(len(samples))); rms < 0.05 {
		t.Errorf("%s：有效值 %.4f，没有做响度归一化", name, rms)
	}
}

// Package audiotest 给测试准备和桌面版录音程序一样的录音：48 kHz 单声道、约 64 kbps 的 AAC-LC，
// ADTS 分帧，再像录音程序那样只在帧的边界上切成分片。用 ffmpeg 编码，没有安装 ffmpeg 时跳过测试。
package audiotest

import (
	"encoding/binary"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const (
	// SampleRate 是桌面版录音的采样率
	SampleRate = 48000
	// FrameSamples 是一个 AAC 帧里的采样数
	FrameSamples = 1024
)

// Tones 生成一段桌面版录音格式的 AAC（整段 ADTS 流）：一段接一段的正弦波，每段 seconds 秒，频率依次是 hz。
// 每段音高不同，解码以后能听出前后顺序有没有乱。
func Tones(t testing.TB, seconds float64, hz ...float64) []byte {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("没有安装 ffmpeg")
	}
	dir := t.TempDir()
	pcm, out := filepath.Join(dir, "tones.pcm"), filepath.Join(dir, "tones.aac")
	if err := os.WriteFile(pcm, sine(seconds, hz), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "s16le", "-ar", "48000", "-ac", "1", "-i", pcm,
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

// sine 生成 16 位单声道的裸 PCM：一段接一段的正弦波。相位接着上一段走，换音高的地方没有爆音。
func sine(seconds float64, hz []float64) []byte {
	n := int(seconds * SampleRate)
	out := make([]byte, 0, 2*n*len(hz))
	phase := 0.0
	for _, f := range hz {
		for range n {
			out = binary.LittleEndian.AppendUint16(out, uint16(int16(10000*math.Sin(phase))))
			phase += 2 * math.Pi * f / SampleRate
		}
	}
	return out
}

// Split 像录音程序那样把 ADTS 流切成分片：每片 seconds 秒，最后一片可能短一些。
// 只在帧的边界上切，分片按序号首尾相接就是原来的流。
func Split(t testing.TB, stream []byte, seconds float64) [][]byte {
	t.Helper()
	perChunk := int(math.Round(seconds * SampleRate / FrameSamples))
	var chunks [][]byte
	start, pos := 0, 0
	for i, size := range frameSizes(t, stream) {
		pos += size
		if (i+1)%perChunk == 0 || pos == len(stream) {
			chunks = append(chunks, stream[start:pos])
			start = pos
		}
	}
	return chunks
}

// Seconds 是 ADTS 流解码出来有多长：每帧 FrameSamples 个采样。
// 比编码前的声音略长一点：编码器在开头垫了一帧左右的静音，最后一帧也要补满。
func Seconds(t testing.TB, stream []byte) float64 {
	t.Helper()
	return float64(len(frameSizes(t, stream))*FrameSamples) / SampleRate
}

// frameSizes 逐帧读 ADTS 帧头，返回每一帧（含帧头）的字节数。顺便确认每一帧都是录音程序的格式：
// AAC-LC、48 kHz、单声道。
func frameSizes(t testing.TB, stream []byte) []int {
	t.Helper()
	var sizes []int
	for pos := 0; pos < len(stream); {
		h := stream[pos:]
		// 帧头 7 个字节：12 位同步字全是 1、layer 是 0；帧长 13 位，算上帧头
		if len(h) < 7 || h[0] != 0xFF || h[1]&0xF6 != 0xF0 {
			t.Fatalf("第 %d 字节不是 ADTS 帧头", pos)
		}
		profile, rate, channels := h[2]>>6, h[2]>>2&0x0F, (h[2]&0x01)<<2|h[3]>>6
		// profile 1 是 AAC-LC，采样率编号 3 是 48 kHz
		if profile != 1 || rate != 3 || channels != 1 {
			t.Fatalf("第 %d 字节的帧不是 AAC-LC、48 kHz、单声道（profile %d，采样率编号 %d，声道 %d）", pos, profile, rate, channels)
		}
		size := int(h[3]&0x03)<<11 | int(h[4])<<3 | int(h[5])>>5
		if size < 7 || size > len(h) {
			t.Fatalf("第 %d 字节的 ADTS 帧长 %d 不对", pos, size)
		}
		sizes = append(sizes, size)
		pos += size
	}
	return sizes
}

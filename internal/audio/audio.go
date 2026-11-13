// Package audio 用 ffmpeg 把上传的音视频转成识别和回放要用的格式。
package audio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"unsafe"
)

// SampleRate 是识别模型要求的采样率。
const SampleRate = 16000

// MaxSeconds 是单场录音的时长上限。整段音频要读进内存（每小时约 230 MB）。
const MaxSeconds = 6 * 3600

// ErrBadMedia 表示 ffmpeg 解不开这个文件。
var ErrBadMedia = errors.New("音频无法解码：文件可能已损坏，或者不是音视频文件")

// ErrTooLong 表示录音超过了时长上限。
var ErrTooLong = fmt.Errorf("录音超过 %d 小时，暂时处理不了，请拆分后再上传", MaxSeconds/3600)

// normalize 是给识别用的音频做的响度归一化（EBU R128，目标 -20 LUFS）。
//
// 会议室里离麦克风远的人声音很小，语音检测（Silero VAD）会把小声的话当成静音扔掉，根本到不了识别模型。
// 在公开的会议录音上量过（tools/asreval，单路麦克风）：加上它以后字错误率
// AliMeeting 从 45–49% 降到 30%，AISHELL-4 从 34% 降到 21%；本来音量就够的录音几乎不受影响。
// 网页录音开了浏览器的自动增益，受益的主要是上传的录音文件和桌面版录的系统声音。
// 回放用的录音不做处理，听到的还是原来的声音。
const normalize = "loudnorm=I=-20"

// Convert 读任意音视频，同时输出两份：
// pcmOut 是 16k 单声道 float32 裸数据（做过响度归一化），给识别用；m4aOut 是 AAC，给网页回放用。
func Convert(ctx context.Context, in, pcmOut, m4aOut string) error {
	return ffmpeg(ctx,
		"-i", in,
		"-map", "0:a:0", "-vn", "-af", normalize, "-ac", "1", "-ar", fmt.Sprint(SampleRate), "-f", "f32le", pcmOut,
		"-map", "0:a:0", "-vn", "-ac", "1", "-c:a", "aac", "-b:a", "64k", "-movflags", "+faststart", m4aOut,
	)
}

// Decode 把已经存好的录音重新解成识别用的 PCM（重试时用），和 Convert 一样做响度归一化。
func Decode(ctx context.Context, in, pcmOut string) error {
	return ffmpeg(ctx,
		"-i", in,
		"-map", "0:a:0", "-vn", "-af", normalize, "-ac", "1", "-ar", fmt.Sprint(SampleRate), "-f", "f32le", pcmOut,
	)
}

func ffmpeg(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "ffmpeg", append([]string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y"}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, exec.ErrNotFound) {
		return errors.New("服务器上没有安装 ffmpeg")
	}
	// ffmpeg 的原话留在日志里排查用，给用户看的是 ErrBadMedia
	return fmt.Errorf("%w（ffmpeg：%s）", ErrBadMedia, lastLine(stderr.String()))
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// ReadPCM 把 Convert/Decode 输出的 float32 裸数据整个读进内存。
func ReadPCM(path string) ([]float32, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	n := int(info.Size() / 4)
	if n > MaxSeconds*SampleRate {
		return nil, ErrTooLong
	}
	if n == 0 {
		return nil, nil
	}
	samples := make([]float32, n)
	// 直接读进 float32 切片的内存里，省一次拷贝。ffmpeg 输出的是小端，和我们支持的平台一致
	raw := unsafe.Slice((*byte)(unsafe.Pointer(unsafe.SliceData(samples))), n*4)
	if _, err := io.ReadFull(f, raw); err != nil {
		return nil, err
	}
	return samples, nil
}

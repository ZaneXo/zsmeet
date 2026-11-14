// Package audio 用 ffmpeg 把上传的音视频转成识别和回放要用的格式，顺便把音量调匀。
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

// SpeechFilter 是送去识别之前对声音做的处理（ffmpeg 的滤镜，-af 的写法），在转成 16k 单声道以后做：
// 动态地把音量调匀。每 0.25 秒看一次峰值，按前后几秒平滑地调增益，让说话的峰值到 0.9，最多放大 20 倍（26 dB）。
//
// 没有自动增益的麦克风录的会，人离麦克风远时说话比正常小 20 dB 左右：语音检测当成静音丢掉，识别也认得差。
// 以前的桌面版就是这样直接录麦克风的（现在录音小程序在混音之前自己调，见 docs/desktop.md 的「麦克风自动增益」），
// 旧版本录的、自动增益没能打开的、上传的录音，以及以前的会议重新转写时，都靠这一步。
// 在一场 11 分钟的真实站会（-39 LUFS）上用 zsmeet transcribe 比较过：不处理只认出 75 句、
// 26% 的时间有字，这样处理后 118 句、75% 的时间有字，多出来的是原来漏掉的话；loudnorm、speechnorm、
// 固定加 20 dB 都不如它，再调快、放大得更多也差不多。只有底噪的地方放大了也认不出字。
// 正常音量的录音（比如网页录音）只响两三 dB，识别结果不变；桌面版自动调过的（说话在 -23 dBFS 左右）
// 再响 8 dB 左右，送去识别时和正常音量的差不多大。时长和时间点一个采样都不变，转写的时间照样对得上录音。
// 声纹（CAM++，按这份声音算）几乎不受影响：同一段合成的说话处理前后算出来的余弦相似度在 0.996 以上
// （认人的门槛是 0.6），加这一步以前存的声纹照样能用。
const SpeechFilter = "dynaudnorm=f=250:g=15:p=0.9:m=20"

// PlaybackFilter 是回放用的录音编码之前对声音做的处理，在转成单声道以后做，采样率不变：
// 和 SpeechFilter 一样动态地调匀音量，只是更慢、更轻（每 0.5 秒看一次，按前后十几秒平滑，峰值到 0.7，
// 最多放大 10 倍），听起来不会忽大忽小。上面那场站会回放时从 -39 LUFS 调到 -27 LUFS，相邻 0.1 秒的增益变化
// 95% 在 0.2 dB 以内，说话和停顿的音量差从 27 dB 变成 22 dB（停顿处的底噪相对响了一点）；正常音量的录音基本不动。
// 重新转写时从这份录音解码（Decode），再按 SpeechFilter 调一遍，认出来的不比第一次少。
const PlaybackFilter = "dynaudnorm=f=500:g=31:p=0.7:m=10"

// Convert 读任意音视频，同时输出两份：
// pcmOut 是 16k 单声道 float32 裸数据，给识别用，先用 SpeechFilter 处理过；
// m4aOut 是 AAC，给网页回放用，先用 PlaybackFilter 处理过。
func Convert(ctx context.Context, in, pcmOut, m4aOut string) error {
	return ConvertWith(ctx, in, pcmOut, m4aOut, SpeechFilter, PlaybackFilter)
}

// ConvertWith 和 Convert 一样，只是两份各用指定的滤镜，空字符串表示不处理。zsmeet transcribe 用它比较不同的处理。
func ConvertWith(ctx context.Context, in, pcmOut, m4aOut, speech, playback string) error {
	args := append([]string{"-i", in}, pcmArgs(pcmOut, speech)...)
	args = append(args, "-map", "0:a:0", "-vn")
	if playback != "" {
		args = append(args, "-af", "aformat=channel_layouts=mono,"+playback)
	}
	args = append(args, "-ac", "1", "-c:a", "aac", "-b:a", "64k", "-movflags", "+faststart", m4aOut)
	return ffmpeg(ctx, args...)
}

// Decode 把已经存好的录音重新解成识别用的 PCM（重新转写时用），和 Convert 一样先用 SpeechFilter 处理。
func Decode(ctx context.Context, in, pcmOut string) error {
	return DecodeWith(ctx, in, pcmOut, SpeechFilter)
}

// DecodeWith 和 Decode 一样，只是用指定的滤镜，空字符串表示不处理（原始的声音）。
func DecodeWith(ctx context.Context, in, pcmOut, speech string) error {
	return ffmpeg(ctx, append([]string{"-i", in}, pcmArgs(pcmOut, speech)...)...)
}

// pcmArgs 是输出识别用的 PCM 的那部分参数。滤镜放在转成 16k 单声道之后：不管上传的是什么格式，
// 滤镜看到的都一样（立体声不会两个声道各调各的），算起来也省（只有 48k 的三分之一）。
func pcmArgs(out, filter string) []string {
	args := []string{"-map", "0:a:0", "-vn"}
	if filter != "" {
		args = append(args, "-af", fmt.Sprintf("aformat=sample_rates=%d:channel_layouts=mono,%s", SampleRate, filter))
	}
	return append(args, "-ac", "1", "-ar", fmt.Sprint(SampleRate), "-f", "f32le", out)
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

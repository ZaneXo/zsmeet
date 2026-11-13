// Package models 管理语音识别要用的模型文件：第一次用到时下载到模型目录，并校验 SHA-256。
//
// 模型都来自 sherpa-onnx 作者在 Hugging Face 上发布的仓库。huggingface.co 连不上时
// 自动换 hf-mirror.com（国内镜像），也可以用 ZSMEET_MODEL_MIRROR 指定别的镜像。
package models

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type file struct {
	// 存到模型目录下的文件名
	name string
	// Hugging Face 上的「仓库/resolve/分支/路径」
	path   string
	size   int64
	sha256 string
}

var files = []file{
	// SenseVoice：识别文字，自带标点，数字写成阿拉伯数字，每个字（连标点）都给出时间
	{
		name:   "sense-voice.int8.onnx",
		path:   "csukuangfj/sherpa-onnx-sense-voice-zh-en-ja-ko-yue-2024-07-17/resolve/main/model.int8.onnx",
		size:   239_233_841,
		sha256: "c71f0ce00bec95b07744e116345e33d8cbbe08cef896382cf907bf4b51a2cd51",
	},
	{
		name:   "sense-voice.tokens.txt",
		path:   "csukuangfj/sherpa-onnx-sense-voice-zh-en-ja-ko-yue-2024-07-17/resolve/main/tokens.txt",
		size:   315_894,
		sha256: "f449eb28dc567533d7fa59be34e2abca8784f771850c78a47fb731a31429a1dc",
	},
	{
		name:   "silero-vad.onnx",
		path:   "csukuangfj/vad/resolve/main/silero_vad.onnx",
		size:   1_807_522,
		sha256: "a35ebf52fd3ce5f1469b2a36158dba761bc47b973ea3382b3186ca15b1f5af28",
	},
	// 区分说话人：3D-Speaker 的 CAM++（中英文，20 万人训练的增强版），也是 3D-Speaker 自己区分说话人时用的
	{
		name:   "speaker-diarization.onnx",
		path:   "csukuangfj/speaker-embedding-models/resolve/main/3dspeaker_speech_campplus_sv_zh_en_16k-common_advanced.onnx",
		size:   28_281_164,
		sha256: "aa3cfc16963a10586a9393f5035d6d6b57e98d358b347f80c2a30bf4f00ceba2",
	},
	// 声纹库（按声音认出改过名的人）：3D-Speaker 的 CAM++ 中文版。存下来的声纹是它算的，换模型就对不上了
	{
		name:   "speaker-embedding-zh.onnx",
		path:   "csukuangfj/speaker-embedding-models/resolve/main/3dspeaker_speech_campplus_sv_zh-cn_16k-common.onnx",
		size:   28_281_138,
		sha256: "f682b514c05d947ee3fa91cd6ec6c5c7543479a128373fa29b1faedccd21fd11",
	},
}

// Paths 给出各个模型文件的位置。
type Paths struct{ Dir string }

func (p Paths) SenseVoice() string  { return filepath.Join(p.Dir, files[0].name) }
func (p Paths) Tokens() string      { return filepath.Join(p.Dir, files[1].name) }
func (p Paths) VAD() string         { return filepath.Join(p.Dir, files[2].name) }
func (p Paths) Diarization() string { return filepath.Join(p.Dir, files[3].name) }
func (p Paths) Embedding() string   { return filepath.Join(p.Dir, files[4].name) }

// 以前的版本用过、现在不用了的模型文件（识别换成 SenseVoice 以前的 Paraformer 和标点模型，
// 区分说话人换成 3D-Speaker 的做法以前的 pyannote 分段模型）。
// 检查模型时顺手删掉，腾出约 500 MB。只删这几个写死的名字，目录里别的东西不碰
var obsolete = []string{"paraformer-zh.int8.onnx", "paraformer-zh.tokens.txt", "punct-zh-en.onnx", "speaker-segmentation.onnx"}

const (
	official = "https://huggingface.co"
	cnMirror = "https://hf-mirror.com"
)

// 同一个进程里同时只下载一份。后来的调用者不重复下载，而是等正在进行的那一次，并能看到它的进度
// （worker 启动时就开始下载了，这时来了第一场会议，它的页面上要能看到下载到哪了）。
var (
	mu      sync.Mutex
	current *download
)

type download struct {
	done, total atomic.Int64
	finished    chan struct{}
	err         error
}

// Ensure 确保模型都在本地，缺的就下载。progress(已下载字节, 总字节) 只在需要下载时回调。
func Ensure(ctx context.Context, dir, mirror string, progress func(done, total int64)) error {
	if progress == nil {
		progress = func(int64, int64) {}
	}
	for {
		mu.Lock()
		d := current
		if d == nil {
			// 没有人在下载，自己来
			d = &download{finished: make(chan struct{})}
			current = d
			mu.Unlock()

			d.err = fetchMissing(ctx, dir, mirror, func(done, total int64) {
				d.done.Store(done)
				d.total.Store(total)
				progress(done, total)
			})
			mu.Lock()
			current = nil
			mu.Unlock()
			close(d.finished)
			return d.err
		}
		mu.Unlock()

		// 已经有人在下载：等它，同时把它的进度报给自己的调用者
		if err := follow(ctx, d, progress); err == nil {
			return nil
		} else if ctx.Err() != nil {
			return ctx.Err()
		}
		// 那一次失败了（比如发起它的任务被取消了），回到开头自己再试
	}
}

func follow(ctx context.Context, d *download, progress func(done, total int64)) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-d.finished:
			return d.err
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if total := d.total.Load(); total > 0 {
				progress(d.done.Load(), total)
			}
		}
	}
}

// fetchMissing 下载目录里缺少的模型文件：先试配置的镜像，不行再试官方地址和国内镜像。
func fetchMissing(ctx context.Context, dir, mirror string, progress func(done, total int64)) error {
	hosts := []string{strings.TrimRight(mirror, "/")}
	for _, h := range []string{official, cnMirror} {
		if h != hosts[0] {
			hosts = append(hosts, h)
		}
	}
	return fetchMissingFrom(ctx, dir, hosts, progress)
}

func fetchMissingFrom(ctx context.Context, dir string, hosts []string, progress func(done, total int64)) error {
	for _, name := range obsolete {
		if err := os.Remove(filepath.Join(dir, name)); err == nil {
			slog.Info("删掉了不再使用的旧模型", "file", name)
		}
	}
	var missing []file
	var total int64
	for _, f := range files {
		if info, err := os.Stat(filepath.Join(dir, f.name)); err == nil && info.Size() == f.size {
			continue
		}
		missing = append(missing, f)
		total += f.size
	}
	if len(missing) == 0 {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("建不了模型目录 %s：%w", dir, err)
	}

	slog.Info("开始下载识别模型", "files", len(missing), "mb", total>>20)
	var done int64
	for _, f := range missing {
		base := done
		host, err := download1(ctx, hosts, f, filepath.Join(dir, f.name), func(n int64) { progress(base+n, total) })
		if err != nil {
			return fmt.Errorf("下载识别模型 %s 失败：%w", f.name, err)
		}
		// 这个源能用，剩下的文件先从它下
		hosts = append([]string{host}, slices.DeleteFunc(slices.Clone(hosts), func(h string) bool { return h == host })...)
		done += f.size
		slog.Info("模型已下载", "file", f.name)
	}
	return nil
}

// download1 下载一个文件，返回最后成功的下载源。
// 每个源轮流试，都不行再来一轮；不在一个连不上的源上连着耗两次超时。
func download1(ctx context.Context, hosts []string, f file, dest string, progress func(int64)) (string, error) {
	var errs []error
	for range 2 {
		for _, host := range hosts {
			err := fetch(ctx, host+"/"+f.path, f, dest, progress)
			if err == nil {
				return host, nil
			}
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			slog.Warn("模型下载失败，换一个下载源再试", "file", f.name, "host", host, "err", err)
			errs = append(errs, fmt.Errorf("%s：%w", host, err))
		}
	}
	return "", errors.Join(errs...)
}

var client = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	},
}

// 这么久一个字节都没收到就认为连接卡死了
const stallTimeout = 60 * time.Second

func fetch(ctx context.Context, url string, f file, dest string, progress func(int64)) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stalled := errors.New("下载卡住了")
	watchdog := time.AfterFunc(stallTimeout, func() { cancel(stalled) })
	defer watchdog.Stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return cmpCause(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	// 先写到临时文件，校验通过再改名，半截文件不会被当成模型加载
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".download-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	hash := sha256.New()
	buf := make([]byte, 1<<20)
	var n int64
	for {
		k, rerr := resp.Body.Read(buf)
		if k > 0 {
			watchdog.Reset(stallTimeout)
			if _, err := tmp.Write(buf[:k]); err != nil {
				return err
			}
			hash.Write(buf[:k])
			n += int64(k)
			progress(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return cmpCause(ctx, rerr)
		}
	}
	if n != f.size {
		return fmt.Errorf("文件大小不对：收到 %d 字节，应该是 %d", n, f.size)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != f.sha256 {
		return fmt.Errorf("校验和不对：%s", got)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dest)
}

// cmpCause 在请求是被我们自己的看门狗取消时，返回取消的原因而不是笼统的 context canceled。
func cmpCause(ctx context.Context, err error) error {
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	return err
}

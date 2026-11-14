package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ZaneXo/zsmeet/internal/audio"
	"github.com/ZaneXo/zsmeet/internal/transcript"
)

// deepseekASR 用 DeepSeek 的在线识别接口（https://api-docs.deepseek.com/zh-cn/guides/asr）识别所有录音。
// 接口一次最多收 120 秒、不返回时间，所以按 from（SenseVoice）的 VAD 切出的句子切录音：
// chunk 为 0 时一句一个请求，和线上的切法一样；否则把相邻的句子连同中间的停顿合到最长 chunk 秒再发，
// 给识别更多上下文。
func deepseekASR(ctx context.Context, f flags, clips []clip, from result, chunk float64) (result, error) {
	name := "deepseek-asr(按句)"
	if chunk > 0 {
		name = fmt.Sprintf("deepseek-asr(合到%.0f秒)", chunk)
	}
	type job struct{ clip, seg int }
	r := result{name: name, segs: make([][]transcript.Segment, len(clips))}
	var jobs []job
	for i, c := range clips {
		for _, s := range spans(from.segs[i], chunk, c.seconds()) {
			r.segs[i] = append(r.segs[i], transcript.Segment{Start: s[0], End: s[1], Speaker: -1})
			jobs = append(jobs, job{i, len(r.segs[i]) - 1})
		}
	}
	log.Printf("%s：%d 个请求…", name, len(jobs))

	var (
		mu     sync.Mutex
		failed int
		first  error
		wg     sync.WaitGroup
	)
	limit := make(chan struct{}, max(f.deepseekConcurrency, 1))
	started := time.Now()
	for _, j := range jobs {
		wg.Go(func() {
			limit <- struct{}{}
			defer func() { <-limit }()
			seg := &r.segs[j.clip][j.seg]
			c := clips[j.clip]
			from, to := int(seg.Start*audio.SampleRate), min(int(seg.End*audio.SampleRate), len(c.samples))
			t := time.Now()
			text, err := deepseekRecognize(ctx, f.deepseekURL, f.deepseekKey, c.samples[from:to])
			mu.Lock()
			defer mu.Unlock()
			r.took += time.Since(t)
			if err != nil {
				failed++
				if first == nil {
					first = err
				}
				return
			}
			seg.Text = text
		})
	}
	wg.Wait()
	r.wall = time.Since(started)
	if err := ctx.Err(); err != nil {
		return r, err
	}
	if failed > 0 {
		log.Printf("%s：%d 个请求里失败 %d 个（算作没识别出字），第一个错误：%v", name, len(jobs), failed, first)
	} else {
		log.Printf("%s：%d 个请求全部成功", name, len(jobs))
	}
	return r, nil
}

// spans 是要发给接口的时间段。chunk 为 0 时就是每一句；否则从一句的开头起，往后合并到不超过 chunk 秒。
func spans(segs []transcript.Segment, chunk, total float64) [][2]float64 {
	var out [][2]float64
	for i := 0; i < len(segs); {
		start, end := segs[i].Start, segs[i].End
		j := i + 1
		for chunk > 0 && j < len(segs) && segs[j].End-start <= chunk {
			end = segs[j].End
			j++
		}
		out = append(out, [2]float64{start, min(end, total)})
		i = j
	}
	return out
}

// deepseekRecognize 发一段 16k 单声道录音，返回识别出的文字。限流、服务忙、超时和网络错误等一会儿重试。
func deepseekRecognize(ctx context.Context, url, key string, samples []float32) (string, error) {
	body, contentType, err := wavForm(samples)
	if err != nil {
		return "", err
	}
	var last error
	for attempt := range 6 {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(1<<attempt) * time.Second):
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		text, retry, err := deepseekPost(ctx, url, key, body, contentType)
		if err == nil {
			return text, nil
		}
		last = err
		if !retry {
			break
		}
	}
	return "", last
}

func deepseekPost(ctx context.Context, url, key string, body []byte, contentType string) (text string, retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", contentType)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", ctx.Err() == nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return "", true, err
	}
	if res.StatusCode != http.StatusOK {
		retry := res.StatusCode == 429 || res.StatusCode >= 500 || res.StatusCode == 408
		return "", retry, fmt.Errorf("HTTP %d：%s", res.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", false, fmt.Errorf("看不懂接口的回答：%w：%s", err, raw)
	}
	return out.Text, false, nil
}

// wavForm 把录音编成 16 位 PCM WAV，放进 multipart 表单的 file 字段。
func wavForm(samples []float32) ([]byte, string, error) {
	var wav bytes.Buffer
	n := uint32(len(samples) * 2)
	wav.WriteString("RIFF")
	binary.Write(&wav, binary.LittleEndian, 36+n)
	wav.WriteString("WAVEfmt ")
	binary.Write(&wav, binary.LittleEndian, struct {
		Size                      uint32
		Format, Channels          uint16
		Rate, ByteRate            uint32
		BlockAlign, BitsPerSample uint16
	}{16, 1, 1, audio.SampleRate, audio.SampleRate * 2, 2, 16})
	wav.WriteString("data")
	binary.Write(&wav, binary.LittleEndian, n)
	pcm := make([]int16, len(samples))
	for i, s := range samples {
		pcm[i] = int16(math.Round(float64(max(-1, min(1, s))) * math.MaxInt16))
	}
	binary.Write(&wav, binary.LittleEndian, pcm)

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("file", "audio.wav")
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(wav.Bytes()); err != nil {
		return nil, "", err
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return body.Bytes(), w.FormDataContentType(), nil
}

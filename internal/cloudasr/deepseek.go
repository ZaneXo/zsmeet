// Package cloudasr 调用在线语音识别接口（目前是 DeepSeek），并把没有时间戳的识别文字
// 对齐到本机 SenseVoice 给出的每个字的时间上。
package cloudasr

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/ZaneXo/zsmeet/internal/audio"
)

// DefaultDeepSeekURL 是 DeepSeek 在线语音识别接口的默认地址。
const DefaultDeepSeekURL = "https://api.deepseek.com/v1/tools/asr"

// Provider 把一段 16k 单声道浮点音频送去在线识别，返回识别出的文字（不带时间戳）。
type Provider interface {
	Recognize(ctx context.Context, samples []float32) (string, error)
}

// AuthError 表示凭证或计费问题（401 Key 不对、402 余额不足、403 没权限）：
// 继续发剩下的分段也没有用，整场会议直接退回本机识别，并把原因提示给用户和管理员。
type AuthError struct {
	Code int
	Msg  string
}

func (e *AuthError) Error() string { return e.Msg }

// IsAuthError 判断是不是凭证或余额类不可恢复错误。
func IsAuthError(err error) bool {
	_, ok := errors.AsType[*AuthError](err)
	return ok
}

// DeepSeek 调用 DeepSeek 的在线语音识别接口（POST multipart/form-data）。
type DeepSeek struct {
	URL    string
	APIKey string
	// HTTP 客户端；为空时用带单次请求超时的默认客户端
	Client *http.Client
	// 第 attempt 次重试（从 1 开始）前等多久；为空时按 1s、2s、4s、8s、16s 退避
	Backoff func(attempt int) time.Duration
}

const (
	// 单次上传 + 识别最多等多久（100 秒的录音正常约 5 秒返回）
	requestTimeout = 90 * time.Second
	// 最多试几次（第一次 + 最多重试 5 次）
	maxAttempts = 6
)

// Recognize 把一段 16k 单声道音频编成 WAV 发给 DeepSeek，遇到限流、超时、5xx 自动重试。
func (d *DeepSeek) Recognize(ctx context.Context, samples []float32) (string, error) {
	body, contentType, err := wavForm(samples)
	if err != nil {
		return "", err
	}
	var last error
	for attempt := range maxAttempts {
		if attempt > 0 {
			wait := time.Duration(1<<(attempt-1)) * time.Second
			if d.Backoff != nil {
				wait = d.Backoff(attempt)
			}
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		text, retry, err := d.post(ctx, body, contentType)
		if err == nil {
			return text, nil
		}
		last = err
		if !retry {
			return "", err
		}
	}
	return "", last
}

// Test 发 1 秒静音检查接口地址和 API Key 能不能用（不重试，免得管理员在设置页等太久）。
func Test(ctx context.Context, url, key string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	d := &DeepSeek{URL: url, APIKey: key}
	body, contentType, err := wavForm(make([]float32, audio.SampleRate))
	if err != nil {
		return err
	}
	_, _, err = d.post(ctx, body, contentType)
	return err
}

func (d *DeepSeek) post(ctx context.Context, body []byte, contentType string) (text string, retry bool, err error) {
	url := strings.TrimSpace(d.URL)
	if url == "" {
		url = DefaultDeepSeekURL
	}
	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", false, errors.New("在线识别的接口地址格式不对，请在「设置 → 语音识别」里检查")
	}
	req.Header.Set("Authorization", "Bearer "+d.APIKey)
	req.Header.Set("Content-Type", contentType)

	client := d.Client
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		return "", true, errors.New("连不上 DeepSeek 在线识别接口，请检查服务器网络和接口地址")
	}
	defer res.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		return "", true, errors.New("读取 DeepSeek 在线识别响应中断，请稍后再试")
	}

	if res.StatusCode != http.StatusOK {
		return "", shouldRetry(res.StatusCode), statusError(res.StatusCode)
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", false, errors.New("在线识别接口返回的不是预期的 JSON，请检查接口地址是否正确")
	}
	return out.Text, false, nil
}

func shouldRetry(code int) bool {
	return code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500
}

// statusError 把 HTTP 状态码转成给用户和管理员看的中文说明，不夹带响应原文里的敏感信息。
func statusError(code int) error {
	switch code {
	case http.StatusUnauthorized:
		return &AuthError{Code: code, Msg: "DeepSeek 的 API Key 不对或已失效，请在「设置 → 语音识别」里检查"}
	case http.StatusPaymentRequired:
		return &AuthError{Code: code, Msg: "DeepSeek 账户余额不足，请充值后再试"}
	case http.StatusForbidden:
		return &AuthError{Code: code, Msg: "DeepSeek 拒绝了请求（API Key 没有权限或账户受限），请在「设置 → 语音识别」里检查"}
	case http.StatusNotFound:
		return fmt.Errorf("在线识别接口地址不对（HTTP 404），默认应该是 %s", DefaultDeepSeekURL)
	case http.StatusTooManyRequests:
		return errors.New("DeepSeek 在线识别请求过于频繁（HTTP 429），请稍后再试")
	case http.StatusRequestEntityTooLarge:
		return errors.New("发送给在线识别的音频片段超过大小限制（HTTP 413）")
	default:
		if code >= 500 {
			return fmt.Errorf("DeepSeek 在线识别服务暂时不可用（HTTP %d），请稍后再试", code)
		}
		return fmt.Errorf("DeepSeek 在线识别请求失败（HTTP %d），请检查配置", code)
	}
}

// wavForm 把 16k 单声道录音编成 16 位 PCM WAV，放进 multipart 表单的 file 字段。
func wavForm(samples []float32) ([]byte, string, error) {
	var wav bytes.Buffer
	n := uint32(len(samples) * 2)
	wav.WriteString("RIFF")
	_ = binary.Write(&wav, binary.LittleEndian, 36+n)
	wav.WriteString("WAVEfmt ")
	_ = binary.Write(&wav, binary.LittleEndian, struct {
		Size                      uint32
		Format, Channels          uint16
		Rate, ByteRate            uint32
		BlockAlign, BitsPerSample uint16
	}{16, 1, 1, audio.SampleRate, audio.SampleRate * 2, 2, 16})
	wav.WriteString("data")
	_ = binary.Write(&wav, binary.LittleEndian, n)
	pcm := make([]int16, len(samples))
	for i, s := range samples {
		pcm[i] = int16(math.Round(float64(max(-1, min(1, s))) * math.MaxInt16))
	}
	_ = binary.Write(&wav, binary.LittleEndian, pcm)

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

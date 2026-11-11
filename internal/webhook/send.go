package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Target 是一个群机器人：平台、Webhook 地址，和钉钉加签的密钥（没开加签的为空）。
type Target struct {
	Platform string
	URL      string
	Secret   string
	// 只在测试里用，见 CheckURL
	TestHost string
}

// SendTimeout 是发一条消息最多等多久
const SendTimeout = 15 * time.Second

// 不跟着跳转：地址只能是官方的，不能被一个跳转带到别的网站去
var httpClient = &http.Client{
	Timeout:       SendTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// Send 把 payload（Payload 返回的 JSON）发到群机器人。失败时返回 *Error，原因里不带地址和密钥。
func Send(ctx context.Context, t Target, payload []byte) error {
	label := Label(t.Platform)
	checked, err := CheckURL(t.Platform, t.URL, t.TestHost)
	if err != nil {
		return &Error{Msg: err.Error()}
	}
	target := checked
	if t.Platform == DingTalk && t.Secret != "" {
		target = signURL(checked, t.Secret, time.Now())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return &Error{Msg: "Webhook 地址不对"}
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := httpClient.Do(req)
	if err != nil {
		// *url.Error 的文字里带着完整的地址（也就带着密钥），只留下真正的原因
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err
		}
		msg := redact(err.Error(), t, checked)
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
			msg = fmt.Sprintf("等了 %d 秒没有响应", int(SendTimeout.Seconds()))
		}
		return &Error{Msg: fmt.Sprintf("连不上%s：%s", label, msg), Retry: true}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	var result error
	switch t.Platform {
	case DingTalk:
		result = dingtalkResult(resp.StatusCode, body)
	case WeCom:
		result = wecomResult(resp.StatusCode, body)
	default:
		result = slackResult(resp.StatusCode, body)
	}
	// HTTP 429 带着 Retry-After（Slack 会给）的，按它说的等
	if e, ok := errors.AsType[*Error](result); ok && resp.StatusCode == http.StatusTooManyRequests {
		e.Wait = retryAfter(resp.Header.Get("Retry-After"), time.Now())
	}
	if e, ok := errors.AsType[*Error](result); ok {
		e.Msg = redact(e.Msg, t, checked)
	}
	return result
}

// Sign 是钉钉「加签」的签名：把「毫秒时间戳 + 换行 + 密钥」用密钥做 HmacSHA256，再 Base64。
// 见 https://help.dingtalk.io/zh/open/dingstart/customize-robot-security-settings.md
func Sign(secret string, ms int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ms, 10) + "\n" + secret))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// signURL 在钉钉的地址后面加上 timestamp 和 sign 两个参数。
func signURL(checked, secret string, now time.Time) string {
	ms := now.UnixMilli()
	return checked + "&timestamp=" + strconv.FormatInt(ms, 10) + "&sign=" + url.QueryEscape(Sign(secret, ms))
}

// Retry-After 的上下限：太短的当作 1 秒；太长的（多半是写错了）最多等 1 小时，
// 超过了后台最晚重试的时间（service.WebhookRetryUntil）就直接记成失败，不白等
const (
	minRetryAfter = time.Second
	maxRetryAfter = time.Hour
)

// retryAfter 解析 HTTP 429 的 Retry-After（秒数，或者一个 HTTP 日期），夹在上下限之间。没有或者看不懂时等 1 分钟。
func retryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	var d time.Duration
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		d = time.Duration(min(max(secs, 0), int64(maxRetryAfter/time.Second))) * time.Second
	} else if at, err := http.ParseTime(v); err == nil {
		d = at.Sub(now)
	} else {
		return minuteThrottle
	}
	return min(max(d, minRetryAfter), maxRetryAfter)
}

// httpResult 处理钉钉、企业微信返回的不是 200 的情况。
func httpResult(label string, status int) error {
	if status == http.StatusTooManyRequests {
		return &Error{Msg: fmt.Sprintf("%s暂时出错了（HTTP %d）", label, status), Retry: true, Wait: minuteThrottle}
	}
	if status >= 500 {
		return &Error{Msg: fmt.Sprintf("%s暂时出错了（HTTP %d）", label, status), Retry: true}
	}
	return &Error{Msg: fmt.Sprintf("%s拒绝了这条消息（HTTP %d）。请检查 Webhook 地址", label, status)}
}

type apiResult struct {
	Code *int   `json:"errcode"`
	Msg  string `json:"errmsg"`
}

func parseResult(label string, status int, body []byte) (apiResult, error) {
	if status != http.StatusOK {
		return apiResult{}, httpResult(label, status)
	}
	var r apiResult
	if err := json.Unmarshal(body, &r); err != nil || r.Code == nil {
		return apiResult{}, &Error{Msg: fmt.Sprintf("%s返回的内容看不懂，可能 Webhook 地址不对", label), Retry: true}
	}
	return r, nil
}

// dingtalkResult 看钉钉的返回。安全设置（关键词、加签、IP 白名单）没通过都是 310000，靠 errmsg 区分。
func dingtalkResult(status int, body []byte) error {
	r, err := parseResult("钉钉", status, body)
	if err != nil {
		return err
	}
	msg := strings.ToLower(r.Msg)
	switch {
	case *r.Code == 0:
		return nil
	case *r.Code == 310000 && strings.Contains(msg, "keyword"):
		return &Error{Msg: "钉钉没有收下这条消息：机器人设置了自定义关键词，消息里没有。请在钉钉机器人的安全设置里加一个关键词「会议纪要」（zsmeet 发的消息都带着这四个字），或者改用加签"}
	case *r.Code == 310000 && (strings.Contains(msg, "sign") || strings.Contains(msg, "timestamp")):
		return &Error{Msg: "钉钉没有收下这条消息：加签没通过。请确认填的加签密钥和钉钉机器人安全设置里的一样，并且服务器的时间是准的"}
	case *r.Code == 310000 && (strings.Contains(msg, "ip") || strings.Contains(msg, "whitelist")):
		return &Error{Msg: "钉钉没有收下这条消息：zsmeet 服务器的 IP 不在机器人设置的 IP 地址（段）里。请把服务器的出口 IP 加进去，或者改用加签"}
	case *r.Code == 310000:
		return &Error{Msg: "钉钉没有收下这条消息：没通过机器人的安全设置（" + short(r.Msg) + "）"}
	case *r.Code == 300001 || strings.Contains(msg, "token"):
		return &Error{Msg: "钉钉找不到这个机器人：Webhook 地址不对，或者机器人已经从群里移除了。请重新复制机器人的 Webhook 地址"}
	case *r.Code == 130101 || strings.Contains(msg, "too fast") || strings.Contains(msg, "frequent"):
		return &Error{Msg: "发得太快了：钉钉限制每个机器人每分钟最多发 20 条，超过以后要等 10 分钟才能再发", Retry: true, Wait: DingTalkThrottle}
	case *r.Code == -1:
		return &Error{Msg: "钉钉系统繁忙", Retry: true}
	}
	return &Error{Msg: fmt.Sprintf("钉钉没有收下这条消息（%d：%s）", *r.Code, short(r.Msg))}
}

// wecomResult 看企业微信的返回。
func wecomResult(status int, body []byte) error {
	r, err := parseResult("企业微信", status, body)
	if err != nil {
		return err
	}
	switch *r.Code {
	case 0:
		return nil
	case 93000:
		return &Error{Msg: "企业微信找不到这个机器人：Webhook 地址不对，或者机器人已经从群里移除了。请重新复制机器人的 Webhook 地址"}
	case 45009, 45033:
		return &Error{Msg: "发得太快了：企业微信限制每个机器人每分钟最多发 20 条", Retry: true, Wait: minuteThrottle}
	case -1:
		return &Error{Msg: "企业微信系统繁忙", Retry: true}
	}
	// 企业微信的 errmsg 后面跟着「, hint: [...], from ip: ..., more info at ...」，只留前面的原因
	reason, _, _ := strings.Cut(r.Msg, ",")
	return &Error{Msg: fmt.Sprintf("企业微信没有收下这条消息（%d：%s）", *r.Code, short(reason))}
}

// slackResult 看 Slack 的返回：成功是 200 和「ok」，失败是 4xx 和一个错误码。
// 见 https://docs.slack.dev/messaging/sending-messages-using-incoming-webhooks
func slackResult(status int, body []byte) error {
	text := strings.TrimSpace(string(body))
	switch {
	case status == http.StatusOK:
		return nil
	case status == http.StatusTooManyRequests:
		return &Error{Msg: "发得太快了，Slack 限制了发送频率", Retry: true, Wait: minuteThrottle}
	case status >= 500:
		return &Error{Msg: fmt.Sprintf("Slack 暂时出错了（HTTP %d）", status), Retry: true}
	}
	switch text {
	case "no_service", "no_service_id", "no_active_hooks", "invalid_token", "no_team", "team_disabled":
		return &Error{Msg: "Slack 找不到这个 Webhook：地址不对，或者已经被停用、删除了。请在 Slack 里重新生成一个 Webhook 地址"}
	case "channel_is_archived", "channel_not_found":
		return &Error{Msg: "Slack 的频道已经归档或者不存在了。请换一个频道，重新生成 Webhook 地址"}
	case "action_prohibited", "posting_to_general_channel_denied":
		return &Error{Msg: "Slack 的管理员限制了往这个频道发消息。请联系 Slack 管理员，或者换一个频道"}
	}
	if text == "" {
		text = fmt.Sprintf("HTTP %d", status)
	}
	return &Error{Msg: "Slack 没有收下这条消息（" + short(text) + "）"}
}

// short 把对方返回的原因截短，免得把一大段网页存下来。
func short(s string) string {
	s = strings.TrimSpace(strings.ToValidUTF8(s, ""))
	if utf8.RuneCountInString(s) > 100 {
		return string([]rune(s)[:99]) + "…"
	}
	return s
}

// redact 把文字里可能出现的地址、密钥换成「***」，免得存进数据库、显示在网页上。
func redact(s string, t Target, checked string) string {
	for _, secret := range []string{t.URL, checked, secretPart(t.Platform, checked), t.Secret} {
		if len(secret) >= 4 {
			s = strings.ReplaceAll(s, secret, "***")
		}
	}
	return s
}

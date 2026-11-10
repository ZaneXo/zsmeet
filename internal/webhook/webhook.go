// Package webhook 把纪要发到钉钉、企业微信、Slack 群里的自定义机器人（Webhook）。
//
// 只发到各平台官方的地址（https + 官方域名），地址填错了、被人换成别的网站，也不会把纪要发出去。
package webhook

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// 支持的平台
const (
	DingTalk = "dingtalk"
	WeCom    = "wecom"
	Slack    = "slack"
)

// Platforms 是支持的平台，按设置页面上的顺序。
var Platforms = []string{DingTalk, WeCom, Slack}

// Label 是平台给人看的名字。
func Label(platform string) string {
	switch platform {
	case DingTalk:
		return "钉钉"
	case WeCom:
		return "企业微信"
	case Slack:
		return "Slack"
	}
	return platform
}

// endpoint 是一个平台的 Webhook 地址长什么样：官方域名、路径，密钥放在哪个查询参数里（Slack 的密钥在路径里）。
type endpoint struct {
	host, path, param string
	example           string
}

var endpoints = map[string]endpoint{
	DingTalk: {host: "oapi.dingtalk.com", path: "/robot/send", param: "access_token",
		example: "钉钉的 Webhook 地址形如 https://oapi.dingtalk.com/robot/send?access_token=…，在钉钉群的「设置 → 机器人」里添加自定义机器人后复制"},
	WeCom: {host: "qyapi.weixin.qq.com", path: "/cgi-bin/webhook/send", param: "key",
		example: "企业微信的 Webhook 地址形如 https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=…，在企业微信群里添加机器人后，在机器人的资料页复制"},
	Slack: {host: "hooks.slack.com", path: "/services/",
		example: "Slack 的 Webhook 地址形如 https://hooks.slack.com/services/T…/B…/…，在 Slack 应用的「Incoming Webhooks」页面复制"},
}

var (
	tokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,200}$`)
	slackPath    = regexp.MustCompile(`^/services/[A-Za-z0-9]+/[A-Za-z0-9]+/[A-Za-z0-9]+$`)
)

// CheckURL 检查 platform 平台的 Webhook 地址，返回整理过的地址（去掉多余的参数）。
// 只接受 https 和平台的官方域名。testHost 只在测试里用：非空时也接受发到 http://testHost（httptest 起的假服务器）。
// 返回的错误可以直接给管理员看。
func CheckURL(platform, raw, testHost string) (string, error) {
	ep, ok := endpoints[platform]
	if !ok {
		return "", errors.New("不认识的平台：" + platform + "（只能是钉钉、企业微信或 Slack）")
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("请填写 Webhook 地址。" + ep.example)
	}
	bad := errors.New("Webhook 地址不对。" + ep.example)
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return "", bad
	}
	host := strings.ToLower(u.Host)
	switch {
	case u.Scheme == "https" && host == ep.host:
	case testHost != "" && u.Scheme == "http" && host == testHost:
	default:
		return "", bad
	}
	if platform == Slack {
		if !slackPath.MatchString(u.Path) || u.RawQuery != "" {
			return "", bad
		}
		return u.Scheme + "://" + host + u.Path, nil
	}
	token := u.Query().Get(ep.param)
	if u.Path != ep.path || !tokenPattern.MatchString(token) {
		return "", bad
	}
	return u.Scheme + "://" + host + ep.path + "?" + url.Values{ep.param: {token}}.Encode(), nil
}

// secretPart 返回地址里相当于密码的那一段：钉钉的 access_token、企业微信的 key、Slack 路径里 /services/ 后面的部分。
func secretPart(platform, checked string) string {
	u, err := url.Parse(checked)
	if err != nil {
		return ""
	}
	if platform == Slack {
		return strings.TrimPrefix(u.Path, "/services/")
	}
	return u.Query().Get(endpoints[platform].param)
}

// Mask 返回给网页显示的地址：密钥只留最后 4 位，认得出是哪一个就够了。checked 是 CheckURL 整理过的地址。
func Mask(platform, checked string) string {
	secret := secretPart(platform, checked)
	if secret == "" {
		return ""
	}
	tail := ""
	if len(secret) > 12 {
		tail = secret[len(secret)-4:]
	}
	return strings.Replace(checked, secret, "••••"+tail, 1)
}

// CheckSecret 检查钉钉「加签」的密钥：机器人安全设置里「加签」下面那一串，以 SEC 开头。
func CheckSecret(secret string) error {
	if !strings.HasPrefix(secret, "SEC") || len(secret) < 20 || len(secret) > 200 || strings.ContainsAny(secret, " \t\r\n") {
		return errors.New("加签的密钥不对：是钉钉机器人安全设置里「加签」下面以 SEC 开头的那一串")
	}
	return nil
}

// Error 是发送失败的原因，Msg 可以直接给人看（不含地址和密钥）。Retry 表示过一会儿再试可能就好了（网络问题、发得太快）；
// 为 false 时是配置不对（地址失效、关键词、加签、IP 白名单），重试也没用。
// Wait 只在平台限流（发得太快）时有：至少要等这么久再试，早了还是会被拒。
type Error struct {
	Msg   string
	Retry bool
	Wait  time.Duration
}

func (e *Error) Error() string { return e.Msg }

// 各平台限流以后要等多久。钉钉每个机器人每分钟超过 20 条就限流 10 分钟，多等 1 分钟；
// 企业微信、Slack 按分钟限流，等 1 分钟
const (
	DingTalkThrottle = 11 * time.Minute
	minuteThrottle   = time.Minute
)

// RetryWait 是 err 要求的最短等待时间：平台限流时才有，其他错误为 0。
func RetryWait(err error) time.Duration {
	if e, ok := errors.AsType[*Error](err); ok && e.Retry {
		return e.Wait
	}
	return 0
}

// Retryable 表示 err 过一会儿重试可能就好了。不是 *Error 的错误（不认识的）也算可以重试。
func Retryable(err error) bool {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Retry
	}
	return true
}

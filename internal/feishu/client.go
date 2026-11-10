// Package feishu 是飞书 / Lark 开放平台的客户端：以自建应用的机器人身份（tenant_access_token）
// 往群里发纪要卡片、把纪要导入成飞书文档、读取飞书会议的云录制，以及用长连接接收「会议结束」事件。
// Lark 是飞书的国际版，接口一样，只是域名不同。
//
// 只用到十来个接口，所以请求是自己用 net/http 发的；只有长连接用官方 SDK（见 events.go）。
package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// 开放平台的地址
const (
	FeishuBase = "https://open.feishu.cn"
	LarkBase   = "https://open.larksuite.com"
)

// BaseURL 返回开放平台的地址：domain 是 "lark" 时用 Lark 的，否则用飞书的。override 不为空时用它（私有部署、测试）。
func BaseURL(domain, override string) string {
	switch {
	case override != "":
		return strings.TrimRight(override, "/")
	case domain == "lark":
		return LarkBase
	default:
		return FeishuBase
	}
}

// ConsoleURL 是这个应用在开发者后台的页面，缺权限时让管理员去这里开通。
func ConsoleURL(domain, appID string) string {
	host := "https://open.feishu.cn"
	if domain == "lark" {
		host = "https://open.larksuite.com"
	}
	return host + "/app/" + url.PathEscape(appID)
}

// Client 以一个自建应用的机器人身份调用开放平台。可以并发使用。
type Client struct {
	base      string
	appID     string
	appSecret string
	http      *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

// New 创建客户端。base 见 BaseURL。
func New(base, appID, appSecret string) *Client {
	return &Client{
		base:      strings.TrimRight(base, "/"),
		appID:     appID,
		appSecret: appSecret,
		http:      &http.Client{Timeout: 60 * time.Second},
	}
}

// Pool 按凭证复用客户端，这样同一个应用的 token 只换一次、用到快过期为止。
type Pool struct {
	mu      sync.Mutex
	clients map[string]*Client
}

// Get 返回这组凭证对应的客户端。
func (p *Pool) Get(base, appID, appSecret string) *Client {
	key := base + "\x00" + appID + "\x00" + appSecret
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clients[key]; ok {
		return c
	}
	if p.clients == nil || len(p.clients) > 20 {
		p.clients = map[string]*Client{} // 凭证改过很多次时清掉旧的
	}
	c := New(base, appID, appSecret)
	p.clients[key] = c
	return c
}

// Error 是开放平台返回的业务错误（code 不为 0），Error() 是给人看的说明。
type Error struct {
	API    string // 哪个接口，例如 im/v1/messages
	Status int    // HTTP 状态码
	Code   int
	Msg    string
	LogID  string // 找飞书技术支持时用
}

func (e *Error) Error() string {
	detail := fmt.Sprintf("飞书返回 %d", e.Code)
	if e.Msg != "" {
		detail += "：" + e.Msg
	}
	if e.Code == 0 {
		detail = fmt.Sprintf("飞书返回 HTTP %d", e.Status)
	}
	if hint := hints[e.Code]; hint != "" {
		return hint + "（" + detail + "）"
	}
	return e.API + " 调用失败（" + detail + "）"
}

// 常见错误码的说明，告诉管理员怎么处理
var hints = map[int]string{
	99991672: "应用缺少需要的权限，请到飞书开发者后台「权限管理」开通，并发布新版本",
	99991679: "应用缺少需要的权限，请到飞书开发者后台「权限管理」开通，并发布新版本",
	99991663: "飞书的访问凭证失效了，请稍后再试",
	99991400: "请求太频繁，被飞书限流了，稍后会自动重试",
	230002:   "应用的机器人不在这个群里，请先在群设置里把它加进群",
	230006:   "应用还没有开启机器人能力，请在飞书开发者后台「添加应用能力」里添加机器人，并发布新版本",
	230018:   "群设置不允许机器人发言",
	230035:   "机器人在这个群里不能发言（被禁言，或者公司限制了）",
	230027:   "没有权限往这个群发消息（外部群需要应用开通对外共享能力）",
	232033:   "没有权限往这个群发消息（外部群需要应用开通对外共享能力）",
	232009:   "这个群已经解散了",
	230020:   "往这个群发消息太频繁了，稍后会自动重试",
	230022:   "内容被飞书判定为敏感内容，没有发出去",
	230028:   "内容里有手机号、邮箱之类的信息，被公司的数据防泄漏策略拦下了",
	230099:   "卡片的格式飞书不认，没有发出去",
	1069908:  "文档要放的文件夹不存在，或者应用没有权限往里放：请把这个文件夹共享给一个有应用机器人的群，并给编辑权限",
	1069912:  "文档要放的文件夹不存在",
	1063002:  "应用没有权限修改这个文档的分享设置",
	1063003:  "设置文档权限被拦下了（公司的安全策略不允许，或者对方看不到这个应用）",
	2091005:  "应用没有读取妙记的权限：请在飞书开发者后台「权限管理」里把妙记的数据范围设为全员，并发布新版本",
	41050:    "应用读不到这个人的通讯录信息：请在飞书开发者后台把通讯录的数据范围设为全员",
}

// 这些错误过一会儿重试可能就好了
var retryableCodes = map[int]bool{
	99991400: true, // 限流
	99991663: true, // token 失效
	230020:   true, // 群里发消息太频繁
	1061045:  true, // 云空间不支持并发操作
	1069923:  true, // 导入限流
	2091006:  true, // 妙记内部错误
	20050:    true, // 服务端错误
}

// Retryable：这个错误过一会儿重试可能就好了（网络问题、飞书那边繁忙、限流）。
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	var e *Error
	if errors.As(err, &e) {
		return retryableCodes[e.Code] || e.Status == http.StatusTooManyRequests || e.Status >= 500
	}
	// 不是飞书的业务错误，就是网络问题或者超时
	return !errors.Is(err, context.Canceled)
}

// IsCode 判断 err 是不是飞书返回的这个错误码。
func IsCode(err error, code int) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// 访问凭证失效的错误码，换一个新 token 重试一次
var tokenInvalid = map[int]bool{99991661: true, 99991663: true, 99991664: true, 99991671: true}

// accessToken 返回机器人身份的访问凭证，快过期（剩不到 3 分钟）时换新的。
func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.expires) {
		return c.token, nil
	}
	body, _ := json.Marshal(map[string]string{"app_id": c.appID, "app_secret": c.appSecret})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/open-apis/auth/v3/tenant_access_token/internal", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("连不上飞书：%w", err)
	}
	defer resp.Body.Close()
	var out struct {
		Code   int    `json:"code"`
		Msg    string `json:"msg"`
		Token  string `json:"tenant_access_token"`
		Expire int    `json:"expire"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", &Error{API: "auth/v3/tenant_access_token", Status: resp.StatusCode, Msg: "返回的内容看不懂"}
	}
	if out.Code != 0 || out.Token == "" {
		if resp.StatusCode >= 500 || out.Code == 20050 {
			return "", &Error{API: "auth/v3/tenant_access_token", Status: resp.StatusCode, Code: out.Code, Msg: out.Msg}
		}
		return "", fmt.Errorf("App ID 或 App Secret 不对，换不到飞书的访问凭证（飞书返回 %d：%s）", out.Code, out.Msg)
	}
	// expire 是剩下的秒数，不一定是两小时
	c.token, c.expires = out.Token, time.Now().Add(time.Duration(out.Expire)*time.Second-3*time.Minute)
	return c.token, nil
}

func (c *Client) dropToken(token string) {
	c.mu.Lock()
	if c.token == token {
		c.token = ""
	}
	c.mu.Unlock()
}

// envelope 是开放平台统一的返回格式。机器人信息接口的内容在 bot 里，其他都在 data 里。
type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
	Bot  json.RawMessage `json:"bot"`
}

// request 是一次接口调用。body 是 JSON 请求体；form 不为空时改成上传文件（multipart）。
type request struct {
	method string
	path   string
	query  url.Values
	body   any
	form   *formBody
}

type formBody struct {
	fields   [][2]string
	fileName string
	file     []byte
}

// call 调用接口，把返回的 data 解析到 out（out 为 nil 时不解析）。凭证失效时换一个新的重试一次。
func (c *Client) call(ctx context.Context, r request, out any) error {
	env, err := c.send(ctx, r)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	raw := env.Data
	if len(raw) == 0 || string(raw) == "null" {
		raw = env.Bot
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return &Error{API: apiName(r.path), Msg: "返回的内容看不懂：" + err.Error()}
	}
	return nil
}

func (c *Client) send(ctx context.Context, r request) (envelope, error) {
	for attempt := 0; ; attempt++ {
		token, err := c.accessToken(ctx)
		if err != nil {
			return envelope{}, err
		}
		env, err := c.once(ctx, r, token)
		var e *Error
		if attempt == 0 && errors.As(err, &e) && tokenInvalid[e.Code] {
			c.dropToken(token)
			continue
		}
		return env, err
	}
}

func (c *Client) once(ctx context.Context, r request, token string) (envelope, error) {
	u := c.base + r.path
	if len(r.query) > 0 {
		u += "?" + r.query.Encode()
	}
	var body io.Reader
	contentType := ""
	switch {
	case r.form != nil:
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		for _, f := range r.form.fields {
			if err := w.WriteField(f[0], f[1]); err != nil {
				return envelope{}, err
			}
		}
		part, err := w.CreateFormFile("file", r.form.fileName)
		if err != nil {
			return envelope{}, err
		}
		if _, err := part.Write(r.form.file); err != nil {
			return envelope{}, err
		}
		if err := w.Close(); err != nil {
			return envelope{}, err
		}
		body, contentType = &buf, w.FormDataContentType()
	case r.body != nil:
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		// 不要把 < > & 写成 < 这样：卡片的内容是放在字符串里的 JSON，会变长很多
		enc.SetEscapeHTML(false)
		if err := enc.Encode(r.body); err != nil {
			return envelope{}, err
		}
		body, contentType = &buf, "application/json; charset=utf-8"
	}
	req, err := http.NewRequestWithContext(ctx, r.method, u, body)
	if err != nil {
		return envelope{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return envelope{}, fmt.Errorf("连不上飞书：%w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return envelope{}, fmt.Errorf("连不上飞书：%w", err)
	}
	var env envelope
	jsonErr := json.Unmarshal(raw, &env)
	logID := resp.Header.Get("X-Tt-Logid")
	switch {
	case jsonErr != nil:
		return envelope{}, &Error{API: apiName(r.path), Status: resp.StatusCode, LogID: logID}
	case env.Code != 0:
		return envelope{}, &Error{API: apiName(r.path), Status: resp.StatusCode, Code: env.Code, Msg: env.Msg, LogID: logID}
	case resp.StatusCode >= 400:
		return envelope{}, &Error{API: apiName(r.path), Status: resp.StatusCode, LogID: logID}
	}
	return env, nil
}

// apiName 把接口路径变成报错里用的短名字，例如 im/v1/messages。
func apiName(path string) string {
	return strings.TrimPrefix(path, "/open-apis/")
}

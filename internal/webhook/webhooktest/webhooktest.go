// Package webhooktest 是测试用的假群机器人：一个服务器同时扮演钉钉、企业微信和 Slack 的 Webhook，
// 记下收到的消息，也可以按密钥指定返回什么（模拟关键词不对、限流、地址失效）。
package webhooktest

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ZaneXo/zsmeet/internal/webhook"
)

// Received 是收到的一条消息。Token 是地址里的密钥（钉钉的 access_token、企业微信的 key、Slack 路径后面那段）。
type Received struct {
	Platform string
	Token    string
	Query    url.Values
	Body     string
}

// Reply 是指定的返回：HTTP 状态码、内容，和要加的响应头（比如 Retry-After）。
type Reply struct {
	Status int
	Body   string
	Header map[string]string
}

type Server struct {
	*httptest.Server
	// Host 是假服务器的 host:port，传给 CheckURL 的 testHost
	Host string

	mu       sync.Mutex
	received []Received
	// Replies 按密钥指定返回什么；没指定的按成功处理
	Replies map[string]Reply
	// Secrets 是钉钉开了加签的机器人（密钥 → 加签的密钥），收到的签名不对时像钉钉一样返回 310000
	Secrets map[string]string
}

func New(t *testing.T) *Server {
	s := &Server{Replies: map[string]Reply{}, Secrets: map[string]string{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Close)
	s.Host = strings.TrimPrefix(s.Server.URL, "http://")
	return s
}

// URL 返回 platform 平台、密钥是 token 的 Webhook 地址（指向假服务器）。
func (s *Server) URL(platform, token string) string {
	switch platform {
	case webhook.DingTalk:
		return s.Server.URL + "/robot/send?access_token=" + token
	case webhook.WeCom:
		return s.Server.URL + "/cgi-bin/webhook/send?key=" + token
	}
	return s.Server.URL + "/services/T0001/B0001/" + token
}

// Received 返回收到的消息，按收到的先后。
func (s *Server) Received() []Received {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Received(nil), s.received...)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	rec := Received{Query: r.URL.Query(), Body: string(body)}
	switch {
	case r.URL.Path == "/robot/send":
		rec.Platform, rec.Token = webhook.DingTalk, rec.Query.Get("access_token")
	case r.URL.Path == "/cgi-bin/webhook/send":
		rec.Platform, rec.Token = webhook.WeCom, rec.Query.Get("key")
	case strings.HasPrefix(r.URL.Path, "/services/"):
		parts := strings.Split(r.URL.Path, "/")
		rec.Platform, rec.Token = webhook.Slack, parts[len(parts)-1]
	default:
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	reply, custom := s.Replies[rec.Token]
	secret := s.Secrets[rec.Token]
	s.mu.Unlock()

	if !custom && secret != "" {
		ms, _ := strconv.ParseInt(rec.Query.Get("timestamp"), 10, 64)
		if rec.Query.Get("sign") != webhook.Sign(secret, ms) {
			reply, custom = Reply{Status: 200, Body: `{"errcode":310000,"errmsg":"sign not match, more: [https://ding-doc.dingtalk.com/doc#/serverapi2/qf2nxq]"}`}, true
		}
	}
	if !custom {
		reply = Reply{Status: 200, Body: `{"errcode":0,"errmsg":"ok"}`}
		if rec.Platform == webhook.Slack {
			reply.Body = "ok"
		}
		// 只记下真的收下了的消息
		s.mu.Lock()
		s.received = append(s.received, rec)
		s.mu.Unlock()
	}
	for k, v := range reply.Header {
		w.Header().Set(k, v)
	}
	w.WriteHeader(reply.Status)
	io.WriteString(w, reply.Body)
}

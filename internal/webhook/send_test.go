package webhook_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZaneXo/zsmeet/internal/webhook"
	"github.com/ZaneXo/zsmeet/internal/webhook/webhooktest"
)

const (
	token  = "tok0123456789secretABCD"
	secret = "SEC0123456789abcdef0123456789abcdef"
)

func target(s *webhooktest.Server, platform, tok string) webhook.Target {
	return webhook.Target{Platform: platform, URL: s.URL(platform, tok), TestHost: s.Host}
}

// 发成功：每个平台都收到；钉钉开了加签时带着对的签名。
func TestSend(t *testing.T) {
	s := webhooktest.New(t)
	for _, platform := range webhook.Platforms {
		if err := webhook.Send(t.Context(), target(s, platform, token), []byte(`{"msgtype":"markdown"}`)); err != nil {
			t.Errorf("%s：%v", platform, err)
		}
	}
	if got := s.Received(); len(got) != 3 || got[0].Platform != webhook.DingTalk || got[2].Platform != webhook.Slack || got[1].Body != `{"msgtype":"markdown"}` {
		t.Errorf("三个平台都收到：%+v", got)
	}

	s.Secrets["signed0123456789"] = secret
	signed := target(s, webhook.DingTalk, "signed0123456789")
	signed.Secret = secret
	if err := webhook.Send(t.Context(), signed, []byte(`{}`)); err != nil {
		t.Fatalf("加签：%v", err)
	}
	last := s.Received()[3]
	if last.Query.Get("timestamp") == "" || last.Query.Get("sign") == "" {
		t.Errorf("加签要带 timestamp 和 sign：%v", last.Query)
	}
	// 密钥填错了：钉钉说签名不对，重试也没用
	signed.Secret = "SECwrong0123456789abcdef0123456789"
	err := webhook.Send(t.Context(), signed, []byte(`{}`))
	if err == nil || webhook.Retryable(err) || !strings.Contains(err.Error(), "加签没通过") {
		t.Errorf("加签不对：%v", err)
	}
}

// 失败：配置不对的（关键词、地址失效、IP 白名单）不重试，说清楚怎么改；发得太快、对方出错的稍后重试。原因里不能有地址和密钥。
func TestSendFailures(t *testing.T) {
	s := webhooktest.New(t)
	for i, tc := range []struct {
		platform string
		reply    webhooktest.Reply
		retry    bool
		want     string
		wait     time.Duration // 限流时至少等多久再试
	}{
		{webhook.DingTalk, webhooktest.Reply{Status: 200, Body: `{"errcode":310000,"errmsg":"keywords not in content, more: [https://ding-doc.dingtalk.com/doc#/serverapi2/qf2nxq]"}`}, false, "关键词「会议纪要」", 0},
		{webhook.DingTalk, webhooktest.Reply{Status: 200, Body: `{"errcode":310000,"errmsg":"ip 1.2.3.4 not in whitelist"}`}, false, "IP", 0},
		{webhook.DingTalk, webhooktest.Reply{Status: 200, Body: `{"errcode":300001,"errmsg":"token is not exist"}`}, false, "找不到这个机器人", 0},
		{webhook.DingTalk, webhooktest.Reply{Status: 200, Body: `{"errcode":130101,"errmsg":"send too fast, exceed 20 times per minute"}`}, true, "等 10 分钟", 11 * time.Minute},
		{webhook.DingTalk, webhooktest.Reply{Status: 502, Body: "bad gateway"}, true, "HTTP 502", 0},
		{webhook.DingTalk, webhooktest.Reply{Status: 200, Body: "<html>"}, true, "看不懂", 0},
		{webhook.WeCom, webhooktest.Reply{Status: 200, Body: `{"errcode":93000,"errmsg":"invalid webhook url, hint: [1700000000_1_abc], from ip: 1.2.3.4, more info at https://open.work.weixin.qq.com/devtool/query?e=93000"}`}, false, "找不到这个机器人", 0},
		{webhook.WeCom, webhooktest.Reply{Status: 200, Body: `{"errcode":45009,"errmsg":"api freq out of limit"}`}, true, "每分钟最多发 20 条", time.Minute},
		{webhook.WeCom, webhooktest.Reply{Status: 200, Body: `{"errcode":40058,"errmsg":"markdown.content exceed max length 4096, hint: [x], from ip: 1.2.3.4"}`}, false, "40058：markdown.content exceed max length 4096）", 0},
		{webhook.Slack, webhooktest.Reply{Status: 404, Body: "no_service"}, false, "找不到这个 Webhook", 0},
		{webhook.Slack, webhooktest.Reply{Status: 410, Body: "channel_is_archived"}, false, "归档", 0},
		{webhook.Slack, webhooktest.Reply{Status: 403, Body: "action_prohibited"}, false, "限制了", 0},
		{webhook.Slack, webhooktest.Reply{Status: 400, Body: "invalid_payload"}, false, "invalid_payload", 0},
		{webhook.Slack, webhooktest.Reply{Status: 429, Body: "rate_limited"}, true, "发得太快了", time.Minute},
		{webhook.Slack, webhooktest.Reply{Status: 500, Body: "oops"}, true, "HTTP 500", 0},
	} {
		tok := fmt.Sprintf("failing%02dsecret0000", i)
		s.Replies[tok] = tc.reply
		err := webhook.Send(t.Context(), target(s, tc.platform, tok), []byte(`{}`))
		if err == nil || webhook.Retryable(err) != tc.retry || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s %s：要%s重试、提示「%s」：%v", tc.platform, tc.reply.Body, map[bool]string{true: "", false: "不"}[tc.retry], tc.want, err)
		}
		if got := webhook.RetryWait(err); got != tc.wait {
			t.Errorf("%s %s：限流以后至少等 %v 再试，现在是 %v", tc.platform, tc.reply.Body, tc.wait, got)
		}
		if err != nil && (strings.Contains(err.Error(), tok) || strings.Contains(err.Error(), s.Host)) {
			t.Errorf("原因里不能有地址和密钥：%v", err)
		}
	}
}

// 不是官方的地址不发；跳转不跟；连不上时原因里不带地址（Go 的报错里本来带着完整的地址）。
func TestSendRefusesOtherHosts(t *testing.T) {
	hit := false
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer other.Close()

	// 没有声明是测试用的假服务器：不发
	err := webhook.Send(t.Context(), webhook.Target{Platform: webhook.DingTalk, URL: other.URL + "/robot/send?access_token=" + token}, []byte(`{}`))
	if err == nil || webhook.Retryable(err) || hit {
		t.Errorf("不是官方地址的不发：%v", err)
	}

	// 跳转到别的网站：不跟
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	rhost := strings.TrimPrefix(redirect.URL, "http://")
	err = webhook.Send(t.Context(), webhook.Target{Platform: webhook.Slack, URL: redirect.URL + "/services/T1/B1/" + token, TestHost: rhost}, []byte(`{}`))
	if err == nil || hit {
		t.Errorf("跳转不跟：%v", err)
	}

	// 连不上
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL, deadHost := dead.URL, strings.TrimPrefix(dead.URL, "http://")
	dead.Close()
	err = webhook.Send(t.Context(), webhook.Target{Platform: webhook.WeCom, URL: deadURL + "/cgi-bin/webhook/send?key=" + token, TestHost: deadHost}, []byte(`{}`))
	if err == nil || !webhook.Retryable(err) || !strings.Contains(err.Error(), "连不上企业微信") || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "/cgi-bin") {
		t.Errorf("连不上时稍后重试，原因里不带地址：%v", err)
	}
}

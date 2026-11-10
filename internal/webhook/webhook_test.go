package webhook

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// 地址只认各平台官方的：https、官方域名、对的路径；多余的参数去掉。测试用的假服务器只在传了 testHost 时才认。
func TestCheckURL(t *testing.T) {
	const token = "0123456789abcdefABCDEF_-xyz"
	for _, tc := range []struct {
		platform, raw, want string
	}{
		{DingTalk, "  https://oapi.dingtalk.com/robot/send?access_token=" + token + "  ", "https://oapi.dingtalk.com/robot/send?access_token=" + token},
		// 复制来的地址带着以前的签名参数：去掉，发的时候重新签
		{DingTalk, "https://OAPI.dingtalk.com/robot/send?access_token=" + token + "&timestamp=1&sign=x", "https://oapi.dingtalk.com/robot/send?access_token=" + token},
		{WeCom, "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=693a91f6-7xxx-4bc4-97a0-0ec2sifa5aaa", "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=693a91f6-7xxx-4bc4-97a0-0ec2sifa5aaa"},
		{Slack, "https://hooks.slack.com/services/T0TEST/B0TEST/fakeForTests", "https://hooks.slack.com/services/T0TEST/B0TEST/fakeForTests"},
	} {
		got, err := CheckURL(tc.platform, tc.raw, "")
		if err != nil || got != tc.want {
			t.Errorf("%s %q：得到 %q %v，应该是 %q", tc.platform, tc.raw, got, err, tc.want)
		}
	}

	for _, tc := range []struct {
		platform, raw, why string
	}{
		{DingTalk, "", "空的"},
		{DingTalk, "http://oapi.dingtalk.com/robot/send?access_token=" + token, "不是 https"},
		{DingTalk, "https://oapi.dingtalk.com.evil.example/robot/send?access_token=" + token, "不是官方域名"},
		{DingTalk, "https://evil.example/robot/send?access_token=" + token, "别的网站"},
		{DingTalk, "https://user@oapi.dingtalk.com/robot/send?access_token=" + token, "带用户名"},
		{DingTalk, "https://oapi.dingtalk.com:8443/robot/send?access_token=" + token, "换了端口"},
		{DingTalk, "https://oapi.dingtalk.com/robot/other?access_token=" + token, "路径不对"},
		{DingTalk, "https://oapi.dingtalk.com/robot/send", "没有 access_token"},
		{DingTalk, "https://oapi.dingtalk.com/robot/send?access_token=a%20b%3Cc", "access_token 里有怪字符"},
		{DingTalk, "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=" + token, "平台选错了"},
		{WeCom, "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?access_token=" + token, "参数名不对"},
		{Slack, "https://hooks.slack.com/triggers/T0/B0/xxx", "不是 Incoming Webhook"},
		{Slack, "https://hooks.slack.com/services/T0/B0/xxx?x=1", "带参数"},
		{Slack, "https://hooks.slack.com/services/T0/B0/../../x", "路径里有 .."},
		{"telegram", "https://api.telegram.org/bot", "不支持的平台"},
		{DingTalk, "http://127.0.0.1:9999/robot/send?access_token=" + token, "没开测试时不认本机"},
	} {
		if got, err := CheckURL(tc.platform, tc.raw, ""); err == nil {
			t.Errorf("%s（%s）应该不认：%q", tc.why, tc.raw, got)
		}
	}
	// 错误要告诉管理员地址长什么样
	if _, err := CheckURL(WeCom, "https://example.com", ""); err == nil || !strings.Contains(err.Error(), "qyapi.weixin.qq.com") {
		t.Errorf("要说清楚地址长什么样：%v", err)
	}

	// 测试用的假服务器：只认传进来的那一个 host
	if got, err := CheckURL(DingTalk, "http://127.0.0.1:9999/robot/send?access_token="+token, "127.0.0.1:9999"); err != nil || !strings.HasPrefix(got, "http://127.0.0.1:9999/") {
		t.Errorf("测试时认假服务器：%q %v", got, err)
	}
	if _, err := CheckURL(DingTalk, "http://127.0.0.1:8888/robot/send?access_token="+token, "127.0.0.1:9999"); err == nil {
		t.Error("测试时也只认那一个假服务器")
	}
}

// 网页上显示的地址：密钥只留最后 4 位。
func TestMask(t *testing.T) {
	for _, tc := range []struct{ platform, url, secret, want string }{
		{DingTalk, "https://oapi.dingtalk.com/robot/send?access_token=abcdef0123456789wxyz", "abcdef0123456789wxyz", "https://oapi.dingtalk.com/robot/send?access_token=••••wxyz"},
		{WeCom, "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=693a91f6-7xxx-4bc4-97a0-0ec2sifa5aaa", "693a91f6-7xxx-4bc4-97a0-0ec2sifa5aaa", "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=••••5aaa"},
		{Slack, "https://hooks.slack.com/services/T0TEST/B0TEST/fakeTokenabcd", "T0TEST/B0TEST/fakeTokenabcd", "https://hooks.slack.com/services/••••abcd"},
		// 太短的密钥一位都不留
		{DingTalk, "https://oapi.dingtalk.com/robot/send?access_token=short123", "short123", "https://oapi.dingtalk.com/robot/send?access_token=••••"},
	} {
		got := Mask(tc.platform, tc.url)
		if got != tc.want || strings.Contains(got, tc.secret) {
			t.Errorf("%s：%q，应该是 %q", tc.platform, got, tc.want)
		}
	}
}

func TestCheckSecret(t *testing.T) {
	if err := CheckSecret("SEC0123456789abcdef0123456789abcdef"); err != nil {
		t.Error(err)
	}
	for _, s := range []string{"", "0123456789abcdef0123456789", "SEC123", "SEC0123456789 abcdef0123456789"} {
		if CheckSecret(s) == nil {
			t.Errorf("%q 不是加签的密钥", s)
		}
	}
}

// 钉钉加签：「毫秒时间戳 + 换行 + 密钥」用密钥做 HmacSHA256 再 Base64；放进地址时再 URL 编码。
func TestSign(t *testing.T) {
	const secret = "SEC0123456789abcdef0123456789abcdef"
	// 用 Python 的 hmac 独立算出来的
	if got := Sign(secret, 1700000000000); got != "hJ95/XWePTzrSwXDHEJJY1Ta0B6qiG/YMxNzB4v4U2w=" {
		t.Errorf("签名：%s", got)
	}
	got := signURL("https://oapi.dingtalk.com/robot/send?access_token=x", secret, time.UnixMilli(1700000000000))
	if got != "https://oapi.dingtalk.com/robot/send?access_token=x&timestamp=1700000000000&sign=hJ95%2FXWePTzrSwXDHEJJY1Ta0B6qiG%2FYMxNzB4v4U2w%3D" {
		t.Errorf("加了签名的地址：%s", got)
	}
}

const sampleMinutes = `## 摘要
**一期**只做网页端，录音器放到二期。

## 待办
- 王悦：整理需求文档
- 李航：评估录音器的工作量
`

func testMessage(minutes string) Message {
	return Message{
		Title: "产品周会", When: time.Date(2026, 3, 5, 14, 30, 0, 0, time.Local), Duration: 35 * 60, Owner: "张敏",
		Minutes: minutes, MeetingURL: "https://meet.example.com/meetings/0b0d2f0e-0000-4000-8000-000000000001",
	}
}

// decode 取出消息里给人看的文字：钉钉的 markdown.text、企业微信的 markdown.content、Slack 的各个 block 的文字。
func decode(t *testing.T, platform string, payload []byte) string {
	t.Helper()
	var v struct {
		MsgType  string `json:"msgtype"`
		Markdown struct {
			Title   string `json:"title"`
			Text    string `json:"text"`
			Content string `json:"content"`
		} `json:"markdown"`
		Text   string `json:"text"`
		Blocks []struct {
			Type string `json:"type"`
			Text struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"text"`
			Elements []struct {
				Text string `json:"text"`
			} `json:"elements"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(payload, &v); err != nil {
		t.Fatalf("%s 的消息不是合法的 JSON：%v\n%s", platform, err, payload)
	}
	switch platform {
	case DingTalk:
		if v.MsgType != "markdown" || !strings.HasPrefix(v.Markdown.Title, "会议纪要｜") {
			t.Errorf("钉钉的 markdown 消息要有标题：%s", payload)
		}
		return v.Markdown.Text
	case WeCom:
		if v.MsgType != "markdown" {
			t.Errorf("企业微信的 markdown 消息：%s", payload)
		}
		return v.Markdown.Content
	}
	if v.Text == "" || len(v.Blocks) == 0 || v.Blocks[0].Type != "header" {
		t.Errorf("Slack 的消息要有 text（通知里显示）和标题：%s", payload)
	}
	var parts []string
	for _, b := range v.Blocks {
		parts = append(parts, b.Text.Text)
		for _, e := range b.Elements {
			parts = append(parts, e.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// 每个平台的消息都有标题、时间时长创建者、摘要、待办和「查看完整纪要」的链接。
func TestPayload(t *testing.T) {
	for _, platform := range Platforms {
		payload, err := testMessage(sampleMinutes).Payload(platform)
		if err != nil {
			t.Fatal(err)
		}
		text := decode(t, platform, payload)
		for _, want := range []string{"会议纪要｜产品周会", "3月5日 14:30", "35 分钟", "创建者 张敏", "摘要", "一期只做网页端", "王悦：整理需求文档", "李航：评估录音器的工作量", "查看完整纪要", "https://meet.example.com/meetings/0b0d2f0e-0000-4000-8000-000000000001"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s 的消息里要有「%s」：\n%s", platform, want, text)
			}
		}
	}
	link := map[string]string{
		DingTalk: "[查看完整纪要](https://meet.example.com/meetings/",
		WeCom:    "[查看完整纪要](https://meet.example.com/meetings/",
		Slack:    "<https://meet.example.com/meetings/0b0d2f0e-0000-4000-8000-000000000001|查看完整纪要>",
	}
	for platform, want := range link {
		payload, _ := testMessage(sampleMinutes).Payload(platform)
		if !strings.Contains(decode(t, platform, payload), want) {
			t.Errorf("%s 的链接写法：%s", platform, payload)
		}
	}

	// 没有 zsmeet 的访问地址：不放链接；纪要里既没有摘要也没有待办时说一句
	m := testMessage("")
	m.MeetingURL = ""
	for _, platform := range Platforms {
		payload, _ := m.Payload(platform)
		text := decode(t, platform, payload)
		if strings.Contains(text, "查看完整纪要") || !strings.Contains(text, "请在 zsmeet 里查看") {
			t.Errorf("%s：没有地址时不放链接：%s", platform, text)
		}
	}
	// 会把链接弄断的地址也不放
	m = testMessage(sampleMinutes)
	m.MeetingURL = "https://meet.example.com/a)(b"
	for _, platform := range Platforms {
		payload, _ := m.Payload(platform)
		if strings.Contains(decode(t, platform, payload), "查看完整纪要") {
			t.Errorf("%s：奇怪的地址不放进链接", platform)
		}
	}
}

// 纪要里的文字不能变成格式、链接、@所有人。
func TestPayloadEscapes(t *testing.T) {
	evil := "## 摘要\n" +
		"<at id=all></at> <!channel> <@U123|boss> <font color=\"warning\">红字</font> [钓鱼](https://evil.example) " +
		"**粗体** `代码` # 标题 > 引用 a_b_c ~删除~ A&B\n\n## 待办\n- [点我](https://evil.example)\n"
	m := testMessage(evil)
	m.Title = "周会 <!here> [x](https://evil.example)\n# 第二行"
	m.Owner = "<@U999>"

	for _, platform := range []string{DingTalk, WeCom} {
		payload, err := m.Payload(platform)
		if err != nil {
			t.Fatal(err)
		}
		text := decode(t, platform, payload)
		// 去掉 zsmeet 自己加的格式，剩下的不能再有 Markdown 和标签的符号
		own := strings.NewReplacer("**摘要**", "", "**待办**", "", "[查看完整纪要](https://meet.example.com/meetings/0b0d2f0e-0000-4000-8000-000000000001)", "",
			"#### 会议纪要", "", "### 会议纪要", "", `<font color="comment">`, "", "</font>", "").Replace(text)
		for _, bad := range []string{"<", ">", "[", "]", "`", "**", "#", "~", "_", "\n# "} {
			if strings.Contains(own, bad) {
				t.Errorf("%s：用户的文字里的「%s」要转义：\n%s", platform, bad, text)
			}
		}
		for _, want := range []string{"＜at id=all＞", "［钓鱼］", "A&B", "a＿b＿c"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s：转义以后文字还在（%s）：\n%s", platform, want, text)
			}
		}
		if strings.Count(text, "](") != 1 {
			t.Errorf("%s：只能有 zsmeet 加的那一个链接：\n%s", platform, text)
		}
	}

	payload, err := m.Payload(Slack)
	if err != nil {
		t.Fatal(err)
	}
	text := decode(t, Slack, payload)
	for _, bad := range []string{"<!channel>", "<!here>", "<@U123", "<@U999>", "<https://evil.example", "A&B"} {
		if strings.Contains(text, bad) || strings.Contains(string(payload), bad) {
			t.Errorf("Slack：「%s」要转义：%s", bad, payload)
		}
	}
	for _, want := range []string{"&lt;!channel&gt;", "A&amp;B", "&lt;@U999&gt;"} {
		if !strings.Contains(text, want) {
			t.Errorf("Slack：要按 Slack 的规定转义（%s）：%s", want, text)
		}
	}
	if strings.Count(text, "<") != 1 {
		t.Errorf("Slack：只能有 zsmeet 加的那一个链接：%s", text)
	}
}

// 太长时先少列待办、再缩短摘要，不超过各平台的限制。
func TestPayloadSizeLimits(t *testing.T) {
	var b strings.Builder
	b.WriteString("## 摘要\n" + strings.Repeat("这次会议讨论了很多事情<&>。", 200) + "\n\n## 待办\n")
	for i := range 40 {
		fmt.Fprintf(&b, "- 第 %d 条待办：%s\n", i+1, strings.Repeat("要做的事<>", 30))
	}
	m := testMessage(b.String())
	m.Title = strings.Repeat("很长的标题", 50)

	for _, platform := range Platforms {
		payload, err := m.Payload(platform)
		if err != nil {
			t.Fatal(err)
		}
		text := decode(t, platform, payload)
		switch platform {
		case WeCom:
			if len(text) > 4096 {
				t.Errorf("企业微信的 content 最多 4096 字节：%d", len(text))
			}
		case DingTalk:
			if len(payload) > dingtalkMaxBytes {
				t.Errorf("钉钉的消息太大：%d", len(payload))
			}
		case Slack:
			var v struct {
				Blocks []struct {
					Text struct{ Text string } `json:"text"`
				} `json:"blocks"`
			}
			json.Unmarshal(payload, &v)
			if n := utf8.RuneCountInString(v.Blocks[0].Text.Text); n > slackHeaderMax {
				t.Errorf("Slack 的标题最多 150 个字：%d", n)
			}
			for _, bl := range v.Blocks {
				if n := utf8.RuneCountInString(bl.Text.Text); n > slackSectionMax {
					t.Errorf("Slack 一段最多 3000 个字：%d", n)
				}
			}
		}
		if !strings.Contains(text, "见完整纪要") || !strings.Contains(text, "查看完整纪要") {
			t.Errorf("%s：列不下的待办说一声，链接留着：\n%s", platform, text)
		}
		if !strings.Contains(text, "这次会议讨论了很多事情") {
			t.Errorf("%s：摘要至少留一段：\n%s", platform, text)
		}
	}
}

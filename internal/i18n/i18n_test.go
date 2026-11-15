package i18n

import (
	"context"
	"strings"
	"testing"
	"unicode"
)

func TestParseAcceptLanguage(t *testing.T) {
	cases := []struct {
		header string
		want   Lang
	}{
		{"", ZH},
		{"*", ZH},
		{"fr-FR,fr;q=0.9", ZH},
		{"zh-CN,zh;q=0.9,en;q=0.8", ZH},
		{"en", EN},
		{"en-US,en;q=0.9,zh-CN;q=0.8", EN},
		{"zh-CN;q=0.5, en-US;q=0.9", EN},
		{"en-US;q=0.4, zh-TW;q=0.8", ZH},
		{"en;q=0, zh;q=0.5", ZH},
		{"zh;q=0, en;q=0.2", EN},
	}
	for _, tc := range cases {
		if got := ParseAcceptLanguage(tc.header); got != tc.want {
			t.Errorf("ParseAcceptLanguage(%q) = %q, want %q", tc.header, got, tc.want)
		}
	}
}

func TestContextLang(t *testing.T) {
	if got := FromContext(context.Background()); got != ZH {
		t.Fatalf("empty context = %q, want %q", got, ZH)
	}
	ctx := WithLang(context.Background(), EN)
	if got := FromContext(ctx); got != EN {
		t.Fatalf("EN context = %q, want %q", got, EN)
	}
}

func TestLocalizeTemplate(t *testing.T) {
	for _, id := range []string{"general", "weekly", "tech", "client"} {
		name, guide := LocalizeTemplate(EN, id, "中文名", "中文说明", false)
		if hasHan(name) || hasHan(guide) {
			t.Errorf("builtin %s still has CJK in EN: %q / %q", id, name, guide)
		}
		zhName, zhGuide := LocalizeTemplate(ZH, id, "中文名", "中文说明", false)
		if zhName != "中文名" || zhGuide != "中文说明" {
			t.Errorf("ZH modified template %s", id)
		}
	}
	// Custom templates stay untouched even in EN.
	name, guide := LocalizeTemplate(EN, "custom-1", "研发例会", "按模块写", true)
	if name != "研发例会" || guide != "按模块写" {
		t.Errorf("custom template should stay unchanged, got %q / %q", name, guide)
	}
}

func TestTranslateMessageSamples(t *testing.T) {
	samples := []string{
		"请先登录",
		"会议不存在，可能已被删除",
		"在线识别没用上：DeepSeek 的 API Key 不对或已失效，请在「设置 → 语音识别」里检查；本场已改用本机识别",
		"在线识别没用上：请求全部失败；本场已改用本机识别",
		"测试消息没发出去：连不上钉钉：等了 15 秒没有响应",
		"第 2 次没发出去，1 分钟后自动重试：企业微信系统繁忙",
		"被限流了，11 分钟后自动重试：发得太快了：钉钉限制每个机器人每分钟最多发 20 条，超过以后要等 10 分钟才能再发",
		"密码输错的次数太多，为了账号安全暂时不能登录，请 15 分钟后再试",
		"提问太频繁了，每分钟最多问 10 次，请 42 秒后再试",
		"纪要刚刚更新成了第 3 版，和你看到的第 2 版不一样。请看一下新的纪要，再重新发送",
		"发到「产品研发群」的上一条还在发送中，等发完再发",
		"大模型返回 401：invalid api key（检查 API Key，以及这个 Key 能不能用这个模型）",
		"应用的机器人不在这个群里，请先在群设置里把它加进群（飞书返回 230002：bot not in chat）",
		"im/v1/messages 调用失败（飞书返回 HTTP 502）",
		"请填写 Webhook 地址。钉钉的 Webhook 地址形如 https://oapi.dingtalk.com/robot/send?access_token=…，在钉钉群的「设置 → 机器人」里添加自定义机器人后复制",
		"识别模型加载失败，模型文件 model.int8.onnx 可能坏了。请管理员删掉模型目录（ZSMEET_MODELS_DIR，docker compose 部署的是 models 数据卷）里的这个文件，再重启后台处理（docker compose restart worker），它会自动重新下载",
	}
	for _, s := range samples {
		if got := TranslateMessage(ZH, s); got != s {
			t.Errorf("ZH changed %q to %q", s, got)
		}
		got := TranslateMessage(EN, s)
		if got == s {
			t.Errorf("EN did not translate %q", s)
		}
		// Except for user-provided group name "产品研发群" or DingTalk keyword "会议纪要", no CJK should remain.
		cleaned := strings.ReplaceAll(got, "产品研发群", "")
		cleaned = strings.ReplaceAll(cleaned, "会议纪要", "")
		if hasHan(cleaned) {
			t.Errorf("EN translation still contains CJK: %q -> %q", s, got)
		}
	}
}

func hasHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

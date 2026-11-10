package webhook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ZaneXo/zsmeet/internal/feishu"
)

// Message 是发到群里的一条纪要：标题、时间、时长、创建者，纪要里的摘要和待办，和回到 zsmeet 的链接。
type Message struct {
	Title    string
	When     time.Time
	Duration int // 秒
	Owner    string
	// 纪要正文（Markdown），消息里只放摘要和待办（和飞书卡片取的一样）
	Minutes string
	// zsmeet 里这场会议的地址；为空时不放链接
	MeetingURL string
}

// 消息里摘要最多多少个字、列几条待办、每条最多多少个字（和飞书卡片一样）
const (
	summaryRunes = 600
	maxTodos     = 8
	todoRunes    = 120
)

// 各平台对消息大小的限制，都留了余量：
// 企业微信 markdown 的 content 最多 4096 字节；钉钉文档没写 markdown 的上限，按整个请求 15 KB 以内算；
// Slack 一个 section 的文字最多 3000 个字、header 最多 150 个字。
const (
	wecomMaxBytes    = 4000
	dingtalkMaxBytes = 15 << 10
	slackSectionMax  = 3000
	slackHeaderMax   = 150
)

// Payload 返回发到 platform 平台的请求体（JSON）。太长时先少列待办、再缩短摘要。
func (m Message) Payload(platform string) ([]byte, error) {
	summary, todos := feishu.Summary(m.Minutes), feishu.Todos(m.Minutes)
	runes, count := summaryRunes, maxTodos
	for {
		c := content{summary: clip(summary, runes), todos: todos, shown: min(count, len(todos))}
		out, fits, err := m.render(platform, c)
		if err != nil || fits || (count == 0 && runes < 50) {
			return out, err
		}
		if count > 0 {
			count--
		} else {
			runes /= 2
		}
	}
}

// content 是这一次要放进消息里的摘要和待办（shown 是列出几条待办）。
type content struct {
	summary string
	todos   []string
	shown   int
}

func (m Message) render(platform string, c content) (out []byte, fits bool, err error) {
	switch platform {
	case DingTalk:
		text := m.markdown(c, "#### ", "\n\n", false)
		out, err = encode(map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]any{"title": "会议纪要｜" + oneLine(clip(m.Title, 60)), "text": text},
		})
		return out, len(out) <= dingtalkMaxBytes, err
	case WeCom:
		text := m.markdown(c, "### ", "\n", true)
		out, err = encode(map[string]any{"msgtype": "markdown", "markdown": map[string]any{"content": text}})
		return out, len(text) <= wecomMaxBytes, err
	case Slack:
		return m.slack(c)
	}
	return nil, false, fmt.Errorf("不认识的平台：%s", platform)
}

// markdown 是钉钉、企业微信的消息正文。两边的 Markdown 差不多：heading 是标题的写法，listBreak 是待办之间的换行
// （钉钉的列表项之间要空一行才分得开）；企业微信能把时间那一行写成灰色的字。
func (m Message) markdown(c content, heading, listBreak string, gray bool) string {
	esc := escapeMarkdown
	info := esc(m.info())
	if gray {
		info = `<font color="comment">` + info + `</font>`
	}
	parts := []string{heading + "会议纪要｜" + esc(clip(strings.TrimSpace(m.Title), 80)), info}
	if c.summary != "" {
		parts = append(parts, "**摘要**\n\n"+esc(c.summary))
	}
	if c.shown > 0 {
		lines := make([]string, 0, c.shown+1)
		for _, t := range c.todos[:c.shown] {
			lines = append(lines, "- "+esc(clip(t, todoRunes)))
		}
		if more := len(c.todos) - c.shown; more > 0 {
			lines = append(lines, fmt.Sprintf("- 还有 %d 条，见完整纪要", more))
		}
		parts = append(parts, "**待办**\n\n"+strings.Join(lines, listBreak))
	}
	if c.summary == "" && c.shown == 0 {
		parts = append(parts, m.empty())
	}
	if link := m.link(); link != "" {
		parts = append(parts, "[查看完整纪要]("+link+")")
	}
	return strings.Join(parts, "\n\n")
}

// slack 用 Block Kit 排版：标题、时间这一行、摘要、待办、链接。text 是通知和不支持 blocks 的地方显示的文字。
func (m Message) slack(c content) ([]byte, bool, error) {
	title := "会议纪要｜" + oneLine(strings.TrimSpace(m.Title))
	// 标题用的是 plain_text，不认 &lt; 这种转义：尖括号换成全角的，免得被当成提醒、链接
	header := strings.NewReplacer("<", "＜", ">", "＞").Replace(clip(title, slackHeaderMax))
	section := func(text string) any {
		return map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": text}}
	}
	blocks := []any{
		map[string]any{"type": "header", "text": map[string]any{"type": "plain_text", "text": header, "emoji": false}},
		map[string]any{"type": "context", "elements": []any{map[string]any{"type": "mrkdwn", "text": escapeSlack(m.info())}}},
	}
	fits := true
	if c.summary != "" {
		text := "*摘要*\n" + escapeSlack(c.summary)
		fits = fits && utf8.RuneCountInString(text) <= slackSectionMax
		blocks = append(blocks, section(text))
	}
	if c.shown > 0 {
		lines := make([]string, 0, c.shown+1)
		for _, t := range c.todos[:c.shown] {
			lines = append(lines, "• "+escapeSlack(clip(t, todoRunes)))
		}
		if more := len(c.todos) - c.shown; more > 0 {
			lines = append(lines, fmt.Sprintf("• 还有 %d 条，见完整纪要", more))
		}
		text := "*待办*\n" + strings.Join(lines, "\n")
		fits = fits && utf8.RuneCountInString(text) <= slackSectionMax
		blocks = append(blocks, section(text))
	}
	if c.summary == "" && c.shown == 0 {
		blocks = append(blocks, section(m.empty()))
	}
	if link := m.link(); link != "" {
		blocks = append(blocks, section("<"+link+"|查看完整纪要>"))
	}
	out, err := encode(map[string]any{"text": escapeSlack(clip(title, 200)), "blocks": blocks})
	return out, fits, err
}

func (m Message) empty() string {
	if m.link() != "" {
		return "纪要已生成，点下面的链接查看。"
	}
	return "纪要已生成，请在 zsmeet 里查看。"
}

// info 是标题下面那一行：时间 · 时长 · 创建者。
func (m Message) info() string {
	parts := []string{m.When.Local().Format("1月2日 15:04")}
	switch d := m.Duration; {
	case d >= 3600:
		parts = append(parts, fmt.Sprintf("%d 小时 %d 分钟", d/3600, d%3600/60))
	case d >= 60:
		parts = append(parts, fmt.Sprintf("%d 分钟", d/60))
	case d > 0:
		parts = append(parts, fmt.Sprintf("%d 秒", d))
	}
	if owner := oneLine(strings.TrimSpace(m.Owner)); owner != "" {
		parts = append(parts, "创建者 "+owner)
	}
	return strings.Join(parts, " · ")
}

// link 返回能放进消息里的会议地址。地址是 zsmeet 自己拼的，这里再确认一遍：
// 只要 http(s)，不能带空格、括号、尖括号、竖线这些会把 Markdown 链接弄断的字符。
func (m Message) link() string {
	u, err := url.Parse(m.MeetingURL)
	if m.MeetingURL == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		strings.ContainsAny(m.MeetingURL, " \t\r\n()[]<>|\"'`") {
		return ""
	}
	return m.MeetingURL
}

// escapeMarkdown 把纪要里的文字放进钉钉、企业微信的 Markdown 之前转义。这两家都没有可靠的转义写法，
// 就把会变成格式、链接、标签（企业微信的 <font>、<@成员>）的符号换成长得差不多的全角字符；换行换成空格，
// 免得文字里的 # > - 开出标题、引用、列表。
func escapeMarkdown(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteRune('＼')
		case '*':
			b.WriteRune('＊')
		case '_':
			b.WriteRune('＿')
		case '~':
			b.WriteRune('～')
		case '`':
			b.WriteRune('｀')
		case '[':
			b.WriteRune('［')
		case ']':
			b.WriteRune('］')
		case '<':
			b.WriteRune('＜')
		case '>':
			b.WriteRune('＞')
		case '#':
			b.WriteRune('＃')
		case '|':
			b.WriteRune('｜')
		case '\r', '\n', '\t':
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// escapeSlack 按 Slack 的规定转义 & < >：文字里的 <!channel>、<@U123>、<网址|文字> 就不会变成提醒所有人、提到某人、链接。
func escapeSlack(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\r", " ", "\n", " ").Replace(s)
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// clip 把 s 截到最多 n 个字，截掉了就加省略号。
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:max(n-1, 0)]) + "…"
}

func encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}

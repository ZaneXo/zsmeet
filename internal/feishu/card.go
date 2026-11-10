package feishu

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Card 是发到群里的纪要卡片的内容（飞书卡片 JSON 2.0）。
type Card struct {
	Title    string
	When     time.Time
	Duration int // 秒
	Owner    string
	// 纪要正文（Markdown），卡片里只放摘要和待办
	Minutes string
	// zsmeet 里这场会议的地址
	MeetingURL string
	// 飞书文档的地址，没有导入时为空
	DocURL string
	// 以前发过，这次是纪要更新了
	Updated bool
}

// 卡片里摘要最多多少个字、列几条待办、每条最多多少个字
const (
	cardSummaryRunes = 600
	cardTodos        = 8
	cardTodoRunes    = 120
	// 飞书限制卡片请求不超过 30 KB，留出余量
	cardMaxBytes = 20 << 10
)

// JSON 返回卡片的 JSON。太长时先少列待办、再缩短摘要。
func (c Card) JSON() (string, error) {
	summary, todos := Summary(c.Minutes), Todos(c.Minutes)
	summaryRunes, todoCount := cardSummaryRunes, cardTodos
	for {
		out, err := c.build(clip(summary, summaryRunes), todos, todoCount)
		if err != nil || len(out) <= cardMaxBytes || (todoCount == 0 && summaryRunes < 100) {
			return out, err
		}
		if todoCount > 0 {
			todoCount--
		} else {
			summaryRunes /= 2
		}
	}
}

func (c Card) build(summary string, todos []string, todoCount int) (string, error) {
	var md []string
	if summary != "" {
		md = append(md, "**摘要**\n"+escape(summary))
	}
	if len(todos) > 0 && todoCount > 0 {
		shown := todos[:min(todoCount, len(todos))]
		lines := make([]string, len(shown))
		for i, t := range shown {
			lines[i] = "- " + escape(clip(t, cardTodoRunes))
		}
		block := "**待办**\n" + strings.Join(lines, "\n")
		if more := len(todos) - len(shown); more > 0 {
			block += fmt.Sprintf("\n- 还有 %d 条，见完整纪要", more)
		}
		md = append(md, block)
	}
	if len(md) == 0 {
		md = append(md, "纪要已生成，点下面的按钮查看。")
	}

	elements := []any{map[string]any{"tag": "markdown", "content": strings.Join(md, "\n\n")}}
	var buttons []any
	button := func(text, link string, primary bool) any {
		kind := "default"
		if primary {
			kind = "primary"
		}
		return map[string]any{
			"tag": "column", "width": "auto",
			"elements": []any{map[string]any{
				"tag": "button", "type": kind,
				"text":      map[string]any{"tag": "plain_text", "content": text},
				"behaviors": []any{map[string]any{"type": "open_url", "default_url": link}},
			}},
		}
	}
	if c.MeetingURL != "" {
		buttons = append(buttons, button("查看纪要和录音", c.MeetingURL, true))
	}
	if c.DocURL != "" {
		buttons = append(buttons, button("飞书文档", c.DocURL, c.MeetingURL == ""))
	}
	if len(buttons) > 0 {
		elements = append(elements, map[string]any{"tag": "column_set", "columns": buttons})
	}

	subtitle, color := c.subtitle(), "blue"
	if c.Updated {
		subtitle, color = "纪要已更新 · "+subtitle, "orange"
	}
	title := clip(strings.TrimSpace(c.Title), 80)
	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{
			"update_multi": true,
			"width_mode":   "fill",
			// 会话列表里显示的预览
			"summary": map[string]any{"content": "会议纪要：" + title},
		},
		"header": map[string]any{
			"title":    map[string]any{"tag": "plain_text", "content": "会议纪要｜" + title},
			"subtitle": map[string]any{"tag": "plain_text", "content": subtitle},
			"template": color,
		},
		"body": map[string]any{"direction": "vertical", "elements": elements},
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(card); err != nil {
		return "", err
	}
	return strings.TrimSpace(buf.String()), nil
}

func (c Card) subtitle() string {
	parts := []string{c.When.Local().Format("1月2日 15:04")}
	switch d := c.Duration; {
	case d >= 3600:
		parts = append(parts, fmt.Sprintf("%d 小时 %d 分钟", d/3600, d%3600/60))
	case d >= 60:
		parts = append(parts, fmt.Sprintf("%d 分钟", d/60))
	case d > 0:
		parts = append(parts, fmt.Sprintf("%d 秒", d))
	}
	if c.Owner != "" {
		parts = append(parts, c.Owner)
	}
	return strings.Join(parts, " · ")
}

// escape 把纪要里的文字放进卡片的 Markdown 之前转义：文字里的 < > 不能变成卡片的标签
// （<at id=all></at> 会 @ 所有人），* ~ [ ] 这些不能变成格式或者链接。
func escape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '&', '<', '>', '*', '~', '[', ']', '(', ')', '`':
			fmt.Fprintf(&b, "&#%d;", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// clip 把 s 截到最多 n 个字，截掉了就加省略号。
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:max(n-1, 0)]) + "…"
}

var (
	headingLine = regexp.MustCompile(`^#{1,6}\s+(.*)$`)
	listItem    = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s+(.*)$`)
	tableRule   = regexp.MustCompile(`^\s*\|?(\s*:?-{3,}:?\s*\|)+\s*:?-*:?\s*\|?\s*$`)
)

// section 返回纪要里标题满足 match 的那一节的各行（不含标题），没有返回 nil。
func section(minutes string, match func(heading string) bool) []string {
	var out []string
	inside, found := false, false
	for line := range strings.SplitSeq(strings.ReplaceAll(minutes, "\r\n", "\n"), "\n") {
		if m := headingLine.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			if inside {
				break
			}
			inside = match(strings.TrimSpace(m[1]))
			found = found || inside
			continue
		}
		if inside {
			out = append(out, line)
		}
	}
	if !found {
		return nil
	}
	return out
}

// plain 去掉一行里的 Markdown 格式（加粗、列表符号），留下给人看的文字。
func plain(line string) string {
	line = strings.TrimSpace(line)
	if m := listItem.FindStringSubmatch(line); m != nil {
		line = m[1]
	}
	line = strings.TrimLeft(line, "> ")
	line = strings.ReplaceAll(line, "**", "")
	line = strings.ReplaceAll(line, "__", "")
	return strings.TrimSpace(line)
}

// Summary 取纪要的摘要：「摘要」一节的文字；没有这一节时取正文的第一段。
func Summary(minutes string) string {
	lines := section(minutes, func(h string) bool { return strings.Contains(h, "摘要") || strings.Contains(h, "总结") })
	if lines == nil {
		// 没有摘要一节：取正文的第一段（跳过标题，到空行或者下一个标题为止）
		for line := range strings.SplitSeq(minutes, "\n") {
			t := strings.TrimSpace(line)
			if t == "" || headingLine.MatchString(t) {
				if len(lines) > 0 {
					break
				}
				continue
			}
			lines = append(lines, t)
		}
	}
	var parts []string
	for _, l := range lines {
		if p := plain(l); p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, " ")
}

// Todos 取纪要里的待办：标题里有「待办」「跟进」「行动项」的那一节的每一条（列表或者表格）。
func Todos(minutes string) []string {
	lines := section(minutes, func(h string) bool {
		return strings.Contains(h, "待办") || strings.Contains(h, "跟进") || strings.Contains(h, "行动项")
	})
	var out []string
	header := true
	for _, l := range lines {
		t := strings.TrimSpace(l)
		switch {
		case t == "" || tableRule.MatchString(t):
		case strings.HasPrefix(t, "|"):
			// 表格的第一行是表头，不算
			if header {
				header = false
				continue
			}
			var cells []string
			for c := range strings.SplitSeq(strings.Trim(t, "|"), "|") {
				if c = plain(c); c != "" {
					cells = append(cells, c)
				}
			}
			if len(cells) > 0 {
				out = append(out, strings.Join(cells, " · "))
			}
		case listItem.MatchString(t):
			if p := plain(t); p != "" && p != "无" && p != "暂无" {
				out = append(out, p)
			}
		}
	}
	return out
}

package export

import (
	"fmt"
	"strings"

	"github.com/ZaneXo/zsmeet/internal/transcript"
)

// MinutesMarkdown 是导出的纪要：一级标题是会议标题，下面几行会议信息，再接纪要正文。
func MinutesMarkdown(title string, info []string, content string) string {
	var b strings.Builder
	b.WriteString("# " + title + "\n\n")
	for _, line := range info {
		b.WriteString("- " + line + "\n")
	}
	if len(info) > 0 {
		b.WriteString("\n")
	}
	b.WriteString(strings.TrimSpace(content) + "\n")
	return b.String()
}

// TranscriptText 是导出的转写（纯文本）：每段一行「[时间] 名字：内容」，段和段之间空一行。
// 同一个人连着说的几句合成一段，和网页上看到的一样。
func TranscriptText(title string, info []string, segs []transcript.Segment, names transcript.Names) string {
	var b strings.Builder
	b.WriteString(title + "\n")
	for _, line := range info {
		b.WriteString(line + "\n")
	}
	for _, p := range paragraphs(segs) {
		fmt.Fprintf(&b, "\n[%s] %s：%s\n", transcript.Clock(p.Start), names.Of(p.Speaker), p.Text)
	}
	return b.String()
}

// TranscriptMarkdown 和 TranscriptText 一样，只是写成 Markdown（名字和时间加粗），用来生成 Word。
func TranscriptMarkdown(title string, info []string, segs []transcript.Segment, names transcript.Names) string {
	var b strings.Builder
	b.WriteString("# " + title + "\n\n")
	for _, line := range info {
		b.WriteString("- " + line + "\n")
	}
	for _, p := range paragraphs(segs) {
		fmt.Fprintf(&b, "\n**[%s] %s**：%s\n", transcript.Clock(p.Start), names.Of(p.Speaker), p.Text)
	}
	return b.String()
}

// 合成段落：同一个人、中间停不到 2.5 秒、一段不超过 280 个字，和网页上的转写一致
func paragraphs(segs []transcript.Segment) []transcript.Segment {
	return transcript.Merge(segs, 2.5, 280, nil)
}

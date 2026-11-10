package export

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"io"
	"strings"
	"testing"

	"github.com/ZaneXo/zsmeet/internal/transcript"
)

// documentXML 解开 .docx，确认每个 XML 文件都是合法的，返回正文。
func documentXML(t *testing.T, docx []byte) string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(docx), int64(len(docx)))
	if err != nil {
		t.Fatalf("不是合法的 zip：%v", err)
	}
	var body string
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(rc)
		rc.Close()
		dec := xml.NewDecoder(bytes.NewReader(raw))
		for {
			if _, err := dec.Token(); err == io.EOF {
				break
			} else if err != nil {
				t.Fatalf("%s 不是合法的 XML：%v", f.Name, err)
			}
		}
		if f.Name == "word/document.xml" {
			body = string(raw)
		}
	}
	for _, need := range []string{"[Content_Types].xml", "_rels/.rels", "word/document.xml", "word/styles.xml", "word/_rels/document.xml.rels"} {
		if !names[need] {
			t.Errorf("缺少 %s", need)
		}
	}
	return body
}

func TestDocx(t *testing.T) {
	md := "# 产品周会\n\n## 结论\n- ★ [00:15] **飞书集成**放到下一版\n  - 子项 <王悦> & 调研\n1. 第一件事\n\n正文第一行\n正文第二行\n\n" +
		"| 事项 | 负责人 |\n|---|---|\n| 调研 | 王悦 |\n\n> 引用\n\n看 [链接](https://example.com)，`代码`，*斜体*\x01。\n\n```\nfmt.Println(1)\n```\n---\n"
	docx, err := Docx(md)
	if err != nil {
		t.Fatal(err)
	}
	body := documentXML(t, docx)
	for _, want := range []string{
		`<w:pStyle w:val="Heading1"/>`, `<w:pStyle w:val="Heading2"/>`, // 标题样式
		`• ★ [00:15] `, `<w:b/></w:rPr><w:t xml:space="preserve">飞书集成</w:t>`, // 列表和加粗
		`<w:ind w:left="720"/>`, `子项 &lt;王悦&gt; &amp; 调研`, // 缩进的子项，特殊字符转义
		`1. 第一件事`, `正文第一行 正文第二行`, // 编号保留；连着的普通行合成一段
		`事项    负责人`, `调研    王悦`, // 表格变成文字
		`<w:pStyle w:val="Quote"/>`, `链接（https://example.com），代码，`, `<w:i/></w:rPr><w:t xml:space="preserve">斜体</w:t>`,
		`<w:pStyle w:val="Code"/></w:pPr><w:r><w:t xml:space="preserve">fmt.Println(1)`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Word 正文里缺少 %q", want)
		}
	}
	if strings.Contains(body, "\x01") || strings.Contains(body, "|---|") || strings.Contains(body, ">---<") || strings.Contains(body, "`") {
		t.Error("控制字符、表格分隔行、分隔线、反引号不该出现在正文里")
	}
}

func TestTranscriptExport(t *testing.T) {
	segs := []transcript.Segment{
		{Start: 0, End: 2, Speaker: 0, Text: "今天开周会。"},
		{Start: 2.2, End: 4, Speaker: 0, Text: "先说结论。"},
		{Start: 5, End: 7, Speaker: 1, Text: "好的。"},
	}
	names := transcript.Names{0: "张敏"}
	text := TranscriptText("产品周会", []string{"时间：11月9日"}, segs, names)
	if text != "产品周会\n时间：11月9日\n\n[00:00] 张敏：今天开周会。先说结论。\n\n[00:05] 说话人 2：好的。\n" {
		t.Errorf("纯文本转写不对：%q", text)
	}
	md := TranscriptMarkdown("产品周会", nil, segs, names)
	if !strings.Contains(md, "**[00:00] 张敏**：今天开周会。先说结论。") {
		t.Errorf("Markdown 转写不对：%q", md)
	}
}

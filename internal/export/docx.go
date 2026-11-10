// Package export 把纪要和转写导出成文件：Markdown 原样，Word（.docx）由这里从 Markdown 生成。
//
// 纪要是大模型按固定格式写的 Markdown，只用到标题、段落、列表、加粗这几种，所以这里只认这些，
// 不引入完整的 Markdown 解析器和 Word 库。.docx 本身是一个 zip 包，里面几个 XML 文件。
package export

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"regexp"
	"strconv"
	"strings"
)

// Docx 把 Markdown 转成 Word 文档。标题用 Word 自带的「标题 1/2/3」样式（导航窗格里能看到目录），
// 列表项前面加圆点或保留编号，**加粗** 和 *斜体* 保留，链接写成「文字（地址）」。
func Docx(markdown string) ([]byte, error) {
	var body strings.Builder
	for _, block := range blocks(markdown) {
		body.WriteString(block)
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	files := []struct{ name, content string }{
		{"[Content_Types].xml", contentTypes},
		{"_rels/.rels", rootRels},
		{"word/_rels/document.xml.rels", documentRels},
		{"word/styles.xml", styles},
		{"word/document.xml", documentHead + body.String() + documentTail},
	}
	for _, f := range files {
		w, err := zw.Create(f.name)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write([]byte(f.content)); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

var (
	heading  = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	bullet   = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
	numbered = regexp.MustCompile(`^(\s*)(\d+[.)])\s+(.*)$`)
	rule     = regexp.MustCompile(`^\s*([-*_])(\s*[-*_]){2,}\s*$`)
	tableSep = regexp.MustCompile(`^\s*\|?(\s*:?-{3,}:?\s*\|)+\s*:?-*:?\s*\|?\s*$`)
)

// blocks 一行一行地把 Markdown 变成 Word 的段落（<w:p>）。连续的普通文字行合成一段。
func blocks(markdown string) []string {
	var out []string
	var para []string
	inCode := false
	flush := func() {
		if len(para) > 0 {
			out = append(out, paragraph("", 0, strings.Join(para, " ")))
			para = nil
		}
	}
	for _, line := range strings.Split(strings.ReplaceAll(markdown, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			flush()
			inCode = !inCode
			continue
		}
		if inCode {
			out = append(out, paragraph("Code", 0, line))
			continue
		}
		switch {
		case trimmed == "":
			flush()
		case rule.MatchString(trimmed), tableSep.MatchString(trimmed):
			flush()
		case heading.MatchString(trimmed):
			flush()
			m := heading.FindStringSubmatch(trimmed)
			level := min(len(m[1]), 3)
			out = append(out, paragraph("Heading"+strconv.Itoa(level), 0, m[2]))
		case bullet.MatchString(line):
			flush()
			m := bullet.FindStringSubmatch(line)
			out = append(out, paragraph("ListParagraph", indentLevel(m[1]), "• "+m[2]))
		case numbered.MatchString(line):
			flush()
			m := numbered.FindStringSubmatch(line)
			out = append(out, paragraph("ListParagraph", indentLevel(m[1]), m[2]+" "+m[3]))
		case strings.HasPrefix(trimmed, "|"):
			// 表格的一行：单元格之间用空格隔开，当成一段文字
			flush()
			cells := strings.Split(strings.Trim(trimmed, "|"), "|")
			for i := range cells {
				cells[i] = strings.TrimSpace(cells[i])
			}
			out = append(out, paragraph("", 0, strings.Join(cells, "    ")))
		case strings.HasPrefix(trimmed, ">"):
			flush()
			out = append(out, paragraph("Quote", 0, strings.TrimSpace(strings.TrimLeft(trimmed, ">"))))
		default:
			para = append(para, trimmed)
		}
	}
	flush()
	return out
}

func indentLevel(space string) int {
	n := len(strings.ReplaceAll(space, "\t", "  "))
	return min(n/2, 4)
}

// paragraph 生成一段。style 是段落样式（空字符串是正文），level 是列表的缩进层级。
func paragraph(style string, level int, text string) string {
	var b strings.Builder
	b.WriteString("<w:p>")
	if style != "" || level > 0 {
		b.WriteString("<w:pPr>")
		if style != "" {
			b.WriteString(`<w:pStyle w:val="` + style + `"/>`)
		}
		if level > 0 {
			b.WriteString(`<w:ind w:left="` + strconv.Itoa(360+level*360) + `"/>`)
		}
		b.WriteString("</w:pPr>")
	}
	for _, r := range runs(text) {
		b.WriteString("<w:r>")
		if r.bold || r.italic {
			b.WriteString("<w:rPr>")
			if r.bold {
				b.WriteString("<w:b/>")
			}
			if r.italic {
				b.WriteString("<w:i/>")
			}
			b.WriteString("</w:rPr>")
		}
		b.WriteString(`<w:t xml:space="preserve">` + escape(r.text) + "</w:t></w:r>")
	}
	b.WriteString("</w:p>")
	return b.String()
}

type run struct {
	text         string
	bold, italic bool
}

var (
	link       = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)\)`)
	inlineCode = regexp.MustCompile("`([^`]+)`")
	emphasis   = regexp.MustCompile(`\*\*(.+?)\*\*|__(.+?)__|\*([^*\s][^*]*?)\*`)
)

// runs 把一段里的 **加粗**、*斜体* 拆成不同格式的片段；链接写成「文字（地址）」，行内代码去掉反引号。
func runs(text string) []run {
	text = link.ReplaceAllString(text, "$1（$2）")
	text = inlineCode.ReplaceAllString(text, "$1")
	var out []run
	last := 0
	for _, m := range emphasis.FindAllStringSubmatchIndex(text, -1) {
		if m[0] > last {
			out = append(out, run{text: text[last:m[0]]})
		}
		switch {
		case m[2] >= 0:
			out = append(out, run{text: text[m[2]:m[3]], bold: true})
		case m[4] >= 0:
			out = append(out, run{text: text[m[4]:m[5]], bold: true})
		default:
			out = append(out, run{text: text[m[6]:m[7]], italic: true})
		}
		last = m[1]
	}
	if last < len(text) {
		out = append(out, run{text: text[last:]})
	}
	return out
}

func escape(s string) string {
	var b bytes.Buffer
	// 去掉 XML 里不允许出现的控制字符，Word 遇到会打不开
	clean := strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' {
			return -1
		}
		return r
	}, s)
	_ = xml.EscapeText(&b, []byte(clean))
	return b.String()
}

const contentTypes = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
<Default Extension="xml" ContentType="application/xml"/>
<Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>
<Override PartName="/word/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.styles+xml"/>
</Types>`

const rootRels = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>
</Relationships>`

const documentRels = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>
</Relationships>`

const documentHead = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>`

// A4 纸，页边距 2.5 厘米
const documentTail = `<w:sectPr><w:pgSz w:w="11906" w:h="16838"/><w:pgMar w:top="1418" w:right="1418" w:bottom="1418" w:left="1418" w:header="851" w:footer="992" w:gutter="0"/></w:sectPr></w:body></w:document>`

// 字体：西文 Calibri，中文微软雅黑（Mac 上没有时 Word 会自动换成苹方之类的中文字体）
const styles = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
<w:docDefaults>
<w:rPrDefault><w:rPr><w:rFonts w:ascii="Calibri" w:hAnsi="Calibri" w:eastAsia="Microsoft YaHei" w:cs="Calibri"/><w:sz w:val="22"/><w:szCs w:val="22"/><w:lang w:val="en-US" w:eastAsia="zh-CN"/></w:rPr></w:rPrDefault>
<w:pPrDefault><w:pPr><w:spacing w:after="120" w:line="300" w:lineRule="auto"/></w:pPr></w:pPrDefault>
</w:docDefaults>
<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/><w:qFormat/></w:style>
<w:style w:type="paragraph" w:styleId="Heading1"><w:name w:val="heading 1"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/><w:pPr><w:keepNext/><w:spacing w:before="360" w:after="160"/><w:outlineLvl w:val="0"/></w:pPr><w:rPr><w:b/><w:sz w:val="36"/><w:szCs w:val="36"/></w:rPr></w:style>
<w:style w:type="paragraph" w:styleId="Heading2"><w:name w:val="heading 2"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/><w:pPr><w:keepNext/><w:spacing w:before="280" w:after="120"/><w:outlineLvl w:val="1"/></w:pPr><w:rPr><w:b/><w:sz w:val="30"/><w:szCs w:val="30"/></w:rPr></w:style>
<w:style w:type="paragraph" w:styleId="Heading3"><w:name w:val="heading 3"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/><w:pPr><w:keepNext/><w:spacing w:before="200" w:after="80"/><w:outlineLvl w:val="2"/></w:pPr><w:rPr><w:b/><w:sz w:val="26"/><w:szCs w:val="26"/></w:rPr></w:style>
<w:style w:type="paragraph" w:styleId="ListParagraph"><w:name w:val="List Paragraph"/><w:basedOn w:val="Normal"/><w:qFormat/><w:pPr><w:spacing w:after="60"/><w:ind w:left="360" w:hanging="240"/></w:pPr></w:style>
<w:style w:type="paragraph" w:styleId="Quote"><w:name w:val="Quote"/><w:basedOn w:val="Normal"/><w:qFormat/><w:pPr><w:ind w:left="480"/></w:pPr><w:rPr><w:i/><w:color w:val="555555"/></w:rPr></w:style>
<w:style w:type="paragraph" w:styleId="Code"><w:name w:val="Code"/><w:basedOn w:val="Normal"/><w:pPr><w:spacing w:after="0"/></w:pPr><w:rPr><w:rFonts w:ascii="Menlo" w:hAnsi="Menlo"/><w:sz w:val="18"/></w:rPr></w:style>
</w:styles>`

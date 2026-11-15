package transcript

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Token 是识别模型给出的一个字（英文是一个词或者词的一部分，也可能是一个标点）和它开始的时间，
// 秒，从这一小段录音的开头算。
type Token struct {
	Text  string
	Start float64
}

// Speech 是语音检测切出的一小段有人说话的区间（Start 到 End 秒），以及本地识别出的字和句子。
type Speech struct {
	Start, End float64
	// 本地识别出的字和标点，Start 是从这一小段开头算的秒数（0 到 End-Start）。模型没给每个字的时间时为空
	Tokens []Token
	// 按标点切出的句子
	Segments []Segment
}

// 句子里攒到这么多字还没遇到句末标点，就在下一个逗号处断开，免得一句太长、定位不准
const longSentence = 50

// Sentences 把一小段录音识别出的字切成句子：遇到句号、问号、感叹号、分号断开，太长时在逗号处断开。
// 每句从它第一个字的时间开始，到下一句第一个字的时间结束；最后一句到 end 结束。
// offset 是这一小段在整场录音里的开始时间，end 是它的结束时间。说话人留给 AssignSpeakers 填。
func Sentences(tokens []Token, offset, end float64) []Segment {
	var out []Segment
	var b strings.Builder
	first := -1 // 这一句第一个字的下标
	flush := func(next int) {
		text := collapseCJKSpaces(strings.TrimSpace(b.String()))
		b.Reset()
		if text != "" && first >= 0 {
			stop := end
			if next < len(tokens) {
				stop = offset + tokens[next].Start
			}
			out = append(out, Segment{Start: offset + tokens[first].Start, End: max(stop, offset+tokens[first].Start), Speaker: -1, Text: text})
		}
		first = -1
	}
	for i, tok := range tokens {
		// 英文单词的开头带一个空格（模型词表里写作 ▁，sherpa-onnx 给出时已经换成了空格）
		piece := strings.ReplaceAll(tok.Text, "▁", " ")
		if strings.TrimSpace(piece) == "" {
			// 单独的空格也是单词之间的分隔，比如数字前面（词表里没有「▁3」，只能拆成「▁」和「3」）。
			// 句子开始以后要留着，不然「have 20 people」会变成「have20 people」
			if first >= 0 && !strings.HasSuffix(b.String(), " ") {
				b.WriteByte(' ')
			}
			continue
		}
		if first < 0 {
			if isPunct(piece) {
				continue // 句子不从标点开始
			}
			first = i
			piece = strings.TrimLeft(piece, " ")
		}
		b.WriteString(piece)
		switch {
		case endsSentence(tokens, i):
			flush(i + 1)
		case (piece == "，" || piece == ",") && utf8.RuneCountInString(b.String()) >= longSentence:
			flush(i + 1)
		}
	}
	flush(len(tokens))
	return out
}

// endsSentence：第 i 个字是不是句末标点。英文的点号只有后面跟着新的单词（开头是空格，或者到头了）才算，
// 免得把 3.5、v1.2 这样的数字切开。
func endsSentence(tokens []Token, i int) bool {
	switch tokens[i].Text {
	case "。", "？", "！", "；", "?", "!", ";", "…":
		return true
	case ".":
		if i+1 == len(tokens) {
			return true
		}
		next := tokens[i+1].Text
		return strings.HasPrefix(next, " ") || strings.HasPrefix(next, "▁")
	}
	return false
}

func isPunct(s string) bool {
	for _, r := range strings.TrimSpace(s) {
		if !unicode.IsPunct(r) && !unicode.IsSymbol(r) {
			return false
		}
	}
	return true
}

// collapseCJKSpaces 去掉日文分词里夹在汉字、假名和全角标点之间的空格（SenseVoice 识别日文时在词组之间会输出单独的空格），
// 但保留英文、数字和韩文（Hangul）词间的空格，也保留中/日文与英文、数字之间的空格。
func collapseCJKSpaces(s string) string {
	if !strings.ContainsRune(s, ' ') {
		return s
	}
	runes := []rune(s)
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(runes); {
		if runes[i] != ' ' {
			b.WriteRune(runes[i])
			i++
			continue
		}
		j := i + 1
		for j < len(runes) && runes[j] == ' ' {
			j++
		}
		if i > 0 && j < len(runes) {
			prev, next := runes[i-1], runes[j]
			if isNoSpaceCJK(prev) && isNoSpaceCJK(next) && (isHanOrKana(prev) || isHanOrKana(next)) {
				i = j
				continue
			}
		}
		b.WriteByte(' ')
		i = j
	}
	return b.String()
}

func isHanOrKana(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.In(r, unicode.Hiragana, unicode.Katakana)
}

func isNoSpaceCJK(r rune) bool {
	if isHanOrKana(r) {
		return true
	}
	switch r {
	case '、', '。', '，', '？', '！', '：', '；', '「', '」', '『', '』', '（', '）', '《', '》':
		return true
	}
	return false
}

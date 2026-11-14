// Package correct 改正语音识别转写里的错字，分两步：
//
//   - Homophones：按热词的拼音，把转写里读音一样、写法不同的地方换成热词。不用大模型，确定、便宜，
//     专治「人名、产品名总被认成同一组同音字」；
//   - LLM：把转写分批交给大模型，按上下文逐句校对同音字、近音字和专有名词。改动太大的句子不采用，
//     免得大模型润色、改写、编造。
//
// 两步都只改句子的文字，不动时间和说话人。改过的句子在 Segment.Orig 里留着识别出的原文。
package correct

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ZaneXo/zsmeet/internal/transcript"
)

// ParseHotwords 把「本场热词」拆成一个个词：用逗号（中英文）、顿号、分号或换行隔开。
// 去掉首尾空白、空的和重复的（不分大小写），保持原来的顺序。
func ParseHotwords(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return strings.ContainsRune(",，、;；\n\r", r)
	})
	var out []string
	seen := map[string]bool{}
	for _, f := range fields {
		f = strings.TrimSpace(f)
		key := strings.ToLower(f)
		if f == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, f)
	}
	return out
}

// set 改一句话的文字。第一次改的时候把识别出的原文存进 Orig，之后再改 Orig 不变。
func set(seg *transcript.Segment, text string) {
	if text == seg.Text {
		return
	}
	if seg.Orig == "" {
		seg.Orig = seg.Text
	}
	seg.Text = text
	// 改回了原文（大模型把拼音这一步改的又改回去了）就不算改过
	if seg.Text == seg.Orig {
		seg.Orig = ""
	}
}

// Tokens 把一句话切成用来比较的单位：一个汉字（以及日文假名、韩文）算一个，连着的字母数字算一个（英文单词、数字），
// 英文统一成小写；标点和空白不算。算改动大不大（见 acceptable）和评测时的字错误率都按它算。
func Tokens(s string) []string {
	var out []string
	var word strings.Builder
	flush := func() {
		if word.Len() > 0 {
			out = append(out, word.String())
			word.Reset()
		}
	}
	for _, r := range s {
		switch {
		case isCJK(r):
			flush()
			out = append(out, string(r))
		case r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r)) || r == '\'' && word.Len() > 0:
			word.WriteRune(unicode.ToLower(r))
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			// 全角字母数字、带音标的字母：统一成半角小写接着拼
			word.WriteString(strings.ToLower(string(widthFold(r))))
		default:
			flush()
		}
	}
	flush()
	return out
}

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r)
}

// widthFold 把全角的字母数字换成半角。
func widthFold(r rune) rune {
	if r >= '！' && r <= '～' {
		return r - '！' + '!'
	}
	return r
}

// Distance 是两串单位之间的编辑距离（插入、删除、替换各算一次）。
func Distance(a, b []string) int {
	if len(a) < len(b) {
		a, b = b, a
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// acceptable 判断大模型改过的一句话能不能用：不能是空的、不能换行，改动的单位数（见 Tokens）不超过原句的四成
// （至少允许改 3 个），长度变化不超过原句的三分之一（至少允许 3 个）。
// 改错字只换几个字；改得比这多，多半是在润色、改写，或者把别的句子的内容挪了过来。
func acceptable(orig, fixed string) bool {
	if strings.TrimSpace(fixed) == "" || strings.ContainsAny(fixed, "\r\n") {
		return false
	}
	a, b := Tokens(orig), Tokens(fixed)
	if len(b) == 0 && len(a) > 0 {
		return false
	}
	n := len(a)
	if abs(len(a)-len(b)) > max(3, n/3) {
		return false
	}
	return Distance(a, b) <= max(3, (n*4+9)/10)
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

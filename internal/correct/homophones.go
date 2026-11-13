package correct

import (
	"cmp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/mozillazg/go-pinyin"

	"github.com/ZaneXo/zsmeet/internal/transcript"
)

// 不带声调、列出多音字的所有读音
var pinyinArgs = pinyin.Args{Style: pinyin.Normal, Heteronym: true, Fallback: func(rune, pinyin.Args) []string { return nil }}

// readings 是一个汉字的所有读音（不带声调）。不认识的字没有读音，和谁都对不上。
type readings []string

func readingsOf(r rune, cache map[rune]readings) readings {
	if rs, ok := cache[r]; ok {
		return rs
	}
	rs := readings(pinyin.SinglePinyin(r, pinyinArgs))
	cache[r] = rs
	return rs
}

// sameSound：两个字有一个读音对得上（不看声调）。同一个字当然算。
func sameSound(a, b rune, cache map[rune]readings) bool {
	if a == b {
		return true
	}
	ra, rb := readingsOf(a, cache), readingsOf(b, cache)
	return slices.ContainsFunc(ra, func(p string) bool { return slices.Contains(rb, p) })
}

// homophoneWord 是能按读音替换的热词：全是汉字、至少两个字。
// 一个字的太容易误伤（热词「敏」会把所有读 min 的字都换掉），带字母数字的读音对不上，都留给大模型。
func homophoneWord(w string) bool {
	if utf8.RuneCountInString(w) < 2 {
		return false
	}
	for _, r := range w {
		if !unicode.Is(unicode.Han, r) {
			return false
		}
	}
	return true
}

// Homophones 把转写里和热词读音相同（不看声调，多音字任一读音对上就算）但写法不同的地方换成热词，返回改了几句。
// 只用全是汉字、至少两个字的热词（见 homophoneWord），其余的跳过。
//
// 从左往右找，同一个位置几个热词都对得上时用最长的；已经是热词的地方原样跳过，不会被短的热词拆开改。
// 这是按读音硬换，不看意思：热词要是个常用词（「权限」），转写里读音一样的别的词（「全线」）也会被换掉，
// 所以热词应该是人名、产品名这类专有名词。
func Homophones(segs []transcript.Segment, hotwords []string) int {
	var words [][]rune
	for _, w := range hotwords {
		if homophoneWord(w) {
			words = append(words, []rune(w))
		}
	}
	if len(words) == 0 {
		return 0
	}
	// 长的在前：同一个位置先试长的
	slices.SortStableFunc(words, func(a, b []rune) int { return cmp.Compare(len(b), len(a)) })

	cache := map[rune]readings{}
	changed := 0
	for i := range segs {
		text := []rune(segs[i].Text)
		out := make([]rune, 0, len(text))
		for pos := 0; pos < len(text); {
			w := matchAt(text, pos, words, cache)
			if w == nil {
				out = append(out, text[pos])
				pos++
				continue
			}
			out = append(out, w...)
			pos += len(w)
		}
		if s := string(out); s != segs[i].Text {
			set(&segs[i], s)
			changed++
		}
	}
	return changed
}

// matchAt 找出 text 从 pos 开始读音对得上的最长热词，没有时返回 nil。
func matchAt(text []rune, pos int, words [][]rune, cache map[rune]readings) []rune {
	for _, w := range words {
		if pos+len(w) > len(text) {
			continue
		}
		ok := true
		for k, r := range w {
			if !sameSound(text[pos+k], r, cache) {
				ok = false
				break
			}
		}
		if ok {
			return w
		}
	}
	return nil
}

// 每个字只取最常用的读音
var firstReading = pinyin.Args{Style: pinyin.Normal, Fallback: func(rune, pinyin.Args) []string { return nil }}

// pinyinOf 给一句话注音：汉字换成拼音（不带声调），字母数字照抄，标点和空白去掉，用空格隔开。
// 没有汉字时为空，这句不用注音。
func pinyinOf(s string) string {
	var parts []string
	var word []rune
	han := false
	flush := func() {
		if len(word) > 0 {
			parts = append(parts, string(word))
			word = word[:0]
		}
	}
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Han, r):
			flush()
			if p := pinyin.SinglePinyin(r, firstReading); len(p) > 0 {
				parts = append(parts, p[0])
				han = true
			} else {
				parts = append(parts, string(r))
			}
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			word = append(word, r)
		default:
			flush()
		}
	}
	flush()
	if !han {
		return ""
	}
	return strings.Join(parts, " ")
}

// Package search 把文本切成 Postgres 全文检索用的词。
//
// 中文没有空格，这里不用词典：每段连续的中日韩文字按「单字 + 相邻两字」切，
// 英文和数字按单词切并转小写。查询「下单流程」会变成「下单」「单流」「流程」三个词，
// 三个都命中才算，效果接近短语匹配，而且不依赖数据库的 locale。
package search

import (
	"strings"
	"unicode"
)

// 一篇文档最多保留这么多个不同的词，防止超出 tsvector 约 1 MB 的上限
const maxTerms = 60_000

// 单个英文词的最大长度，超长的多半是链接或乱码
const maxWordLen = 64

func isCJK(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul)
}

func isWord(r rune) bool {
	return !isCJK(r) && (unicode.IsLetter(r) || unicode.IsDigit(r))
}

// runs 依次回调每一段连续的中日韩文字或英文数字单词。
func runs(text string, fn func(run []rune, cjk bool)) {
	var cur []rune
	curCJK := false
	flush := func() {
		if len(cur) > 0 {
			fn(cur, curCJK)
			cur = nil
		}
	}
	for _, r := range text {
		switch {
		case isCJK(r):
			if !curCJK {
				flush()
			}
			curCJK = true
			cur = append(cur, r)
		case isWord(r):
			if curCJK {
				flush()
			}
			curCJK = false
			cur = append(cur, unicode.ToLower(r))
		default:
			flush()
		}
	}
	flush()
}

// DocTerms 返回文档里所有不重复的检索词，用来建索引。
func DocTerms(texts ...string) []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(t string) {
		if len(out) >= maxTerms {
			return
		}
		if _, ok := seen[t]; !ok {
			seen[t] = struct{}{}
			out = append(out, t)
		}
	}
	for _, text := range texts {
		runs(text, func(run []rune, cjk bool) {
			if !cjk {
				if len(run) <= maxWordLen {
					add(string(run))
				}
				return
			}
			for i, r := range run {
				add(string(r))
				if i+1 < len(run) {
					add(string(run[i : i+2]))
				}
			}
		})
		// 两段中文之间只隔着空格时（「产品 周会」），跨过空格的两个字也记下来，搜「产品周会」也能找到。
		// 换行不算：转写一句一行，跨句的字连不成词。查询里的空格是分隔符，不这样处理
		var prev rune
		gap := false
		for _, r := range text {
			switch {
			case isCJK(r):
				if prev != 0 && gap {
					add(string([]rune{prev, r}))
				}
				prev, gap = r, false
			case unicode.IsSpace(r) && r != '\n' && r != '\r':
				gap = prev != 0
			default:
				prev, gap = 0, false
			}
		}
	}
	return out
}

// QueryTerms 返回查询要匹配的词：中文取相邻两字（只有一个字时取单字），英文取单词。
func QueryTerms(query string) []string {
	seen := make(map[string]struct{})
	out := []string{}
	add := func(t string) {
		if _, ok := seen[t]; !ok {
			seen[t] = struct{}{}
			out = append(out, t)
		}
	}
	runs(strings.TrimSpace(query), func(run []rune, cjk bool) {
		switch {
		case !cjk:
			if len(run) <= maxWordLen {
				add(string(run))
			}
		case len(run) == 1:
			add(string(run))
		default:
			for i := range len(run) - 1 {
				add(string(run[i : i+2]))
			}
		}
	})
	return out
}

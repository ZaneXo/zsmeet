package main

import (
	"regexp"
	"strings"
)

// 评分前把阿拉伯数字换成汉字的读法，标准文字和识别结果都换。公开数据集的标准文字大多按读音写数字
// （「十八到三十五岁」），识别模型却会写成「18到35岁」，不换的话同一句话会算出一堆错字；
// 两边都换，标准文字本来写的是阿拉伯数字时也照样对得上。
//
// 读法按最常见的说法，猜不准的（「两千」还是「二千」）统一一种，对所有方案都一样，不影响比较。

var numberPattern = regexp.MustCompile(`\d+(?:\.\d+)?%|\d{1,2}:\d{2}|\d+\.\d+|\d+`)

var digitNames = []string{"零", "一", "二", "三", "四", "五", "六", "七", "八", "九"}

// spokenNumbers 把 s 里的阿拉伯数字换成汉字读法。
func spokenNumbers(s string) string {
	locs := numberPattern.FindAllStringIndex(s, -1)
	if locs == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, loc := range locs {
		b.WriteString(s[last:loc[0]])
		b.WriteString(readNumber(s[loc[0]:loc[1]], s[loc[1]:]))
		last = loc[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

// readNumber 读一个数。after 是数后面的文字，用来判断是不是年份。
func readNumber(n, after string) string {
	switch {
	case strings.HasSuffix(n, "%"):
		return "百分之" + readNumber(strings.TrimSuffix(n, "%"), "")
	case strings.Contains(n, ":"):
		h, m, _ := strings.Cut(n, ":")
		if m == "00" {
			return readInteger(h) + "点"
		}
		return readInteger(h) + "点" + readInteger(m)
	case strings.Contains(n, "."):
		i, f, _ := strings.Cut(n, ".")
		return readInteger(i) + "点" + readDigits(f)
	case len(n) == 4 && strings.HasPrefix(after, "年"):
		// 2025年：一个一个读
		return readDigits(n)
	case len(n) > 1 && n[0] == '0' || len(n) > 8:
		// 编号、电话：一个一个读
		return readDigits(n)
	default:
		return readInteger(n)
	}
}

func readDigits(n string) string {
	var b strings.Builder
	for _, c := range n {
		b.WriteString(digitNames[c-'0'])
	}
	return b.String()
}

// readInteger 读一个不超过 8 位的整数：12 → 十二，105 → 一百零五，30000 → 三万，1200 → 一千二百。
func readInteger(n string) string {
	n = strings.TrimLeft(n, "0")
	if n == "" {
		return "零"
	}
	if len(n) > 4 {
		high, low := n[:len(n)-4], n[len(n)-4:]
		out := readSection(high) + "万"
		low = strings.TrimLeft(low, "0")
		if low == "" {
			return out
		}
		if len(low) < 4 {
			out += "零"
		}
		return out + readSection(low)
	}
	s := readSection(n)
	// 十几读作「十几」，不是「一十几」
	if len(n) == 2 && n[0] == '1' {
		s = strings.TrimPrefix(s, "一")
	}
	return s
}

// readSection 读一个不超过 4 位的数（没有前导的零），中间的零只读一次。
func readSection(n string) string {
	units := []string{"千", "百", "十", ""}[4-len(n):]
	var b strings.Builder
	zero := false
	for i, c := range n {
		d := int(c - '0')
		if d == 0 {
			zero = true
			continue
		}
		if zero && b.Len() > 0 {
			b.WriteString("零")
		}
		zero = false
		b.WriteString(digitNames[d] + units[i])
	}
	return b.String()
}

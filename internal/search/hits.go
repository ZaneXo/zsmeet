package search

import (
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/ZaneXo/zsmeet/internal/transcript"
)

// 一场会议最多列出多少句命中的转写、多少条命中的纪要
const (
	maxTranscriptHits = 5
	maxMinutesHits    = 2
	// 至少这么多个字的中文词，才允许错一个字也算命中（短词错一个字就成了别的词）
	fuzzyMinRunes = 4
)

// TSQuery 把几种写法拼成 Postgres 的 tsquery，用来在索引里粗筛会议：任何一种写法的词全部出现就算。
// 没有能查的词时返回空字符串。
func TSQuery(spellings []string) string {
	var g groups
	for _, sp := range spellings {
		g.add(QueryTerms(sp))
	}
	return g.String()
}

// FuzzyTSQuery 粗筛用户输入的词「错了一个字」的会议，只对够长的纯中文词，其他情况返回空字符串。
// 第 k 个字错了，跨过它的两个「两字词」就对不上，其余的「两字词」和没被它们盖住的单字都还在。
// 搜到的会议和 TSQuery 的有重叠，调用方自己去重。
func FuzzyTSQuery(query string) string {
	q := fuzzyQuery(query)
	if q == nil {
		return ""
	}
	var g groups
	for k := range q {
		var terms []string
		covered := make([]bool, len(q))
		for i := 0; i+1 < len(q); i++ {
			if i == k || i+1 == k {
				continue
			}
			terms = append(terms, string(q[i:i+2]))
			covered[i], covered[i+1] = true, true
		}
		for i, r := range q {
			if i != k && !covered[i] {
				terms = append(terms, string(r))
			}
		}
		g.add(terms)
	}
	return g.String()
}

// groups 收集 tsquery 里用「|」连起来的各组，每组是用「&」连起来的词，重复的组只留一个。
type groups struct {
	list []string
	seen map[string]bool
}

func (g *groups) add(terms []string) {
	if len(terms) == 0 {
		return
	}
	quoted := make([]string, len(terms))
	for i, t := range terms {
		quoted[i] = "'" + strings.ReplaceAll(strings.ReplaceAll(t, `\`, `\\`), "'", "''") + "'"
	}
	s := "(" + strings.Join(quoted, " & ") + ")"
	if g.seen == nil {
		g.seen = map[string]bool{}
	}
	if !g.seen[s] {
		g.seen[s] = true
		g.list = append(g.list, s)
	}
}

func (g *groups) String() string { return strings.Join(g.list, " | ") }

// fuzzyQuery 返回允许错一个字的那个词：用户输入的是一个够长的纯中文词（中间没有空格、标点）时就是它，否则是 nil。
func fuzzyQuery(query string) []rune {
	q := []rune(strings.TrimFunc(query, notWordOrCJK))
	if len(q) < fuzzyMinRunes {
		return nil
	}
	for _, r := range q {
		if !isCJK(r) {
			return nil
		}
	}
	return q
}

func notWordOrCJK(r rune) bool { return !isCJK(r) && !isWord(r) }

// Hit 是一处命中：转写里的一句话，或者纪要里的一行。
type Hit struct {
	// 从录音开头算的秒数；纪要里没写时间点的行是 -1
	Start float64
	// 说话人编号，纪要里的行是 -1
	Speaker int
	Text    string
	// Text 里命中的词，原样（大小写不变），网页上用它们高亮
	Matches []string
	Source  string // "transcript" 或 "minutes"
	// 只是错了一个字的近似命中
	Fuzzy bool
}

// Result 是在一场会议里找的结果。
type Result struct {
	// 命中的句子和纪要行，最相关的在前
	Hits []Hit
	// 搜到了：某一种写法的每个词都在这场会议里出现过（标题、纪要、转写都算，可以在不同的地方）
	Matched bool
	// 没搜到，但用户输入的词错一个字的样子出现过
	Fuzzy bool
}

// Find 在一场会议的标题、当前纪要（按行）和转写（按句）里找搜索词。spellings 是要找的几种写法，第一个是用户输入的。
//
// 写法按索引的切法拆成词（一段连续的中文，或者一个英文单词），每个词都出现过才算搜到，和索引粗筛的口径一致；
// 比较时不分大小写，中文和英文之间有没有空格都一样，英文要整个单词对上（搜「AI」不算 email）。
// 列出的命中按相关程度排：整个写法连在一起出现的、写法的词都在这一句里的，都没有时才列只有部分词的句子，
// 最后是错了一个字的（只对用户输入的够长的中文词）。
func Find(title, minutes string, segs []transcript.Segment, spellings []string) Result {
	m := newMatcher(spellings)
	if len(m.pats) == 0 {
		return Result{}
	}
	var res Result

	fuzzySeen := m.inspect(title).fuzzy != ""

	var tr ranked
	for _, s := range segs {
		x := m.inspect(s.Text)
		fuzzySeen = fuzzySeen || x.fuzzy != ""
		tr.add(x, Hit{Start: s.Start, Speaker: s.Speaker, Text: s.Text, Source: "transcript"})
	}
	res.Hits = tr.top(maxTranscriptHits)

	var mn ranked
	for line := range strings.SplitSeq(minutes, "\n") {
		text := cleanLine(line)
		x := m.inspect(text)
		fuzzySeen = fuzzySeen || x.fuzzy != ""
		if x.rank == 0 && x.fuzzy == "" {
			continue
		}
		mn.add(x, minutesHit(text, x))
	}
	res.Hits = append(res.Hits, mn.top(maxMinutesHits)...)

	res.Matched = m.allFound()
	res.Fuzzy = !res.Matched && fuzzySeen
	return res
}

// minutesHit 把纪要里命中的一行变成 Hit：一行里常有好几个时间点，取命中的词前面最近的那个，前面没有就取第一个；
// 开头的时间点网页上单独显示，正文里不再重复。
func minutesHit(text string, x inspected) Hit {
	start := -1.0
	if refs := clockRef.FindAllStringSubmatchIndex(text, -1); len(refs) > 0 {
		pick := refs[0]
		for _, r := range refs {
			if r[0] < x.at {
				pick = r
			}
		}
		start = parseClock(text[pick[2]:pick[3]])
	}
	text = strings.TrimSpace(text[leadingClockLen(text):])
	return Hit{Start: start, Speaker: -1, Text: text, Source: "minutes"}
}

// ranked 按相关程度攒命中：连在一起出现的 > 词都在这一句里 > 只有部分词 > 错一个字，同一档按出现的先后。
type ranked struct {
	byRank [4][]Hit // 下标是 inspected.rank：3、2、1 各一档，0 放错一个字的
}

func (r *ranked) add(x inspected, h Hit) {
	switch {
	case x.rank > 0:
		h.Matches = x.matches
		r.byRank[x.rank] = append(r.byRank[x.rank], h)
	case x.fuzzy != "":
		h.Matches, h.Fuzzy = []string{x.fuzzy}, true
		r.byRank[0] = append(r.byRank[0], h)
	}
}

func (r *ranked) top(n int) []Hit {
	out := slices.Concat(r.byRank[3], r.byRank[2])
	// 只有部分词的句子相关性弱，有更好的命中时不列
	if len(out) == 0 {
		out = r.byRank[1]
	}
	out = append(out, r.byRank[0]...)
	return out[:min(n, len(out))]
}

// pattern 是拆好的一种写法。
type pattern struct {
	whole []rune   // 整个写法（规整过的），连在一起出现时最相关
	parts [][]rune // 按索引的切法拆成的词：一段连续的中日韩文字，或者一个英文、数字单词
}

// matcher 拿几种写法去比较一场会议里的各段文字，记着每种写法的每个词有没有出现过。
type matcher struct {
	pats  []pattern
	found [][]bool
	fuzzy []rune // 允许错一个字的词，nil 表示不放宽
}

func newMatcher(spellings []string) *matcher {
	m := &matcher{}
	for _, sp := range spellings {
		p := pattern{whole: fold(strings.TrimFunc(sp, notWordOrCJK)).runes}
		runs(sp, func(run []rune, _ bool) { p.parts = append(p.parts, run) })
		if len(p.parts) > 0 {
			m.pats = append(m.pats, p)
			m.found = append(m.found, make([]bool, len(p.parts)))
		}
	}
	if len(spellings) > 0 {
		m.fuzzy = fuzzyQuery(spellings[0])
	}
	return m
}

// inspected 是一段文字和各种写法比较的结果。
type inspected struct {
	// 0 没命中；1 有某种写法的部分词；2 某种写法的词都在这段里；3 某种写法连在一起出现
	rank int
	// 命中的词，原文里的样子，按出现的先后
	matches []string
	// 第一个命中的词在原文里的字节位置
	at int
	// 没命中时，错一个字的样子（原文），没有是空字符串
	fuzzy string
}

func (m *matcher) inspect(text string) inspected {
	f := fold(text)
	x := inspected{at: -1}
	type span struct{ a, b int }
	var spans []span
	for i, p := range m.pats {
		if a := f.index(p.whole); a >= 0 {
			x.rank = 3
			spans = append(spans, span{a, a + len(p.whole)})
		}
		n := 0
		var partSpans []span
		for j, part := range p.parts {
			if a := f.index(part); a >= 0 {
				m.found[i][j] = true
				n++
				partSpans = append(partSpans, span{a, a + len(part)})
			}
		}
		switch {
		case n == 0:
		case n == len(p.parts) && len(p.parts) > 1:
			x.rank = max(x.rank, 2)
			spans = append(spans, partSpans...)
		default:
			x.rank = max(x.rank, 1)
			spans = append(spans, partSpans...)
		}
	}
	if x.rank == 0 {
		if m.fuzzy != nil {
			if a := f.fuzzyIndex(m.fuzzy); a >= 0 {
				x.fuzzy = f.orig(text, a, a+len(m.fuzzy))
				x.at = f.start[a]
			}
		}
		return x
	}
	// 按出现的先后列出命中的词；被更长的命中盖住的不再单独列
	slices.SortFunc(spans, func(p, q span) int {
		if p.a != q.a {
			return p.a - q.a
		}
		return q.b - p.b
	})
	end := -1
	for _, s := range spans {
		if s.b <= end {
			continue
		}
		if word := f.orig(text, s.a, s.b); !slices.Contains(x.matches, word) {
			x.matches = append(x.matches, word)
		}
		if x.at < 0 {
			x.at = f.start[s.a]
		}
		end = max(end, s.b)
	}
	return x
}

func (m *matcher) allFound() bool {
	for _, parts := range m.found {
		if !slices.Contains(parts, false) {
			return true
		}
	}
	return false
}

// folded 是为了比较而规整过的文本：英文转小写；紧挨着中日韩文字的空白去掉（「Go 语言」和「Go语言」算一样）；
// 其他连续的空白合成一个空格（英文单词之间的空格要留着，「a pi」不能等于「api」）。
// start、end 是每个字在原文里的字节范围，用来把比较出来的位置换回原文。
type folded struct {
	runes      []rune
	start, end []int
}

func fold(s string) folded {
	type at struct {
		r    rune
		a, b int
	}
	var src []at
	for i, r := range s {
		if n := len(src); n > 0 {
			src[n-1].b = i
		}
		src = append(src, at{r: r, a: i})
	}
	if n := len(src); n > 0 {
		src[n-1].b = len(s)
	}
	var f folded
	push := func(r rune, c at) {
		f.runes = append(f.runes, r)
		f.start = append(f.start, c.a)
		f.end = append(f.end, c.b)
	}
	for i := 0; i < len(src); i++ {
		c := src[i]
		if !unicode.IsSpace(c.r) {
			push(unicode.ToLower(c.r), c)
			continue
		}
		j := i
		for j < len(src) && unicode.IsSpace(src[j].r) {
			j++
		}
		// 两边都不是中日韩文字时留一个空格；开头、结尾的空白不要
		if n := len(f.runes); n > 0 && j < len(src) && !isCJK(f.runes[n-1]) && !isCJK(src[j].r) {
			push(' ', c)
		}
		i = j - 1
	}
	return f
}

// index 返回 needle 在 f 里第一次出现的位置（第几个字），没有返回 -1。needle 的头或尾是英文字母、数字时，
// 那一头挨着的不能也是字母、数字：搜「AI」不算 email 里的 ai，搜「20」不算 2025 里的 20。
func (f folded) index(needle []rune) int {
	n := len(needle)
	if n == 0 {
		return -1
	}
	for i := 0; i+n <= len(f.runes); i++ {
		if !slices.Equal(f.runes[i:i+n], needle) {
			continue
		}
		if isWord(needle[0]) && i > 0 && isWord(f.runes[i-1]) {
			continue
		}
		if isWord(needle[n-1]) && i+n < len(f.runes) && isWord(f.runes[i+n]) {
			continue
		}
		return i
	}
	return -1
}

// fuzzyIndex 找和 q（纯中文的词）一样长、只差一个字的地方，差的那个字也得是中日韩文字：
// 标点、空格、英文不算听错的字。没有返回 -1。
func (f folded) fuzzyIndex(q []rune) int {
	for i := 0; i+len(q) <= len(f.runes); i++ {
		diff := 0
		for j, r := range q {
			if t := f.runes[i+j]; t != r {
				if !isCJK(t) {
					diff = 2
				} else {
					diff++
				}
				if diff > 1 {
					break
				}
			}
		}
		if diff == 1 {
			return i
		}
	}
	return -1
}

// orig 返回规整后第 a 到第 b 个字（不含 b）在原文 s 里对应的那一段。
func (f folded) orig(s string, a, b int) string {
	return s[f.start[a]:f.end[b-1]]
}

// 纪要里的时间点，例如 [12:34] 或 [1:02:03]
var clockRef = regexp.MustCompile(`\[(\d{1,2}:\d{2}(?::\d{2})?)\]`)

// leadingClockLen：text 开头的时间点（连同后面的空格）有多长，开头不是时间点时是 0。
func leadingClockLen(text string) int {
	if loc := clockRef.FindStringIndex(text); loc != nil && loc[0] == 0 {
		end := loc[1]
		for end < len(text) && text[end] == ' ' {
			end++
		}
		return end
	}
	return 0
}

func parseClock(s string) float64 {
	sec := 0.0
	for part := range strings.SplitSeq(s, ":") {
		n := 0
		for _, c := range part {
			n = n*10 + int(c-'0')
		}
		sec = sec*60 + float64(n)
	}
	return sec
}

// cleanLine 去掉 Markdown 的列表符号、标题符号和加粗，留下给人看的文字。
func cleanLine(line string) string {
	line = strings.TrimSpace(line)
	line = strings.TrimLeft(line, "#>-*+ ")
	line = strings.ReplaceAll(line, "**", "")
	return strings.TrimSpace(line)
}

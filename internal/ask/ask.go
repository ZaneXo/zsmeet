// Package ask 用大模型回答关于会议的问题：先让大模型把问题变成搜索词，再把搜到的会议片段编上号交给它回答，
// 回答里引用的编号换回会议和时间点，网页上点了能直接听原话。
//
// 这里只管提示词、解析和排版，不查数据库；查哪些会议、谁能看由 service 决定。
package ask

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ZaneXo/zsmeet/internal/transcript"
)

// 最多用几个搜索词，每个最长几个字
const (
	maxKeywords    = 5
	maxKeywordLen  = 20
	maxPeriodDays  = 3 * 366
	dateLayout     = "2006-01-02"
	maxLineChars   = 300
	maxQuoteChars  = 120
	maxAnswerChars = 4000
)

var weekdays = [...]string{"日", "一", "二", "三", "四", "五", "六"}

// today 写成「2026-11-10，星期二」，让大模型能换算「上周」「昨天」。
func today(now time.Time) string {
	return fmt.Sprintf("%s，星期%s", now.Format(dateLayout), weekdays[now.Weekday()])
}

// Plan 是从问题里看出来的搜索条件。
type Plan struct {
	// 搜索词，可能为空（问题只限定了时间，比如「上周开了哪些会」；或者大模型没给出、问题里也拆不出词）
	Keywords []string
	// 问题限定的日期范围（本地时间的日期，含首尾）；没限定时是零值
	From, To time.Time
}

// HasPeriod 表示问题限定了时间。
func (p Plan) HasPeriod() bool { return !p.From.IsZero() || !p.To.IsZero() }

const planSystem = `你帮同事在团队的会议记录（会议转写和纪要）里查资料。请把问题变成搜索用的关键词，并判断问题有没有限定时间。

要求：
1. keywords：1 到 5 个短词，每个 2 到 8 个字，是会上可能原样说出来的词：人名、产品、项目、客户、具体的事物（如“报价”“合同”“上线时间”）。
   不要“会议”“讨论”“决定”“什么”“上周”这类泛泛的词，不要整句话。用会议的语言（一般是中文），英文名保持原样。
2. 问题限定了时间（如“上周”“十月”“昨天”“最近一个月”）时，按今天的日期换算成起止日期 from、to（格式 YYYY-MM-DD，含首尾两天）；没限定时都写空字符串。
3. 只输出一个 JSON 对象，不要输出别的话：{"keywords": ["关键词"], "from": "", "to": ""}`

// PlanPrompt 返回把问题变成搜索条件的提示词。
func PlanPrompt(question string, now time.Time) (system, user string) {
	return planSystem, fmt.Sprintf("今天是 %s。\n问题：%s", today(now), question)
}

// ParsePlan 解析大模型给的搜索条件。写坏了、没给关键词也没限定时间时，从问题里拆出几个词去搜（见 questionTerms）。
// 关键词去掉太长、没有字的和重复的；日期写错、前后颠倒、跨度太大时当作没限定。
func ParsePlan(out, question string, now time.Time) Plan {
	var p Plan
	if raw, ok := planJSON(out); ok {
		seen := map[string]bool{}
		for _, k := range raw.Keywords {
			k = strings.Join(strings.Fields(k), " ")
			key := strings.ToLower(strings.ReplaceAll(k, " ", ""))
			if utf8.RuneCountInString(k) < 2 || utf8.RuneCountInString(k) > maxKeywordLen || !hasLetter(k) || seen[key] {
				continue
			}
			seen[key] = true
			p.Keywords = append(p.Keywords, k)
			if len(p.Keywords) == maxKeywords {
				break
			}
		}
		p.From, p.To = parsePeriod(raw.From, raw.To, now)
	}
	if len(p.Keywords) == 0 && !p.HasPeriod() {
		p.Keywords = questionTerms(question)
	}
	return p
}

type rawPlan struct {
	Keywords []string `json:"keywords"`
	From     string   `json:"from"`
	To       string   `json:"to"`
}

// planJSON 找出回复里的第一个 JSON 对象：从每个「{」开始试着解析一个值，后面跟着什么都不管。
// 大模型常在 JSON 前后加几句话、包在代码块里，话里也可能带花括号。
func planJSON(out string) (rawPlan, bool) {
	for i := strings.IndexByte(out, '{'); i >= 0; {
		var raw rawPlan
		if json.NewDecoder(strings.NewReader(out[i:])).Decode(&raw) == nil {
			return raw, true
		}
		next := strings.IndexByte(out[i+1:], '{')
		if next < 0 {
			break
		}
		i += 1 + next
	}
	return rawPlan{}, false
}

// 从问题里拆搜索词时去掉的词：问法、代词、时间、「会议」这类在会议记录里到处都是的词。长的排在前面，先认长的
var stopWords = func() []string {
	words := strings.Fields(`什么时候 为什么 怎么样 有没有 是不是 能不能 会不会 要不要 上个月 这个月
		有哪些 是什么 什么 怎么 怎样 如何 为何 哪些 哪个 哪里 哪儿 哪天 多少 几个 几次 是否
		我们 你们 他们 咱们 大家 关于 有关 一下 一些 这些 那些 这个 那个
		上周 这周 本周 下周 上月 本月 今年 去年 昨天 今天 前天 明天 最近 之前 以前 之后 时候 现在 目前 当时
		会议 会上 开会 例会 讨论 决定 说了 说过 提到 提过 聊了 谈了 定了 定的 情况 结果 事情 东西
		请问 帮我 告诉 总结 介绍 可以 需要 应该 还有 以及 或者 然后 一共 总共`)
	slices.SortStableFunc(words, func(a, b string) int { return utf8.RuneCountInString(b) - utf8.RuneCountInString(a) })
	return words
}()

// 单独一个字就把词隔开的：语气词、代词
const stopChars = "的了吗呢吧啊呀么嘛谁哪啥我你他她它"

// 英文问题里的虚词
var stopEnglish = map[string]bool{
	"what": true, "when": true, "where": true, "who": true, "why": true, "how": true, "which": true,
	"the": true, "an": true, "of": true, "to": true, "in": true, "on": true, "at": true, "for": true, "about": true,
	"and": true, "or": true, "is": true, "are": true, "was": true, "were": true, "did": true, "do": true, "does": true,
	"we": true, "you": true, "they": true, "it": true, "last": true, "this": true, "week": true, "meeting": true, "meetings": true,
}

// 拆出来的一串汉字最长几个字，再长就切成几段
const (
	maxTermRun   = 8
	termRunPiece = 4
)

// questionTerms 在大模型没给出搜索词时，从问题里拆出几个短词：按标点、空格和中英文的交界切开，
// 去掉问法、代词、时间这些词，每个至少两个字，最多 maxKeywords 个，每个不超过 maxKeywordLen 个字。
// 一个也拆不出来时返回空（问的是「最近开了哪些会」这类，按时间列出会议）。
func questionTerms(question string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(term string) {
		term = string([]rune(term)[:min(utf8.RuneCountInString(term), maxKeywordLen)])
		key := strings.ToLower(term)
		if len(out) == maxKeywords || utf8.RuneCountInString(term) < 2 || !hasLetter(term) || seen[key] || stopEnglish[key] {
			return
		}
		seen[key] = true
		out = append(out, term)
	}
	// 一串汉字：去掉停用词，剩下的每段太长时切开
	addHan := func(run []rune) {
		var parts [][]rune
		start := 0
		for i := 0; i < len(run); {
			cut := 0
			for _, w := range stopWords {
				if strings.HasPrefix(string(run[i:]), w) {
					cut = utf8.RuneCountInString(w)
					break
				}
			}
			if cut == 0 && strings.ContainsRune(stopChars, run[i]) {
				cut = 1
			}
			if cut == 0 {
				i++
				continue
			}
			parts = append(parts, run[start:i])
			i += cut
			start = i
		}
		parts = append(parts, run[start:])
		for _, p := range parts {
			if len(p) <= maxTermRun {
				add(string(p))
				continue
			}
			for len(p) > 0 {
				n := min(len(p), termRunPiece)
				if len(p)-n == 1 {
					n++ // 别剩下一个字
				}
				add(string(p[:n]))
				p = p[n:]
			}
		}
	}

	han := func(r rune) bool { return unicode.Is(unicode.Han, r) }
	word := func(r rune) bool { return (unicode.IsLetter(r) || unicode.IsDigit(r)) && !han(r) }
	runes := []rune(question)
	for i := 0; i < len(runes); {
		j := i + 1
		switch {
		case han(runes[i]):
			for j < len(runes) && han(runes[j]) {
				j++
			}
			addHan(runes[i:j])
		case word(runes[i]):
			for j < len(runes) && word(runes[j]) {
				j++
			}
			add(string(runes[i:j]))
		}
		i = j
	}
	return out
}

func hasLetter(s string) bool { return strings.IndexFunc(s, unicode.IsLetter) >= 0 }

// parsePeriod 解析起止日期。只给了开始时算到今天，只给了结束时不限开始。
func parsePeriod(from, to string, now time.Time) (time.Time, time.Time) {
	parse := func(s string) (time.Time, bool) {
		t, err := time.ParseInLocation(dateLayout, strings.TrimSpace(s), now.Location())
		return t, err == nil
	}
	f, okFrom := parse(from)
	t, okTo := parse(to)
	switch {
	case !okFrom && !okTo:
		return time.Time{}, time.Time{}
	case !okTo:
		t = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	}
	if okFrom && (t.Before(f) || t.Sub(f) > maxPeriodDays*24*time.Hour) {
		return time.Time{}, time.Time{}
	}
	return f, t
}

// Line 是材料里的一行：纪要的一行，或者转写的一句。
type Line struct {
	// 从录音开头算的秒数，-1 表示没有时间点（纪要里没标时间的行）
	Start float64
	// 说话人的称呼；纪要的行为空
	Speaker string
	Text    string
	Minutes bool
	// 纪要的行所在的小节（「结论」「待办」），不知道时为空
	Section string
}

// Source 是一场会议交给大模型的内容。
type Source struct {
	MeetingID uuid.UUID
	Title     string
	Date      time.Time
	// 纪要里的相关的行
	Minutes []Line
	// 转写片段，每段是连着的几句
	Transcript [][]Line
}

// Snippet 是材料里编了号的一行，回答引用它时换回会议和时间点。
type Snippet struct {
	ID        string
	MeetingID uuid.UUID
	Title     string
	Date      time.Time
	Start     float64
	Minutes   bool
	Text      string
}

// Material 是排好的材料。
type Material struct {
	Text     string
	Snippets map[string]Snippet
	// 实际放进材料的会议有几场（太长被挤掉的不算）
	Meetings int
}

// Size 是一场会议全部放进材料要多少个字，判断整场放不放得下。和 Build 把它当第一场会议排出来的字数一样：
// budget 不少于 Size 时 Build 一行不落地放进去，少一个字就放不下。
func Size(src Source) int {
	n := utf8.RuneCountInString(meetingHeader(1, src))
	for i, l := range src.Minutes {
		n += lineSize(1, i+1, l)
	}
	return n + transcriptSize(1, len(src.Minutes), src.Transcript)
}

// lineSize 是第 k 场会议的第 n 行在材料里占多少个字，连换行。
func lineSize(k, n int, l Line) int {
	return utf8.RuneCountInString(formatLine(lineID(k, n), l)) + 1
}

// transcriptSize 是第 k 场会议已经放了 n 行以后，接着放下全部转写片段要多少个字，连隔开片段的「……」。
func transcriptSize(k, n int, segs [][]Line) int {
	size := 0
	for _, seg := range segs {
		for i, l := range seg {
			if i == 0 && n > 0 {
				size += utf8.RuneCountInString(segmentGap)
			}
			n++
			size += lineSize(k, n, l)
		}
	}
	return size
}

// Build 把几场会议排成材料，总共不超过大约 budget 个字。排在前面的会议先放；
// 每场分到剩下的字数除以剩下的场数，用不完的留给后面的会议。
// 一场会议里先放纪要再放转写片段；有转写片段时纪要最多用掉这场的一半（转写用不完一半时纪要可以多用），
// 免得纪要太长把搜到的转写片段全挤掉。
// 每一行都编一个号，比如 [M1-3]（第 1 场会议的第 3 行），回答时用它标出处。
func Build(sources []Source, budget int) Material {
	out := Material{Snippets: map[string]Snippet{}}
	var b strings.Builder
	left := budget
	for i, src := range sources {
		share := left / (len(sources) - i)
		var part strings.Builder
		k := out.Meetings + 1
		n := 0
		used := 0
		// 下一行前面要不要先放「……」隔开转写片段：等下一行放得下才放，免得材料末尾多一个
		sep := false
		add := func(l Line, limit int) bool {
			id := lineID(k, n+1)
			line := formatLine(id, l)
			size := utf8.RuneCountInString(line) + 1
			if sep {
				size += utf8.RuneCountInString(segmentGap)
			}
			if used+size > limit {
				return false
			}
			if sep {
				part.WriteString(segmentGap)
				sep = false
			}
			used += size
			n++
			part.WriteString(line + "\n")
			out.Snippets[id] = Snippet{ID: id, MeetingID: src.MeetingID, Title: src.Title, Date: src.Date, Start: l.Start, Minutes: l.Minutes, Text: truncate(l.Text, maxLineChars)}
			return true
		}
		header := meetingHeader(k, src)
		used += utf8.RuneCountInString(header)
		minutesLimit := share
		if len(src.Transcript) > 0 {
			// 转写片段按排在全部纪要后面算；纪要没放完时编号短一些，实际只会比这少
			minutesLimit = max(share/2, share-transcriptSize(k, len(src.Minutes), src.Transcript))
		}
		for _, l := range src.Minutes {
			if !add(l, minutesLimit) {
				break
			}
		}
		full := false
		for _, seg := range src.Transcript {
			if full {
				break
			}
			sep = n > 0
			for _, l := range seg {
				if !add(l, share) {
					full = true
					break
				}
			}
		}
		if n == 0 {
			// 一行都放不下就整场不放：只有标题没有内容的会议没法引用
			continue
		}
		out.Meetings++
		b.WriteString(header)
		b.WriteString(part.String())
		b.WriteString("\n")
		left -= used
	}
	out.Text = strings.TrimSpace(b.String())
	return out
}

// 材料里隔开同一场会议的两段转写片段
const segmentGap = "……\n"

// meetingHeader 是材料里第 k 场会议开头的一行，带换行。
func meetingHeader(k int, src Source) string {
	return fmt.Sprintf("## 会议 M%d：%s（%s）\n", k, src.Title, src.Date.Format("2006-01-02 15:04"))
}

// lineID 是第 k 场会议第 n 行的编号，比如 M1-3。
func lineID(k, n int) string { return fmt.Sprintf("M%d-%d", k, n) }

// formatLine 是材料里编号为 id 的一行，不带换行：纪要的行写成「[M1-1] 纪要·结论（01:15）：……」，
// 转写的句子写成「[M1-3] 01:15 张敏：……」，太长的截断。Build 和算字数的 Size 都用它，算出来的才和放进去的一样。
func formatLine(id string, l Line) string {
	text := truncate(l.Text, maxLineChars)
	if !l.Minutes {
		return fmt.Sprintf("[%s] %s %s：%s", id, transcript.Clock(l.Start), l.Speaker, text)
	}
	label := "纪要"
	if l.Section != "" {
		label += "·" + l.Section
	}
	if l.Start >= 0 {
		label += "（" + transcript.Clock(l.Start) + "）"
	}
	return fmt.Sprintf("[%s] %s：%s", id, label, text)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

const answerRules = `要求：
1. 只根据材料回答，不要用材料以外的知识补充，不要编造。材料里的会议是从团队的会议记录里按关键词找出来的，不一定都和问题有关。
2. 材料里没有答案时，直接说“会议记录里没有找到……”，不要猜；只有部分答案时，说清楚哪些找到了、哪些没找到。
3. 转写来自语音识别，会有错别字和同音字，请结合上下文理解。
4. 简洁：先给结论，再补充必要的细节，一般不超过 300 字。不同会议的说法不一样时，按时间先后说明，以最近的为准。
5. 每句话后面标出它依据的材料编号，格式和材料里的一样，如 [M1-3]；依据几行就写几个，如 [M1-3][M2-1]。只能用材料里出现过的编号，没有依据的话不要写。
6. 提到某场会议时用它的标题和日期，不要写“M1”这样的代号。
7. 用提问的语言回答。输出纯文本，可以分段、用简单的列表，不要标题，不要代码块。`

// AnswerPrompt 返回回答问题的提示词。
func AnswerPrompt(question string, m Material, now time.Time) (system, user string) {
	system = "你是团队的会议助理，根据会议记录回答同事的问题。下面的材料是从会议纪要和转写里找出来的片段，每一行开头方括号里的是这一行的编号。\n\n" + answerRules
	user = fmt.Sprintf("今天是 %s。\n\n问题：%s\n\n材料：\n%s", today(now), question, m.Text)
	return system, user
}

// Citation 是回答里的一处出处。
type Citation struct {
	// 回答里写成 [N]
	N int
	Snippet
}

// 回答里引用的编号：[M1-3]、【M1-3】、[M1-3, M2-1]、(M1-3) 这几种写法都认
var citeGroup = regexp.MustCompile(`[\[【(（]\s*M\d+\s*-\s*\d+(?:\s*[,，、;；]?\s*M\d+\s*-\s*\d+)*\s*[\]】)）]`)
var citeID = regexp.MustCompile(`M(\d+)\s*-\s*(\d+)`)
var numberRun = regexp.MustCompile(`(?:\[\d+\])+`)
var spaceBeforeRef = regexp.MustCompile(`[ \t]+(\[\d+\])`)
var spaceBeforePunct = regexp.MustCompile(`[ \t]+([，。；：！？、）」])`)
var spaceAfterPunct = regexp.MustCompile(`([，。；：！？、（「])[ \t]+`)

// Cite 把回答里引用的编号换成 [1]、[2] 这样的序号，按第一次出现的先后编；
// 材料里没有的编号（大模型自己编的）直接去掉，不留没有出处的链接。返回换好的回答和对应的出处。
func Cite(answer string, snippets map[string]Snippet) (string, []Citation) {
	number := map[string]int{}
	var cites []Citation
	text := citeGroup.ReplaceAllStringFunc(answer, func(group string) string {
		var b strings.Builder
		for _, m := range citeID.FindAllStringSubmatch(group, -1) {
			id := "M" + m[1] + "-" + m[2]
			sn, ok := snippets[id]
			if !ok {
				continue
			}
			n, ok := number[id]
			if !ok {
				n = len(cites) + 1
				number[id] = n
				sn.Text = truncate(sn.Text, maxQuoteChars)
				cites = append(cites, Citation{N: n, Snippet: sn})
			}
			b.WriteString("[" + strconv.Itoa(n) + "]")
		}
		return b.String()
	})
	// 挨着的同一个序号只留一个（[1][1] → [1]）
	text = numberRun.ReplaceAllStringFunc(text, func(run string) string {
		var seen []string
		for _, n := range strings.SplitAfter(run, "]") {
			if n != "" && !slices.Contains(seen, n) {
				seen = append(seen, n)
			}
		}
		return strings.Join(seen, "")
	})
	// 序号紧跟在它说明的那句话后面；编号去掉以后可能在中文标点旁边留下空格
	text = spaceBeforeRef.ReplaceAllString(text, "$1")
	text = spaceBeforePunct.ReplaceAllString(text, "$1")
	text = spaceAfterPunct.ReplaceAllString(text, "$1")
	return truncate(text, maxAnswerChars), cites
}

// 纪要里的时间点，例如 [12:34] 或 [1:02:03]
var clockRef = regexp.MustCompile(`\[(\d{1,2}):(\d{2})(?::(\d{2}))?\]`)

// MinutesLines 把整篇纪要按行拆开：标题不单独成行，记成下面各行所在的小节；去掉 Markdown 的列表符号、加粗，
// 和行首的时间点（时间点另外记着，取这一行里的第一个，没有是 -1）。空行、分隔线不要。
func MinutesLines(content string) []Line {
	var out []Line
	section := ""
	for line := range strings.SplitSeq(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			section = strings.TrimSpace(strings.ReplaceAll(strings.TrimLeft(line, "# "), "**", ""))
			continue
		}
		start := -1.0
		if m := clockRef.FindStringSubmatch(line); m != nil {
			a, _ := strconv.Atoi(m[1])
			b, _ := strconv.Atoi(m[2])
			start = float64(a*60 + b)
			if m[3] != "" {
				c, _ := strconv.Atoi(m[3])
				start = start*60 + float64(c)
			}
		}
		line = strings.TrimSpace(strings.ReplaceAll(strings.TrimLeft(line, "-*+> "), "**", ""))
		if loc := clockRef.FindStringIndex(line); loc != nil && loc[0] == 0 {
			line = strings.TrimSpace(line[loc[1]:])
		}
		if strings.Trim(line, "-*_ ") == "" {
			continue
		}
		out = append(out, Line{Start: start, Text: line, Minutes: true, Section: section})
	}
	return out
}

// TranscriptLines 把转写的句子变成材料里的行，说话人用起过的名字。
func TranscriptLines(segs []transcript.Segment, names transcript.Names) []Line {
	out := make([]Line, len(segs))
	for i, s := range segs {
		out[i] = Line{Start: s.Start, Speaker: names.Of(s.Speaker), Text: s.Text}
	}
	return out
}

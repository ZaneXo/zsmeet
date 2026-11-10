// Package minutes 把转写交给大模型写成会议纪要。
package minutes

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ZaneXo/zsmeet/internal/transcript"
)

// Template 是一种纪要写法。一期内置四种，自定义模板后面再做。
type Template struct {
	ID   string
	Name string
	// 告诉大模型纪要要有哪些部分、每部分写什么
	Guide string
}

var Templates = []Template{
	{
		ID:   "general",
		Name: "通用会议",
		Guide: `1. 摘要：三到五句话说清楚这场会讨论了什么、定了什么
2. 结论：逐条列出达成一致的事
3. 讨论要点：按议题整理，每条标注对应的时间点
4. 待办：每条写清楚谁、做什么、什么时候之前；没提到的写“未定”`,
	},
	{
		ID:   "weekly",
		Name: "周会",
		Guide: `1. 摘要：两三句话概括本周整体情况
2. 各人进展：按说话人整理，每个人上周做了什么、这周计划做什么、需要谁配合
3. 风险与需要决策的事
4. 待办：每条写清楚谁、做什么、什么时候之前`,
	},
	{
		ID:   "tech",
		Name: "技术评审",
		Guide: `1. 摘要：评审的是什么方案，结论是通过、有条件通过还是需要修改
2. 方案要点：被评审方案的核心设计
3. 问题与结论：评审中提出的每个问题、对应的讨论结果，标注时间点
4. 遗留问题与需要补充的材料
5. 待办：每条写清楚谁、做什么、什么时候之前`,
	},
	{
		ID:   "client",
		Name: "客户访谈",
		Guide: `1. 摘要：客户是谁、这次沟通的主题和整体态度
2. 客户反馈：尽量保留客户原话，标注时间点
3. 痛点与需求：客户遇到的问题、明确提出的需求
4. 提到的竞品或替代方案
5. 我方承诺的跟进事项：谁、做什么、什么时候之前`,
	},
}

// Find 按 ID 找模板。
func Find(id string) (Template, bool) {
	for _, t := range Templates {
		if t.ID == id {
			return t, true
		}
	}
	return Template{}, false
}

// Input 是写一份纪要需要的材料。
type Input struct {
	Title    string
	Date     time.Time
	Hotwords string
	Template Template
	Segments []transcript.Segment
	// 录音时标记的重点（录音开始后的秒数，从小到大）
	Marks []float64
	// 给说话人起的名字，转写里直接用名字
	Speakers transcript.Names
}

// ChatFunc 发一轮对话给大模型。
type ChatFunc func(ctx context.Context, system, user string) (string, error)

// 一次请求里最多放这么多字的转写。超过就先分段提取要点，再汇总成纪要。
// 取值照顾上下文只有 32K 的本地模型；以后做成可配置。
const maxCharsPerCall = 20_000

const rules = `要求：
1. 只写转写里有依据的内容，不要编造。没提到的负责人、时间写“未定”。
2. 转写来自语音识别，会有错别字和同音字，请结合上下文理解。%s
3. 说话人沿用转写里的称呼（如“说话人 1”）。能从对话里确定名字时，写成“说话人 1（张敏）”。
4. 提到具体讨论内容时，在句首标注它在转写里的时间点，格式和转写一致，如 [12:34]。
5. 用转写的主要语言书写。`

// markTag 标在转写某一段的末尾，表示开会的人在这个时间点标记了重点。
func markTag(t float64) string {
	return "【★ " + transcript.Clock(t) + " 标记为重点】"
}

const markRule = "\n6. 转写里带「【★ 时间 标记为重点】」的那一段，是开会的人在那个时间点特意标记的重点，一般指这一段最后刚说的一两句。" +
	"纪要里要把这些内容写清楚，在对应条目的开头加「★」，并标注标记的时间点。不要在纪要里写「有人标记了重点」这样的话。"

// 长会议分段处理时，最后汇总那一次看不到转写，标记的时间点直接列在材料后面
const markSummaryRule = "\n6. 材料最后列出了开会的人特意标记为重点的时间点，一般指那之前刚说的一两句（分段要点里开头带「★」的就是）。" +
	"纪要里要把这些内容写清楚，在对应条目的开头加「★」，并标注标记的时间点。不要在纪要里写「有人标记了重点」这样的话。"

// lines 把转写排成一行一段，标记的重点标在标记时正在说的那一段末尾（转写合并时已经在标记处断开，
// 所以那一段的最后一两句就是标记时刚说的话）。比第一段还早的标记算在第一段上。
func lines(in Input) []string {
	out := transcript.Lines(in.Segments, in.Speakers)
	for _, t := range in.Marks {
		i := 0
		for j, seg := range in.Segments {
			if seg.Start > t {
				break
			}
			i = j
		}
		if i < len(out) {
			out[i] += markTag(t)
		}
	}
	return out
}

func hotwordRule(hotwords string) string {
	if hotwords = strings.TrimSpace(hotwords); hotwords == "" {
		return ""
	}
	return "以下专有名词如果被识别成了近音字，请纠正过来：" + hotwords + "。"
}

// Result 是写好的纪要，加上会上专有名词的术语表。
type Result struct {
	Content string
	Terms   []Term
	// 大模型给出了术语表（哪怕是空的）。没给、或者写坏了解析不出来时是 false，
	// 这时调用方应该保留这场会议原来的术语表，不要用空的覆盖
	TermsFound bool
}

// Term 是一个专有名词的正确写法，和它在转写里被听错的写法。搜索时用它扩展搜索词：
// 搜「飞书」时，连转写里被听成「非书」的地方也能找到。
type Term struct {
	Term     string   `json:"term"`
	Variants []string `json:"variants"`
}

// 最多记多少个专有名词
const maxTerms = 30

// termsRule 是要求大模型列出术语表的那条要求。n 是编号，after 是代码块跟在什么后面（「纪要正文」「要点」）。
func termsRule(n int, after string) string {
	return fmt.Sprintf("\n%d. %s后面另起一行，输出一个 terms 代码块（以 ```terms 开头、``` 结尾），里面是合法的 JSON：", n, after) +
		`{"terms": [{"term": "正确写法", "variants": ["转写里出现过的听错写法"]}]}。` +
		"列出会上提到的人名、产品、项目和专有名词，正确写法优先用热词里的；variants 只写转写里真的出现过的听错写法，没有就写空数组；" +
		`最多 30 个，一个都没有就写 {"terms": []}。` + after + "一律用正确写法。"
}

var termsBlock = regexp.MustCompile("(?s)```terms[^\n]*\n(.*?)(?:\n```[ \t]*(?:\n|$)|$)")

// splitTerms 把大模型输出末尾的 terms 代码块取出来，返回去掉它以后的纪要、解析出的术语表，
// 以及有没有解析出术语表（{"terms": []} 也算）。代码块写坏了（不是合法 JSON、被截断）就当没有，纪要照常用。
func splitTerms(out string) (string, []Term, bool) {
	var terms []Term
	found := false
	for _, m := range termsBlock.FindAllStringSubmatch(out, -1) {
		var parsed struct {
			Terms []Term `json:"terms"`
		}
		if json.Unmarshal([]byte(m[1]), &parsed) != nil {
			continue
		}
		found = true
		terms = mergeTerms(terms, parsed.Terms)
	}
	return strings.TrimSpace(termsBlock.ReplaceAllString(out, "")), terms, found
}

// mergeTerms 把 more 并进 terms：同一个词（不分大小写、不管空格）的听错写法合在一起，
// 去掉没用的写法（见 UsefulTerm），最多 maxTerms 个。
func mergeTerms(terms, more []Term) []Term {
	index := map[string]int{}
	for i, t := range terms {
		index[termKey(t.Term)] = i
	}
	for _, t := range more {
		t.Term = strings.TrimSpace(t.Term)
		if !UsefulTerm(t.Term) {
			continue
		}
		key := termKey(t.Term)
		i, ok := index[key]
		if !ok {
			if len(terms) >= maxTerms {
				continue
			}
			i = len(terms)
			index[key] = i
			// 存进数据库时是 []，不是 null
			terms = append(terms, Term{Term: t.Term, Variants: []string{}})
		}
		for _, v := range t.Variants {
			v = strings.TrimSpace(v)
			// 包含正确写法的（「这识别模型」）不算听错：搜正确写法时本来就能找到它
			if !UsefulTerm(v) || strings.Contains(termKey(v), key) || slices.ContainsFunc(terms[i].Variants, func(x string) bool { return termKey(x) == termKey(v) }) {
				continue
			}
			terms[i].Variants = append(terms[i].Variants, v)
		}
	}
	return terms
}

// termKey 是判断两个写法是不是同一个时用的：转小写、去掉所有空白（「Go 语言」和「go语言」算一个）。
func termKey(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), "")
}

// UsefulTerm 判断一个写法能不能放进术语表、拿来扩展搜索：至少两个字，而且有字母或汉字。
// 只有一个字的听错写法（「AI」听成「爱」）会让搜索命中几乎所有会议（可爱、爱好……），只有标点、数字的也没有意义。
func UsefulTerm(s string) bool {
	s = strings.TrimSpace(s)
	return utf8.RuneCountInString(s) >= 2 && strings.IndexFunc(s, unicode.IsLetter) >= 0
}

// Generate 生成纪要（Markdown）。progress 在每完成一次大模型调用后回调。
func Generate(ctx context.Context, chat ChatFunc, in Input, progress func(done, total int)) (Result, error) {
	parts := split(lines(in), maxCharsPerCall)
	// 有标记的重点时多一条要求，最后那条「用 Markdown 输出」的编号跟着往后挪
	extra, last := "", 6
	if len(in.Marks) > 0 {
		extra, last = markRule, 7
	}
	// 分了段就多一次汇总调用
	total := len(parts)
	if total > 1 {
		total++
	}
	done := 0
	step := func() {
		done++
		if progress != nil {
			progress(done, total)
		}
	}

	material := "会议转写：\n" + strings.Join(parts[0], "\n")
	var terms []Term
	termsFound := false
	if len(parts) > 1 {
		// 术语表在每一部分里要：最后汇总那一次只看得到要点，要点里听错的字已经纠正了，看不到转写里的听错写法
		notes := make([]string, len(parts))
		for i, part := range parts {
			system := fmt.Sprintf("你是一名专业的会议记录员。下面是一场长会议转写的第 %d 部分（共 %d 部分）。"+
				"请提取这一部分的要点：讨论了什么、得出了什么结论、谁要做什么。用 Markdown 列表输出，除了最后的 terms 代码块，不要输出别的话。\n\n"+rules+extra,
				i+1, len(parts), hotwordRule(in.Hotwords)) + termsRule(last, "要点")
			note, err := chat(ctx, system, strings.Join(part, "\n"))
			if err != nil {
				return Result{}, err
			}
			note, partTerms, ok := splitTerms(note)
			terms, termsFound = mergeTerms(terms, partTerms), termsFound || ok
			notes[i] = fmt.Sprintf("### 第 %d 部分\n%s", i+1, note)
			step()
		}
		material = "会议较长，下面是按时间顺序分段整理出的要点：\n\n" + strings.Join(notes, "\n\n")
		if len(in.Marks) > 0 {
			clocks := make([]string, len(in.Marks))
			for i, t := range in.Marks {
				clocks[i] = "[" + transcript.Clock(t) + "]"
			}
			material += "\n\n开会的人特意标记为重点的时间点：" + strings.Join(clocks, "、")
			extra = markSummaryRule
		}
	}

	system := "你是一名专业的会议记录员，根据会议内容写一份会议纪要。\n\n" + fmt.Sprintf(rules, hotwordRule(in.Hotwords)) + extra
	if len(parts) == 1 {
		system += fmt.Sprintf("\n%d. 用 Markdown 输出，用二级标题（##）分节，不要一级标题。除了最后的 terms 代码块，不要输出纪要以外的话，正文不要用代码块包裹。", last) +
			termsRule(last+1, "纪要正文")
	} else {
		// 分段处理时术语表已经从各部分拿到了，汇总这一次看不到转写，不再要
		system += fmt.Sprintf("\n%d. 用 Markdown 输出，用二级标题（##）分节，不要一级标题。不要输出纪要以外的话，正文不要用代码块包裹。", last)
	}
	user := fmt.Sprintf("会议标题：%s\n会议时间：%s\n\n纪要按下面的结构写：\n%s\n\n%s",
		in.Title, in.Date.Format("2006-01-02 15:04"), in.Template.Guide, material)
	out, err := chat(ctx, system, user)
	if err != nil {
		return Result{}, err
	}
	step()
	content, finalTerms, ok := splitTerms(out)
	terms, termsFound = mergeTerms(finalTerms, terms), termsFound || ok
	return Result{Content: unwrapFence(content), Terms: terms, TermsFound: termsFound}, nil
}

// split 把转写按行分成若干段，每段不超过 limit 个字。单行超长时独占一段。
func split(lines []string, limit int) [][]string {
	var parts [][]string
	var cur []string
	size := 0
	for _, line := range lines {
		n := len([]rune(line)) + 1
		if size+n > limit && len(cur) > 0 {
			parts = append(parts, cur)
			cur, size = nil, 0
		}
		cur = append(cur, line)
		size += n
	}
	if len(cur) > 0 || len(parts) == 0 {
		parts = append(parts, cur)
	}
	return parts
}

// unwrapFence 去掉模型有时会加在整篇外面的 ```markdown 代码块。
// 结尾那行 ``` 漏了（或者被后面的 terms 代码块吃掉了）、只剩开头一行 ```markdown 时也去掉。
func unwrapFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	first, body, ok := strings.Cut(s, "\n")
	if !ok {
		return s
	}
	switch strings.Count(s, "```") {
	case 2:
		if strings.HasSuffix(s, "```") {
			return strings.TrimSpace(strings.TrimSuffix(body, "```"))
		}
	case 1:
		if lang := strings.TrimSpace(strings.TrimPrefix(first, "```")); lang == "" || lang == "markdown" || lang == "md" {
			return strings.TrimSpace(body)
		}
	}
	return s
}

// 纪要里对说话人的称呼：「说话人 3」，后面可能跟着括号里的名字「说话人 3（张敏）」
var speakerRef = regexp.MustCompile(`说话人 ?(\d+)(（[^）\n]{0,20}）|\([^)\n]{0,20}\))?`)

// RenameSpeakers 把纪要里对说话人的称呼换成新名字。before 是这次改之前各说话人的名字，
// renamed 是这次改的：编号 → [以前的名字, 新名字]。
//
// 「说话人 N」（连同后面括号里大模型猜的名字）换成新名字。以前起过的名字在正文里也换成新名字，
// 但有歧义的不换，留给人手动改：旧名字只有一个字、旧名字不止一个说话人在用、新名字和别的说话人重名。
// 所有替换一次做完，一次互换两个人的名字也不会串；英文名要整个单词对上（Li 不会换掉 Linux 里的 Li）。
// 返回换完的纪要。
func RenameSpeakers(content string, before transcript.Names, renamed map[int][2]string) string {
	after := make(transcript.Names, len(before))
	for idx, name := range before {
		after[idx] = name
	}
	for idx, names := range renamed {
		if names[1] == "" {
			delete(after, idx)
		} else {
			after[idx] = names[1]
		}
	}
	uses := func(names transcript.Names, name string) int {
		n := 0
		for _, v := range names {
			if v == name {
				n++
			}
		}
		return n
	}
	pairs := map[string]string{}
	for _, names := range renamed {
		old, now := names[0], names[1]
		if now == "" || old == now || utf8.RuneCountInString(old) < 2 || uses(before, old) > 1 || uses(after, now) > 1 {
			continue
		}
		pairs[old] = now
	}

	// 「说话人 N」和旧名字放在同一个正则里一次换完：换上去的新名字不会再被当成旧名字换一遍
	alts := []string{speakerRef.String()}
	olds := make([]string, 0, len(pairs))
	for old := range pairs {
		olds = append(olds, old)
	}
	// 长的名字先比，免得「张敏敏」里的「张敏」先被换掉
	slices.SortFunc(olds, func(a, b string) int { return len(b) - len(a) })
	for _, old := range olds {
		p := regexp.QuoteMeta(old)
		if first, _ := utf8.DecodeRuneInString(old); isASCIIWord(first) {
			p = `\b` + p
		}
		if last, _ := utf8.DecodeLastRuneInString(old); isASCIIWord(last) {
			p += `\b`
		}
		alts = append(alts, p)
	}
	re := regexp.MustCompile(strings.Join(alts, "|"))
	return re.ReplaceAllStringFunc(content, func(m string) string {
		if ref := speakerRef.FindStringSubmatch(m); ref != nil && ref[0] == m {
			n, err := strconv.Atoi(ref[1])
			if err != nil {
				return m
			}
			if names, ok := renamed[n-1]; ok && names[1] != "" {
				return names[1]
			}
			return m
		}
		if now, ok := pairs[m]; ok {
			return now
		}
		return m
	})
}

func isASCIIWord(r rune) bool {
	return r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_')
}

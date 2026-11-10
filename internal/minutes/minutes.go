// Package minutes 把转写交给大模型写成会议纪要。
package minutes

import (
	"context"
	"fmt"
	"strings"
	"time"

	"zsmeet/internal/transcript"
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
	out := transcript.Lines(in.Segments)
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

// Generate 生成纪要（Markdown）。progress 在每完成一次大模型调用后回调。
func Generate(ctx context.Context, chat ChatFunc, in Input, progress func(done, total int)) (string, error) {
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
	if len(parts) > 1 {
		notes := make([]string, len(parts))
		for i, part := range parts {
			system := fmt.Sprintf("你是一名专业的会议记录员。下面是一场长会议转写的第 %d 部分（共 %d 部分）。"+
				"请提取这一部分的要点：讨论了什么、得出了什么结论、谁要做什么。用 Markdown 列表输出，不要输出别的话。\n\n"+rules+extra,
				i+1, len(parts), hotwordRule(in.Hotwords))
			note, err := chat(ctx, system, strings.Join(part, "\n"))
			if err != nil {
				return "", err
			}
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

	system := "你是一名专业的会议记录员，根据会议内容写一份会议纪要。\n\n" +
		fmt.Sprintf(rules, hotwordRule(in.Hotwords)) + extra +
		fmt.Sprintf("\n%d. 用 Markdown 输出，用二级标题（##）分节，不要一级标题，不要输出纪要以外的话，不要用代码块包裹。", last)
	user := fmt.Sprintf("会议标题：%s\n会议时间：%s\n\n纪要按下面的结构写：\n%s\n\n%s",
		in.Title, in.Date.Format("2006-01-02 15:04"), in.Template.Guide, material)
	out, err := chat(ctx, system, user)
	if err != nil {
		return "", err
	}
	step()
	return unwrapFence(out), nil
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
func unwrapFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") || !strings.HasSuffix(s, "```") || strings.Count(s, "```") != 2 {
		return s
	}
	s = strings.TrimSuffix(s, "```")
	if _, body, ok := strings.Cut(s, "\n"); ok {
		return strings.TrimSpace(body)
	}
	return s
}

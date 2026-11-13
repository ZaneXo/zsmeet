package correct

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/ZaneXo/zsmeet/internal/transcript"
)

// ChatFunc 发一轮对话给大模型。
type ChatFunc func(ctx context.Context, system, user string) (string, error)

// Input 是校对时交给大模型的会议信息，帮它判断专有名词该怎么写。
type Input struct {
	Title    string
	Hotwords []string
	// 给说话人起的名字：转写里按名字称呼，名字本身也是要写对的专有名词
	Speakers transcript.Names
	// 一次请求里最多放多少字的转写，0 时用 DefaultBatchChars
	BatchChars int
	// 同时发几个请求，0 时用 DefaultParallel
	Parallel int
	// 每句后面附上识别结果的拼音。认错的字大多和正确的字同音、近音，有拼音对照模型更容易看出来
	// （Tang 等，Interspeech 2024；PY-GEC），代价是请求长了一倍多
	Pinyin bool
}

const (
	// DefaultBatchChars：每批两千字左右（十几分钟的会）。批小一点模型改得仔细，返回的内容也短，
	// 8K 上下文的本地模型也放得下
	DefaultBatchChars = 2_000
	// DefaultParallel：同时发三批。云上的模型三批一起发快得多；本地模型本来就一批一批算，多发也不吃亏
	DefaultParallel = 3
)

// Stats 是一次校对的结果。
type Stats struct {
	// 分成了几批、几批失败了（请求出错，或者回答解析不出来）
	Batches, Failed int
	// 改了几句；大模型改了但改动太大、没有采用的有几句
	Changed, Rejected int
	// 第一批失败的原因，没有失败时为 nil
	Err error
}

const system = `你是会议转写的校对员。下面是语音识别出的会议转写，每行一句，开头方括号里是句子编号，后面是说话人和这句话。
语音识别会把字认错：同音字、近音字，人名、产品名、英文术语被认成发音相近的汉字或别的单词。请结合上下文和会议信息改正这些错。

要求：
1. 只改认错的字词。不要润色，不要换说法，不要删语气词和重复的话，不要补充内容，不要合并或拆分句子。
2. 拿不准的不改。读起来别扭但可能就是原话的，不改。
3. 人名、产品名、项目名优先用会议信息里给出的写法。
4. 标点、数字的写法保持原样，除非它们本身就是认错的一部分。
5. 只输出 JSON，不要输出别的话：{"fixes": [{"id": 句子编号, "text": "改正后的整句"}]}。
   只列出改过的句子，text 是改正后的完整一句（不带编号和说话人）；没有要改的就输出 {"fixes": []}。`

const pinyinHint = `每句下面一行「拼音：」是按识别出的字注的音（不带声调）。认错的字往往和正确的字同音或近音，
可以拿拼音对照上下文和会议信息找出认错的地方。拼音只是参考，输出的 text 里不要带拼音。`

// LLM 把转写分批交给大模型校对，改正的句子直接改在 segs 里（原文存进 Orig），progress 每做完一批回调一次。
//
// 某一批请求失败、或者回答解析不出来，这一批就不改，接着做别的批，失败的情况记在 Stats 里：
// 校对是锦上添花，不该因为它让整场会议处理失败。只有 ctx 被取消时才返回错误。
func LLM(ctx context.Context, chat ChatFunc, segs []transcript.Segment, in Input, progress func(done, total int)) (Stats, error) {
	limit := cmpOr(in.BatchChars, DefaultBatchChars)
	parallel := cmpOr(in.Parallel, DefaultParallel)
	batches := split(segs, in.Speakers, limit, in.Pinyin)
	stats := Stats{Batches: len(batches)}
	if len(batches) == 0 {
		return stats, nil
	}
	sys := system
	if in.Pinyin {
		sys += "\n\n" + pinyinHint
	}
	if info := meetingInfo(in); info != "" {
		sys += "\n\n" + info
	}

	type result struct {
		fixes map[int]string
		err   error
	}
	results := make(chan result)
	sem := make(chan struct{}, parallel)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for _, b := range batches {
		go func() {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results <- result{err: ctx.Err()}
				return
			}
			defer func() { <-sem }()
			out, err := chat(ctx, sys, b.text)
			if err != nil {
				results <- result{err: err}
				return
			}
			fixes, err := parseFixes(out, b.from, b.to)
			results <- result{fixes: fixes, err: err}
		}()
	}

	// 各批改的是不同的句子，回来一批改一批，都在这一个 goroutine 里改，不用加锁
	for done := 1; done <= len(batches); done++ {
		r := <-results
		if r.err != nil {
			stats.Failed++
			if stats.Err == nil {
				stats.Err = r.err
			}
		}
		for id, text := range r.fixes {
			// 模型有时把行首的说话人也抄了回来
			text = strings.TrimSpace(strings.TrimPrefix(text, in.Speakers.Of(segs[id].Speaker)+"："))
			if text == segs[id].Text {
				continue
			}
			if !acceptable(segs[id].Text, text) {
				stats.Rejected++
				continue
			}
			set(&segs[id], text)
			stats.Changed++
		}
		if progress != nil {
			progress(done, len(batches))
		}
	}
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	return stats, nil
}

func cmpOr(n, def int) int {
	if n > 0 {
		return n
	}
	return def
}

// meetingInfo 是系统提示末尾的会议信息：标题、热词、说话人的名字。都没有时为空。
func meetingInfo(in Input) string {
	var lines []string
	if t := strings.TrimSpace(in.Title); t != "" {
		lines = append(lines, "会议标题："+t)
	}
	if len(in.Hotwords) > 0 {
		lines = append(lines, "会上会提到的专有名词（正确写法）："+strings.Join(in.Hotwords, "、"))
	}
	// 按说话人编号排，提示每次都一样
	var names []string
	for _, idx := range slices.Sorted(maps.Keys(in.Speakers)) {
		if name := strings.TrimSpace(in.Speakers[idx]); name != "" && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	if len(names) > 0 {
		lines = append(lines, "参会的人："+strings.Join(names, "、"))
	}
	if len(lines) == 0 {
		return ""
	}
	return "会议信息：\n" + strings.Join(lines, "\n")
}

// batch 是交给大模型的一批：第 from 到 to 句（不含 to），排好的文字。
type batch struct {
	from, to int
	text     string
}

// split 把转写按顺序分批，每批的文字不超过 limit 个字（单句超长时独占一批，拼音不算字数）。
// 没有文字的句子不发。withPinyin 时每句下面多一行这句的拼音。
func split(segs []transcript.Segment, names transcript.Names, limit int, withPinyin bool) []batch {
	var out []batch
	var b strings.Builder
	from, size := 0, 0
	flush := func(to int) {
		if b.Len() > 0 {
			out = append(out, batch{from: from, to: to, text: b.String()})
		}
		b.Reset()
		from, size = to, 0
	}
	for i, s := range segs {
		if strings.TrimSpace(s.Text) == "" {
			continue
		}
		line := fmt.Sprintf("[%d] %s：%s\n", i, names.Of(s.Speaker), s.Text)
		n := utf8.RuneCountInString(line)
		if size > 0 && size+n > limit {
			flush(i)
		}
		b.WriteString(line)
		if withPinyin {
			if py := pinyinOf(s.Text); py != "" {
				b.WriteString("    拼音：" + py + "\n")
			}
		}
		size += n
	}
	flush(len(segs))
	return out
}

// parseFixes 从大模型的回答里取出改正的句子：编号 → 改正后的整句。只要 from 到 to（不含）之间的编号，
// 别的编号（模型编出来的）不要。回答里找不到合法的 JSON 时返回错误。
func parseFixes(out string, from, to int) (map[int]string, error) {
	start, end := strings.Index(out, "{"), strings.LastIndex(out, "}")
	if start < 0 || end < start {
		return nil, fmt.Errorf("大模型的回答里没有 JSON：%s", clip(out))
	}
	var parsed struct {
		Fixes []struct {
			ID   flexInt `json:"id"`
			Text string  `json:"text"`
		} `json:"fixes"`
	}
	if err := json.Unmarshal([]byte(out[start:end+1]), &parsed); err != nil {
		return nil, fmt.Errorf("大模型的回答解析不出来（%v）：%s", err, clip(out))
	}
	fixes := map[int]string{}
	for _, f := range parsed.Fixes {
		if id := int(f.ID); id >= from && id < to {
			fixes[id] = strings.TrimSpace(f.Text)
		}
	}
	return fixes, nil
}

// flexInt 是句子编号：有的模型把编号写成字符串（"id": "3"），也认。
type flexInt int

func (n *flexInt) UnmarshalJSON(b []byte) error {
	var v int
	if err := json.Unmarshal(bytes.Trim(b, `"`), &v); err != nil {
		return err
	}
	*n = flexInt(v)
	return nil
}

func clip(s string) string {
	if r := []rune(s); len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return s
}

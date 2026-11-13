// Package transcript 定义转写结果，以及把「识别出的句子」和「说话人时间段」合到一起的逻辑。
package transcript

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Segment 是转写里的一段话。时间单位是秒。
type Segment struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	// 说话人编号，从 0 开始，按第一次开口的先后排；-1 表示没分出来
	Speaker int    `json:"speaker"`
	Text    string `json:"text"`
	// 识别出的原文。只有文字被纠错改过的句子才有（见 internal/correct），没改过的为空
	Orig string `json:"orig,omitempty"`
}

// Turn 是说话人区分的结果：某个人在一段时间里说话。
type Turn struct {
	Start, End float64
	Speaker    int
}

// AssignSpeakers 给每句话找说话人：取时间重叠最多的那个人；
// 完全没有重叠时取 1 秒内最近的那个人。最后按第一次开口的顺序重新编号。
// 返回说话人区分给的编号 → 转写里的编号；一句话都没分到的人不在里面。
func AssignSpeakers(segs []Segment, turns []Turn) map[int]int {
	for i := range segs {
		segs[i].Speaker = pickSpeaker(segs[i], turns)
	}
	return renumber(segs)
}

// RenumberTurns 把说话人区分的结果换成转写里的编号（ids 是 AssignSpeakers 的返回值），
// 一句话都没分到的人的时间段去掉。
func RenumberTurns(turns []Turn, ids map[int]int) []Turn {
	out := make([]Turn, 0, len(turns))
	for _, t := range turns {
		if id, ok := ids[t.Speaker]; ok {
			t.Speaker = id
			out = append(out, t)
		}
	}
	return out
}

func pickSpeaker(seg Segment, turns []Turn) int {
	overlap := make(map[int]float64)
	nearest, nearestGap := -1, 1.0
	for _, t := range turns {
		if o := min(seg.End, t.End) - max(seg.Start, t.Start); o > 0 {
			overlap[t.Speaker] += o
			continue
		}
		gap := max(t.Start-seg.End, seg.Start-t.End)
		if gap < nearestGap {
			nearest, nearestGap = t.Speaker, gap
		}
	}
	best, bestOverlap := nearest, 0.0
	for speaker, o := range overlap {
		// 重叠一样长时取编号小的，保证结果稳定
		if o > bestOverlap || (o == bestOverlap && bestOverlap > 0 && speaker < best) {
			best, bestOverlap = speaker, o
		}
	}
	return best
}

func renumber(segs []Segment) map[int]int {
	ids := make(map[int]int)
	for i := range segs {
		s := segs[i].Speaker
		if s < 0 {
			continue
		}
		if _, ok := ids[s]; !ok {
			ids[s] = len(ids)
		}
		segs[i].Speaker = ids[s]
	}
	return ids
}

// joinText 把两句接在一起。英文的两句之间要留一个空格，中文直接接上。
func joinText(a, b string) string {
	if a == "" || b == "" {
		return a + b
	}
	last, _ := utf8.DecodeLastRuneInString(a)
	next, _ := utf8.DecodeRuneInString(b)
	if last < utf8.RuneSelf && next < utf8.RuneSelf && last != ' ' && next != ' ' {
		return a + " " + b
	}
	return a + b
}

// markedBetween：from 到 to 之间（含 from、不含 to）有没有标记。
func markedBetween(marks []float64, from, to float64) bool {
	for _, t := range marks {
		if t >= from && t < to {
			return true
		}
	}
	return false
}

// Merge 把同一个人连着说的短句并成一段，方便阅读，也省大模型的字数。
// 停顿超过 maxGap 秒、或并起来超过 maxChars 个字就另起一段。
// breaks 是录音时标记重点的时间点（从小到大）：不跨过它们合并，标记那一刻正在说的那句话是一段的结尾，
// 后面的话另起一段。这样纪要和会议页面都能对上标记时在说哪句话。
func Merge(segs []Segment, maxGap float64, maxChars int, breaks []float64) []Segment {
	out := make([]Segment, 0, len(segs))
	for _, s := range segs {
		if n := len(out); n > 0 {
			last := &out[n-1]
			if last.Speaker == s.Speaker && s.Start-last.End <= maxGap &&
				len([]rune(last.Text))+len([]rune(s.Text)) <= maxChars && !markedBetween(breaks, last.Start, s.Start) {
				last.Text = joinText(last.Text, s.Text)
				last.End = s.End
				continue
			}
		}
		// 合成的段落只用来阅读、交给大模型，不带纠错前的原文（那只对单句有意义）
		s.Orig = ""
		out = append(out, s)
	}
	return out
}

// Clock 把秒数写成 mm:ss，超过一小时写成 h:mm:ss。
func Clock(sec float64) string {
	s := int(sec)
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s%3600/60, s%60)
	}
	return fmt.Sprintf("%02d:%02d", s/60, s%60)
}

// SpeakerName 是说话人的默认称呼。
func SpeakerName(speaker int) string {
	if speaker < 0 {
		return "说话人"
	}
	return fmt.Sprintf("说话人 %d", speaker+1)
}

// Names 是给说话人起的名字，键是说话人编号（从 0 开始）。
type Names map[int]string

// ParseNames 读数据库里存的名字（{"0": "张敏"}）。读不出来就当没起名字。
func ParseNames(raw []byte) Names {
	var stored map[string]string
	if json.Unmarshal(raw, &stored) != nil {
		return Names{}
	}
	names := make(Names, len(stored))
	for k, v := range stored {
		if n, err := strconv.Atoi(k); err == nil && v != "" {
			names[n] = v
		}
	}
	return names
}

// Of 返回说话人的名字，没起名字时是「说话人 N」。
func (n Names) Of(speaker int) string {
	if name := n[speaker]; name != "" {
		return name
	}
	return SpeakerName(speaker)
}

// Marshal 写成数据库里存的样子（{"0": "张敏"}）。
func (n Names) Marshal() ([]byte, error) {
	stored := make(map[string]string, len(n))
	for idx, name := range n {
		stored[strconv.Itoa(idx)] = name
	}
	return json.Marshal(stored)
}

// Diff 返回从 n 改成 now 改了哪些：编号 → [以前的名字, 现在的名字]，去掉了的现在的名字是空字符串。
func (n Names) Diff(now Names) map[int][2]string {
	out := map[int][2]string{}
	for idx, name := range now {
		if n[idx] != name {
			out[idx] = [2]string{n[idx], name}
		}
	}
	for idx, name := range n {
		if _, ok := now[idx]; !ok {
			out[idx] = [2]string{name, ""}
		}
	}
	return out
}

// Lines 把转写写成一行一段的文本：`[mm:ss] 说话人 1：内容`。给大模型和导出用。
func Lines(segs []Segment, names Names) []string {
	lines := make([]string, len(segs))
	for i, s := range segs {
		lines[i] = fmt.Sprintf("[%s] %s：%s", Clock(s.Start), names.Of(s.Speaker), s.Text)
	}
	return lines
}

// PlainText 只取文字，用来建搜索索引。
func PlainText(segs []Segment) string {
	var b strings.Builder
	for _, s := range segs {
		b.WriteString(s.Text)
		b.WriteByte('\n')
	}
	return b.String()
}

// RemapNames 重新识别以后，同一个人的说话人编号可能变了。把以前起的名字对到新的编号上（见 SpeakerMap），
// 对不上的名字不要了，宁可让人重新起，也不要张冠李戴。
func RemapNames(old, segs []Segment, names Names) Names {
	out := Names{}
	if len(names) == 0 {
		return out
	}
	ids := SpeakerMap(old, segs)
	for a, name := range names {
		if b, ok := ids[a]; ok {
			out[b] = name
		}
	}
	return out
}

// SpeakerMap 按新旧两份转写里各个说话人说话时间的重叠，找出旧编号 → 新编号：
// 只有旧的 a 和新的 b 互相是对方重叠最多的那个，才算同一个人。对不上的旧编号不在里面。
func SpeakerMap(old, segs []Segment) map[int]int {
	overlap := map[[2]int]float64{}
	for _, o := range old {
		if o.Speaker < 0 {
			continue
		}
		for _, n := range segs {
			if n.Speaker < 0 {
				continue
			}
			if d := min(o.End, n.End) - max(o.Start, n.Start); d > 0 {
				overlap[[2]int{o.Speaker, n.Speaker}] += d
			}
		}
	}
	// best[a] 是和旧的 a 重叠最多的新编号，back[b] 是和新的 b 重叠最多的旧编号；并列的算对不上（-1）
	type pick struct {
		id   int
		secs float64
	}
	best, back := map[int]pick{}, map[int]pick{}
	consider := func(m map[int]pick, key, id int, secs float64) {
		cur, ok := m[key]
		switch {
		case !ok || secs > cur.secs:
			m[key] = pick{id, secs}
		case secs == cur.secs:
			m[key] = pick{-1, secs}
		}
	}
	for k, secs := range overlap {
		consider(best, k[0], k[1], secs)
		consider(back, k[1], k[0], secs)
	}
	out := map[int]int{}
	for a, b := range best {
		if b.id >= 0 && back[b.id].id == a {
			out[a] = b.id
		}
	}
	return out
}

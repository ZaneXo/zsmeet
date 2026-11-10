// Package transcript 定义转写结果，以及把「识别出的句子」和「说话人时间段」合到一起的逻辑。
package transcript

import (
	"fmt"
	"strings"
)

// Segment 是转写里的一段话。时间单位是秒。
type Segment struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	// 说话人编号，从 0 开始，按第一次开口的先后排；-1 表示没分出来
	Speaker int    `json:"speaker"`
	Text    string `json:"text"`
}

// Turn 是说话人区分的结果：某个人在一段时间里说话。
type Turn struct {
	Start, End float64
	Speaker    int
}

// AssignSpeakers 给每句话找说话人：取时间重叠最多的那个人；
// 完全没有重叠时取 1 秒内最近的那个人。最后按第一次开口的顺序重新编号。
func AssignSpeakers(segs []Segment, turns []Turn) {
	for i := range segs {
		segs[i].Speaker = pickSpeaker(segs[i], turns)
	}
	renumber(segs)
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

func renumber(segs []Segment) {
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
				last.Text += s.Text
				last.End = s.End
				continue
			}
		}
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

// Lines 把转写写成一行一段的文本：`[mm:ss] 说话人 1：内容`。给大模型和导出用。
func Lines(segs []Segment) []string {
	lines := make([]string, len(segs))
	for i, s := range segs {
		lines[i] = fmt.Sprintf("[%s] %s：%s", Clock(s.Start), SpeakerName(s.Speaker), s.Text)
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

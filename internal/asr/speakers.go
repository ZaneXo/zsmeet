package asr

import (
	"cmp"
	"maps"
	"slices"

	"github.com/ZaneXo/zsmeet/internal/transcript"
	"github.com/ZaneXo/zsmeet/internal/voiceprint"
)

// 按人数区分说话人分两步：先按阈值自动区分，分出的人通常比实际的多（同一个人被拆成几个，还有一些零碎）；
// 再给每个人算声纹，把声音最像的两组反复并起来，直到剩下要求的人数。
//
// 不直接让库按人数聚类：它把录音切成很多 10 秒的小块，每块里的每个人算一个声纹再聚类，
// 只有一两块的零碎（比如两个人抢话、笑声）也算一类；这些零碎在最后一步被丢掉，分出的人比要求的少
// （测试录音里 4 个人说话，要求分 4 个只分出 2 个）。这里每个人的声纹用他最长的几段话（最多 30 秒）算，
// 比小块上算的稳得多。零碎的问题合并时一样有：它们和谁都不像，会一直单独留着，占掉人数，
// 所以合并时只数说话够 minGroupSeconds 秒的组，零碎最后归到声音最像的那组（pyannote 也是这么做的）；
// 够秒数的组凑不齐人数时（有人只说了几句），才让说得最多的零碎也算一个人。

// 合并后一组人说话加起来到这么多秒才算一位说话人；不够的是零碎，归到声音最像的那组。
// 正常开会每个人至少会说上十几秒；抢话、笑声、咳嗽之类的零碎一般只有几秒
const minGroupSeconds = 10

// group 是合并中的一组说话人。sum 是组里每人的声纹按秒数加权的和，weight 是这些秒数的和，
// 所以两组之间每一对人的声纹相似度的加权平均就是 sum 的点积除以两边的 weight。talk 是组里的人说话加起来的秒数。
type group struct {
	sum     []float64
	weight  float64
	talk    float64
	members []int
}

func newGroup(speaker int, v voiceprint.Voice, talk float64) *group {
	g := &group{sum: make([]float64, len(v.Embedding)), weight: v.Seconds, talk: talk, members: []int{speaker}}
	for i, x := range v.Embedding {
		g.sum[i] = v.Seconds * float64(x)
	}
	return g
}

func (g *group) clone() *group {
	return &group{sum: slices.Clone(g.sum), weight: g.weight, talk: g.talk, members: slices.Clone(g.members)}
}

func (g *group) absorb(o *group) {
	for i, x := range o.sum {
		g.sum[i] += x
	}
	g.weight += o.weight
	g.talk += o.talk
	g.members = append(g.members, o.members...)
}

// similarity 是 g 和 o 之间每一对人的声纹相似度（余弦）的加权平均。
func (g *group) similarity(o *group) float64 {
	dot := 0.0
	for i, x := range g.sum {
		dot += x * o.sum[i]
	}
	return dot / (g.weight * o.weight)
}

// mostSimilar 返回 groups 里和 g 最像的那组的下标。一样像时取前面的，保证结果稳定。
func mostSimilar(g *group, groups []*group) int {
	best, bestSim := 0, g.similarity(groups[0])
	for i := 1; i < len(groups); i++ {
		if sim := g.similarity(groups[i]); sim > bestSim {
			best, bestSim = i, sim
		}
	}
	return best
}

// countLarge 数说话够 minGroupSeconds 秒的组。
func countLarge(groups []*group) int {
	n := 0
	for _, g := range groups {
		if g.talk >= minGroupSeconds {
			n++
		}
	}
	return n
}

// mergeOnce 把最像的两组并起来。一样像时取前面的一对，保证结果稳定。
func mergeOnce(groups []*group) []*group {
	bi, bj, best := 0, 1, groups[0].similarity(groups[1])
	for i := range groups {
		for j := i + 1; j < len(groups); j++ {
			if sim := groups[i].similarity(groups[j]); sim > best {
				bi, bj, best = i, j, sim
			}
		}
	}
	groups[bi].absorb(groups[bj])
	return slices.Delete(groups, bj, bj+1)
}

// mergeGroups 把 groups 一步步并起来，找说话够 minGroupSeconds 秒的组正好是 n 个的那一步（最早的一步）；
// 没有正好 n 个的一步时取最接近的，大组不够时说得最多的小组补上。返回那一步的 n 组（不够 n 组时有几组算几组），
// 剩下的零碎已经归到声音最像的组里。一开始就没有大组时（录音很短，谁都说得不多），按组数并到 n 组。
func mergeGroups(groups []*group, n int) []*group {
	if countLarge(groups) == 0 {
		for len(groups) > n {
			groups = mergeOnce(groups)
		}
		return groups
	}

	// 先试着并到底，记下大组的数量最接近 n 的那一步，再从头重放到那一步
	trial := make([]*group, len(groups))
	for i, g := range groups {
		trial[i] = g.clone()
	}
	bestStep, bestDiff := 0, abs(countLarge(trial)-n)
	for step := 1; len(trial) > 1 && bestDiff > 0; step++ {
		trial = mergeOnce(trial)
		if diff := abs(countLarge(trial) - n); diff < bestDiff {
			bestStep, bestDiff = step, diff
		}
	}
	for range bestStep {
		groups = mergeOnce(groups)
	}

	var large, small []*group
	for _, g := range groups {
		if g.talk >= minGroupSeconds {
			large = append(large, g)
		} else {
			small = append(small, g)
		}
	}
	// 大组不够 n 个：要求的人里有人只说了几句，说得最多的几个小组也各算一个人
	if len(large) < n {
		slices.SortStableFunc(small, func(a, b *group) int { return cmp.Compare(b.talk, a.talk) })
		k := min(n-len(large), len(small))
		large = append(large, small[:k]...)
		small = small[k:]
	}
	// 先都找好归到哪组再并，免得先并进去的改变了后面的比较
	targets := make([]int, len(small))
	for i, g := range small {
		targets[i] = mostSimilar(g, large)
	}
	for i, g := range small {
		large[targets[i]].absorb(g)
	}
	return large
}

func abs(x int) int {
	return max(x, -x)
}

// mergeSpeakers 把 turns 里的说话人并成 n 个。voices 是每个人的声纹（录音太短、算不出来的没有）。
// 返回新的时间段：同一组的人用同一个编号，同一个人连着的时间段接成一段。
// 人数本来就不超过 n、或者一个声纹都没有时原样返回。
func mergeSpeakers(turns []transcript.Turn, voices map[int]voiceprint.Voice, n int) []transcript.Turn {
	talk := map[int]float64{}
	for _, t := range turns {
		if t.Speaker >= 0 {
			talk[t.Speaker] += t.End - t.Start
		}
	}
	if len(talk) <= n || len(voices) == 0 {
		return turns
	}

	// 声纹够可靠（说话够 minVoiceSeconds 秒）的人参与合并；都不够时都参与
	ids := slices.Sorted(maps.Keys(voices))
	var core []int
	for _, s := range ids {
		if voices[s].Seconds >= minVoiceSeconds {
			core = append(core, s)
		}
	}
	if len(core) == 0 {
		core = ids
	}
	groups := make([]*group, len(core))
	for i, s := range core {
		groups[i] = newGroup(s, voices[s], talk[s])
	}
	groups = mergeGroups(groups, n)

	label := map[int]int{}
	for gi, g := range groups {
		for _, s := range g.members {
			label[s] = gi
		}
	}
	// 声纹不可靠的人归到声音最像的那组
	for _, s := range ids {
		if _, ok := label[s]; !ok {
			label[s] = mostSimilar(newGroup(s, voices[s], talk[s]), groups)
		}
	}

	out := make([]transcript.Turn, 0, len(turns))
	for _, t := range turns {
		if l, ok := label[t.Speaker]; ok {
			t.Speaker = l
			out = append(out, t)
		}
	}
	// 连声纹都算不出来的人（只有零星几小段）：每段归到离它最近的那段话的那组
	for _, t := range turns {
		if _, ok := label[t.Speaker]; ok || t.Speaker < 0 {
			continue
		}
		if l, ok := nearestSpeaker(t, out); ok {
			t.Speaker = l
			out = append(out, t)
		}
	}
	return joinTurns(out)
}

// nearestSpeaker 是离 t 最近（重叠的算 0）的那段话的说话人。
func nearestSpeaker(t transcript.Turn, turns []transcript.Turn) (int, bool) {
	speaker, nearest := -1, 0.0
	for _, o := range turns {
		gap := max(o.Start-t.End, t.Start-o.End, 0)
		if speaker < 0 || gap < nearest {
			speaker, nearest = o.Speaker, gap
		}
	}
	return speaker, speaker >= 0
}

// joinTurns 按开始时间排好，同一个人重叠或者首尾相接的时间段接成一段。
func joinTurns(turns []transcript.Turn) []transcript.Turn {
	slices.SortStableFunc(turns, func(a, b transcript.Turn) int {
		return cmp.Or(cmp.Compare(a.Start, b.Start), cmp.Compare(a.End, b.End))
	})
	var out []transcript.Turn
	last := map[int]int{} // 说话人 → 他在 out 里的最后一段
	for _, t := range turns {
		if i, ok := last[t.Speaker]; ok && t.Start <= out[i].End {
			out[i].End = max(out[i].End, t.End)
			continue
		}
		last[t.Speaker] = len(out)
		out = append(out, t)
	}
	return out
}

// speakerCount 是 turns 里有几位说话人。
func speakerCount(turns []transcript.Turn) int {
	seen := map[int]bool{}
	for _, t := range turns {
		if t.Speaker >= 0 {
			seen[t.Speaker] = true
		}
	}
	return len(seen)
}

package main

import (
	"slices"

	"github.com/ZaneXo/zsmeet/internal/cloudasr"
	"github.com/ZaneXo/zsmeet/internal/correct"
	"github.com/ZaneXo/zsmeet/internal/transcript"
)

// cpScore 计算一段录音的带说话人字错误数（cpCER 分子和分母）：
// 先按线上一样的 AssignSpeakers 把 turns 分给 hypSegs，再把每位说话人名下的句子按时间接起来，
// 在「标准说话人 ↔ 识别出的说话人」的所有一一对应里取总编辑距离最小的那一种。
func cpScore(refSegs, hypSegs []transcript.Segment, turns []transcript.Turn) (errs, words int) {
	refBySpeaker := groupSpeakerTokens(refSegs)
	for _, toks := range refBySpeaker {
		words += len(toks)
	}
	if words == 0 {
		return 0, 0
	}

	assigned := slices.Clone(hypSegs)
	transcript.AssignSpeakers(assigned, turns)

	var unassigned int
	var withSpeaker []transcript.Segment
	for _, seg := range assigned {
		if seg.Speaker < 0 {
			unassigned += len(correct.Tokens(cloudasr.SpokenNumbers(seg.Text)))
			continue
		}
		withSpeaker = append(withSpeaker, seg)
	}
	hypBySpeaker := groupSpeakerTokens(withSpeaker)

	k := max(len(refBySpeaker), len(hypBySpeaker))
	for len(refBySpeaker) < k {
		refBySpeaker = append(refBySpeaker, nil)
	}
	for len(hypBySpeaker) < k {
		hypBySpeaker = append(hypBySpeaker, nil)
	}

	cost := make([][]int, k)
	for i := range k {
		cost[i] = make([]int, k)
		for j := range k {
			cost[i][j] = correct.Distance(refBySpeaker[i], hypBySpeaker[j])
		}
	}
	return minAssignmentCost(cost) + unassigned, words
}

func groupSpeakerTokens(segs []transcript.Segment) [][]string {
	idxOf := map[int]int{}
	var out [][]string
	for _, seg := range segs {
		toks := correct.Tokens(cloudasr.SpokenNumbers(seg.Text))
		if len(toks) == 0 {
			continue
		}
		idx, ok := idxOf[seg.Speaker]
		if !ok {
			idx = len(out)
			idxOf[seg.Speaker] = idx
			out = append(out, nil)
		}
		out[idx] = append(out[idx], toks...)
	}
	return out
}

// minAssignmentCost 用匈牙利算法求 k×k 代价矩阵的最小完美匹配代价（O(k^3)）。
func minAssignmentCost(cost [][]int) int {
	n := len(cost)
	if n == 0 {
		return 0
	}
	const inf = int(^uint(0) >> 2)
	u := make([]int, n+1)
	v := make([]int, n+1)
	p := make([]int, n+1)
	way := make([]int, n+1)
	for i := 1; i <= n; i++ {
		p[0] = i
		j0 := 0
		minv := make([]int, n+1)
		used := make([]bool, n+1)
		for j := range minv {
			minv[j] = inf
		}
		for {
			used[j0] = true
			i0 := p[j0]
			delta := inf
			j1 := 0
			for j := 1; j <= n; j++ {
				if used[j] {
					continue
				}
				cur := cost[i0-1][j-1] - u[i0] - v[j]
				if cur < minv[j] {
					minv[j] = cur
					way[j] = j0
				}
				if minv[j] < delta {
					delta = minv[j]
					j1 = j
				}
			}
			for j := 0; j <= n; j++ {
				if used[j] {
					u[p[j]] += delta
					v[j] -= delta
				} else {
					minv[j] -= delta
				}
			}
			j0 = j1
			if p[j0] == 0 {
				break
			}
		}
		for {
			j1 := way[j0]
			p[j0] = p[j1]
			j0 = j1
			if j0 == 0 {
				break
			}
		}
	}
	total := 0
	for j := 1; j <= n; j++ {
		if p[j] > 0 {
			total += cost[p[j]-1][j-1]
		}
	}
	return total
}

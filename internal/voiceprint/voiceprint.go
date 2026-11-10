// Package voiceprint 是「记住说话人」用到的计算：声音特征的归一化、平均、相似度，
// 以及把一场会议里的说话人和声纹库里的人按声音配上对。不碰数据库，也不碰识别模型。
//
// 声音特征是说话人模型（3D-Speaker CAM++）从一段说话里算出的一串数字，同一个人的两段话算出来的方向接近。
// 这里存的都是长度归一化成 1 的向量，两个向量的余弦相似度就是它们的点积。
package voiceprint

import (
	"cmp"
	"math"
	"slices"
	"strings"
)

// Threshold 是认定「是同一个人」的最低余弦相似度。越大越保守：认不出的人多一些，但不容易认错。
// 0.6 是 CAM++ 中文模型做说话人确认时常用的取值，还需要用真实的会议录音校准。
// 认错的代价（纪要里张冠李戴）比认不出（显示「说话人 N」）大，宁可偏高
const Threshold = 0.6

// Voice 是一位说话人的声音特征。
type Voice struct {
	// 长度归一化成 1
	Embedding []float32
	// 用了多少秒的说话算出来的
	Seconds float64
}

// Normalize 返回长度归一化成 1 的向量。全零（或者有 NaN）的向量没有方向，返回 nil。
func Normalize(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	norm := math.Sqrt(sum)
	if norm == 0 || math.IsNaN(norm) || math.IsInf(norm, 0) {
		return nil
	}
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) / norm)
	}
	return out
}

// Mean 是几个样本的平均方向（先平均再归一化），也就是一个人的声纹。
// 维数和第一个不一样的样本不算（换过模型时可能有）；一个能用的都没有时返回 nil。
func Mean(samples [][]float32) []float32 {
	if len(samples) == 0 {
		return nil
	}
	dim := len(samples[0])
	sum := make([]float64, dim)
	for _, s := range samples {
		if len(s) != dim {
			continue
		}
		for i, x := range s {
			sum[i] += float64(x)
		}
	}
	out := make([]float32, dim)
	for i, x := range sum {
		out[i] = float32(x)
	}
	return Normalize(out)
}

// Cosine 是两个归一化过的向量的余弦相似度。维数不一样时返回 0（不可能是同一个人）。
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot
}

// Print 是声纹库里的一个人：名字和声纹（所有样本的平均方向）。
type Print struct {
	Name   string
	Vector []float32
}

// Match 给一场会议里还没有名字的说话人按声音配上声纹库里的人，返回 说话人编号 → 名字。
//
//   - 相似度达到 Threshold 才算；
//   - 已经有名字的说话人不动（重新转写后对回来的名字、处理期间有人改的名字）；
//   - 会议里已经有人叫这个名字了，这个声纹就不再用；
//   - 一个声纹在一场会议里最多配一位说话人，几位都像同一个人时取最像的那位，
//     一位说话人像几个人时也取最像的那个。
func Match(voices map[int][]float32, prints []Print, named map[int]string) map[int]string {
	used := map[string]bool{}
	for _, name := range named {
		used[strings.ToLower(name)] = true
	}
	type pair struct {
		speaker, print int
		sim            float64
	}
	var pairs []pair
	for speaker, v := range voices {
		if named[speaker] != "" {
			continue
		}
		for i, p := range prints {
			if used[strings.ToLower(p.Name)] {
				continue
			}
			if sim := Cosine(v, p.Vector); sim >= Threshold {
				pairs = append(pairs, pair{speaker, i, sim})
			}
		}
	}
	// 最像的先配。相似度一样时按编号，保证结果稳定
	slices.SortFunc(pairs, func(a, b pair) int {
		return cmp.Or(cmp.Compare(b.sim, a.sim), cmp.Compare(a.speaker, b.speaker), cmp.Compare(a.print, b.print))
	})
	out := map[int]string{}
	for _, p := range pairs {
		name := prints[p.print].Name
		if _, done := out[p.speaker]; done || used[strings.ToLower(name)] {
			continue
		}
		out[p.speaker] = name
		used[strings.ToLower(name)] = true
	}
	return out
}

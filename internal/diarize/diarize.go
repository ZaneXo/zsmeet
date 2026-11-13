// Package diarize 是区分说话人的算法部分，照着 3D-Speaker（阿里达摩院开源的说话人工具包，
// speakerlab/bin/infer_diarization.py 和 speakerlab/process/cluster.py）的做法：
//
//  1. 把有人说话的地方切成 1.5 秒长、每隔 0.75 秒一段的小段（Windows）；
//  2. 调用方给每个小段算一个声纹（asr 包用 CAM++ 模型算）；
//  3. 谱聚类：相似度矩阵每行只留最像的少数几个，算拉普拉斯矩阵最小的几个特征向量，再用 k-means 分组；
//     不知道有几个人时，按特征值之间最大的间隔估计人数（Cluster）；
//  4. 很小的类（零碎）归到最像的大类；自动估计人数时，声音很像的两个人再并成一个；
//  5. 相邻的小段接成每个人说话的时间段，两个人的小段重叠的地方从中间分开；
//     太短、没切出小段的话归到时间上最近的小段的人（Turns）。
//
// 这里只有不依赖模型的计算，方便测试。
package diarize

import (
	"cmp"
	"maps"
	"math"
	"math/rand/v2"
	"slices"

	"gonum.org/v1/gonum/mat"

	"github.com/ZaneXo/zsmeet/internal/transcript"
)

// Span 是一段时间，单位秒。
type Span struct{ Start, End float64 }

// 小段的长度和间隔（秒）
const (
	WindowSeconds = 1.5
	stepSeconds   = 0.75
)

// Windows 把有人说话的每一段切成 1.5 秒长、每隔 0.75 秒一段的小段，最后一段到这段话结束（可能短一些）。
// 不到 0.75 秒的话不切：太短算不出可靠的声纹，它们的说话人由转写时挨着的句子决定。
func Windows(speech []Span) []Span {
	var out []Span
	for _, s := range speech {
		for i := 0; ; i++ {
			st := s.Start + float64(i)*stepSeconds
			if st+WindowSeconds >= s.End+stepSeconds {
				break
			}
			out = append(out, Span{st, min(st+WindowSeconds, s.End)})
		}
	}
	return out
}

// 聚类的参数，和 3D-Speaker 的默认值一样
const (
	// 自动区分时最多分出几个人
	maxAutoSpeakers = 15
	// 相似度矩阵每行只留最像的 1.2%（至少 minKeep 个），其余的当成 0：只看最像的几个邻居，噪音和抢话的小段影响不到别人
	keepRatio = 0.012
	minKeep   = 6
	// 不超过这么多小段（约 3 秒）的类是零碎，归到最像的大类
	minClusterSize = 4
	// 自动区分时，平均声纹的相似度到这么高的两个人并成一个
	mergeCos = 0.8
	// 小段不到这么多时（说话加起来不到半分钟）用层次聚类：谱聚类在点很少时不稳
	shortLine = 40
	// 层次聚类的阈值：两组之间的平均相似度不低于它就并
	ahcCos = 0.4
)

// maxSpectral 是谱聚类最多用多少个小段：要算这么大的矩阵的全部特征向量，1000 个约 1 秒，1500 个约 5 秒，
// 2000 个要 10 秒以上。更多时（说话加起来超过约 19 分钟）均匀地挑这么多个来聚类，其余的归到最像的类。变量是为了测试
var maxSpectral = 1500

// Cluster 给每个小段分配说话人编号（从 0 开始，按第一次出现的先后）。emb 是每个小段的声纹，要已经归一化。
// speakers 大于 0 时分成这么多人（零碎太多时可能少几个），0 是自动估计人数。
func Cluster(emb [][]float32, speakers int) []int {
	n := len(emb)
	if n <= 1 {
		return make([]int, n)
	}
	var labels []int
	if n < shortLine {
		labels = ahc(emb, speakers)
	} else {
		labels = spectralSampled(emb, speakers)
	}
	labels = filterMinor(emb, labels)
	if speakers <= 0 {
		labels = mergeByCos(emb, labels)
	}
	return renumber(labels)
}

// spectralSampled 在小段太多时只拿均匀挑出的 maxSpectral 个做谱聚类，其余的归到平均声纹最像的类。
func spectralSampled(emb [][]float32, speakers int) []int {
	n := len(emb)
	if n <= maxSpectral {
		return spectral(emb, speakers)
	}
	picked := make([]int, maxSpectral)
	sample := make([][]float32, maxSpectral)
	for i := range picked {
		picked[i] = i * n / maxSpectral
		sample[i] = emb[picked[i]]
	}
	sampleLabels := spectral(sample, speakers)
	labels := make([]int, n)
	for i := range labels {
		labels[i] = -1
	}
	for i, idx := range picked {
		labels[idx] = sampleLabels[i]
	}
	centers := centroids(emb, labels)
	for i := range labels {
		if labels[i] < 0 {
			labels[i] = nearest(emb[i], centers)
		}
	}
	return labels
}

// spectral 是谱聚类：剪枝后的相似度矩阵 → 拉普拉斯矩阵（不归一化）→ 最小的几个特征向量 → k-means。
func spectral(emb [][]float32, speakers int) []int {
	n := len(emb)
	sim := make([]float64, n*n)
	for i := range n {
		for j := i; j < n; j++ {
			s := dot(emb[i], emb[j])
			sim[i*n+j], sim[j*n+i] = s, s
		}
	}
	// 每行只留最大的 keep 个（3D-Speaker：把最小的 int((1-pval)*n) 个、但至少留 min_pnum 个以外的置 0）
	keep := n - min(int((1-keepRatio)*float64(n)), n-minKeep)
	order := make([]int, n)
	pruned := make([]float64, n*n)
	for i := range n {
		row := sim[i*n : (i+1)*n]
		for j := range order {
			order[j] = j
		}
		// 从大到小；一样大时下标小的在前，保证结果稳定
		slices.SortStableFunc(order, func(a, b int) int { return cmpDesc(row[a], row[b]) })
		for _, j := range order[:keep] {
			pruned[i*n+j] = row[j]
		}
	}
	// 对称化，去掉对角线，拉普拉斯矩阵 L = D - A
	lap := mat.NewSymDense(n, nil)
	for i := range n {
		deg := 0.0
		for j := range n {
			if i == j {
				continue
			}
			a := 0.5 * (pruned[i*n+j] + pruned[j*n+i])
			deg += math.Abs(a)
			if j > i {
				lap.SetSym(i, j, -a)
			}
		}
		lap.SetSym(i, i, deg)
	}
	var eig mat.EigenSym
	if !eig.Factorize(lap, true) {
		// 分解失败（不会发生）：都算一个人
		return make([]int, n)
	}
	values := eig.Values(nil) // 从小到大
	k := speakers
	if k <= 0 {
		k = eigenGapCount(values)
	}
	k = min(k, n)
	var vectors mat.Dense
	eig.VectorsTo(&vectors)
	points := make([][]float64, n)
	for i := range points {
		points[i] = make([]float64, k)
		for c := range k {
			points[i][c] = vectors.At(i, c)
		}
	}
	return kmeans(points, k)
}

// eigenGapCount 按最小的几个特征值之间最大的间隔估计人数（1 到 maxAutoSpeakers）。
func eigenGapCount(values []float64) int {
	top := values[:min(len(values), maxAutoSpeakers+1)]
	best, bestGap := 1, math.Inf(-1)
	for i := 0; i+1 < len(top); i++ {
		if gap := top[i+1] - top[i]; gap > bestGap {
			best, bestGap = i+1, gap
		}
	}
	return best
}

// kmeans 把点分成 k 组：k-means++ 选初始中心，跑 10 次取总距离最小的一次（随机数种子固定，结果可以重现）。
func kmeans(points [][]float64, k int) []int {
	n := len(points)
	labels := make([]int, n)
	if k <= 1 {
		return labels
	}
	if n <= k {
		// 点不比组多：一个点一组
		for i := range labels {
			labels[i] = i
		}
		return labels
	}
	rng := rand.New(rand.NewPCG(1, 2))
	var best []int
	bestCost := math.Inf(1)
	for range 10 {
		labels, cost := kmeansOnce(points, k, rng)
		if cost < bestCost {
			best, bestCost = labels, cost
		}
	}
	return best
}

func kmeansOnce(points [][]float64, k int, rng *rand.Rand) ([]int, float64) {
	n, dim := len(points), len(points[0])
	centers := make([][]float64, 0, k)
	centers = append(centers, slices.Clone(points[rng.IntN(n)]))
	dist := make([]float64, n)
	for len(centers) < k {
		total := 0.0
		for i, p := range points {
			dist[i] = math.Inf(1)
			for _, c := range centers {
				dist[i] = min(dist[i], sqDist(p, c))
			}
			total += dist[i]
		}
		if total == 0 {
			// 剩下的点都和中心重合：随便挑一个
			centers = append(centers, slices.Clone(points[rng.IntN(n)]))
			continue
		}
		r := rng.Float64() * total
		pick := n - 1
		for i, d := range dist {
			if r -= d; r <= 0 {
				pick = i
				break
			}
		}
		centers = append(centers, slices.Clone(points[pick]))
	}

	labels := make([]int, n)
	for iter := range 300 {
		changed := iter == 0
		for i, p := range points {
			best, bestD := 0, math.Inf(1)
			for c, center := range centers {
				if d := sqDist(p, center); d < bestD {
					best, bestD = c, d
				}
			}
			if labels[i] != best {
				labels[i], changed = best, true
			}
		}
		if !changed {
			break
		}
		counts := make([]int, k)
		for c := range centers {
			clear(centers[c])
		}
		for i, p := range points {
			counts[labels[i]]++
			for d := range dim {
				centers[labels[i]][d] += p[d]
			}
		}
		for c := range centers {
			if counts[c] == 0 {
				// 空了的组：拿离自己的中心最远的点当新中心
				far, farD := 0, -1.0
				for i, p := range points {
					if d := sqDist(p, centers[labels[i]]); d > farD {
						far, farD = i, d
					}
				}
				copy(centers[c], points[far])
				continue
			}
			for d := range dim {
				centers[c][d] /= float64(counts[c])
			}
		}
	}
	cost := 0.0
	for i, p := range points {
		cost += sqDist(p, centers[labels[i]])
	}
	return labels, cost
}

// ahc 是层次聚类（平均连接）：反复把平均相似度最高的两组并起来。speakers 大于 0 时并到这么多组，
// 否则并到最像的两组的平均相似度低于 ahcCos 为止。
func ahc(emb [][]float32, speakers int) []int {
	n := len(emb)
	sim := make([][]float64, n)
	for i := range n {
		sim[i] = make([]float64, n)
		for j := range n {
			sim[i][j] = dot(emb[i], emb[j])
		}
	}
	groups := make([][]int, n)
	for i := range groups {
		groups[i] = []int{i}
	}
	average := func(a, b []int) float64 {
		s := 0.0
		for _, i := range a {
			for _, j := range b {
				s += sim[i][j]
			}
		}
		return s / float64(len(a)*len(b))
	}
	for len(groups) > 1 && (speakers <= 0 || len(groups) > speakers) {
		bi, bj, best := 0, 1, math.Inf(-1)
		for i := range groups {
			for j := i + 1; j < len(groups); j++ {
				if s := average(groups[i], groups[j]); s > best {
					bi, bj, best = i, j, s
				}
			}
		}
		if speakers <= 0 && best < ahcCos {
			break
		}
		groups[bi] = append(groups[bi], groups[bj]...)
		groups = slices.Delete(groups, bj, bj+1)
	}
	labels := make([]int, n)
	for g, members := range groups {
		for _, i := range members {
			labels[i] = g
		}
	}
	return labels
}

// filterMinor 把不超过 minClusterSize 个小段的类（零碎）里的每个小段归到平均声纹最像的大类。
// 一个大类都没有时都算一个人。
func filterMinor(emb [][]float32, labels []int) []int {
	size := map[int]int{}
	for _, l := range labels {
		size[l]++
	}
	major := map[int]bool{}
	for l, s := range size {
		if s > minClusterSize {
			major[l] = true
		}
	}
	if len(major) == len(size) {
		return labels
	}
	out := slices.Clone(labels)
	if len(major) == 0 {
		clear(out)
		return out
	}
	majorOnly := slices.Clone(labels)
	for i, l := range majorOnly {
		if !major[l] {
			majorOnly[i] = -1
		}
	}
	centers := centroids(emb, majorOnly)
	for i, l := range labels {
		if !major[l] {
			out[i] = nearest(emb[i], centers)
		}
	}
	return out
}

// mergeByCos 反复把平均声纹最像的两个类并起来，直到最像的两个的相似度低于 mergeCos。
func mergeByCos(emb [][]float32, labels []int) []int {
	out := slices.Clone(labels)
	for {
		centers := centroids(emb, out)
		ids := slices.Sorted(maps.Keys(centers))
		if len(ids) <= 1 {
			return out
		}
		bi, bj, best := -1, -1, math.Inf(-1)
		for x, a := range ids {
			for _, b := range ids[x+1:] {
				if s := cosine(centers[a], centers[b]); s > best {
					bi, bj, best = a, b, s
				}
			}
		}
		if best < mergeCos {
			return out
		}
		for i, l := range out {
			if l == bj {
				out[i] = bi
			}
		}
	}
}

// centroids 是每个类（labels 里小于 0 的不算）的声纹的平均。
func centroids(emb [][]float32, labels []int) map[int][]float64 {
	sums := map[int][]float64{}
	for i, l := range labels {
		if l < 0 {
			continue
		}
		s, ok := sums[l]
		if !ok {
			s = make([]float64, len(emb[i]))
			sums[l] = s
		}
		for d, x := range emb[i] {
			s[d] += float64(x)
		}
	}
	return sums
}

// nearest 是和 v 余弦相似度最高的中心的编号。一样高时编号小的优先，保证结果稳定。
func nearest(v []float32, centers map[int][]float64) int {
	best, bestSim := -1, math.Inf(-1)
	for _, id := range slices.Sorted(maps.Keys(centers)) {
		if s := cosine32(v, centers[id]); s > bestSim {
			best, bestSim = id, s
		}
	}
	return best
}

// renumber 把编号换成 0、1、2…，按第一次出现的先后。
func renumber(labels []int) []int {
	ids := map[int]int{}
	out := make([]int, len(labels))
	for i, l := range labels {
		id, ok := ids[l]
		if !ok {
			id = len(ids)
			ids[l] = id
		}
		out[i] = id
	}
	return out
}

// Turns 整理出每个人说话的时间段。windows 是按时间排好的小段，labels 是它们的说话人，speech 是切小段之前
// 有人说话的片段（也按时间排好）：同一个人相邻（重叠或首尾相接）的小段接起来，换人时两个小段重叠的部分从中间分开；
// 没有小段的片段（太短，或者声纹没算出来）整段归到时间上最近的小段的说话人，免得这几句话没有说话人。
func Turns(speech, windows []Span, labels []int) []transcript.Turn {
	out := compress(windows, labels)
	if len(windows) == 0 {
		return out
	}
	j := 0
	for _, s := range speech {
		for j < len(windows) && windows[j].End <= s.Start {
			j++
		}
		if j < len(windows) && windows[j].Start < s.End {
			continue
		}
		// 前后各有一个最近的小段，取离得近的；一样近时取前面的
		best, gap := -1, math.Inf(1)
		if j > 0 {
			best, gap = j-1, s.Start-windows[j-1].End
		}
		if j < len(windows) && windows[j].Start-s.End < gap {
			best = j
		}
		out = append(out, transcript.Turn{Start: s.Start, End: s.End, Speaker: labels[best]})
	}
	slices.SortStableFunc(out, func(a, b transcript.Turn) int { return cmp.Compare(a.Start, b.Start) })
	return out
}

// compress 把小段接成时间段：同一个人相邻的接起来，换人时重叠的部分从中间分开（3D-Speaker 的 compressed_seg）。
func compress(windows []Span, labels []int) []transcript.Turn {
	var out []transcript.Turn
	for i, w := range windows {
		st, ed, speaker := w.Start, w.End, labels[i]
		if len(out) == 0 {
			out = append(out, transcript.Turn{Start: st, End: ed, Speaker: speaker})
			continue
		}
		last := &out[len(out)-1]
		if speaker == last.Speaker {
			if st > last.End {
				out = append(out, transcript.Turn{Start: st, End: ed, Speaker: speaker})
			} else {
				last.End = max(last.End, ed)
			}
			continue
		}
		if st < last.End {
			middle := (last.End + st) / 2
			last.End, st = middle, middle
		}
		out = append(out, transcript.Turn{Start: st, End: ed, Speaker: speaker})
	}
	return out
}

func dot(a, b []float32) float64 {
	s := 0.0
	for i, x := range a {
		s += float64(x) * float64(b[i])
	}
	return s
}

func cosine(a, b []float64) float64 {
	var ab, aa, bb float64
	for i, x := range a {
		ab += x * b[i]
		aa += x * x
		bb += b[i] * b[i]
	}
	if aa == 0 || bb == 0 {
		return 0
	}
	return ab / math.Sqrt(aa*bb)
}

func cosine32(a []float32, b []float64) float64 {
	var ab, aa, bb float64
	for i, x := range a {
		ab += float64(x) * b[i]
		aa += float64(x) * float64(x)
		bb += b[i] * b[i]
	}
	if aa == 0 || bb == 0 {
		return 0
	}
	return ab / math.Sqrt(aa*bb)
}

func sqDist(a, b []float64) float64 {
	s := 0.0
	for i, x := range a {
		d := x - b[i]
		s += d * d
	}
	return s
}

func cmpDesc(a, b float64) int {
	switch {
	case a > b:
		return -1
	case a < b:
		return 1
	}
	return 0
}

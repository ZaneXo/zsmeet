package diarize

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/ZaneXo/zsmeet/internal/transcript"
)

// 1.5 秒一段、每隔 0.75 秒；最后一段到这段话结束；不到 0.75 秒的话不切。
func TestWindows(t *testing.T) {
	got := Windows([]Span{{0, 0.7}, {1, 1.8}, {10, 13}, {20, 23.2}})
	want := []Span{
		{1, 1.8},
		{10, 11.5}, {10.75, 12.25}, {11.5, 13},
		{20, 21.5}, {20.75, 22.25}, {21.5, 23}, {22.25, 23.2},
	}
	if !slices.Equal(got, want) {
		t.Errorf("切成了 %v，应该是 %v", got, want)
	}
}

// 同一个人相邻的小段接起来；换人时重叠的部分从中间分开；中间隔着没人说话的地方另起一段；
// 太短、没有小段的话归到时间上最近的小段的人。
func TestTurns(t *testing.T) {
	speech := []Span{{0, 3.75}, {4, 4.5}, {5, 7}, {9, 9.6}}
	windows := []Span{{0, 1.5}, {0.75, 2.25}, {1.5, 3}, {2.25, 3.75}, {5, 6.5}, {5.75, 7}}
	labels := []int{0, 0, 1, 1, 1, 0}
	got := Turns(speech, windows, labels)
	want := []transcript.Turn{
		{Start: 0, End: 1.875, Speaker: 0},
		{Start: 1.875, End: 3.75, Speaker: 1},
		{Start: 4, End: 4.5, Speaker: 1}, // 离前面的近（0.25 秒，后面的 0.5 秒）
		{Start: 5, End: 6.125, Speaker: 1},
		{Start: 6.125, End: 7, Speaker: 0},
		{Start: 9, End: 9.6, Speaker: 0}, // 后面没有小段了，归到前面最近的
	}
	if !slices.Equal(got, want) {
		t.Errorf("整理成了 %v，应该是 %v", got, want)
	}
}

// voices 造一段对话的声纹：speakers 个人轮流说，每人每次 turn 个小段；声纹是这个人的中心加上噪音。
// 同一个人的小段之间相似度约 0.7，不同的人之间约 0。返回声纹和每个小段实际是谁。
func voices(speakers, rounds, turn int, seed uint64) ([][]float32, []int) {
	rng := rand.New(rand.NewPCG(seed, 7))
	const dim = 64
	centers := make([][]float64, speakers)
	for s := range centers {
		centers[s] = make([]float64, dim)
		for d := range dim {
			centers[s][d] = rng.NormFloat64()
		}
		normalize(centers[s])
	}
	var emb [][]float32
	var truth []int
	for range rounds {
		for s := range speakers {
			for range turn {
				v := make([]float64, dim)
				for d := range dim {
					v[d] = centers[s][d] + 0.08*rng.NormFloat64()
				}
				normalize(v)
				f := make([]float32, dim)
				for d, x := range v {
					f[d] = float32(x)
				}
				emb = append(emb, f)
				truth = append(truth, s)
			}
		}
	}
	return emb, truth
}

func normalize(v []float64) {
	n := 0.0
	for _, x := range v {
		n += x * x
	}
	n = math.Sqrt(n)
	for i := range v {
		v[i] /= n
	}
}

// agree 检查分出的组和实际的人一一对应（编号可以不同），返回分对的比例。
func agree(labels, truth []int) float64 {
	pairs := map[[2]int]int{}
	for i := range labels {
		pairs[[2]int{labels[i], truth[i]}]++
	}
	// 每个分出的组对应它里面最多的那个人
	best := map[int]int{}
	count := map[int]int{}
	for p, c := range pairs {
		if c > count[p[0]] {
			best[p[0]], count[p[0]] = p[1], c
		}
	}
	right := 0
	for i := range labels {
		if best[labels[i]] == truth[i] {
			right++
		}
	}
	return float64(right) / float64(len(labels))
}

func distinct(labels []int) int {
	seen := map[int]bool{}
	for _, l := range labels {
		seen[l] = true
	}
	return len(seen)
}

// 谱聚类（小段多）：不知道人数时估得出有几个人，知道人数时按人数分，分得都对。
func TestClusterSpectral(t *testing.T) {
	emb, truth := voices(5, 6, 8, 1) // 240 个小段
	for _, speakers := range []int{0, 5} {
		labels := Cluster(emb, speakers)
		if n := distinct(labels); n != 5 {
			t.Errorf("speakers=%d：应该分出 5 个人，分出了 %d 个", speakers, n)
		}
		if a := agree(labels, truth); a < 0.99 {
			t.Errorf("speakers=%d：只分对了 %.0f%%", speakers, 100*a)
		}
	}
	// 要求的人比实际的少：并成要求的人数
	if n := distinct(Cluster(emb, 3)); n != 3 {
		t.Errorf("要求分成 3 个人，分出了 %d 个", n)
	}
}

// 小段太多时只拿一部分做谱聚类，其余的归到最像的类，结果一样对。
func TestClusterSampled(t *testing.T) {
	old := maxSpectral
	maxSpectral = 60
	defer func() { maxSpectral = old }()
	emb, truth := voices(4, 5, 10, 2) // 200 个小段
	labels := Cluster(emb, 0)
	if n := distinct(labels); n != 4 {
		t.Errorf("应该分出 4 个人，分出了 %d 个", n)
	}
	if a := agree(labels, truth); a < 0.99 {
		t.Errorf("只分对了 %.0f%%", 100*a)
	}
}

// 小段很少（录音很短）时用层次聚类，按相似度的阈值或者按人数分。
func TestClusterShort(t *testing.T) {
	emb, truth := voices(3, 2, 5, 3) // 30 个小段
	for _, speakers := range []int{0, 3} {
		labels := Cluster(emb, speakers)
		if n := distinct(labels); n != 3 {
			t.Errorf("speakers=%d：应该分出 3 个人，分出了 %d 个", speakers, n)
		}
		if a := agree(labels, truth); a < 0.99 {
			t.Errorf("speakers=%d：只分对了 %.0f%%", speakers, 100*a)
		}
	}
}

// 零碎（几个小段的抢话、笑声）不算一个人，归到最像的人；声音很像的两个人在自动区分时并成一个。
func TestClusterMinorAndMerge(t *testing.T) {
	emb, truth := voices(3, 6, 8, 4)
	// 加 3 个和谁都不像的小段
	odd, _ := voices(1, 1, 3, 99)
	emb = append(emb, odd...)
	labels := Cluster(emb, 0)
	if n := distinct(labels); n != 3 {
		t.Errorf("零碎不该算一个人：分出了 %d 个", n)
	}
	if a := agree(labels[:len(truth)], truth); a < 0.99 {
		t.Errorf("只分对了 %.0f%%", 100*a)
	}

	// 同一个人的声纹分成两半，两半之间很像（大于 0.8）：自动区分时并成一个
	labels = mergeByCos(emb[:len(truth)], func() []int {
		l := slices.Clone(truth)
		for i := range l {
			if l[i] == 0 && i%2 == 0 {
				l[i] = 5
			}
		}
		return l
	}())
	if n := distinct(labels); n != 3 {
		t.Errorf("很像的两组应该并成一个：%d 个", n)
	}
}

// 只有一个小段、或者没有：都算一个人。
func TestClusterTiny(t *testing.T) {
	if got := Cluster(nil, 0); len(got) != 0 {
		t.Errorf("没有小段：%v", got)
	}
	if got := Cluster([][]float32{{1, 0}}, 3); !slices.Equal(got, []int{0}) {
		t.Errorf("一个小段：%v", got)
	}
}

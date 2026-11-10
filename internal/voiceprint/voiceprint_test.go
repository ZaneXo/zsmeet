package voiceprint

import (
	"maps"
	"math"
	"testing"
)

// 张敏的声纹朝 x 轴，王悦的朝 z 轴。at 返回在 x、y 平面里、和 x 轴的相似度正好是 sim 的单位向量（和王悦的是 0）。
var x, z = []float32{1, 0, 0}, []float32{0, 0, 1}

func at(sim float64) []float32 {
	return []float32{float32(sim), float32(math.Sqrt(1 - sim*sim)), 0}
}

func TestNormalizeAndMean(t *testing.T) {
	n := Normalize([]float32{3, 4})
	if math.Abs(float64(n[0])-0.6) > 1e-6 || math.Abs(float64(n[1])-0.8) > 1e-6 {
		t.Errorf("归一化不对：%v", n)
	}
	if Normalize([]float32{0, 0}) != nil || Normalize(nil) != nil {
		t.Error("全零的向量没有方向")
	}
	if Normalize([]float32{float32(math.NaN()), 1}) != nil {
		t.Error("有 NaN 的向量不能用")
	}
	m := Mean([][]float32{{1, 0}, {0, 1}, {1, 2, 3}})
	if math.Abs(Cosine(m, Normalize([]float32{1, 1}))-1) > 1e-6 {
		t.Errorf("平均方向不对（维数不一样的样本不算）：%v", m)
	}
	if Mean(nil) != nil || Mean([][]float32{{1, 0}, {-1, 0}}) != nil {
		t.Error("没有样本、或者互相抵消时没有声纹")
	}
	if Cosine([]float32{1, 0}, []float32{1, 0, 0}) != 0 {
		t.Error("维数不一样时相似度是 0")
	}
}

func TestMatch(t *testing.T) {
	prints := []Print{{Name: "张敏", Vector: x}, {Name: "王悦", Vector: z}}

	tests := []struct {
		name   string
		voices map[int][]float32
		named  map[int]string
		want   map[int]string
	}{
		{
			name:   "达到阈值才认",
			voices: map[int][]float32{0: at(Threshold + 0.01), 1: at(Threshold - 0.01)},
			want:   map[int]string{0: "张敏"},
		},
		{
			name:   "一个声纹在一场会议里只配最像的那位",
			voices: map[int][]float32{0: at(0.8), 1: at(0.95), 2: at(0.9)},
			want:   map[int]string{1: "张敏"},
		},
		{
			name:   "像两个人时取最像的",
			voices: map[int][]float32{0: Normalize([]float32{0.8, 0, 0.9})},
			want:   map[int]string{0: "王悦"},
		},
		{
			name:   "已经有名字的说话人不动",
			voices: map[int][]float32{0: at(0.99), 1: at(0.9)},
			named:  map[int]string{0: "李航"},
			want:   map[int]string{1: "张敏"},
		},
		{
			name:   "会议里已经有人叫这个名字，这个声纹就不再用",
			voices: map[int][]float32{0: at(0.99)},
			named:  map[int]string{3: "张敏"},
			want:   map[int]string{},
		},
		{
			name:   "两个声纹各配各的",
			voices: map[int][]float32{0: at(0.9), 1: z},
			want:   map[int]string{0: "张敏", 1: "王悦"},
		},
		{
			name:   "维数不一样的不配",
			voices: map[int][]float32{0: {1, 0}},
			want:   map[int]string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Match(tt.voices, prints, tt.named); !maps.Equal(got, tt.want) {
				t.Errorf("得到 %v，应该是 %v", got, tt.want)
			}
		})
	}

	upper := []Print{{Name: "Zhang Min", Vector: x}}
	if got := Match(map[int][]float32{0: x, 1: x}, upper, map[int]string{2: "zhang min"}); len(got) != 0 {
		t.Errorf("名字不分大小写：%v", got)
	}
}

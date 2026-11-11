package service

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ZaneXo/zsmeet/internal/ask"
)

func TestRateLimit(t *testing.T) {
	r := newRateLimit(2, time.Minute)
	a, b := uuid.New(), uuid.New()
	t0 := time.Now()
	if r.take(a, t0) != 0 || r.take(a, t0.Add(10*time.Second)) != 0 {
		t.Fatal("前两次不限")
	}
	if wait := r.take(a, t0.Add(20*time.Second)); wait != 40*time.Second {
		t.Errorf("第三次要等到第一次满一分钟，还要 40 秒，实际 %v", wait)
	}
	if r.take(b, t0.Add(20*time.Second)) != 0 {
		t.Error("别人不受影响")
	}
	if r.take(a, t0.Add(61*time.Second)) != 0 {
		t.Error("第一次过了一分钟以后又能问了")
	}
	r.take(b, t0.Add(10*time.Minute))
	if len(r.hits) != 1 {
		t.Errorf("过期的记录要清掉：%v", r.hits)
	}
}

func TestWindows(t *testing.T) {
	lines := make([]ask.Line, 10)
	for i := range lines {
		lines[i] = ask.Line{Start: float64(i)}
	}
	starts := func(ws [][]ask.Line) string {
		var out [][]float64
		for _, w := range ws {
			var s []float64
			for _, l := range w {
				s = append(s, l.Start)
			}
			out = append(out, s)
		}
		return fmt.Sprint(out)
	}
	if got := starts(windows(lines, []int{5, 0, 2, 9, 5}, 1)); got != "[[0 1 2 3 4 5 6] [8 9]]" {
		t.Errorf("挨着或重叠的并成一段，按时间排：%s", got)
	}
	if got := windows(lines, nil, 1); got != nil {
		t.Errorf("没有命中就没有片段：%v", got)
	}
}

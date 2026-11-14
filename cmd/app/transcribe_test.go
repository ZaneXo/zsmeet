package main

import (
	"flag"
	"io"
	"slices"
	"testing"

	"github.com/ZaneXo/zsmeet/internal/transcript"
)

// 有字的时长：几句重叠的部分只算一次；空档包括开头和结尾，只算比 minHole 长的。
func TestCover(t *testing.T) {
	segs := []transcript.Segment{
		{Start: 25, End: 30},
		{Start: 12, End: 14},
		{Start: 13, End: 16}, // 和上一句重叠 1 秒
		{Start: 50, End: 52},
	}
	c := cover(segs, 70, 10)
	if c.covered != 11 {
		t.Errorf("有字的时长应该是 11 秒，得到 %v", c.covered)
	}
	want := [][2]float64{{0, 12}, {30, 50}, {52, 70}}
	if !slices.Equal(c.holes, want) {
		t.Errorf("空档应该是 %v，得到 %v", want, c.holes)
	}
	if c := cover(nil, 8, 10); c.covered != 0 || len(c.holes) != 0 {
		t.Errorf("8 秒一句话都没有，不到 10 秒不算空档：%+v", c)
	}
}

// 选项写在文件名前面、后面都行；只能有一个文件。
func TestParseFileArgs(t *testing.T) {
	parse := func(args ...string) (string, float64, error) {
		fs := flag.NewFlagSet("transcribe", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		hole := fs.Float64("hole", 10, "")
		file, err := parseFileArgs(fs, args)
		return file, *hole, err
	}
	for _, args := range [][]string{{"-hole", "5", "a.m4a"}, {"a.m4a", "-hole", "5"}} {
		if file, hole, err := parse(args...); err != nil || file != "a.m4a" || hole != 5 {
			t.Errorf("%v：文件 %q，-hole %v，错误 %v", args, file, hole, err)
		}
	}
	if _, _, err := parse("a.m4a", "b.m4a"); err == nil {
		t.Error("两个文件应该报错")
	}
	if _, _, err := parse("-hole", "5"); err == nil {
		t.Error("没有文件应该报错")
	}
}

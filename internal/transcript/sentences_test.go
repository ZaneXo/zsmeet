package transcript

import (
	"strings"
	"testing"
)

// toks 把「字@秒」写法的字符串变成 Token，方便写测试。
func toks(spec string) []Token {
	var out []Token
	for _, f := range strings.Fields(spec) {
		text, at, _ := strings.Cut(f, "@")
		var sec float64
		for _, c := range at {
			if c == '.' {
				continue
			}
			sec = sec*10 + float64(c-'0')
		}
		if dot := strings.IndexByte(at, '.'); dot >= 0 {
			for range len(at) - dot - 1 {
				sec /= 10
			}
		}
		out = append(out, Token{Text: text, Start: sec})
	}
	return out
}

func TestSentencesSplitAtFullStops(t *testing.T) {
	// SenseVoice 在一段 10.69 秒开始的录音上真实给出的字和时间
	tokens := toks("飞@0.06 书@0.24 集@0.54 成@0.66 放@1.02 到@1.2 下@1.38 一@1.62 个@1.8 版@1.98 本@2.1 ，@2.4 " +
		"由@2.64 王@2.88 悦@3.18 负@3.48 责@3.6 调@3.84 研@4.02 。@4.26 第@4.62 三@4.74 件@5.04 事@5.22 。@5.58")
	got := Sentences(tokens, 10.69, 16.34)
	if len(got) != 2 {
		t.Fatalf("应该切成两句：%+v", got)
	}
	first, second := got[0], got[1]
	if first.Text != "飞书集成放到下一个版本，由王悦负责调研。" || !near(first.Start, 10.75) || !near(first.End, 15.31) {
		t.Errorf("第一句不对：%+v", first)
	}
	if second.Text != "第三件事。" || !near(second.Start, 15.31) || !near(second.End, 16.34) || second.Speaker != -1 {
		t.Errorf("第二句不对：%+v", second)
	}
}

func TestSentencesEnglishAndNumbers(t *testing.T) {
	// sherpa-onnx 真实给出的样子：单词开头是空格，数字前面是一个单独的空格（词表里没有「▁3」）
	tokens := []Token{
		{"V", 0}, {"ers", 0.1}, {"ion", 0.2}, {" ", 0.3}, {"3", 0.4}, {".", 0.5}, {"5", 0.6}, {" is", 0.8}, {" out", 1}, {".", 1.2},
		{" We", 1.5}, {" have", 1.7}, {" ", 1.9}, {"2", 2}, {"0", 2.1}, {" people", 2.3}, {".", 2.6},
		{" Great", 3}, {"!", 3.3},
	}
	got := Sentences(tokens, 0, 4)
	want := []string{"Version 3.5 is out.", "We have 20 people.", "Great!"}
	if len(got) != len(want) {
		t.Fatalf("英文的句号要断句，数字里的点号不断：%+v", got)
	}
	for i, s := range got {
		if s.Text != want[i] {
			t.Errorf("第 %d 句：得到 %q，想要 %q", i+1, s.Text, want[i])
		}
	}
	if !near(got[1].Start, 1.5) || !near(got[1].End, 3) {
		t.Errorf("第二句的时间不对：%+v", got[1])
	}

	// 词表里的 ▁ 没被换成空格时也一样
	got = Sentences(toks("▁version@0 ▁@0.3 3@0.4 .@0.5 5@0.6 ▁is@0.8 ▁out@1 .@1.2 ▁great@1.5 !@1.9"), 0, 2.5)
	if len(got) != 2 || got[0].Text != "version 3.5 is out." || got[1].Text != "great!" {
		t.Errorf("▁ 开头的写法：%+v", got)
	}
}

func TestSentencesEdgeCases(t *testing.T) {
	if got := Sentences(nil, 0, 1); len(got) != 0 {
		t.Errorf("没有字就没有句子：%+v", got)
	}
	// 开头的标点不算一句的开始；没有句号结尾的最后一句也要留下
	got := Sentences(toks("，@0 好@0.2 的@0.4"), 3, 4)
	if len(got) != 1 || got[0].Text != "好的" || !near(got[0].Start, 3.2) || !near(got[0].End, 4) {
		t.Errorf("开头的标点要跳过：%+v", got)
	}
	// 一直不说句号：攒够长度后在逗号处断开
	var long []Token
	for i := range 60 {
		long = append(long, Token{Text: "字", Start: float64(i) / 10})
		if i == 54 {
			long = append(long, Token{Text: "，", Start: 5.45})
		}
	}
	got = Sentences(long, 0, 10)
	if len(got) != 2 || !strings.HasSuffix(got[0].Text, "，") || !near(got[1].Start, 5.5) {
		t.Errorf("太长的句子要在逗号处断开：%+v", got)
	}
}

func near(a, b float64) bool { return a-b < 0.001 && b-a < 0.001 }

package search

import (
	"slices"
	"testing"
)

func TestDocTerms(t *testing.T) {
	got := DocTerms("产品周会 River v2")
	want := []string{"产", "产品", "品", "品周", "周", "周会", "会", "river", "v2"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestQueryTerms(t *testing.T) {
	cases := map[string][]string{
		"下单流程":         {"下单", "单流", "流程"},
		"会":            {"会"},
		"  Docker 部署 ": {"docker", "部署"},
		"，。！":          {},
		"":             {},
	}
	for q, want := range cases {
		if got := QueryTerms(q); !slices.Equal(got, want) {
			t.Errorf("QueryTerms(%q) = %q, want %q", q, got, want)
		}
	}
}

// 查询词必须都出现在文档词里，否则搜不到
func TestQueryMatchesDoc(t *testing.T) {
	doc := DocTerms("一期只做 Web 端，桌面录音器放到二期")
	for _, q := range []string{"录音器", "web", "二期", "做"} {
		for _, term := range QueryTerms(q) {
			if !slices.Contains(doc, term) {
				t.Errorf("查询 %q 的词 %q 不在文档词里", q, term)
			}
		}
	}
}

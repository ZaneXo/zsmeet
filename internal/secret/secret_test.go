package secret

import (
	"errors"
	"strings"
	"testing"
)

func TestSealOpen(t *testing.T) {
	box, err := New("a-master-key-for-tests")
	if err != nil {
		t.Fatal(err)
	}
	sealed := box.Seal("sk-123")
	if sealed == "sk-123" || sealed == "" {
		t.Fatalf("没有加密：%q", sealed)
	}
	if got, err := box.Open(sealed); err != nil || got != "sk-123" {
		t.Fatalf("解不回原文：%q %v", got, err)
	}
	if box.Seal("") != "" {
		t.Fatal("空字符串应该原样返回")
	}

	other, _ := New("another-master-key")
	if _, err := other.Open(sealed); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("换了主密钥应该解不开，得到 %v", err)
	}
}

// 之前版本存进数据库的凭证要一直解得开。这段密文是用主密钥 "sample-master-key" 加密 "sk-sample-000" 得到的，
// 密钥派生方式（derivePrefix）一改，这个测试就会失败。
func TestOpensEarlierCiphertext(t *testing.T) {
	box, err := New("sample-master-key")
	if err != nil {
		t.Fatal(err)
	}
	got, err := box.Open("uVREWxlZkr5kcZ9RQ2xJ7Bpe+Tu0GMwxcCs37PszZl7XxkzbQWRJSfA=")
	if err != nil || got != "sk-sample-000" {
		t.Fatalf("解不开之前版本加密的凭证，密钥派生方式被改过了：%q %v", got, err)
	}
}

// SealFor 的东西只能用同样的用途解开；被改过一个字、换了主密钥、拿数据库里的凭证来冒充都不行。
func TestSealFor(t *testing.T) {
	box, _ := New("a-master-key-for-tests")
	sealed := box.SealFor("feishu-oauth", `{"s":"abc"}`)
	if strings.ContainsAny(sealed, "+/=") {
		t.Errorf("要能直接放进 Cookie 和地址：%q", sealed)
	}
	if got, err := box.OpenFor("feishu-oauth", sealed); err != nil || got != `{"s":"abc"}` {
		t.Fatalf("解不回原文：%q %v", got, err)
	}
	if _, err := box.OpenFor("other", sealed); !errors.Is(err, ErrTampered) {
		t.Errorf("用途不同不能解开：%v", err)
	}
	b := []byte(sealed)
	b[len(b)/2] ^= 1
	if _, err := box.OpenFor("feishu-oauth", string(b)); !errors.Is(err, ErrTampered) {
		t.Errorf("改过的不能解开：%v", err)
	}
	other, _ := New("another-master-key")
	if _, err := other.OpenFor("feishu-oauth", sealed); !errors.Is(err, ErrTampered) {
		t.Errorf("换了主密钥不能解开：%v", err)
	}
	for _, bad := range []string{"", "x", box.Seal("sk-123")} {
		if _, err := box.OpenFor("feishu-oauth", bad); !errors.Is(err, ErrTampered) {
			t.Errorf("%q 不能解开：%v", bad, err)
		}
	}
}

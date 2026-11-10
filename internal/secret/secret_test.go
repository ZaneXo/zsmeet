package secret

import (
	"errors"
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

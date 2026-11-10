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

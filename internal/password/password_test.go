package password

import "testing"

func TestHashAndVerify(t *testing.T) {
	h := Hash("correct horse")
	if ok, err := Verify("correct horse", h); err != nil || !ok {
		t.Fatalf("正确的密码没通过：ok=%v err=%v", ok, err)
	}
	if ok, err := Verify("wrong", h); err != nil || ok {
		t.Fatalf("错误的密码通过了：ok=%v err=%v", ok, err)
	}
	if Hash("correct horse") == h {
		t.Fatal("两次哈希结果相同，盐没有生效")
	}
	if _, err := Verify("x", "not-a-hash"); err == nil {
		t.Fatal("格式不对的哈希应该报错")
	}
}

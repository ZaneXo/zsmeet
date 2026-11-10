package password

import (
	"testing"
	"time"
)

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

// 名额占满时，新的哈希要排队，有名额空出来以后才开始算。
func TestHashWaitsForSlot(t *testing.T) {
	for range cap(slots) {
		slots <- struct{}{}
	}
	done := make(chan string)
	go func() { done <- Hash("correct horse") }()
	select {
	case <-done:
		t.Fatal("名额占满了还在算哈希")
	case <-time.After(200 * time.Millisecond):
	}
	for range cap(slots) {
		<-slots
	}
	if ok, err := Verify("correct horse", <-done); err != nil || !ok {
		t.Fatalf("排队算出来的哈希不对：ok=%v err=%v", ok, err)
	}
}

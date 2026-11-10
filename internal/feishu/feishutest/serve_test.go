package feishutest

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// TestServeForBrowser 不是普通的测试：设置了 FEISHU_MOCK_ADDR 时，在这个地址上一直开着假的飞书开放平台，
// 用来在浏览器里把飞书的功能走一遍（部署好的 zsmeet 用 ZSMEET_FEISHU_BASE_URL 指向它）。没设时直接跳过。
//
//	FEISHU_MOCK_ADDR=0.0.0.0:18091 FEISHU_MOCK_URL=http://host.docker.internal:18091 \
//	FEISHU_MOCK_EMAIL=admin@example.com FEISHU_MOCK_MEDIA=/path/to/talk.wav \
//	go test -run TestServeForBrowser -timeout 0 -v ./internal/feishu/feishutest
//
// 假应用的凭证是 AppID / AppSecret。机器人在「产品组」「研发群」两个群里；飞书会议 demo-1 有云录制，
// 内容是 FEISHU_MOCK_MEDIA 这个文件，组织者 ou_demo 的邮箱是 FEISHU_MOCK_EMAIL。
// 用飞书登录时，授权页上登录着的也是 ou_demo，企业邮箱是 FEISHU_MOCK_EMAIL（浏览器要能打开 FEISHU_MOCK_URL）。
func TestServeForBrowser(t *testing.T) {
	addr := os.Getenv("FEISHU_MOCK_ADDR")
	if addr == "" {
		t.Skip("没设 FEISHU_MOCK_ADDR")
	}
	s := &Server{
		t:          t,
		Folders:    map[string]map[string]string{},
		Recordings: map[string]*Recording{"demo-1": {MinuteToken: "obcndemominute0000000001", Processing: 1}},
		Media:      map[string]*Media{},
		Users:      map[string]User{"ou_demo": {Name: "演示用户", Email: os.Getenv("FEISHU_MOCK_EMAIL")}},
		FailChats:  map[string]int{},
		Calls:      map[string]int{},
		tasks:      map[string]*Import{},
		grants:     map[string]*grant{},
		userTokens: map[string]userGrant{},
		// 用飞书登录时，授权页上登录着的是演示用户
		LoginAs: &LoginUser{OpenID: "ou_demo", UnionID: "on_demo", Name: "演示用户", EnterpriseEmail: os.Getenv("FEISHU_MOCK_EMAIL")},
		Chats:   [][2]string{{"oc_demo_team", "产品组"}, {"oc_demo_dev", "研发群"}},
	}
	if path := os.Getenv("FEISHU_MOCK_MEDIA"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		s.Media["obcndemominute0000000001"] = &Media{Data: data, NotReady: 1}
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	s.Server = &httptest.Server{Listener: ln, Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Logf("%s %s", r.Method, r.URL.Path)
		s.serve(w, r)
	})}}
	s.Start()
	if u := os.Getenv("FEISHU_MOCK_URL"); u != "" {
		s.URL = u // 下载地址要用容器里访问得到的地址
	}
	t.Logf("假飞书开着：%s", s.URL)

	// 每隔几秒报一下收到的卡片、文档和权限
	seen := [3]int{}
	for range time.Tick(3 * time.Second) {
		s.mu.Lock()
		for _, m := range s.Messages[seen[0]:] {
			t.Logf("发到群 %s 的卡片：%s", m.ChatID, m.Content)
		}
		for _, d := range s.Imports[seen[1]:] {
			t.Logf("导入的文档 %s（文件夹 %s）：\n%s", d.Name, d.Folder, d.Markdown)
		}
		for _, g := range s.Grants[seen[2]:] {
			t.Logf("文档权限：%+v", g)
		}
		seen = [3]int{len(s.Messages), len(s.Imports), len(s.Grants)}
		s.mu.Unlock()
	}
}

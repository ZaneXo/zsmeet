package settings

import (
	"testing"

	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/secret"
	"github.com/ZaneXo/zsmeet/internal/testdb"
)

// 保存飞书配置时把老版本存在飞书配置里的访问地址搬到通用设置里：读完通用设置（还空着）之后、
// 搬过去之前，有人在「设置 → 群机器人」里填了新地址的话，不能被旧地址盖掉。
func TestSaveFeishuKeepsConcurrentSiteURL(t *testing.T) {
	pool := testdb.Open(t, "settings")
	box, err := secret.New("test-secret-key-0123456789")
	if err != nil {
		t.Fatal(err)
	}
	s := New(sqlc.New(pool), box)
	ctx := t.Context()
	if _, err := pool.Exec(ctx, `INSERT INTO settings (key, value) VALUES ('feishu', '{"domain":"feishu","app_id":"cli_x","site_url":"https://old.example.com/"}')`); err != nil {
		t.Fatal(err)
	}

	// 没人同时改：旧地址搬过去（去掉结尾的斜杠）
	if err := s.SaveFeishu(ctx, Feishu{Domain: "feishu", AppID: "cli_x"}); err != nil {
		t.Fatal(err)
	}
	if site, err := s.Site(ctx); err != nil || site.URL != "https://old.example.com" {
		t.Fatalf("旧地址搬到通用设置里：%+v %v", site, err)
	}

	// 恢复成老版本的样子，这次在读完通用设置之后有人填了新地址
	for _, stmt := range []string{
		"DELETE FROM settings WHERE key = 'site'",
		`UPDATE settings SET value = jsonb_set(value, '{site_url}', '"https://old.example.com"') WHERE key = 'feishu'`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	testHookAfterSiteRead = func() {
		if err := s.SaveSite(ctx, Site{URL: "https://new.example.com"}); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { testHookAfterSiteRead = func() {} })
	if err := s.SaveFeishu(ctx, Feishu{Domain: "feishu", AppID: "cli_x"}); err != nil {
		t.Fatal(err)
	}
	if site, err := s.Site(ctx); err != nil || site.URL != "https://new.example.com" {
		t.Errorf("同时填的新地址不能被旧地址盖掉：%+v %v", site, err)
	}
	if url, err := s.SiteURL(ctx); err != nil || url != "https://new.example.com" {
		t.Errorf("链接用新地址：%q %v", url, err)
	}
}

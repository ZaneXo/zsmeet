package api

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ZaneXo/zsmeet/internal/feishu/feishutest"
	"github.com/ZaneXo/zsmeet/internal/transcript"
)

// feishu 起一个假的飞书开放平台，让服务调它。
func (c *client) feishu() *feishutest.Server {
	c.t.Helper()
	fs := feishutest.New(c.t)
	fs.Chats = [][2]string{{"oc_team", "产品组"}, {"oc_dev", "研发群"}}
	c.svc.FeishuBaseURL = fs.URL
	return fs
}

var feishuApp = obj{"domain": "feishu", "app_id": feishutest.AppID, "app_secret": feishutest.AppSecret}

// 飞书配置：只有管理员能看能改；App Secret 加密保存、不会返回；留空表示沿用；各项填得不对时说清楚。
func TestFeishuSettings(t *testing.T) {
	c := newServer(t)
	c.setup()
	c.feishu()
	member := c.addMember("member@example.com", "李航")
	member.want(member.do("GET", "/api/settings/feishu", nil), 403)
	member.want(member.do("PUT", "/api/settings/feishu", feishuApp), 403)

	r := c.want(c.do("GET", "/api/settings/feishu", nil), 200)
	if r.Body["domain"] != "feishu" || r.Body["app_secret_set"] != false || r.Body["console_url"] != "" {
		t.Errorf("还没配置：%s", r.Raw)
	}
	if s := c.want(c.do("GET", "/api/status", nil), 200); s.Body["feishu_ready"] != false {
		t.Errorf("还没配置飞书：%s", s.Raw)
	}

	// 测试连接：凭证对的话返回机器人和它所在的群；不对的话说清楚
	test := c.want(c.do("POST", "/api/settings/feishu/test", feishuApp), 200)
	if test.Body["bot_name"] != "会议助手" || len(test.Body["chats"].([]any)) != 2 {
		t.Errorf("测试连接：%s", test.Raw)
	}
	bad := c.want(c.do("POST", "/api/settings/feishu/test", obj{"app_id": feishutest.AppID, "app_secret": "wrong"}), 422)
	if !strings.Contains(bad.Raw, "App ID 或 App Secret 不对") {
		t.Errorf("凭证不对要说清楚：%s", bad.Raw)
	}

	full := obj{
		"domain": "feishu", "app_id": feishutest.AppID, "app_secret": feishutest.AppSecret,
		"site_url": "https://meet.example.com/", "chat_id": "oc_team", "chat_name": "产品组", "auto_push": true,
		"with_doc": true, "doc_folder": "https://example.feishu.cn/drive/folder/fldcnAbCdEf123456?from=x", "link_share": true,
		"import_recordings": true,
	}
	saved := c.want(c.do("PUT", "/api/settings/feishu", full), 200)
	if saved.Body["app_secret_set"] != true || strings.Contains(saved.Raw, feishutest.AppSecret) || saved.Body["site_url"] != "https://meet.example.com" ||
		saved.Body["doc_folder"] != "fldcnAbCdEf123456" || !strings.HasSuffix(saved.Body["console_url"].(string), "/app/"+feishutest.AppID) {
		t.Errorf("保存的配置：Secret 不返回，地址去掉结尾的斜杠，文件夹链接换成 token：%s", saved.Raw)
	}
	var raw string
	if err := c.pool.QueryRow(t.Context(), "SELECT value::text FROM settings WHERE key = 'feishu'").Scan(&raw); err != nil || strings.Contains(raw, feishutest.AppSecret) {
		t.Errorf("App Secret 要加密以后再存：%s %v", raw, err)
	}
	if s := c.want(c.do("GET", "/api/status", nil), 200); s.Body["feishu_ready"] != true {
		t.Errorf("配置了飞书：%s", s.Raw)
	}
	// Secret 留空：沿用保存的
	c.want(c.do("POST", "/api/settings/feishu/test", obj{"app_id": feishutest.AppID}), 200)
	full["app_secret"] = ""
	if r := c.want(c.do("PUT", "/api/settings/feishu", full), 200); r.Body["app_secret_set"] != true {
		t.Errorf("Secret 留空要沿用保存的：%s", r.Raw)
	}

	for _, tc := range []struct {
		change obj
		want   string
	}{
		{obj{"app_id": "12345"}, "cli_"},
		{obj{"app_id": "cli_other_app"}, "请填写 App Secret"}, // 换了应用，要重新填 Secret
		{obj{"doc_folder": "https://example.feishu.cn/wiki/wikcnAbc123456"}, "知识库"},
		{obj{"doc_folder": "随便写的"}, "文件夹的链接"},
		{obj{"site_url": "meet.example.com"}, "http://"},
		{obj{"site_url": ""}, "访问地址"},
		{obj{"chat_id": "", "auto_push": true}, "默认的群"},
	} {
		body := obj{}
		for k, v := range full {
			body[k] = v
		}
		for k, v := range tc.change {
			body[k] = v
		}
		if r := c.want(c.do("PUT", "/api/settings/feishu", body), 422); !strings.Contains(r.Raw, tc.want) {
			t.Errorf("%v：要提示「%s」：%s", tc.change, tc.want, r.Raw)
		}
	}

	// 打开了自动导入、但后台处理（worker）没在运行（测试里没有 worker）：运行情况要说清楚
	if st := c.want(c.do("GET", "/api/settings/feishu/status", nil), 200); st.Body["state"] != "error" || !strings.Contains(st.Raw, "没有在运行") {
		t.Errorf("worker 没在运行：%s", st.Raw)
	}

	// 清空 App ID：不用飞书了，其他配置一起清掉
	off := c.want(c.do("PUT", "/api/settings/feishu", obj{"app_id": ""}), 200)
	if off.Body["app_secret_set"] != false || off.Body["chat_id"] != "" || off.Body["import_recordings"] != false {
		t.Errorf("清空 App ID 后配置都清掉：%s", off.Raw)
	}
	st := c.want(c.do("GET", "/api/settings/feishu/status", nil), 200)
	if st.Body["state"] != "off" || len(st.Body["imports"].([]any)) != 0 {
		t.Errorf("运行情况：%s", st.Raw)
	}
}

// 把纪要发到飞书：创建者和管理员能发；要有纪要、配好飞书、机器人在群里；同一场会议一次只发一个。
func TestShareToFeishu(t *testing.T) {
	c := newServer(t)
	c.setup()
	c.feishu()
	member := c.addMember("member@example.com", "李航")
	id := c.processed("产品周会", []transcript.Segment{{Start: 1, Text: "飞书集成放到下一个版本。"}}, "## 摘要\n好。", `[]`)
	base := "/api/meetings/" + id + "/feishu"

	if r := c.want(c.do("POST", base, obj{"chat_id": "oc_team"}), 409); !strings.Contains(r.Raw, "还没有配置飞书") {
		t.Errorf("没配置飞书：%s", r.Raw)
	}
	if r := c.want(c.do("GET", "/api/feishu/chats", nil), 409); !strings.Contains(r.Raw, "还没有配置飞书") {
		t.Errorf("没配置飞书时列不出群：%s", r.Raw)
	}
	c.want(c.do("PUT", "/api/settings/feishu", obj{
		"app_id": feishutest.AppID, "app_secret": feishutest.AppSecret, "site_url": "https://meet.example.com",
		"chat_id": "oc_team", "chat_name": "产品组", "with_doc": true,
	}), 200)

	chats := member.want(member.do("GET", "/api/feishu/chats", nil), 200)
	if len(chats.Body["items"].([]any)) != 2 || chats.Body["default_chat_id"] != "oc_team" || chats.Body["with_doc"] != true {
		t.Errorf("成员也能看到可以发的群和默认的选择：%s", chats.Raw)
	}

	member.want(member.do("POST", base, obj{"chat_id": "oc_team"}), 403)
	c.want(c.do("POST", base, obj{}), 422)
	if r := c.want(c.do("POST", base, obj{"chat_id": "oc_nowhere"}), 422); !strings.Contains(r.Raw, "机器人不在这个群里") {
		t.Errorf("机器人不在的群：%s", r.Raw)
	}
	share := c.want(c.do("POST", base, obj{"chat_id": "oc_dev", "with_doc": true}), 200)
	if share.Body["status"] != "pending" || share.Body["chat_name"] != "研发群" || share.Body["minutes_version"] != float64(1) || share.Body["requested_by_name"] != "张敏" {
		t.Errorf("发送记录：%s", share.Raw)
	}
	if r := c.want(c.do("POST", base, obj{"chat_id": "oc_team"}), 409); !strings.Contains(r.Raw, "还在发送中") {
		t.Errorf("上一次还没发完：%s", r.Raw)
	}
	// 半小时还没发完的（后台任务丢了）不再挡着
	if _, err := c.pool.Exec(t.Context(), "UPDATE feishu_shares SET created_at = now() - interval '1 hour'"); err != nil {
		t.Fatal(err)
	}
	stuck := c.want(c.do("POST", base, obj{"chat_id": "oc_team"}), 200)
	if _, err := c.pool.Exec(t.Context(), "DELETE FROM feishu_shares WHERE id = $1", stuck.Body["id"]); err != nil {
		t.Fatal(err)
	}
	var jobs int
	if err := c.pool.QueryRow(t.Context(), "SELECT count(*) FROM river_job WHERE kind = 'feishu_share' AND args->>'share_id' = $1", share.Body["id"]).Scan(&jobs); err != nil || jobs != 1 {
		t.Errorf("要加一个后台任务：%d %v", jobs, err)
	}
	list := member.want(member.do("GET", base, nil), 200)
	if items := list.Body["items"].([]any); len(items) != 1 || items[0].(map[string]any)["id"] != share.Body["id"] {
		t.Errorf("团队里的人都能看发送记录：%s", list.Raw)
	}

	// 还没有纪要的会议不能发
	empty := c.want(c.do("POST", "/api/meetings", obj{"title": "刚建的会", "source": "upload"}), 201).Body["id"].(string)
	if r := c.want(c.do("POST", "/api/meetings/"+empty+"/feishu", obj{"chat_id": "oc_team"}), 409); !strings.Contains(r.Raw, "还没有纪要") {
		t.Errorf("没有纪要：%s", r.Raw)
	}
	c.want(c.do("GET", fmt.Sprintf("/api/meetings/%s/feishu", "00000000-0000-0000-0000-000000000000"), nil), 404)
}

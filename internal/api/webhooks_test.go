package api

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/service"
	"github.com/ZaneXo/zsmeet/internal/transcript"
	"github.com/ZaneXo/zsmeet/internal/webhook"
	"github.com/ZaneXo/zsmeet/internal/webhook/webhooktest"
)

// webhooks 起一个假的群机器人服务器，让服务认它的地址（线上只认各平台官方的地址）。
func (c *client) webhooks() *webhooktest.Server {
	c.t.Helper()
	s := webhooktest.New(c.t)
	c.svc.WebhookTestHost = s.Host
	return s
}

const (
	hookToken  = "dingtoken0123456789abcdef"
	hookSecret = "SEC0123456789abcdef0123456789abcdef"
)

// 群机器人：只有管理员能看能改；地址只认官方的；地址和加签的密钥加密保存、不会返回，留空表示沿用；能发测试消息，失败时说清楚怎么改。
func TestWebhookSettings(t *testing.T) {
	c := newServer(t)
	c.setup()
	member := c.addMember("member@example.com", "李航")
	for _, r := range []result{
		member.do("GET", "/api/settings/webhooks", nil),
		member.do("POST", "/api/settings/webhooks", obj{"platform": "slack", "name": "x", "url": "https://hooks.slack.com/services/T1/B1/abc"}),
		member.do("PUT", "/api/settings/webhooks/"+uuid.NewString(), obj{"platform": "slack", "name": "x"}),
		member.do("DELETE", "/api/settings/webhooks/"+uuid.NewString(), nil),
		member.do("POST", "/api/settings/webhooks/"+uuid.NewString()+"/test", nil),
		member.do("GET", "/api/settings/site", nil),
		member.do("PUT", "/api/settings/site", obj{"url": "https://meet.example.com"}),
	} {
		member.want(r, 403)
	}

	// 不是官方的地址不收：数据不能发到随便哪个网站
	fake := webhooktest.New(t)
	for _, u := range []string{"https://evil.example/robot/send?access_token=" + hookToken, fake.URL(webhook.DingTalk, hookToken), "http://oapi.dingtalk.com/robot/send?access_token=" + hookToken} {
		if r := c.want(c.do("POST", "/api/settings/webhooks", obj{"platform": "dingtalk", "name": "产品群", "url": u}), 422); !strings.Contains(r.Raw, "https://oapi.dingtalk.com/robot/send") {
			t.Errorf("%s 不是钉钉官方的地址：%s", u, r.Raw)
		}
	}
	c.svc.WebhookTestHost = fake.Host

	for _, tc := range []struct {
		body obj
		want string
	}{
		{obj{"platform": "dingtalk", "name": " ", "url": fake.URL(webhook.DingTalk, hookToken)}, "起个名字"},
		{obj{"platform": "dingtalk", "name": "产品群"}, "请填写 Webhook 地址"},
		{obj{"platform": "dingtalk", "name": "产品群", "url": fake.URL(webhook.DingTalk, hookToken), "secret": "abc"}, "SEC"},
		{obj{"platform": "wecom", "name": "产品群", "url": fake.URL(webhook.DingTalk, hookToken)}, "qyapi.weixin.qq.com"},
	} {
		if r := c.want(c.do("POST", "/api/settings/webhooks", tc.body), 422); !strings.Contains(r.Raw, tc.want) {
			t.Errorf("%v：要提示「%s」：%s", tc.body, tc.want, r.Raw)
		}
	}
	c.want(c.do("POST", "/api/settings/webhooks", obj{"platform": "telegram", "name": "x", "url": "https://t.me"}), 422)

	created := c.want(c.do("POST", "/api/settings/webhooks", obj{
		"platform": "dingtalk", "name": "产品群", "url": fake.URL(webhook.DingTalk, hookToken) + "&timestamp=1&sign=old", "secret": hookSecret, "auto_push": true,
	}), 201)
	id := created.Body["id"].(string)
	if created.Body["secret_set"] != true || created.Body["auto_push"] != true || !strings.HasSuffix(created.Body["url_masked"].(string), "access_token=••••cdef") {
		t.Errorf("加好的群机器人：%s", created.Raw)
	}
	list := c.want(c.do("GET", "/api/settings/webhooks", nil), 200)
	for _, raw := range []string{created.Raw, list.Raw} {
		if strings.Contains(raw, hookToken) || strings.Contains(raw, hookSecret) {
			t.Errorf("地址和密钥都不能返回给网页：%s", raw)
		}
	}
	var storedURL, storedSecret string
	if err := c.pool.QueryRow(t.Context(), "SELECT url, secret FROM webhooks WHERE id = $1", id).Scan(&storedURL, &storedSecret); err != nil ||
		strings.Contains(storedURL, hookToken) || strings.Contains(storedSecret, hookSecret) || storedSecret == "" {
		t.Errorf("地址和密钥要加密以后再存：%q %q %v", storedURL, storedSecret, err)
	}

	// 测试消息：带着加签，标题里有「会议纪要」（钉钉设了这个关键词也能发出去）
	fake.Secrets[hookToken] = hookSecret
	c.want(c.do("POST", "/api/settings/webhooks/"+id+"/test", nil), 204)
	got := fake.Received()
	if len(got) != 1 || got[0].Query.Get("sign") == "" || got[0].Query.Get("timestamp") == "1" ||
		!strings.Contains(got[0].Body, "会议纪要｜测试消息") {
		t.Errorf("测试消息：%+v", got)
	}

	// 改：地址和密钥留空表示沿用；换了平台要重新填地址；可以不再加签
	updated := c.want(c.do("PUT", "/api/settings/webhooks/"+id, obj{"platform": "dingtalk", "name": "产品组群", "auto_push": false}), 200)
	if updated.Body["name"] != "产品组群" || updated.Body["secret_set"] != true || updated.Body["url_masked"] != created.Body["url_masked"] || updated.Body["auto_push"] != false {
		t.Errorf("留空沿用保存的：%s", updated.Raw)
	}
	c.want(c.do("POST", "/api/settings/webhooks/"+id+"/test", nil), 204)
	if r := c.want(c.do("PUT", "/api/settings/webhooks/"+id, obj{"platform": "wecom", "name": "产品组群"}), 422); !strings.Contains(r.Raw, "换了平台") {
		t.Errorf("换平台要重新填地址：%s", r.Raw)
	}
	if r := c.want(c.do("PUT", "/api/settings/webhooks/"+id, obj{"platform": "dingtalk", "name": "产品组群", "clear_secret": true}), 200); r.Body["secret_set"] != false {
		t.Errorf("不再加签：%s", r.Raw)
	}

	// 测试消息发不出去：说清楚原因和改法，不带地址
	fake.Replies[hookToken] = webhooktest.Reply{Status: 200, Body: `{"errcode":310000,"errmsg":"keywords not in content"}`}
	if r := c.want(c.do("POST", "/api/settings/webhooks/"+id+"/test", nil), 422); !strings.Contains(r.Raw, "关键词「会议纪要」") || strings.Contains(r.Raw, hookToken) {
		t.Errorf("关键词不对：%s", r.Raw)
	}

	// 删
	c.want(c.do("DELETE", "/api/settings/webhooks/"+id, nil), 204)
	c.want(c.do("DELETE", "/api/settings/webhooks/"+id, nil), 404)
	c.want(c.do("POST", "/api/settings/webhooks/"+id+"/test", nil), 404)
	c.want(c.do("PUT", "/api/settings/webhooks/not-a-uuid", obj{"platform": "slack", "name": "x"}), 404)
	if items := c.want(c.do("GET", "/api/settings/webhooks", nil), 200).Body["items"].([]any); len(items) != 0 {
		t.Errorf("删掉以后列不出来：%v", items)
	}
}

// zsmeet 的访问地址：发到群里的消息用它拼链接。没填时用飞书配置里填的。
func TestSiteSettings(t *testing.T) {
	c := newServer(t)
	c.setup()
	if r := c.want(c.do("GET", "/api/settings/site", nil), 200); r.Body["url"] != "" || r.Body["feishu_url"] != "" {
		t.Errorf("还没填：%s", r.Raw)
	}
	for _, bad := range []string{"meet.example.com", "ftp://meet.example.com", "https://meet.example.com/a b", "https://meet.example.com/(x)", "https://meet.example.com/?a=1"} {
		c.want(c.do("PUT", "/api/settings/site", obj{"url": bad}), 422)
	}
	if r := c.want(c.do("PUT", "/api/settings/site", obj{"url": " https://meet.example.com/ "}), 200); r.Body["url"] != "https://meet.example.com" {
		t.Errorf("去掉空格和结尾的斜杠：%s", r.Raw)
	}
	if r := c.want(c.do("PUT", "/api/settings/site", obj{"url": ""}), 200); r.Body["url"] != "" {
		t.Errorf("可以清空：%s", r.Raw)
	}
}

// 访问地址只存一份：飞书设置和群机器人设置里改的是同一个。以前存在飞书配置里的旧地址照样能用，清掉飞书配置时不会跟着丢。
func TestSiteURLSharedWithFeishu(t *testing.T) {
	c := newServer(t)
	c.setup()
	feishu := func(site string) obj {
		return obj{"domain": "feishu", "app_id": "cli_test_app", "app_secret": "test-app-secret", "site_url": site}
	}
	c.want(c.do("PUT", "/api/settings/feishu", feishu("https://meet.example.com/a b")), 422)
	c.want(c.do("PUT", "/api/settings/feishu", feishu("https://meet.example.com/")), 200)
	if r := c.want(c.do("GET", "/api/settings/site", nil), 200); r.Body["url"] != "https://meet.example.com" {
		t.Errorf("飞书设置里填的地址就是通用的访问地址：%s", r.Raw)
	}
	c.want(c.do("PUT", "/api/settings/site", obj{"url": "https://zsmeet.example.com"}), 200)
	if r := c.want(c.do("GET", "/api/settings/feishu", nil), 200); r.Body["site_url"] != "https://zsmeet.example.com" ||
		r.Body["login_redirect_url"] != "https://zsmeet.example.com/api/auth/feishu/callback" {
		t.Errorf("群机器人设置里改了，飞书设置里看到的也变了：%s", r.Raw)
	}
	var stored string
	if err := c.pool.QueryRow(t.Context(), "SELECT value->>'site_url' FROM settings WHERE key = 'feishu'").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "" {
		t.Errorf("飞书配置里不再单独存地址：%q", stored)
	}

	// 老版本把地址存在飞书配置里：通用的还空着时用它；清掉飞书配置时先搬到通用设置里
	for _, stmt := range []string{
		"DELETE FROM settings WHERE key = 'site'",
		`UPDATE settings SET value = jsonb_set(value, '{site_url}', '"https://old.example.com"') WHERE key = 'feishu'`,
	} {
		if _, err := c.pool.Exec(t.Context(), stmt); err != nil {
			t.Fatal(err)
		}
	}
	if r := c.want(c.do("GET", "/api/settings/site", nil), 200); r.Body["url"] != "" || r.Body["feishu_url"] != "https://old.example.com" {
		t.Errorf("用飞书配置里的旧地址：%s", r.Raw)
	}
	c.want(c.do("PUT", "/api/settings/feishu", obj{"domain": "feishu", "app_id": ""}), 200)
	if r := c.want(c.do("GET", "/api/settings/site", nil), 200); r.Body["url"] != "https://old.example.com" {
		t.Errorf("清掉飞书配置时旧地址搬到通用设置里：%s", r.Raw)
	}
}

// 发到群里：能编辑这场会议的人能发，只能看的人不能；看不到的会议当作不存在；同一个群一次只发一条；要有纪要。
func TestSendToWebhooks(t *testing.T) {
	c := newServer(t)
	c.setup()
	fake := c.webhooks()
	viewer := c.addMember("viewer@example.com", "王悦")
	editor := c.addMember("editor@example.com", "赵雷")
	var editorID string
	if err := c.pool.QueryRow(t.Context(), "SELECT id FROM users WHERE email = 'editor@example.com'").Scan(&editorID); err != nil {
		t.Fatal(err)
	}
	id := c.processed("产品周会", []transcript.Segment{{Start: 1, Text: "好。"}}, "## 摘要\n好。", `[]`)
	base := "/api/meetings/" + id + "/webhooks"

	// 还没有群机器人：列表是空的（会议页上不显示「发到群里」）
	if r := viewer.want(viewer.do("GET", base, nil), 200); len(r.Body["items"].([]any)) != 0 {
		t.Errorf("还没有群机器人：%s", r.Raw)
	}
	c.want(c.do("POST", base, obj{"webhook_ids": []string{uuid.NewString()}}), 422)

	ding := c.want(c.do("POST", "/api/settings/webhooks", obj{"platform": "dingtalk", "name": "产品群", "url": fake.URL(webhook.DingTalk, hookToken)}), 201).Body["id"].(string)
	slack := c.want(c.do("POST", "/api/settings/webhooks", obj{"platform": "slack", "name": "#product", "url": fake.URL(webhook.Slack, "slacktoken0123456789")}), 201).Body["id"].(string)

	list := viewer.want(viewer.do("GET", base, nil), 200)
	if items := list.Body["items"].([]any); len(items) != 2 || strings.Contains(list.Raw, hookToken) || strings.Contains(list.Raw, "url") {
		t.Errorf("能看会议的人看得到群机器人的名字，看不到地址：%s", list.Raw)
	}
	if r := viewer.want(viewer.do("POST", base, obj{"webhook_ids": []string{ding}}), 403); !strings.Contains(r.Raw, "不能修改这场会议") {
		t.Errorf("只能看的人不能发：%s", r.Raw)
	}
	editor.want(editor.do("POST", base, obj{"webhook_ids": []string{ding}}), 403)
	c.want(c.do("PUT", "/api/meetings/"+id+"/access", obj{"visibility": "team", "members": []obj{{"user_id": editorID, "can_edit": true}}}), 200)

	c.want(c.do("POST", base, obj{"webhook_ids": []string{}}), 422)
	sent := editor.want(editor.do("POST", base, obj{"webhook_ids": []string{ding, slack, ding}}), 200)
	items := sent.Body["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("重复选的只发一次：%s", sent.Raw)
	}
	first := items[0].(map[string]any)
	if first["status"] != "pending" || first["webhook_id"] != ding || first["webhook_name"] != "产品群" || first["platform"] != "dingtalk" ||
		first["minutes_version"] != float64(1) || first["requested_by_name"] != "赵雷" {
		t.Errorf("发送记录：%s", sent.Raw)
	}
	if r := c.want(c.do("POST", base, obj{"webhook_ids": []string{ding}}), 409); !strings.Contains(r.Raw, "「产品群」的上一条还在发送中") {
		t.Errorf("上一条还没发完：%s", r.Raw)
	}
	var jobs int
	if err := c.pool.QueryRow(t.Context(), "SELECT count(*) FROM river_job WHERE kind = 'webhook_send'").Scan(&jobs); err != nil || jobs != 2 {
		t.Errorf("每个群一个后台任务：%d %v", jobs, err)
	}
	for _, it := range viewer.want(viewer.do("GET", base, nil), 200).Body["items"].([]any) {
		if latest, _ := it.(map[string]any)["latest"].(map[string]any); latest == nil || latest["status"] != "pending" {
			t.Errorf("每个群最近一次的情况：%v", it)
		}
	}
	// 发送中、还没到期限：还挡着，网页上还是发送中
	ageSends := func(age time.Duration) {
		t.Helper()
		if _, err := c.pool.Exec(t.Context(), "UPDATE webhook_sends SET created_at = now() - make_interval(secs => $1)", age.Seconds()); err != nil {
			t.Fatal(err)
		}
	}
	latestOf := func(hook string) map[string]any {
		t.Helper()
		for _, it := range viewer.want(viewer.do("GET", base, nil), 200).Body["items"].([]any) {
			if it.(map[string]any)["id"] == hook {
				latest, _ := it.(map[string]any)["latest"].(map[string]any)
				return latest
			}
		}
		t.Fatalf("没有群 %s", hook)
		return nil
	}
	ageSends(service.WebhookPendingWindow - time.Minute)
	if latest := latestOf(ding); latest["status"] != "pending" {
		t.Errorf("还没到期限，还是发送中：%v", latest)
	}
	c.want(c.do("POST", base, obj{"webhook_ids": []string{ding}}), 409)
	// 发送中超过期限（后台任务丢了）：报成失败、写上原因，网页上就能重新发、不再一直刷新；服务器也允许重新发，用的是同一个期限
	ageSends(service.WebhookPendingWindow + time.Minute)
	if latest := latestOf(ding); latest["status"] != "failed" || latest["error"] != service.WebhookStaleMessage {
		t.Errorf("发送中太久的报成失败：%v", latest)
	}
	c.want(c.do("POST", base, obj{"webhook_ids": []string{ding}}), 200)
	if latest := latestOf(ding); latest["status"] != "pending" {
		t.Errorf("重新发的是新的一条，发送中：%v", latest)
	}

	// 删掉的群机器人不再列出来，也不能选
	c.want(c.do("DELETE", "/api/settings/webhooks/"+slack, nil), 204)
	if items := viewer.want(viewer.do("GET", base, nil), 200).Body["items"].([]any); len(items) != 1 {
		t.Errorf("删掉的不列：%v", items)
	}
	c.want(c.do("POST", base, obj{"webhook_ids": []string{slack}}), 422)
	c.want(c.do("POST", base, obj{"webhook_ids": []string{"not-a-uuid"}}), 422)

	// 看不到的会议：当作不存在
	private := c.processed("一对一", []transcript.Segment{{Start: 1, Text: "好。"}}, "## 摘要\n好。", `[]`)
	c.want(c.do("PUT", "/api/meetings/"+private+"/access", obj{"visibility": "private"}), 200)
	viewer.want(viewer.do("GET", "/api/meetings/"+private+"/webhooks", nil), 404)
	viewer.want(viewer.do("POST", "/api/meetings/"+private+"/webhooks", obj{"webhook_ids": []string{ding}}), 404)

	// 还没有纪要的会议不能发
	empty := c.want(c.do("POST", "/api/meetings", obj{"title": "刚建的会", "source": "upload"}), 201).Body["id"].(string)
	if r := c.want(c.do("POST", "/api/meetings/"+empty+"/webhooks", obj{"webhook_ids": []string{ding}}), 409); !strings.Contains(r.Raw, "还没有纪要") {
		t.Errorf("没有纪要：%s", r.Raw)
	}
	c.want(c.do("GET", "/api/meetings/"+uuid.NewString()+"/webhooks", nil), 404)
}

// whileLocked 在 lock 拿着锁（相当于别人正在改）的时候发出请求 req，等它在锁上排队以后执行 change、提交，返回请求的结果。
// 请求不等锁就做完了，测试失败：说明检查是在锁外面做的，change 没被看到。
func (c *client) whileLocked(lock func(tx pgx.Tx) error, req func() result, change func(tx pgx.Tx) error) result {
	c.t.Helper()
	ctx := c.t.Context()
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		c.t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err := lock(tx); err != nil {
		c.t.Fatal(err)
	}
	done := make(chan result, 1)
	go func() { done <- req() }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := c.pool.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'").Scan(&waiting); err != nil {
			c.t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		select {
		case r := <-done:
			c.t.Fatalf("锁被别人拿着时不该做完：%d %s", r.Status, r.Raw)
		default:
		}
		if time.Now().After(deadline) {
			c.t.Fatal("等了 10 秒也没在锁上排队")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := change(tx); err != nil {
		c.t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		c.t.Fatal(err)
	}
	select {
	case r := <-done:
		return r
	case <-time.After(10 * time.Second):
		c.t.Fatal("锁放开以后应该做完")
	}
	return result{}
}

// 发到群里时，权限、纪要版本都按锁住会议以后的算：等锁的时候被收回了编辑权限、被停用了的不能发；纪要重新生成了的发新的一版，
// 发送记录的时间是真正记下的时间（不是开始等锁的时间）。网页带上看到的版本时，版本变了不发，说清楚现在是第几版。
func TestSendToWebhooksRechecksUnderLock(t *testing.T) {
	c := newServer(t)
	c.setup()
	fake := c.webhooks()
	editor := c.addMember("editor@example.com", "赵雷")
	editorID := c.memberID("editor@example.com")
	id := c.processed("产品周会", []transcript.Segment{{Start: 1, Text: "好。"}}, "## 摘要\n好。", `[]`)
	meetingID := uuid.MustParse(id)
	base := "/api/meetings/" + id + "/webhooks"
	ding := c.want(c.do("POST", "/api/settings/webhooks", obj{"platform": "dingtalk", "name": "产品群", "url": fake.URL(webhook.DingTalk, hookToken)}), 201).Body["id"].(string)
	slack := c.want(c.do("POST", "/api/settings/webhooks", obj{"platform": "slack", "name": "#product", "url": fake.URL(webhook.Slack, "slacktoken0123456789")}), 201).Body["id"].(string)
	c.want(c.do("PUT", "/api/meetings/"+id+"/access", obj{"visibility": "team", "members": []obj{{"user_id": editorID, "can_edit": true}}}), 200)
	lockMeeting := func(tx pgx.Tx) error {
		_, err := sqlc.New(c.pool).WithTx(tx).LockMeeting(t.Context(), meetingID)
		return err
	}

	// 等锁的时候创建者把他移出了能编辑的人
	r := c.whileLocked(lockMeeting, func() result { return editor.do("POST", base, obj{"webhook_ids": []string{ding}}) }, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), "DELETE FROM meeting_members WHERE meeting_id = $1", meetingID)
		return err
	})
	if r.Status != 403 {
		t.Errorf("等锁的时候被收回了编辑权限，不能再发：%d %s", r.Status, r.Raw)
	}
	// 等锁的时候他被停用了：请求带着的账号是请求开始时读的，锁住以后要按现在的再看
	c.want(c.do("PUT", "/api/meetings/"+id+"/access", obj{"visibility": "team", "members": []obj{{"user_id": editorID, "can_edit": true}}}), 200)
	r = c.whileLocked(lockMeeting, func() result { return editor.do("POST", base, obj{"webhook_ids": []string{ding}}) }, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), "UPDATE users SET disabled = true WHERE id = $1", editorID)
		return err
	})
	if r.Status != 403 || !strings.Contains(r.Raw, "已被停用") {
		t.Errorf("等锁的时候被停用了，不能再发：%d %s", r.Status, r.Raw)
	}
	if n := c.count("SELECT count(*) FROM webhook_sends"); n != 0 {
		t.Errorf("没有权限了，不记发送：%d", n)
	}

	// 等锁的时候纪要重新生成成了第 2 版：发的是第 2 版；记录的时间在第 2 版生成以后
	var regenerated time.Time
	r = c.whileLocked(lockMeeting, func() result { return c.do("POST", base, obj{"webhook_ids": []string{ding}}) }, func(tx pgx.Tx) error {
		if _, err := sqlc.New(c.pool).WithTx(tx).AddMinutesVersion(t.Context(), sqlc.AddMinutesVersionParams{MeetingID: meetingID, Content: "## 摘要\n新的。", Source: "auto"}); err != nil {
			return err
		}
		return tx.QueryRow(t.Context(), "SELECT clock_timestamp()").Scan(&regenerated)
	})
	c.want(r, 200)
	sent := r.Body["items"].([]any)[0].(map[string]any)
	if sent["minutes_version"] != float64(2) {
		t.Errorf("发的是锁住会议时的当前版本（第 2 版）：%s", r.Raw)
	}
	var created time.Time
	if err := c.pool.QueryRow(t.Context(), "SELECT created_at FROM webhook_sends WHERE id = $1", sent["id"]).Scan(&created); err != nil {
		t.Fatal(err)
	}
	if !created.After(regenerated) {
		t.Errorf("发送记录的时间是锁住会议以后真正记下的时间（%v），不是开始等锁的时间：%v", regenerated, created)
	}

	// 网页上看到的还是第 1 版：不发，告诉他现在是第 2 版
	if r := c.want(c.do("POST", base, obj{"webhook_ids": []string{slack}, "minutes_version": 1}), 409); !strings.Contains(r.Raw, "第 2 版") {
		t.Errorf("版本变了要说清楚：%s", r.Raw)
	}
	if r := c.want(c.do("POST", base, obj{"webhook_ids": []string{slack}, "minutes_version": 2}), 200); r.Body["items"].([]any)[0].(map[string]any)["minutes_version"] != float64(2) {
		t.Errorf("看到的就是当前版本，照常发：%s", r.Raw)
	}
}

// 群机器人最多 50 个：两个管理员同时加第 50 个，后一个要看到前一个加的。
func TestWebhookLimitUnderConcurrency(t *testing.T) {
	c := newServer(t)
	c.setup()
	fake := c.webhooks()
	q := sqlc.New(c.pool)
	for i := range 49 {
		if _, err := q.CreateWebhook(t.Context(), sqlc.CreateWebhookParams{Platform: "slack", Name: fmt.Sprintf("群 %d", i), Url: "x", UrlMasked: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	add := obj{"platform": "slack", "name": "第 50 个", "url": fake.URL(webhook.Slack, "slacktoken0123456789")}
	// 另一个管理员正在加（拿着群机器人的锁 service.webhooksLock，还没提交）
	r := c.whileLocked(func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), "SELECT pg_advisory_xact_lock(7368825)")
		return err
	}, func() result { return c.do("POST", "/api/settings/webhooks", add) }, func(tx pgx.Tx) error {
		_, err := sqlc.New(c.pool).WithTx(tx).CreateWebhook(t.Context(), sqlc.CreateWebhookParams{Platform: "slack", Name: "别人加的", Url: "x", UrlMasked: "x"})
		return err
	})
	if r.Status != 422 || !strings.Contains(r.Raw, "最多加 50 个") {
		t.Errorf("别人刚加满 50 个，这一个加不了：%d %s", r.Status, r.Raw)
	}
	if n := c.count("SELECT count(*) FROM webhooks"); n != 50 {
		t.Errorf("最多 50 个，现在 %d 个", n)
	}
}

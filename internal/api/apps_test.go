package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ZaneXo/zsmeet/internal/transcript"
)

// pkce 生成一对 PKCE 的 code_verifier 和 S256 的 code_challenge，和桌面版做的一样。
func pkce() (verifier, challenge string) {
	raw := make([]byte, 32)
	rand.Read(raw)
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// authorizeApp 在网页上（c 是登录着的浏览器）点「允许」，返回授权码。
func (c *client) authorizeApp(device, challenge string) string {
	c.t.Helper()
	r := c.want(c.do("POST", "/api/apps/authorize", obj{"client": "desktop", "device_name": device, "code_challenge": challenge}), 200)
	return r.Body["code"].(string)
}

// appLogin 走一遍 App 登录：c 在网页上允许，一个新的 App 用授权码换令牌。返回带着令牌的 App 客户端（没有任何 Cookie）。
func (c *client) appLogin(device string) *client {
	c.t.Helper()
	verifier, challenge := pkce()
	code := c.authorizeApp(device, challenge)
	app := c.another()
	r := app.want(app.do("POST", "/api/apps/token", obj{"code": code, "code_verifier": verifier}), 200)
	app.bearer = r.Body["token"].(string)
	return app
}

// noCookie 断言响应没有下发 Cookie。
func noCookie(t *testing.T, r result, what string) {
	t.Helper()
	if v := r.Header.Values("Set-Cookie"); len(v) > 0 {
		t.Errorf("%s不该下发 Cookie：%v", what, v)
	}
}

func TestAppLoginFlow(t *testing.T) {
	c := newServer(t)
	c.setup()
	member := c.addMember("lihang@example.com", "李航")

	// 没登录不能发授权码；参数不对给出能看懂的说明
	_, challenge := pkce()
	c.another().want(c.another().do("POST", "/api/apps/authorize", obj{"client": "desktop", "device_name": "MacBook", "code_challenge": challenge}), 401)
	bad := []obj{
		{"client": "android", "device_name": "MacBook", "code_challenge": challenge},
		{"client": "desktop", "device_name": "   ", "code_challenge": challenge},
		{"client": "desktop", "device_name": strings.Repeat("电", 101), "code_challenge": challenge},
		{"client": "desktop", "device_name": "Mac\nBook", "code_challenge": challenge},
		{"client": "desktop", "device_name": "MacBook", "code_challenge": challenge[:42]},
		{"client": "desktop", "device_name": "MacBook", "code_challenge": challenge[:42] + "="},
	}
	for _, body := range bad {
		member.want(member.do("POST", "/api/apps/authorize", body), 422)
	}

	verifier, challenge := pkce()
	code := member.authorizeApp("  李航的 MacBook Pro  ", challenge)
	if len(code) < 43 {
		t.Fatalf("授权码要足够长、猜不出来：%q", code)
	}
	app := member.another()
	tok := app.want(app.do("POST", "/api/apps/token", obj{"code": code, "code_verifier": verifier}), 200)
	noCookie(t, tok, "换令牌")
	token, _ := tok.Body["token"].(string)
	if !strings.HasPrefix(token, "zsm_") || len(token) != len("zsm_")+43 {
		t.Fatalf("令牌应该是 zsm_ 加 32 字节随机数：%q", token)
	}
	if u := tok.Body["user"].(obj); u["email"] != "lihang@example.com" || u["role"] != "member" {
		t.Errorf("换令牌时要返回是谁：%s", tok.Raw)
	}
	var stored []byte
	if err := c.pool.QueryRow(t.Context(), "SELECT token_hash FROM app_tokens").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256([]byte(token)); string(stored) != string(sum[:]) {
		t.Error("数据库里只存令牌的 SHA-256")
	}
	var device string
	if err := c.pool.QueryRow(t.Context(), "SELECT device_name FROM app_tokens").Scan(&device); err != nil || device != "李航的 MacBook Pro" {
		t.Errorf("设备名要去掉首尾空白：%q %v", device, err)
	}
	app.bearer = token

	// 带令牌访问：和网页登录一样能用，而且不读写会话 Cookie
	list := app.want(app.do("GET", "/api/meetings", nil), 200)
	noCookie(t, list, "带令牌的请求")
	st := app.want(app.do("GET", "/api/status", nil), 200)
	if st.Body["user"].(obj)["email"] != "lihang@example.com" || st.Body["api"] != float64(1) {
		t.Errorf("状态里要认出 App 的主人，并带接口版本号：%s", st.Raw)
	}
	noCookie(t, st, "带令牌查状态")
	if anon := c.another().want(c.another().do("GET", "/api/status", nil), 200); anon.Body["api"] != float64(1) {
		t.Errorf("没登录时也要有接口版本号：%s", anon.Raw)
	}
	var sessions int
	if err := c.pool.QueryRow(t.Context(), "SELECT count(*) FROM sessions").Scan(&sessions); err != nil {
		t.Fatal(err)
	}

	// 桌面版录音的会议
	m := app.want(app.do("POST", "/api/meetings", obj{"title": "客户电话", "source": "desktop"}), 201)
	if m.Body["source"] != "desktop" || m.Body["owner_name"] != "李航" {
		t.Errorf("要能建桌面版录音的会议：%s", m.Raw)
	}
	app.want(app.do("POST", "/api/meetings", obj{"title": "x", "source": "feishu"}), 422)

	// 录音回放（不经过 huma 的接口）也认令牌
	id := app.processed("周会", []transcript.Segment{{Start: 0, End: 1, Text: "好。"}}, "## 摘要\n好。", `[]`)
	if err := os.MkdirAll(c.store.Path("audio"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.store.Path("audio/"+id+".m4a"), []byte("m4a"), 0o644); err != nil {
		t.Fatal(err)
	}
	audio := app.want(app.do("GET", "/api/meetings/"+id+"/audio", nil), 200)
	if audio.Raw != "m4a" {
		t.Errorf("录音内容不对：%q", audio.Raw)
	}
	noCookie(t, audio, "带令牌取录音")
	wrong := app.another()
	wrong.bearer = "zsm_" + strings.Repeat("A", 43)
	wrong.want(wrong.do("GET", "/api/meetings/"+id+"/audio", nil), 401)
	wrong.want(wrong.do("GET", "/api/meetings", nil), 401)
	wrong.bearer = "not-a-token"
	wrong.want(wrong.do("GET", "/api/meetings", nil), 401)
	if r := wrong.want(wrong.do("GET", "/api/status", nil), 200); r.Body["user"] != nil {
		t.Errorf("令牌不对时状态里不该有用户：%s", r.Raw)
	}

	// 管理员才能用的接口：规则和网页一样
	app.want(app.do("GET", "/api/members", nil), 403)
	adminApp := c.appLogin("管理员的 iMac")
	adminApp.want(adminApp.do("GET", "/api/members", nil), 200)

	// 同时带着 Cookie 和令牌时只认令牌：管理员的浏览器里带上李航的令牌，就是李航
	mixed := *c
	mixed.bearer = token
	if r := mixed.want(mixed.do("GET", "/api/status", nil), 200); r.Body["user"].(obj)["email"] != "lihang@example.com" {
		t.Errorf("带令牌时不该按 Cookie 认人：%s", r.Raw)
	}
	mixed.want(mixed.do("GET", "/api/members", nil), 403)
	mixed.bearer = "zsm_" + strings.Repeat("B", 43)
	mixed.want(mixed.do("GET", "/api/meetings", nil), 401)
	// 不是 zsm_ 开头的 Bearer（比如前面的反向代理自己加的）不当成 App 令牌，照常按 Cookie 认人
	mixed.bearer = "sso-proxy-token"
	if r := mixed.want(mixed.do("GET", "/api/status", nil), 200); r.Body["user"].(obj)["email"] != "admin@example.com" {
		t.Errorf("别的 Bearer 令牌不该挡住 Cookie 登录：%s", r.Raw)
	}
	mixed.want(mixed.do("GET", "/api/members", nil), 200)
	// 管理员的网页登录不受影响
	c.want(c.do("GET", "/api/members", nil), 200)

	// App 不能用自己的令牌再发新的授权码，也不能调用读写网页会话的接口
	_, ch := pkce()
	app.want(app.do("POST", "/api/apps/authorize", obj{"client": "desktop", "device_name": "另一台", "code_challenge": ch}), 403)
	app.want(app.do("POST", "/api/login", obj{"email": "lihang@example.com", "password": "member-pass-123"}), 400)
	app.want(app.do("POST", "/api/logout", nil), 400)
	app.want(app.do("POST", "/api/me/feishu/link", obj{"password": "member-pass-123"}), 403)
	var after int
	if err := c.pool.QueryRow(t.Context(), "SELECT count(*) FROM sessions").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != sessions {
		t.Errorf("带令牌的请求不该建会话：之前 %d 条，之后 %d 条", sessions, after)
	}
	// 令牌还能用
	app.want(app.do("GET", "/api/meetings", nil), 200)
}

func TestAppCodeRules(t *testing.T) {
	c := newServer(t)
	c.setup()
	member := c.addMember("lihang@example.com", "李航")
	app := c.another()
	exchange := func(code, verifier string) result {
		return app.do("POST", "/api/apps/token", obj{"code": code, "code_verifier": verifier})
	}
	var messages []string
	rejected := func(r result) {
		t.Helper()
		app.want(r, 422)
		messages = append(messages, r.Raw)
	}

	// code_verifier 对不上：拒绝，而且授权码用掉了，换对的也不行
	verifier, challenge := pkce()
	code := member.authorizeApp("MacBook", challenge)
	other, _ := pkce()
	rejected(exchange(code, other))
	rejected(exchange(code, verifier))

	// 过期
	verifier, challenge = pkce()
	code = member.authorizeApp("MacBook", challenge)
	if _, err := c.pool.Exec(t.Context(), "UPDATE app_auth_codes SET expires_at = now() - interval '1 second' WHERE used_at IS NULL"); err != nil {
		t.Fatal(err)
	}
	rejected(exchange(code, verifier))

	// 只能用一次
	verifier, challenge = pkce()
	code = member.authorizeApp("MacBook", challenge)
	app.want(exchange(code, verifier), 200)
	rejected(exchange(code, verifier))

	// 不存在的、空的
	rejected(exchange(strings.Repeat("A", 43), verifier))
	rejected(exchange("", verifier))

	// 停用的账号：授权码一起作废
	verifier, challenge = pkce()
	code = member.authorizeApp("MacBook", challenge)
	c.want(c.do("PATCH", "/api/members/"+c.memberID("lihang@example.com"), obj{"disabled": true}), 200)
	rejected(exchange(code, verifier))

	// 每种情况都是同一句话，试不出是哪一种
	for _, m := range messages[1:] {
		if m != messages[0] {
			t.Errorf("换令牌失败时说法要一样：\n%s\n%s", messages[0], m)
		}
	}
	if !strings.Contains(messages[0], "重新点「在浏览器里登录」") {
		t.Errorf("要告诉用户怎么办：%s", messages[0])
	}
}

func TestAppDevicesAndLogout(t *testing.T) {
	c := newServer(t)
	c.setup()
	member := c.addMember("lihang@example.com", "李航")
	first := member.appLogin("MacBook")
	second := member.appLogin("iMac")
	adminApp := c.appLogin("管理员的电脑")

	list := member.want(member.do("GET", "/api/me/devices", nil), 200)
	items := list.Body["items"].([]any)
	if len(items) != 2 || items[0].(obj)["device_name"] != "iMac" || items[1].(obj)["device_name"] != "MacBook" {
		t.Fatalf("只列自己的，新登录的在前：%s", list.Raw)
	}
	for _, it := range items {
		if it.(obj)["current"] != false || it.(obj)["client"] != "desktop" {
			t.Errorf("网页上看时没有哪台是「当前」：%s", list.Raw)
		}
	}
	mine := first.want(first.do("GET", "/api/me/devices", nil), 200)
	for _, it := range mine.Body["items"].([]any) {
		d := it.(obj)
		if (d["device_name"] == "MacBook") != (d["current"] == true) {
			t.Errorf("App 自己看时，自己那台是「当前」：%s", mine.Raw)
		}
	}

	// 别人的设备退不掉
	adminDevice := c.want(c.do("GET", "/api/me/devices", nil), 200).Body["items"].([]any)[0].(obj)["id"].(string)
	member.want(member.do("DELETE", "/api/me/devices/"+adminDevice, nil), 404)
	member.want(member.do("DELETE", "/api/me/devices/not-a-uuid", nil), 422)
	adminApp.want(adminApp.do("GET", "/api/meetings", nil), 200)

	// 在网页上让 iMac 退出
	secondID := items[0].(obj)["id"].(string)
	member.want(member.do("DELETE", "/api/me/devices/"+secondID, nil), 204)
	second.want(second.do("GET", "/api/meetings", nil), 401)
	member.want(member.do("DELETE", "/api/me/devices/"+secondID, nil), 404)

	// App 自己退出
	member.want(member.do("POST", "/api/apps/logout", nil), 401)
	first.want(first.do("POST", "/api/apps/logout", nil), 204)
	first.want(first.do("GET", "/api/meetings", nil), 401)
	if r := member.want(member.do("GET", "/api/me/devices", nil), 200); len(r.Body["items"].([]any)) != 0 {
		t.Errorf("退出的设备不该再列出来：%s", r.Raw)
	}
	// 网页上的登录不受影响
	member.want(member.do("GET", "/api/meetings", nil), 200)
}

func TestAppTokensRevokedWithPasswordAndDisable(t *testing.T) {
	c := newServer(t)
	c.setup()
	member := c.addMember("lihang@example.com", "李航")
	memberID := c.memberID("lihang@example.com")

	// 网页上改密码：App 全部退出，还没换成令牌的授权码也作废
	app := member.appLogin("MacBook")
	verifier, challenge := pkce()
	pending := member.authorizeApp("iMac", challenge)
	member.want(member.do("POST", "/api/me/password", obj{"current_password": "member-pass-123", "new_password": "second-pass-456"}), 204)
	app.want(app.do("GET", "/api/meetings", nil), 401)
	app.another().want(app.another().do("POST", "/api/apps/token", obj{"code": pending, "code_verifier": verifier}), 422)
	member.want(member.do("GET", "/api/meetings", nil), 200)

	// 在 App 里改密码：这台 App 不退出，别的 App 和网页上的登录都退出
	here := member.appLogin("MacBook")
	there := member.appLogin("iMac")
	here.want(here.do("POST", "/api/me/password", obj{"current_password": "second-pass-456", "new_password": "third-pass-789"}), 204)
	here.want(here.do("GET", "/api/meetings", nil), 200)
	there.want(there.do("GET", "/api/meetings", nil), 401)
	member.want(member.do("GET", "/api/meetings", nil), 401)
	member.want(member.do("POST", "/api/login", obj{"email": "lihang@example.com", "password": "third-pass-789"}), 200)

	// 用重置链接设新密码：App 全部退出
	resetToken := c.want(c.do("POST", "/api/members/"+memberID+"/reset-link", nil), 200).Body["token"].(string)
	visitor := c.another()
	visitor.want(visitor.do("POST", "/api/account-links/accept", obj{"token": resetToken, "password": "fourth-pass-000"}), 200)
	here.want(here.do("GET", "/api/meetings", nil), 401)

	// 停用账号：App 全部退出；重新启用以后也不会复活
	app = visitor.appLogin("MacBook")
	app.want(app.do("GET", "/api/meetings", nil), 200)
	c.want(c.do("PATCH", "/api/members/"+memberID, obj{"disabled": true}), 200)
	app.want(app.do("GET", "/api/meetings", nil), 401)
	c.want(c.do("PATCH", "/api/members/"+memberID, obj{"disabled": false}), 200)
	app.want(app.do("GET", "/api/meetings", nil), 401)
	var left int
	if err := c.pool.QueryRow(t.Context(), "SELECT count(*) FROM app_tokens WHERE user_id = $1", memberID).Scan(&left); err != nil || left != 0 {
		t.Errorf("停用时令牌要删掉：还有 %d 个（%v）", left, err)
	}
}

func TestAppTokenLastUsedThrottled(t *testing.T) {
	c := newServer(t)
	c.setup()
	app := c.appLogin("MacBook")
	lastUsed := func() *time.Time {
		t.Helper()
		var at *time.Time
		if err := c.pool.QueryRow(t.Context(), "SELECT last_used_at FROM app_tokens").Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	if lastUsed() != nil {
		t.Error("刚登录还没用过")
	}
	app.want(app.do("GET", "/api/meetings", nil), 200)
	first := lastUsed()
	if first == nil {
		t.Fatal("用过以后要记下时间")
	}
	// 一分钟之内再用，不再写
	app.want(app.do("GET", "/api/meetings", nil), 200)
	if got := lastUsed(); !got.Equal(*first) {
		t.Errorf("一分钟之内不该再写：%v → %v", first, got)
	}
	// 过了一分钟，再用就更新
	if _, err := c.pool.Exec(t.Context(), "UPDATE app_tokens SET last_used_at = now() - interval '2 minutes'"); err != nil {
		t.Fatal(err)
	}
	app.want(app.do("GET", "/api/meetings", nil), 200)
	if got := lastUsed(); got == nil || time.Since(*got) > 30*time.Second {
		t.Errorf("过了一分钟要更新：%v", got)
	}
	if r := c.want(c.do("GET", "/api/me/devices", nil), 200); r.Body["items"].([]any)[0].(obj)["last_used_at"] == nil {
		t.Errorf("设备列表里要有上次使用的时间：%s", r.Raw)
	}
}

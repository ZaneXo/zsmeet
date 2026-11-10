package api

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2/memstore"
	"github.com/google/uuid"

	"github.com/ZaneXo/zsmeet/internal/feishu/feishutest"
	"github.com/ZaneXo/zsmeet/internal/secret"
	"github.com/ZaneXo/zsmeet/internal/service"
)

// noFollow 发一个 GET，不跟着跳转，返回状态码和要跳到哪。像浏览器一样带着、收下 Cookie。
func (c *client) noFollow(rawURL string) (int, string) {
	c.t.Helper()
	resp := c.noFollowResp(rawURL)
	return resp.StatusCode, resp.Header.Get("Location")
}

func (c *client) noFollowResp(rawURL string) *http.Response {
	c.t.Helper()
	hc := *c.http
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Get(rawURL)
	if err != nil {
		c.t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

// startLink 在账号页上输入密码、点「关联飞书」。返回状态码，和要整页打开的飞书授权页地址（发起不了时是 feishu_error 的代号，前面加 "error:"）。
func (c *client) startLink(pass string) (int, string) {
	c.t.Helper()
	r := c.do("POST", "/api/me/feishu/link", obj{"password": pass})
	if r.Status != http.StatusOK {
		return r.Status, ""
	}
	if code, _ := r.Body["error"].(string); code != "" {
		return r.Status, "error:" + code
	}
	return r.Status, r.Body["url"].(string)
}

// feishuAuth 在浏览器里点「用飞书登录」（mode 为空）或者在账号页上输入密码点「关联飞书」（mode=link，密码是 addMember 给的）：
// 跳到飞书的授权页，飞书带着授权码跳回 zsmeet。返回最后跳回的网页地址（去掉参数）和地址里的错误代号。
func (c *client) feishuAuth(fs *feishutest.Server, mode string) (page, code string) {
	c.t.Helper()
	var status int
	var loc string
	if mode == "link" {
		status, loc = c.startLink("member-pass-123")
		if status != http.StatusOK {
			c.t.Fatalf("输入密码点「关联飞书」：%d", status)
		}
		if code, ok := strings.CutPrefix(loc, "error:"); ok {
			return "/settings", code
		}
		status = http.StatusFound
	} else {
		status, loc = c.noFollow(c.base + "/api/auth/feishu/start")
	}
	if status != http.StatusFound {
		c.t.Fatalf("点「用飞书登录」应该跳转，得到 %d", status)
	}
	if strings.HasPrefix(loc, fs.URL) {
		// 飞书的授权页跳回 zsmeet
		if status, loc = c.noFollow(loc); status != http.StatusFound || !strings.HasPrefix(loc, c.base+service.FeishuCallbackPath+"?") {
			c.t.Fatalf("授权页要跳回 zsmeet 的回调地址：%d %s", status, loc)
		}
		status, loc = c.noFollow(loc)
		if status != http.StatusFound {
			c.t.Fatalf("回调应该跳转，得到 %d", status)
		}
	}
	return splitBack(c.t, loc)
}

// feishuLogin 是 feishuAuth 的登录模式。
func (c *client) feishuLogin(fs *feishutest.Server) (string, string) {
	c.t.Helper()
	return c.feishuAuth(fs, "")
}

// splitBack 拆开跳回网页的地址：路径，和地址里的 feishu_error。只允许跳回站内，参数只有固定的几种。
func splitBack(t *testing.T, loc string) (string, string) {
	t.Helper()
	u, err := url.Parse(loc)
	if err != nil || u.Host != "" || !strings.HasPrefix(u.Path, "/") {
		t.Fatalf("要跳回站内的网页：%s", loc)
	}
	for k := range u.Query() {
		if k != "feishu_error" && k != "feishu" {
			t.Errorf("跳回网页的地址里只能有固定的参数：%s", loc)
		}
	}
	return u.Path, u.Query().Get("feishu_error")
}

// whoami 返回当前登录的人的邮箱，没登录时为空。
func (c *client) whoami() string {
	c.t.Helper()
	user, _ := c.want(c.do("GET", "/api/status", nil), 200).Body["user"].(map[string]any)
	if user == nil {
		return ""
	}
	return user["email"].(string)
}

// enableFeishuLogin 配好飞书应用、打开飞书登录。访问地址就是测试服务的地址，飞书登录完跳回这里。
func (c *client) enableFeishuLogin(autoJoin bool) *feishutest.Server {
	c.t.Helper()
	fs := c.feishu()
	c.want(c.do("PUT", "/api/settings/feishu", obj{
		"app_id": feishutest.AppID, "app_secret": feishutest.AppSecret, "site_url": c.base,
		"login": true, "login_auto_join": autoJoin,
	}), 200)
	return fs
}

// count 数一张表里满足条件的行。
func (c *client) countRows(query string, args ...any) int {
	c.t.Helper()
	var n int
	if err := c.pool.QueryRow(c.t.Context(), "SELECT count(*) FROM "+query, args...).Scan(&n); err != nil {
		c.t.Fatal(err)
	}
	return n
}

// linkedTo 返回这个邮箱的账号关联的飞书账号（subject），没关联时为空。
func (c *client) linkedTo(email string) string {
	c.t.Helper()
	var subject string
	c.pool.QueryRow(c.t.Context(), "SELECT i.subject FROM user_identities i JOIN users u ON u.id = i.user_id WHERE u.email = $1", email).Scan(&subject)
	return subject
}

// stateCookie 返回这个浏览器里放 state 的 Cookie（没有时为空）。
func (c *client) stateCookie() string {
	c.t.Helper()
	u, _ := url.Parse(c.base + service.FeishuCallbackPath)
	for _, ck := range c.http.Jar.Cookies(u) {
		if ck.Name == feishuStateCookie {
			return ck.Value
		}
	}
	return ""
}

func (c *client) setStateCookie(value string) {
	u, _ := url.Parse(c.base + service.FeishuCallbackPath)
	c.http.Jar.SetCookies(u, []*http.Cookie{{Name: feishuStateCookie, Value: value, Path: feishuCookiePath}})
}

// 设置：只有管理员能改；保存后给出要填进飞书的重定向 URL；登录页的开关跟着走；不用飞书了一起关掉。
func TestFeishuLoginSettings(t *testing.T) {
	c := newServer(t)
	c.setup()
	c.feishu()
	anon := c.another()
	if s := anon.want(anon.do("GET", "/api/status", nil), 200); s.Body["feishu_login"] != false {
		t.Errorf("还没配置飞书：%s", s.Raw)
	}

	member := c.addMember("member@example.com", "李航")
	member.want(member.do("PUT", "/api/settings/feishu", obj{"app_id": feishutest.AppID, "app_secret": feishutest.AppSecret, "site_url": c.base, "login": true}), 403)

	// 只配了飞书、没打开登录
	r := c.want(c.do("PUT", "/api/settings/feishu", obj{"app_id": feishutest.AppID, "app_secret": feishutest.AppSecret, "site_url": "https://meet.example.com/"}), 200)
	if r.Body["login"] != false || r.Body["login_redirect_url"] != "https://meet.example.com/api/auth/feishu/callback" {
		t.Errorf("重定向 URL 是访问地址加回调路径：%s", r.Raw)
	}
	if s := anon.want(anon.do("GET", "/api/status", nil), 200); s.Body["feishu_login"] != false {
		t.Errorf("没打开飞书登录：%s", s.Raw)
	}
	if r := member.want(member.do("GET", "/api/me/feishu", nil), 200); r.Body["enabled"] != false {
		t.Errorf("没打开飞书登录时账号页上不能关联：%s", r.Raw)
	}

	r = c.want(c.do("PUT", "/api/settings/feishu", obj{
		"domain": "lark", "app_id": feishutest.AppID, "site_url": "https://meet.example.com", "login": true, "login_auto_join": true,
	}), 200)
	if r.Body["login"] != true || r.Body["login_auto_join"] != true || strings.Contains(r.Raw, feishutest.AppSecret) {
		t.Errorf("打开飞书登录：%s", r.Raw)
	}
	if s := anon.want(anon.do("GET", "/api/status", nil), 200); s.Body["feishu_login"] != true {
		t.Errorf("没登录的人也要知道能用飞书登录：%s", s.Raw)
	}
	if r := member.want(member.do("GET", "/api/me/feishu", nil), 200); r.Body["enabled"] != true || r.Body["linked"] != false {
		t.Errorf("打开飞书登录后账号页上能关联：%s", r.Raw)
	}
	// Lark 的授权页在 accounts.larksuite.com（测试里没设 ZSMEET_FEISHU_BASE_URL 时）
	c.svc.FeishuBaseURL = ""
	_, loc := anon.noFollow(anon.base + "/api/auth/feishu/start")
	u, err := url.Parse(loc)
	if err != nil || u.Scheme+"://"+u.Host+u.Path != "https://accounts.larksuite.com/open-apis/authen/v1/authorize" ||
		u.Query().Get("client_id") != feishutest.AppID || u.Query().Get("redirect_uri") != "https://meet.example.com/api/auth/feishu/callback" ||
		u.Query().Get("code_challenge_method") != "S256" || len(u.Query().Get("state")) < 40 ||
		!strings.Contains(u.Query().Get("scope"), "contact:user.employee:readonly") {
		t.Errorf("Lark 的授权页地址：%s", loc)
	}
	if strings.Contains(loc, feishutest.AppSecret) {
		t.Errorf("授权页地址里不能有 App Secret：%s", loc)
	}

	// 不用飞书了：飞书登录一起关掉
	off := c.want(c.do("PUT", "/api/settings/feishu", obj{"app_id": ""}), 200)
	if off.Body["login"] != false || off.Body["login_auto_join"] != false {
		t.Errorf("清空 App ID 后飞书登录也关掉：%s", off.Raw)
	}
	if s := anon.want(anon.do("GET", "/api/status", nil), 200); s.Body["feishu_login"] != false {
		t.Errorf("不用飞书以后登录页不显示飞书登录：%s", s.Raw)
	}
}

// 没打开飞书登录、系统还没初始化时，点「用飞书登录」不去飞书，带着错误代号回到登录页，不写会话。
func TestFeishuLoginUnavailable(t *testing.T) {
	c := newServer(t)
	fs := c.feishu()
	fs.LoginAs = &feishutest.LoginUser{OpenID: "ou_1", UnionID: "on_1", Name: "张敏", EnterpriseEmail: "admin@example.com"}

	// 还没有管理员：即使数据库里已经有飞书配置（比如从别处恢复的），也不能用飞书登录
	if _, err := c.svc.SaveFeishu(t.Context(), service.FeishuInput{
		AppID: feishutest.AppID, AppSecret: feishutest.AppSecret, SiteURL: c.base, Login: true, LoginAutoJoin: true,
	}); err != nil {
		t.Fatal(err)
	}
	if s := c.want(c.do("GET", "/api/status", nil), 200); s.Body["feishu_login"] != false {
		t.Errorf("还没初始化：%s", s.Raw)
	}
	if page, code := c.feishuLogin(fs); page != "/" || code != service.FeishuUnavailable {
		t.Errorf("还没初始化时不能用飞书登录：%s %q", page, code)
	}
	if n, _ := c.svc.Initialized(t.Context()); n {
		t.Fatal("飞书登录不能创建出第一个用户")
	}
	if fs.Count("GET /open-apis/authen/v1/authorize") != 0 {
		t.Error("还没初始化时不该去飞书")
	}
	if n := c.countRows("sessions"); n != 0 {
		t.Errorf("没登录成功不能写会话：%d 条", n)
	}

	c.setup()
	sessions := c.countRows("sessions")
	// 关掉飞书登录：start 不去飞书、不写会话、不发 state；之前发起、还没回来的也不能完成
	c.want(c.do("PUT", "/api/settings/feishu", obj{"app_id": feishutest.AppID, "site_url": c.base, "login": true}), 200)
	anon := c.another()
	_, authorize := anon.noFollow(anon.base + "/api/auth/feishu/start")
	c.want(c.do("PUT", "/api/settings/feishu", obj{"app_id": feishutest.AppID, "site_url": c.base, "login": false}), 200)
	fresh := c.another()
	if page, code := fresh.feishuLogin(fs); page != "/" || code != service.FeishuUnavailable || fresh.stateCookie() != "" {
		t.Errorf("没打开飞书登录：%s %q", page, code)
	}
	_, back := anon.noFollow(authorize)
	if code, loc := anon.noFollow(back); code != http.StatusFound || loc != "/?feishu_error=unavailable" || anon.whoami() != "" {
		t.Errorf("关掉飞书登录后，进行到一半的飞书登录也不能完成：%d %s", code, loc)
	}
	// 关联也一样：密码对了也发起不了，账号页上显示原因
	if status, loc := c.startLink(admin["password"].(string)); status != http.StatusOK || loc != "error:"+service.FeishuUnavailable || c.stateCookie() != "" {
		t.Errorf("没打开飞书登录时不能关联：%d %q", status, loc)
	}
	if n := c.countRows("sessions"); n != sessions {
		t.Errorf("没打开飞书登录时点按钮不能写会话：%d → %d", sessions, n)
	}
}

// state 放在加密的短期 Cookie 里：只能用一次、只认发起登录的那个浏览器、过期作废、改过作废。
// 这些失败都不写会话。用户拒绝授权、飞书换不出凭证时给出对应的代号。
func TestFeishuLoginState(t *testing.T) {
	c := newServer(t)
	c.setup()
	fs := c.enableFeishuLogin(false)
	fs.LoginAs = &feishutest.LoginUser{OpenID: "ou_admin", UnionID: "on_admin", Name: "张敏", EnterpriseEmail: "admin@example.com"}
	sessions := c.countRows("sessions")
	expired := func(who *client, back, why string) {
		t.Helper()
		code, loc := who.noFollow(back)
		if _, e := splitBack(t, loc); code != http.StatusFound || e != service.FeishuExpired || who.whoami() != "" {
			t.Errorf("%s：%d %s", why, code, loc)
		}
	}

	// Cookie 的样子：只发给飞书登录的地址、脚本读不到、跨站整页跳回来时会带上、10 分钟
	anon := c.another()
	resp := anon.noFollowResp(anon.base + "/api/auth/feishu/start")
	var ck *http.Cookie
	for _, k := range resp.Cookies() {
		if k.Name == feishuStateCookie {
			ck = k
		}
	}
	if ck == nil || !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode || ck.Path != "/api/auth/feishu" || ck.MaxAge != 600 || ck.Secure {
		t.Fatalf("state 的 Cookie：%+v", ck)
	}
	authorize, _ := url.Parse(resp.Header.Get("Location"))
	if strings.Contains(ck.Value, authorize.Query().Get("state")) {
		t.Error("Cookie 里的 state 要加密")
	}
	if n := c.countRows("sessions"); n != sessions {
		t.Errorf("点「用飞书登录」不能写会话：%d → %d", sessions, n)
	}

	// 没点「用飞书登录」就直接来到回调（或者是别人发来的回调链接）：没有 Cookie
	expired(c.another(), anon.base+service.FeishuCallbackPath+"?code=x&state=y", "没有发起过飞书登录")

	// 攻击者自己走到授权页，拿到带着授权码的回调链接，再骗受害者打开：受害者的浏览器里没有这个 state
	attacker := c.another()
	_, toFeishu := attacker.noFollow(attacker.base + "/api/auth/feishu/start")
	_, back := attacker.noFollow(toFeishu)
	victim := c.another()
	victim.noFollow(victim.base + "/api/auth/feishu/start") // 受害者自己也点过，但 state 不一样
	expired(victim, back, "别人的回调链接不能让我登录")

	// 地址里的 state 被改了
	u, _ := url.Parse(back)
	q := u.Query()
	q.Set("state", q.Get("state")+"x")
	u.RawQuery = q.Encode()
	expired(attacker, u.String(), "state 对不上")
	// 上面那次已经把 Cookie 清掉了：原样的回调链接也不行了
	if attacker.stateCookie() != "" {
		t.Error("回调以后 state 的 Cookie 要清掉")
	}
	expired(attacker, back, "state 只能用一次")

	// 正常登录以后，同一个回调链接再打开一次也不行
	b := c.another()
	_, toFeishu = b.noFollow(b.base + "/api/auth/feishu/start")
	saved := b.stateCookie()
	_, back = b.noFollow(toFeishu)
	if code, loc := b.noFollow(back); code != http.StatusFound || loc != "/" || b.whoami() != "admin@example.com" {
		t.Fatalf("正常登录：%d %s", code, loc)
	}
	sessions = c.countRows("sessions")
	b2 := c.another()
	expired(b2, back, "别的浏览器重放回调链接")
	// Cookie 被偷出来、和回调链接一起重放：飞书的授权码只能用一次，换不出凭证
	b2.setStateCookie(saved)
	if _, loc := b2.noFollow(back); !strings.Contains(loc, "feishu_error="+service.FeishuRetry) || b2.whoami() != "" {
		t.Errorf("授权码用过了：%s", loc)
	}

	// Cookie 被改过、过期、用途不对
	_, toFeishu = b2.noFollow(b2.base + "/api/auth/feishu/start")
	_, back = b2.noFollow(toFeishu)
	good := b2.stateCookie()
	tampered := []byte(good)
	tampered[10] ^= 1
	b2.setStateCookie(string(tampered))
	expired(b2, back, "改过的 Cookie")
	st, err := c.svc.OpenFeishuState(good)
	if err != nil {
		t.Fatal(err)
	}
	st.Expires = time.Now().Add(-time.Second).Unix()
	b2.setStateCookie(c.svc.SealFeishuState(st))
	expired(b2, back, "过期的 Cookie")
	st.Expires, st.Mode = time.Now().Add(time.Minute).Unix(), "other"
	b2.setStateCookie(c.svc.SealFeishuState(st))
	expired(b2, back, "不认识的用途")
	b2.setStateCookie("")
	expired(b2, back, "Cookie 是空的")
	// 都没登录上，也没写会话
	if n := c.countRows("sessions"); n != sessions {
		t.Errorf("失败的回调不能写会话：%d → %d", sessions, n)
	}

	// 用户在授权页上点了拒绝
	late := c.another()
	fs.LoginAs = nil
	if _, code := late.feishuLogin(fs); code != service.FeishuDenied {
		t.Errorf("拒绝授权：%q", code)
	}

	// 飞书换不出凭证：说清楚是哪种情况，不把飞书的原始报错、授权码甩给用户
	fs.LoginAs = &feishutest.LoginUser{OpenID: "ou_admin", UnionID: "on_admin", Name: "张敏", EnterpriseEmail: "admin@example.com"}
	for failure, want := range map[int]string{
		20071: service.FeishuRedirectMismatch, 20010: service.FeishuOutOfScope, 20002: service.FeishuAppCredentials, 20999: service.FeishuFailed,
	} {
		fs.FailToken = failure
		if _, code := late.feishuLogin(fs); code != want {
			t.Errorf("飞书返回 %d：%q", failure, code)
		}
	}
	if n := c.countRows("sessions"); n != sessions {
		t.Errorf("失败的回调不能写会话：%d → %d", sessions, n)
	}
	fs.FailToken = 0
	if _, code := late.feishuLogin(fs); code != "" || late.whoami() != "admin@example.com" {
		t.Errorf("恢复以后能登录：%q", code)
	}
}

// 打开 ZSMEET_SECURE_COOKIE 时，state 的 Cookie 也只走 HTTPS。
func TestFeishuStateCookieSecure(t *testing.T) {
	h := &Handlers{sess: NewSessions(memstore.New(), true)}
	if ck := h.stateCookie("x", 600); !ck.Secure || !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode {
		t.Errorf("%+v", ck)
	}
	if ck := h.stateCookie("", -1); ck.MaxAge >= 0 {
		t.Errorf("清掉 Cookie：%+v", ck)
	}
}

// 按邮箱对应：只认企业邮箱（飞书验证过）；通讯录里的邮箱没验证过，对上了成员也不登录、不关联。
// 第一次对应上以后记下关联，以后按关联登录，邮箱改了也不影响；会话和密码登录一样换新的。
func TestFeishuLoginExistingMember(t *testing.T) {
	c := newServer(t)
	c.setup()
	fs := c.enableFeishuLogin(true) // 自动加入也打开：确认通讯录邮箱对上成员时不会另建一个账号
	c.addMember("lihang@corp.example.com", "李航")
	c.addMember("other@example.com", "王五")

	// 企业邮箱是李航的、通讯录里的邮箱是王五的：只能登录成李航。大小写不同也算同一个
	fs.LoginAs = &feishutest.LoginUser{OpenID: "ou_li", UnionID: "on_li", Name: "李航（飞书）", Email: "Other@Example.com", EnterpriseEmail: "LiHang@Corp.Example.com"}
	b := c.another()
	b.noFollow(b.base + "/api/status") // 先有一个没登录的会话
	before := b.sessionCookie()
	if page, code := b.feishuLogin(fs); page != "/" || code != "" {
		t.Fatalf("已有的成员用飞书登录：%s %q", page, code)
	}
	if b.whoami() != "lihang@corp.example.com" {
		t.Fatalf("应该登录成企业邮箱对应的李航：%s", b.whoami())
	}
	if after := b.sessionCookie(); after == "" || after == before {
		t.Error("登录成功要换一个新的会话令牌")
	}
	if c.linkedTo("lihang@corp.example.com") != "on_li" || c.linkedTo("other@example.com") != "" {
		t.Error("关联的是企业邮箱对应的人")
	}
	// 登录后能正常用，和密码登录的会话一样
	b.want(b.do("GET", "/api/meetings", nil), 200)
	if r := b.want(b.do("GET", "/api/me/feishu", nil), 200); r.Body["linked"] != true {
		t.Errorf("账号页要显示已关联飞书：%s", r.Raw)
	}
	var name string
	c.pool.QueryRow(t.Context(), "SELECT name FROM users WHERE email = 'lihang@corp.example.com'").Scan(&name)
	if name != "李航" {
		t.Errorf("已有成员的名字不改：%s", name)
	}

	// 只有通讯录里的邮箱对得上（王五）：不登录、不关联、不新建，告诉他先用密码登录再关联
	users := c.countRows("users")
	sessions := c.countRows("sessions")
	fs.LoginAs = &feishutest.LoginUser{OpenID: "ou_x", UnionID: "on_x", Name: "冒充的人", Email: "other@example.com"}
	evil := c.another()
	if page, code := evil.feishuLogin(fs); page != "/" || code != service.FeishuContactEmailMember || evil.whoami() != "" {
		t.Errorf("通讯录邮箱对上成员：%s %q", page, code)
	}
	if c.linkedTo("other@example.com") != "" || c.countRows("user_identities WHERE subject = 'on_x'") != 0 ||
		c.countRows("users") != users || c.countRows("sessions") != sessions {
		t.Error("通讯录邮箱对上成员时什么都不能改")
	}
	// 企业邮箱对不上谁、通讯录邮箱对上了：一样拒绝（企业邮箱不会被拿来新建账号）
	fs.LoginAs.EnterpriseEmail = "someone@corp.example.com"
	if _, code := evil.feishuLogin(fs); code != service.FeishuContactEmailMember || c.countRows("users") != users {
		t.Errorf("企业邮箱没对上、通讯录邮箱对上了：%q", code)
	}

	// 管理员在 zsmeet 里改了李航的邮箱、飞书里的邮箱也换了：按关联照样登录
	if _, err := c.pool.Exec(t.Context(), "UPDATE users SET email = 'hang.li@corp.example.com' WHERE email = 'lihang@corp.example.com'"); err != nil {
		t.Fatal(err)
	}
	fs.LoginAs = &feishutest.LoginUser{OpenID: "ou_li", UnionID: "on_li", Name: "李航", Email: "new-address@example.com"}
	again := c.another()
	if _, code := again.feishuLogin(fs); code != "" || again.whoami() != "hang.li@corp.example.com" {
		t.Errorf("按关联登录：%q %s", code, again.whoami())
	}

	// 飞书里另一个人的企业邮箱被改成了李航的：李航已经关联了飞书账号，不能被接管
	fs.LoginAs = &feishutest.LoginUser{OpenID: "ou_evil", UnionID: "on_evil", Name: "冒充的人", EnterpriseEmail: "hang.li@corp.example.com"}
	if _, code := evil.feishuLogin(fs); evil.whoami() != "" || code != service.FeishuUserLinkedOther {
		t.Errorf("已经关联过的账号不能按邮箱被别的飞书账号接管：%q", code)
	}

	// 李航换了飞书账号：解除关联以后，新的飞书账号按企业邮箱重新对应
	b.want(b.do("DELETE", "/api/me/feishu", nil), 204)
	b.want(b.do("DELETE", "/api/me/feishu", nil), 404)
	if r := b.want(b.do("GET", "/api/me/feishu", nil), 200); r.Body["linked"] != false {
		t.Errorf("解除关联以后：%s", r.Raw)
	}
	fs.LoginAs = &feishutest.LoginUser{OpenID: "ou_li2", UnionID: "on_li2", Name: "李航", EnterpriseEmail: "hang.li@corp.example.com"}
	if _, code := evil.feishuLogin(fs); code != "" || evil.whoami() != "hang.li@corp.example.com" {
		t.Errorf("解除关联以后新的飞书账号按企业邮箱对应：%q", code)
	}
	// 没有 union_id 时用 open_id 关联
	fs.LoginAs = &feishutest.LoginUser{OpenID: "ou_admin", Name: "张敏", EnterpriseEmail: "admin@example.com"}
	admin := c.another()
	if _, code := admin.feishuLogin(fs); code != "" || admin.whoami() != "admin@example.com" {
		t.Errorf("管理员用飞书登录：%q", code)
	}
	if subject := c.linkedTo("admin@example.com"); subject != "ou_admin" {
		t.Errorf("没有 union_id 时用 open_id：%q", subject)
	}
}

// 邀请：发给企业邮箱的邀请，用飞书登录时直接用掉，按邀请的角色建账号；发给通讯录邮箱的邀请不能这样用掉。
func TestFeishuLoginInvite(t *testing.T) {
	c := newServer(t)
	c.setup()
	fs := c.enableFeishuLogin(true)

	invite := func(email, role string) string {
		t.Helper()
		return c.want(c.do("POST", "/api/invites", obj{"email": email, "role": role}), 201).Body["invite"].(map[string]any)["id"].(string)
	}
	pending := func(email string) bool {
		t.Helper()
		return c.countRows("account_links WHERE kind = 'invite' AND email = $1 AND used_at IS NULL", email) == 1
	}

	// 发给企业邮箱的邀请：建成管理员（邀请里的角色），名字取飞书的，邀请用掉，记下关联
	invite("chen@corp.example.com", "admin")
	fs.LoginAs = &feishutest.LoginUser{OpenID: "ou_chen", UnionID: "on_chen", Name: "  陈晨  ", Email: "chen@example.com", EnterpriseEmail: "Chen@Corp.Example.com"}
	b := c.another()
	if page, code := b.feishuLogin(fs); page != "/" || code != "" {
		t.Fatalf("用企业邮箱的邀请加入：%s %q", page, code)
	}
	me := c.want(b.do("GET", "/api/status", nil), 200).Body["user"].(map[string]any)
	if me["email"] != "chen@corp.example.com" || me["role"] != "admin" || me["name"] != "陈晨" {
		t.Errorf("按邀请建的账号：%v", me)
	}
	if pending("chen@corp.example.com") || c.linkedTo("chen@corp.example.com") != "on_chen" {
		t.Error("邀请要用掉，并且记下关联")
	}
	if invites := c.want(c.do("GET", "/api/members", nil), 200).Body["invites"].([]any); len(invites) != 0 {
		t.Errorf("用掉的邀请不再显示：%v", invites)
	}

	// 发给通讯录邮箱的邀请：不用掉，不建账号（即使打开了自动加入），请他打开邀请链接
	invite("zhao@example.com", "member")
	users := c.countRows("users")
	fs.LoginAs = &feishutest.LoginUser{OpenID: "ou_zhao", UnionID: "on_zhao", Name: "赵六", Email: "zhao@example.com"}
	z := c.another()
	if page, code := z.feishuLogin(fs); page != "/" || code != service.FeishuContactEmailInvite || z.whoami() != "" {
		t.Errorf("通讯录邮箱的邀请：%s %q", page, code)
	}
	if !pending("zhao@example.com") || c.countRows("users") != users || c.countRows("user_identities WHERE subject = 'on_zhao'") != 0 {
		t.Error("通讯录邮箱的邀请不能被用掉")
	}
	// 有企业邮箱、但对不上谁，通讯录邮箱有邀请：一样不用掉，也不按企业邮箱自动建账号
	fs.LoginAs.EnterpriseEmail = "zhao@corp.example.com"
	if _, code := z.feishuLogin(fs); code != service.FeishuContactEmailInvite || !pending("zhao@example.com") || c.countRows("users") != users {
		t.Errorf("企业邮箱没对上、通讯录邮箱有邀请：%q", code)
	}

	// 企业邮箱的邀请已经失效（过期）：不建账号，请管理员重新生成；重新生成以后就能用飞书加入
	id := invite("wu@corp.example.com", "member")
	if _, err := c.pool.Exec(t.Context(), "UPDATE account_links SET expires_at = now() - interval '1 second' WHERE email = 'wu@corp.example.com'"); err != nil {
		t.Fatal(err)
	}
	fs.LoginAs = &feishutest.LoginUser{OpenID: "ou_wu", UnionID: "on_wu", Name: "吴七", EnterpriseEmail: "wu@corp.example.com"}
	w := c.another()
	if _, code := w.feishuLogin(fs); code != service.FeishuInviteUnusable || w.whoami() != "" || !pending("wu@corp.example.com") || c.countRows("users") != users {
		t.Errorf("失效的邀请：%q", code)
	}
	c.want(c.do("POST", "/api/invites/"+id+"/renew", nil), 200)
	if _, code := w.feishuLogin(fs); code != "" || w.whoami() != "wu@corp.example.com" || pending("wu@corp.example.com") {
		t.Errorf("重新生成邀请以后：%q", code)
	}
}

// 还不是成员：默认拒绝，请管理员邀请；打开「自动加入」后新建成员账号（先用企业邮箱，没有时用通讯录邮箱），
// 拿不到邮箱时说明要开什么权限。
func TestFeishuLoginNewUser(t *testing.T) {
	c := newServer(t)
	c.setup()
	fs := c.enableFeishuLogin(false)
	fs.LoginAs = &feishutest.LoginUser{OpenID: "ou_new", UnionID: "on_new", Name: "王芳", Email: "wangfang@example.com", EnterpriseEmail: "fang.wang@corp.example.com"}

	b := c.another()
	if _, code := b.feishuLogin(fs); b.whoami() != "" || code != service.FeishuNotMember {
		t.Errorf("不是成员：%q", code)
	}
	if n := c.countRows("users"); n != 1 {
		t.Errorf("没打开自动加入时不能新建账号：%d", n)
	}

	// 打开自动加入
	c.want(c.do("PUT", "/api/settings/feishu", obj{"app_id": feishutest.AppID, "site_url": c.base, "login": true, "login_auto_join": true}), 200)
	if _, code := b.feishuLogin(fs); code != "" || b.whoami() != "fang.wang@corp.example.com" {
		t.Fatalf("自动加入，用企业邮箱：%q %s", code, b.whoami())
	}
	me := c.want(b.do("GET", "/api/status", nil), 200).Body["user"].(map[string]any)
	if me["name"] != "王芳" || me["role"] != "member" {
		t.Errorf("自动加入的是普通成员，名字取飞书的：%v", me)
	}
	// 自动创建的账号没有能用的密码
	b2 := c.another()
	b2.want(b2.do("POST", "/api/login", obj{"email": "fang.wang@corp.example.com", "password": ""}), 401)
	// 第二次登录走关联，不会再建一个
	again := c.another()
	if _, code := again.feishuLogin(fs); code != "" || again.whoami() != "fang.wang@corp.example.com" {
		t.Errorf("再次登录：%q", code)
	}
	if n := c.countRows("users"); n != 2 {
		t.Errorf("同一个飞书账号只建一个账号：%d", n)
	}
	b.want(b.do("GET", "/api/settings/feishu", nil), 403)

	// 没有企业邮箱时用通讯录邮箱，名字为空时用邮箱 @ 前面的部分
	fs.LoginAs = &feishutest.LoginUser{OpenID: "ou_li", UnionID: "on_li", Email: "Li.Si@example.com"}
	li := c.another()
	if _, code := li.feishuLogin(fs); code != "" || li.whoami() != "li.si@example.com" {
		t.Errorf("没有企业邮箱时用通讯录邮箱：%q", code)
	}
	if u := c.want(li.do("GET", "/api/status", nil), 200).Body["user"].(map[string]any); u["name"] != "li.si" {
		t.Errorf("没有名字时：%v", u)
	}

	// 拿不到邮箱
	fs.LoginAs = &feishutest.LoginUser{OpenID: "ou_noemail", UnionID: "on_noemail", Name: "赵六"}
	if _, code := b2.feishuLogin(fs); b2.whoami() != "" || code != service.FeishuNoEmail {
		t.Errorf("自动加入但拿不到邮箱：%q", code)
	}
	c.want(c.do("PUT", "/api/settings/feishu", obj{"app_id": feishutest.AppID, "site_url": c.base, "login": true}), 200)
	if _, code := b2.feishuLogin(fs); code != service.FeishuNotMember {
		t.Errorf("没打开自动加入、拿不到邮箱：请管理员邀请：%q", code)
	}
}

// 关联飞书：登录着的人在账号页上关联，不看邮箱；已经关联了别人的飞书账号不行；关联不写新的会话。
// 解除关联：本人在账号页上，或者管理员在成员列表里。
func TestFeishuLink(t *testing.T) {
	c := newServer(t)
	c.setup()
	fs := c.enableFeishuLogin(false)
	li := c.addMember("lihang@example.com", "李航")
	chen := c.addMember("chen@example.com", "陈晨")

	// 飞书里一点邮箱都没有：一样能关联到当前登录的人
	fs.LoginAs = &feishutest.LoginUser{OpenID: "ou_li", UnionID: "on_li", Name: "李航"}
	sessions, cookie := c.countRows("sessions"), li.sessionCookie()
	// 用地址直接发起关联不行，要先输入密码；密码不对时不发起
	if status, loc := li.noFollow(li.base + "/api/auth/feishu/start?mode=link"); status != http.StatusFound || loc != "/settings?feishu_error="+service.FeishuLinkPassword {
		t.Fatalf("不输入密码就关联：%d %s", status, loc)
	}
	if status, _ := li.startLink("wrong-pass-123"); status != 422 || li.stateCookie() != "" {
		t.Fatalf("密码不对：%d", status)
	}
	status, loc := li.startLink("member-pass-123")
	if status != http.StatusOK || !strings.HasPrefix(loc, fs.URL) {
		t.Fatalf("关联飞书要去飞书的授权页：%d %s", status, loc)
	}
	_, back := li.noFollow(loc)
	if _, loc = li.noFollow(back); loc != "/settings?feishu=linked" {
		t.Fatalf("关联成功回到账号页：%s", loc)
	}
	if c.linkedTo("lihang@example.com") != "on_li" || li.whoami() != "lihang@example.com" {
		t.Error("飞书账号关联到当前登录的人")
	}
	if c.countRows("sessions") != sessions || li.sessionCookie() != cookie {
		t.Error("关联飞书不写新的会话")
	}
	if r := li.want(li.do("GET", "/api/me/feishu", nil), 200); r.Body["linked"] != true || r.Body["linked_at"] == nil {
		t.Errorf("账号页显示已关联：%s", r.Raw)
	}
	// 再关联一次同一个：没有变化
	if page, code := li.feishuAuth(fs, "link"); page != "/settings" || code != "" {
		t.Errorf("重复关联同一个飞书账号：%s %q", page, code)
	}
	// 之后用这个飞书账号登录
	other := c.another()
	if _, code := other.feishuLogin(fs); code != "" || other.whoami() != "lihang@example.com" {
		t.Errorf("关联以后能用飞书登录：%q", code)
	}

	// 别人想把李航的飞书账号关联到自己身上：不行
	if page, code := chen.feishuAuth(fs, "link"); page != "/settings" || code != service.FeishuLinkedElsewhere || c.linkedTo("chen@example.com") != "" {
		t.Errorf("已经关联了别人的飞书账号：%s %q", page, code)
	}
	// 李航已经关联了，又想关联另一个飞书账号：先解除
	fs.LoginAs = &feishutest.LoginUser{OpenID: "ou_li2", UnionID: "on_li2", Name: "李航"}
	if _, code := li.feishuAuth(fs, "link"); code != service.FeishuAlreadyLinked || c.linkedTo("lihang@example.com") != "on_li" {
		t.Errorf("已经关联了另一个飞书账号：%q", code)
	}

	// 没登录就点关联：要先登录，不去飞书
	anon := c.another()
	if status, _ := anon.startLink("member-pass-123"); status != 401 || anon.stateCookie() != "" {
		t.Errorf("没登录时关联：%d", status)
	}
	// 发起关联以后退出、换成别人登录：飞书跳回来时不能关联到别人身上
	fs.LoginAs = &feishutest.LoginUser{OpenID: "ou_chen", UnionID: "on_chen", Name: "陈晨"}
	_, loc = chen.startLink("member-pass-123")
	_, back = chen.noFollow(loc)
	chen.want(chen.do("POST", "/api/logout", nil), 204)
	chen.want(chen.do("POST", "/api/login", obj{"email": "lihang@example.com", "password": "member-pass-123"}), 200)
	if _, loc = chen.noFollow(back); loc != "/settings?feishu_error=signed_out" || c.countRows("user_identities WHERE subject = 'on_chen'") != 0 {
		t.Errorf("中间换了账号：%s", loc)
	}
	// 发起关联以后退出：一样不行
	chen.want(chen.do("POST", "/api/logout", nil), 204)
	chen.want(chen.do("POST", "/api/login", obj{"email": "chen@example.com", "password": "member-pass-123"}), 200)
	_, loc = chen.startLink("member-pass-123")
	_, back = chen.noFollow(loc)
	chen.want(chen.do("POST", "/api/logout", nil), 204)
	if _, loc = chen.noFollow(back); loc != "/settings?feishu_error=signed_out" || c.countRows("user_identities WHERE subject = 'on_chen'") != 0 {
		t.Errorf("中间退出了：%s", loc)
	}
	chen.want(chen.do("POST", "/api/login", obj{"email": "chen@example.com", "password": "member-pass-123"}), 200)
	// 拒绝授权
	fs.LoginAs = nil
	if page, code := chen.feishuAuth(fs, "link"); page != "/settings" || code != service.FeishuDenied {
		t.Errorf("关联时拒绝授权：%s %q", page, code)
	}

	// 管理员在成员列表里看得到谁关联了飞书，可以替他解除；普通成员不行
	members := c.want(c.do("GET", "/api/members", nil), 200).Body["members"].([]any)
	var liID string
	for _, m := range members {
		m := m.(map[string]any)
		if m["feishu_linked"] != (m["email"] == "lihang@example.com") {
			t.Errorf("成员列表上的飞书关联：%v", m)
		}
		if m["email"] == "lihang@example.com" {
			liID = m["id"].(string)
		}
	}
	chen.want(chen.do("DELETE", "/api/members/"+liID+"/feishu", nil), 403)
	c.want(c.do("DELETE", "/api/members/"+liID+"/feishu", nil), 204)
	c.want(c.do("DELETE", "/api/members/"+liID+"/feishu", nil), 404)
	c.want(c.do("DELETE", "/api/members/"+uuid.NewString()+"/feishu", nil), 404)
	if c.linkedTo("lihang@example.com") != "" {
		t.Error("管理员解除关联")
	}
	// 解除以后，这个飞书账号又能关联给别人了
	fs.LoginAs = &feishutest.LoginUser{OpenID: "ou_li", UnionID: "on_li", Name: "李航"}
	if _, code := chen.feishuAuth(fs, "link"); code != "" || c.linkedTo("chen@example.com") != "on_li" {
		t.Errorf("解除以后：%q", code)
	}
	// 本人解除
	chen.want(chen.do("DELETE", "/api/me/feishu", nil), 204)
	if r := chen.want(chen.do("GET", "/api/me/feishu", nil), 200); r.Body["linked"] != false || c.linkedTo("chen@example.com") != "" {
		t.Errorf("本人解除：%s", r.Raw)
	}
}

// 停用的账号不能用飞书登录：不管是按企业邮箱对应的，还是已经关联过的；已经登录着的也会立刻失效。
func TestFeishuLoginDisabled(t *testing.T) {
	c := newServer(t)
	c.setup()
	fs := c.enableFeishuLogin(true)
	c.addMember("lihang@example.com", "李航")
	fs.LoginAs = &feishutest.LoginUser{OpenID: "ou_li", UnionID: "on_li", Name: "李航", EnterpriseEmail: "lihang@example.com"}
	linked := c.another()
	if _, code := linked.feishuLogin(fs); code != "" {
		t.Fatalf("先关联：%q", code)
	}
	c.addMember("chen@example.com", "陈晨")
	c.addMember("zhao@example.com", "赵六")

	members := c.want(c.do("GET", "/api/members", nil), 200).Body["members"].([]any)
	for _, m := range members {
		m := m.(map[string]any)
		if m["email"] != "admin@example.com" {
			c.want(c.do("PATCH", "/api/members/"+m["id"].(string), obj{"disabled": true}), 200)
		}
	}
	if linked.whoami() != "" {
		t.Error("停用后已经登录着的飞书会话立刻失效")
	}
	b := c.another()
	if _, code := b.feishuLogin(fs); b.whoami() != "" || code != service.FeishuAccountDisabled {
		t.Errorf("已关联的停用账号：%q", code)
	}
	// 按企业邮箱对应到停用的账号：不关联、不新建（即使打开了自动加入）
	fs.LoginAs = &feishutest.LoginUser{OpenID: "ou_chen", UnionID: "on_chen", Name: "陈晨", EnterpriseEmail: "chen@example.com"}
	if _, code := b.feishuLogin(fs); b.whoami() != "" || code != service.FeishuAccountDisabled {
		t.Errorf("按企业邮箱对应的停用账号：%q", code)
	}
	// 通讯录邮箱对上停用的账号：一样拒绝
	fs.LoginAs = &feishutest.LoginUser{OpenID: "ou_zhao", UnionID: "on_zhao", Name: "赵六", Email: "zhao@example.com"}
	if _, code := b.feishuLogin(fs); b.whoami() != "" || code != service.FeishuAccountDisabled {
		t.Errorf("按通讯录邮箱对应的停用账号：%q", code)
	}
	if c.linkedTo("chen@example.com") != "" || c.linkedTo("zhao@example.com") != "" || c.countRows("users") != 4 {
		t.Error("停用的账号不记关联，也不新建")
	}
}

// sessionCookie 返回这个浏览器现在的会话 Cookie。
func (c *client) sessionCookie() string {
	c.t.Helper()
	u, _ := url.Parse(c.base)
	for _, ck := range c.http.Jar.Cookies(u) {
		if ck.Name == CookieName {
			return ck.Value
		}
	}
	return ""
}

// 改密码、用重置链接设新密码以后，关联的飞书账号一起解除：会话被人偷用、关联了他自己的飞书时，改一次密码就能把他关在外面。
func TestPasswordChangeDropsFeishuLink(t *testing.T) {
	c := newServer(t)
	c.setup()
	fs := c.enableFeishuLogin(false)
	li := c.addMember("lihang@example.com", "李航")
	var liID string
	if err := c.pool.QueryRow(t.Context(), "SELECT id FROM users WHERE email = 'lihang@example.com'").Scan(&liID); err != nil {
		t.Fatal(err)
	}
	link := func(pass string) {
		t.Helper()
		status, loc := li.startLink(pass)
		if status != http.StatusOK || !strings.HasPrefix(loc, fs.URL) {
			t.Fatalf("关联飞书：%d %s", status, loc)
		}
		_, back := li.noFollow(loc)
		if _, loc = li.noFollow(back); loc != "/settings?feishu=linked" || c.linkedTo("lihang@example.com") != "on_li" {
			t.Fatalf("关联成功：%s", loc)
		}
	}
	fs.LoginAs = &feishutest.LoginUser{OpenID: "ou_li", UnionID: "on_li", Name: "李航"}

	link("member-pass-123")
	li.want(li.do("POST", "/api/me/password", obj{"current_password": "member-pass-123", "new_password": "new-password-456"}), 204)
	if c.linkedTo("lihang@example.com") != "" {
		t.Error("改了密码，飞书的关联要一起解除")
	}
	if _, code := c.another().feishuLogin(fs); code != service.FeishuNotMember {
		t.Errorf("解除以后不能再用这个飞书账号登录：%q", code)
	}

	link("new-password-456")
	token := c.want(c.do("POST", "/api/members/"+liID+"/reset-link", nil), 200).Body["token"].(string)
	c.another().want(c.another().do("POST", "/api/account-links/accept", obj{"token": token, "password": "reset-password-789"}), 200)
	if c.linkedTo("lihang@example.com") != "" {
		t.Error("用重置链接设了新密码，飞书的关联也要解除")
	}
}

// 点了「关联飞书」以后、飞书跳回来之前改了密码：改密码时解除了关联，这次关联也作废，不能在改密码之后又关联上去。
func TestFeishuLinkAfterPasswordChange(t *testing.T) {
	c := newServer(t)
	c.setup()
	fs := c.enableFeishuLogin(false)
	li := c.addMember("lihang@example.com", "李航")
	fs.LoginAs = &feishutest.LoginUser{OpenID: "ou_li", UnionID: "on_li", Name: "李航"}

	status, loc := li.startLink("member-pass-123")
	if status != http.StatusOK || !strings.HasPrefix(loc, fs.URL) {
		t.Fatalf("关联飞书：%d %s", status, loc)
	}
	_, back := li.noFollow(loc)
	li.want(li.do("POST", "/api/me/password", obj{"current_password": "member-pass-123", "new_password": "new-password-456"}), 204)
	if li.whoami() != "lihang@example.com" {
		t.Fatal("改密码的这台设备保持登录")
	}
	if _, loc = li.noFollow(back); loc != "/settings?feishu_error="+service.FeishuPasswordChanged || c.linkedTo("lihang@example.com") != "" {
		t.Errorf("改密码之前发起的关联要作废：%s，关联到 %q", loc, c.linkedTo("lihang@example.com"))
	}

	// 用新密码重新发起就可以
	status, loc = li.startLink("new-password-456")
	if status != http.StatusOK || !strings.HasPrefix(loc, fs.URL) {
		t.Fatalf("重新关联：%d %s", status, loc)
	}
	_, back = li.noFollow(loc)
	if _, loc = li.noFollow(back); loc != "/settings?feishu=linked" || c.linkedTo("lihang@example.com") != "on_li" {
		t.Errorf("用新密码重新关联：%s", loc)
	}
}

// ZSMEET_SECRET_KEY 改过、保存的 App Secret 和 API Key 解不开时：网页照样能打开（登录页上只是没有「用飞书登录」），
// 大模型和飞书当作没配置好，管理员到设置里重新填就行。
func TestStatusWithUndecryptableSecrets(t *testing.T) {
	c := newServer(t)
	c.setup()
	fs := c.enableFeishuLogin(false)
	li := c.addMember("lihang@example.com", "李航")
	fs.LoginAs = &feishutest.LoginUser{OpenID: "ou_li", UnionID: "on_li", Name: "李航"}

	other, err := secret.New("another-secret-key-0123456789")
	if err != nil {
		t.Fatal(err)
	}
	sealed := `"` + other.Seal("sealed-with-the-old-key") + `"`
	for _, stmt := range []string{
		`UPDATE settings SET value = jsonb_set(value, '{app_secret}', '` + sealed + `') WHERE key = 'feishu'`,
		`INSERT INTO settings (key, value) VALUES ('llm', jsonb_build_object('base_url', 'http://llm.example.com/v1', 'model', 'm', 'api_key', ` + strings.ReplaceAll(sealed, `"`, `'`) + `))`,
	} {
		if _, err := c.pool.Exec(t.Context(), stmt); err != nil {
			t.Fatal(err)
		}
	}

	anon := c.another()
	if r := anon.want(anon.do("GET", "/api/status", nil), 200); r.Body["feishu_login"] != false || r.Body["initialized"] != true {
		t.Errorf("没登录的人照样看到登录页，只是没有「用飞书登录」：%s", r.Raw)
	}
	if r := c.want(c.do("GET", "/api/status", nil), 200); r.Body["user"] == nil || r.Body["llm_ready"] != false || r.Body["feishu_ready"] != false {
		t.Errorf("登录着的人照样能用，大模型和飞书当作没配置好：%s", r.Raw)
	}
	if r := li.want(li.do("GET", "/api/me/feishu", nil), 200); r.Body["enabled"] != false {
		t.Errorf("账号页上不能关联飞书：%s", r.Raw)
	}
	if page, code := anon.feishuLogin(fs); page != "/" || code != service.FeishuUnavailable {
		t.Errorf("用飞书登录：%s %q", page, code)
	}
}

package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexedwards/scs/pgxstore"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZaneXo/zsmeet/internal/config"
	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/password"
	"github.com/ZaneXo/zsmeet/internal/queue"
	"github.com/ZaneXo/zsmeet/internal/search"
	"github.com/ZaneXo/zsmeet/internal/secret"
	"github.com/ZaneXo/zsmeet/internal/service"
	"github.com/ZaneXo/zsmeet/internal/settings"
	"github.com/ZaneXo/zsmeet/internal/storage"
	"github.com/ZaneXo/zsmeet/internal/testdb"
	"github.com/ZaneXo/zsmeet/internal/transcript"
	"github.com/ZaneXo/zsmeet/web"
)

// 这里的测试走真实的 HTTP：会话 Cookie、登录校验、参数校验都和线上一样，只是不启动 worker。

// client 是带 Cookie 的测试客户端，相当于一个浏览器。
type client struct {
	t    *testing.T
	base string
	http *http.Client
	pool *pgxpool.Pool
	// 直接调业务层，给那些不经过网页的操作用（服务器上的救急命令）
	svc   *service.Service
	store *storage.Local
	// 不为空时每个请求都带 Authorization: Bearer <bearer>，相当于一个 App
	bearer string
}

// result 是一次请求的结果。Body 是解析后的 JSON（没有响应体时为空）。
type result struct {
	Status int
	Body   map[string]any
	Raw    string
	Header http.Header
}

// newServer 起一个完整的接口服务，返回一个还没登录的客户端。
func newServer(t *testing.T) *client {
	t.Helper()
	pool := testdb.Open(t, "api")
	store, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	box, err := secret.New("test-secret-key-0123456789")
	if err != nil {
		t.Fatal(err)
	}
	inserter, err := queue.NewInserter(pool)
	if err != nil {
		t.Fatal(err)
	}
	svc := service.New(pool, store, settings.New(sqlc.New(pool), box), inserter)
	// 和线上同一份会话配置、同一个存储（sessions 表），测试能数出写了几条会话。不开定时清理
	sess := NewSessions(pgxstore.NewWithCleanupInterval(pool, 0), false)

	// 和线上的默认值一样只信本机：测试客户端从本机连过来，相当于前面有一个同机的反向代理
	proxies, err := config.ParseTrustedProxies("")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewHandler(svc, sess, web.FS(), proxies, "test"))
	t.Cleanup(server.Close)
	c := &client{t: t, base: server.URL, pool: pool, svc: svc, store: store}
	return c.another()
}

// another 返回连着同一个服务、但没有登录的另一个客户端（另一个人的浏览器）。
func (c *client) another() *client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		c.t.Fatal(err)
	}
	return &client{t: c.t, base: c.base, http: &http.Client{Jar: jar}, pool: c.pool, svc: c.svc, store: c.store}
}

// do 发一个请求。body 是 []byte 时按二进制发，其他非 nil 值按 JSON 发。
func (c *client) do(method, path string, body any) result {
	c.t.Helper()
	var reader io.Reader
	contentType := ""
	switch b := body.(type) {
	case nil:
	case []byte:
		reader, contentType = bytes.NewReader(b), "application/octet-stream"
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			c.t.Fatal(err)
		}
		reader, contentType = bytes.NewReader(raw), "application/json"
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		c.t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	r := result{Status: resp.StatusCode, Raw: string(raw), Header: resp.Header}
	if len(raw) > 0 && strings.Contains(resp.Header.Get("Content-Type"), "json") {
		if err := json.Unmarshal(raw, &r.Body); err != nil {
			c.t.Fatalf("%s %s 返回的不是合法的 JSON：%s", method, path, raw)
		}
	}
	return r
}

// post 用指定的 Content-Type 发一段原始内容，测试接口对请求格式的要求时用。
func (c *client) post(path, contentType, body string) result {
	c.t.Helper()
	resp, err := c.http.Post(c.base+path, contentType, strings.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return result{Status: resp.StatusCode, Raw: string(raw)}
}

// want 断言状态码，不对就把响应内容打出来。
func (c *client) want(r result, status int) result {
	c.t.Helper()
	if r.Status != status {
		c.t.Fatalf("状态码应该是 %d，得到 %d：%s", status, r.Status, r.Raw)
	}
	return r
}

type obj = map[string]any

var admin = obj{"email": "admin@example.com", "name": "张敏", "password": "sample-pass-123"}

// setup 创建管理员并保持登录。
func (c *client) setup() {
	c.t.Helper()
	c.want(c.do("POST", "/api/setup", admin), 200)
}

// addMember 直接往数据库里加一个普通成员，返回登录好的客户端。
// 不走邀请流程，给那些只是需要「另一个人」的测试用。
func (c *client) addMember(email, name string) *client {
	c.t.Helper()
	_, err := sqlc.New(c.pool).CreateUser(c.t.Context(), sqlc.CreateUserParams{
		Email: email, Name: name, PasswordHash: password.Hash("member-pass-123"), Role: "member",
	})
	if err != nil {
		c.t.Fatal(err)
	}
	member := c.another()
	member.want(member.do("POST", "/api/login", obj{"email": email, "password": "member-pass-123"}), 200)
	return member
}

func TestSetupLoginLogout(t *testing.T) {
	c := newServer(t)

	status := c.want(c.do("GET", "/api/status", nil), 200)
	if status.Body["initialized"] != false || status.Body["user"] != nil {
		t.Fatalf("全新的系统应该是未初始化、未登录：%s", status.Raw)
	}
	// 没登录时业务接口一律拒绝
	c.want(c.do("GET", "/api/meetings", nil), 401)

	c.want(c.do("POST", "/api/setup", obj{"email": "不是邮箱", "name": "张敏", "password": "sample-pass-123"}), 422)
	c.want(c.do("POST", "/api/setup", obj{"email": "admin@example.com", "name": "张敏", "password": "短"}), 422)
	user := c.want(c.do("POST", "/api/setup", admin), 200)
	if user.Body["role"] != "admin" {
		t.Errorf("第一个用户应该是管理员：%s", user.Raw)
	}
	if strings.Contains(user.Raw, "password") {
		t.Errorf("响应里不应该出现密码相关的字段：%s", user.Raw)
	}

	// 初始化成功后已经是登录状态
	status = c.want(c.do("GET", "/api/status", nil), 200)
	if status.Body["initialized"] != true || status.Body["user"].(obj)["email"] != "admin@example.com" {
		t.Fatalf("初始化后应该已登录：%s", status.Raw)
	}
	c.want(c.do("GET", "/api/meetings", nil), 200)

	// 初始化只能做一次，别人不能再抢一个管理员
	stranger := c.another()
	stranger.want(stranger.do("POST", "/api/setup", obj{"email": "evil@example.com", "name": "路人", "password": "sample-pass-123"}), 409)

	c.want(c.do("POST", "/api/logout", nil), 204)
	c.want(c.do("GET", "/api/meetings", nil), 401)

	// 密码不对和邮箱不存在给同一个提示
	wrong := c.want(c.do("POST", "/api/login", obj{"email": "admin@example.com", "password": "wrong-password"}), 401)
	missing := c.want(c.do("POST", "/api/login", obj{"email": "nobody@example.com", "password": "wrong-password"}), 401)
	if wrong.Body["detail"] != missing.Body["detail"] {
		t.Errorf("两种登录失败的提示应该一样：%q 和 %q", wrong.Body["detail"], missing.Body["detail"])
	}
	c.want(c.do("POST", "/api/login", obj{"email": "Admin@Example.com", "password": "sample-pass-123"}), 200)
	c.want(c.do("GET", "/api/meetings", nil), 200)
}

// 别的网站可以让用户的浏览器向我们提交表单，但表单只能用下面三种内容类型。
// 登录这类不需要 Cookie 的接口如果接受它们，别人就能替用户「登录」成另一个账号。
func TestFormPostsCannotLogIn(t *testing.T) {
	c := newServer(t)
	c.setup()
	c.want(c.do("POST", "/api/logout", nil), 204)

	credentials := `{"email":"admin@example.com","password":"sample-pass-123"}`
	for _, contentType := range []string{"text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x"} {
		if r := c.post("/api/login", contentType, credentials); r.Status != 415 {
			t.Errorf("用 %s 提交登录应该被拒绝（415），得到 %d：%s", contentType, r.Status, r.Raw)
		}
	}
	c.want(c.do("GET", "/api/meetings", nil), 401)
	if r := c.post("/api/login", "application/json", credentials); r.Status != 200 {
		t.Errorf("正常的 JSON 登录应该成功，得到 %d：%s", r.Status, r.Raw)
	}
}

func TestDisabledUserIsLoggedOutImmediately(t *testing.T) {
	c := newServer(t)
	c.setup()
	member := c.addMember("member@example.com", "李航")
	member.want(member.do("GET", "/api/meetings", nil), 200)

	if _, err := c.pool.Exec(t.Context(), "UPDATE users SET disabled = true WHERE email = 'member@example.com'"); err != nil {
		t.Fatal(err)
	}
	member.want(member.do("GET", "/api/meetings", nil), 401)
	member.want(member.do("POST", "/api/login", obj{"email": "member@example.com", "password": "member-pass-123"}), 403)
}

func TestUploadFlow(t *testing.T) {
	c := newServer(t)
	c.setup()

	c.want(c.do("POST", "/api/meetings", obj{"title": "x", "source": "upload", "template": "不存在的模板"}), 422)
	c.want(c.do("POST", "/api/meetings", obj{"title": "x", "source": "feishu"}), 422)

	m := c.want(c.do("POST", "/api/meetings", obj{"title": "产品周会", "source": "upload", "hotwords": "River"}), 201)
	id := m.Body["id"].(string)
	if m.Body["status"] != "uploading" || m.Body["template"] != "general" || m.Body["owner_name"] != "张敏" {
		t.Fatalf("新会议应该在等上传、用默认模板：%s", m.Raw)
	}
	base := "/api/meetings/" + id

	// 第 1 片先到，第 0 片后到；同一片重传一次。都应该没问题
	c.want(c.do("PUT", base+"/chunks/1", []byte("world")), 204)
	c.want(c.do("PUT", base+"/chunks/0", []byte("hello ")), 204)
	c.want(c.do("PUT", base+"/chunks/1", []byte("world")), 204)
	// 空的分片在进到业务逻辑之前就被拒绝了
	c.want(c.do("PUT", base+"/chunks/0", []byte{}), 400)
	c.want(c.do("PUT", base+"/chunks/4000", []byte("x")), 422)

	chunks := c.want(c.do("GET", base+"/chunks", nil), 200)
	if got := chunks.Body["received"].([]any); len(got) != 2 || got[0] != float64(0) || got[1] != float64(1) {
		t.Errorf("应该收到第 0、1 两片：%s", chunks.Raw)
	}

	// 声明有 3 片但只传了 2 片：告诉用户缺哪一片
	missing := c.want(c.do("POST", base+"/complete", obj{"chunks": 3}), 409)
	if !strings.Contains(missing.Body["detail"].(string), "第 3 片") {
		t.Errorf("应该指出缺第 3 片：%s", missing.Raw)
	}

	done := c.want(c.do("POST", base+"/complete", obj{"chunks": 2}), 200)
	if done.Body["status"] != "queued" {
		t.Fatalf("传完应该进入排队：%s", done.Raw)
	}
	var jobs int
	if err := c.pool.QueryRow(t.Context(),
		"SELECT count(*) FROM river_job WHERE kind = 'process_meeting' AND args->>'meeting_id' = $1", id).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 {
		t.Errorf("应该正好有一个后台任务，实际 %d 个", jobs)
	}

	// 已经交给后台了：不能重复提交，也不能再传分片
	c.want(c.do("POST", base+"/complete", obj{"chunks": 2}), 409)
	c.want(c.do("PUT", base+"/chunks/0", []byte("again")), 409)
	// 还没处理完：不能重试、不能编辑纪要、不能重新生成
	c.want(c.do("POST", base+"/retry", nil), 409)
	c.want(c.do("PUT", base+"/minutes", obj{"content": "## 摘要"}), 409)

	// 还没有转写和纪要时返回空结果，不是报错
	transcript := c.want(c.do("GET", base+"/transcript", nil), 200)
	if len(transcript.Body["segments"].([]any)) != 0 {
		t.Errorf("还没转写时应该是空列表：%s", transcript.Raw)
	}
	minutes := c.want(c.do("GET", base+"/minutes", nil), 200)
	if minutes.Body["version"] != float64(0) {
		t.Errorf("还没有纪要时版本应该是 0：%s", minutes.Raw)
	}
	if audio := c.do("GET", base+"/audio", nil); audio.Status != 404 {
		t.Errorf("还没转码时没有录音，应该是 404，得到 %d", audio.Status)
	}
}

// 所有成员都能看全部会议，但只有创建者和管理员能改。
func TestOnlyOwnerAndAdminCanModifyMeeting(t *testing.T) {
	c := newServer(t)
	c.setup()
	owner := c.addMember("owner@example.com", "李航")
	other := c.addMember("other@example.com", "王悦")

	created := owner.want(owner.do("POST", "/api/meetings", obj{"title": "李航的会", "source": "upload"}), 201)
	id := created.Body["id"].(string)
	base := "/api/meetings/" + id
	if created.Body["can_edit"] != true {
		t.Errorf("创建者应该能改自己的会议：%s", created.Raw)
	}

	// 别的成员：看得到，但 can_edit 是 false
	seen := other.want(other.do("GET", base, nil), 200)
	if seen.Body["can_edit"] != false {
		t.Errorf("别的成员不应该能改：%s", seen.Raw)
	}
	other.want(other.do("GET", base+"/transcript", nil), 200)
	other.want(other.do("GET", base+"/minutes", nil), 200)
	other.want(other.do("GET", base+"/chunks", nil), 200)
	list := other.want(other.do("GET", "/api/meetings", nil), 200)
	if items := list.Body["items"].([]any); len(items) != 1 || items[0].(obj)["can_edit"] != false {
		t.Errorf("别的成员的列表里应该有这场会，且不能改：%s", list.Raw)
	}

	// 别的成员的每一种修改都被拒绝，而且是在检查会议状态之前就拒绝
	for name, r := range map[string]result{
		"改标题":  other.do("PATCH", base, obj{"title": "被别人改了"}),
		"传分片":  other.do("PUT", base+"/chunks/0", []byte("data")),
		"提交上传": other.do("POST", base+"/complete", obj{"chunks": 1}),
		"重试":   other.do("POST", base+"/retry", nil),
		"编辑纪要": other.do("PUT", base+"/minutes", obj{"content": "## 被别人改了"}),
		"重新生成": other.do("POST", base+"/minutes/regenerate", obj{"template": "general"}),
	} {
		if r.Status != 403 {
			t.Errorf("别的成员%s应该被拒绝（403），得到 %d：%s", name, r.Status, r.Raw)
		}
	}
	if title := owner.want(owner.do("GET", base, nil), 200).Body["title"]; title != "李航的会" {
		t.Errorf("标题不应该被改掉，现在是 %v", title)
	}

	// 创建者和管理员可以
	owner.want(owner.do("PUT", base+"/chunks/0", []byte("data")), 204)
	owner.want(owner.do("PATCH", base, obj{"title": "李航改的"}), 200)
	byAdmin := c.want(c.do("PATCH", base, obj{"title": "管理员改的"}), 200)
	if byAdmin.Body["can_edit"] != true {
		t.Errorf("管理员应该能改任何会议：%s", byAdmin.Raw)
	}

	// 「我创建的」只列自己的
	count := func(who *client) int {
		t.Helper()
		return len(who.want(who.do("GET", "/api/meetings?mine=true", nil), 200).Body["items"].([]any))
	}
	if count(owner) != 1 || count(other) != 0 || count(c) != 0 {
		t.Errorf("「我创建的」筛选不对：创建者 %d，别的成员 %d，管理员 %d", count(owner), count(other), count(c))
	}
}

func TestMeetingNotFound(t *testing.T) {
	c := newServer(t)
	c.setup()
	for _, path := range []string{
		"/api/meetings/00000000-0000-0000-0000-000000000001",
		"/api/meetings/00000000-0000-0000-0000-000000000001/transcript",
		"/api/meetings/00000000-0000-0000-0000-000000000001/minutes",
		"/api/meetings/00000000-0000-0000-0000-000000000001/chunks",
	} {
		c.want(c.do("GET", path, nil), 404)
	}
	c.want(c.do("GET", "/api/meetings/不是合法的ID", nil), 422)
	c.want(c.do("POST", "/api/meetings/00000000-0000-0000-0000-000000000001/retry", nil), 404)
}

func TestRenameAndSearch(t *testing.T) {
	c := newServer(t)
	c.setup()
	a := c.want(c.do("POST", "/api/meetings", obj{"title": "产品周会", "source": "upload"}), 201).Body["id"].(string)
	c.want(c.do("POST", "/api/meetings", obj{"title": "Design Review", "source": "upload"}), 201)

	count := func(q string) int {
		t.Helper()
		r := c.want(c.do("GET", "/api/meetings?q="+q, nil), 200)
		return len(r.Body["items"].([]any))
	}
	if n := count(""); n != 2 {
		t.Fatalf("不搜索时应该列出 2 场，得到 %d", n)
	}
	for q, want := range map[string]int{"周会": 1, "design": 1, "REVIEW": 1, "火星": 0, "%EF%BC%8C": 0} {
		if n := count(q); n != want {
			t.Errorf("搜 %q 应该找到 %d 场，得到 %d", q, want, n)
		}
	}

	c.want(c.do("PATCH", "/api/meetings/"+a, obj{"title": ""}), 422)
	renamed := c.want(c.do("PATCH", "/api/meetings/"+a, obj{"title": "季度规划会"}), 200)
	if renamed.Body["title"] != "季度规划会" {
		t.Errorf("标题没有改过来：%s", renamed.Raw)
	}
	// 改名以后按新标题搜得到，按旧标题搜不到
	if count("规划") != 1 || count("周会") != 0 {
		t.Error("改名后搜索索引没有更新")
	}
}

func TestSettingsAreAdminOnly(t *testing.T) {
	c := newServer(t)
	c.setup()
	member := c.addMember("member@example.com", "李航")

	// 普通成员能用系统，但看不到也改不了团队配置
	member.want(member.do("GET", "/api/templates", nil), 200)
	member.want(member.do("GET", "/api/settings/llm", nil), 403)
	member.want(member.do("PUT", "/api/settings/llm", obj{"base_url": "http://evil.example/v1", "model": "m"}), 403)
	member.want(member.do("POST", "/api/settings/llm/test", obj{"base_url": "http://evil.example/v1", "model": "m"}), 403)
	member.want(member.do("POST", "/api/settings/llm/models", obj{"base_url": "http://evil.example/v1"}), 403)

	c.want(c.do("PUT", "/api/settings/llm", obj{"base_url": "ftp://x", "model": "m"}), 422)
	saved := c.want(c.do("PUT", "/api/settings/llm", obj{"base_url": "http://llm.example/v1/", "model": " qwen ", "api_key": "sk-sample-000"}), 200)
	if saved.Body["base_url"] != "http://llm.example/v1" || saved.Body["model"] != "qwen" || saved.Body["api_key_set"] != true {
		t.Errorf("保存结果不对：%s", saved.Raw)
	}
	if strings.Contains(saved.Raw, "sk-sample-000") {
		t.Errorf("API Key 不应该回传给网页：%s", saved.Raw)
	}

	// Key 在数据库里是加密的
	var stored string
	if err := c.pool.QueryRow(t.Context(), "SELECT value::text FROM settings WHERE key = 'llm'").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "sk-sample-000") {
		t.Errorf("API Key 明文进了数据库：%s", stored)
	}

	// 改别的字段、不填 Key 时沿用原来的 Key
	updated := c.want(c.do("PUT", "/api/settings/llm", obj{"base_url": "http://llm.example/v1", "model": "qwen-max"}), 200)
	if updated.Body["api_key_set"] != true || updated.Body["model"] != "qwen-max" {
		t.Errorf("不填 Key 时应该保留原来的：%s", updated.Raw)
	}

	// 配好以后，登录用户能从状态接口知道大模型已就绪
	status := member.want(member.do("GET", "/api/status", nil), 200)
	if status.Body["llm_ready"] != true {
		t.Errorf("状态接口应该显示大模型已配置：%s", status.Raw)
	}
}

// 上下文长度：以前保存的配置里没有这一项，按 32K 算；只能选列表里的几档；保存以后读出来。
func TestLLMContextTokens(t *testing.T) {
	c := newServer(t)
	c.setup()
	contextOf := func(r result) any { return r.Body["context_tokens"] }

	// 老版本存的配置没有 context_tokens
	if _, err := c.pool.Exec(t.Context(), `INSERT INTO settings (key, value) VALUES ('llm', '{"base_url":"http://llm.example/v1","model":"qwen","api_key":""}')`); err != nil {
		t.Fatal(err)
	}
	if r := c.want(c.do("GET", "/api/settings/llm", nil), 200); contextOf(r) != float64(32_768) {
		t.Errorf("没设置过时按 32K：%s", r.Raw)
	}
	if cfg, err := c.svc.LLMSettings(t.Context()); err != nil || cfg.ContextTokens != 0 || cfg.Context() != 32_768 {
		t.Errorf("老配置读出来按 32K 算：%+v %v", cfg, err)
	}

	body := func(tokens any) obj {
		return obj{"base_url": "http://llm.example/v1", "model": "qwen", "context_tokens": tokens}
	}
	for _, bad := range []any{1, 4_096, 32_000, 1_000_000, -8_192} {
		r := c.want(c.do("PUT", "/api/settings/llm", body(bad)), 422)
		if !strings.Contains(r.Raw, "上下文长度只能从列表里选") {
			t.Errorf("%v：不在列表里的要说清楚：%s", bad, r.Raw)
		}
		c.want(c.do("POST", "/api/settings/llm/test", body(bad)), 422)
	}
	if r := c.want(c.do("GET", "/api/settings/llm", nil), 200); contextOf(r) != float64(32_768) {
		t.Errorf("不合法的值不能存进去：%s", r.Raw)
	}

	if r := c.want(c.do("PUT", "/api/settings/llm", body(8_192)), 200); contextOf(r) != float64(8_192) {
		t.Errorf("保存的结果：%s", r.Raw)
	}
	if r := c.want(c.do("GET", "/api/settings/llm", nil), 200); contextOf(r) != float64(8_192) {
		t.Errorf("保存以后读出来：%s", r.Raw)
	}
	if cfg, err := c.svc.LLMSettings(t.Context()); err != nil || cfg.Context() != 8_192 {
		t.Errorf("写纪要、问会议用的配置：%+v %v", cfg, err)
	}

	// 不填（老的脚本）按默认的 32K
	if r := c.want(c.do("PUT", "/api/settings/llm", obj{"base_url": "http://llm.example/v1", "model": "qwen"}), 200); contextOf(r) != float64(32_768) {
		t.Errorf("不填时按 32K：%s", r.Raw)
	}
}

func TestWebAndUnknownAPIPaths(t *testing.T) {
	c := newServer(t)

	// 前端路由的地址都返回网页，由前端自己决定显示什么
	page := c.do("GET", "/meetings/anything", nil)
	if page.Status != 200 || !strings.Contains(page.Raw, "<!doctype html>") {
		t.Errorf("前端路由的地址应该返回网页，得到 %d", page.Status)
	}
	// 不存在的接口返回 404，而不是网页
	if r := c.do("GET", "/api/nope", nil); r.Status != 404 || strings.Contains(r.Raw, "<!doctype html>") {
		t.Errorf("不存在的接口应该是 404，得到 %d：%s", r.Status, r.Raw)
	}
	// 接口文档不需要登录
	if r := c.do("GET", "/api/openapi.json", nil); r.Status != 200 || !strings.Contains(r.Raw, "/api/meetings/{id}/chunks/{seq}") {
		t.Errorf("接口文档应该能直接访问，得到 %d", r.Status)
	}
}

// web/openapi.json 是提交进仓库的接口文档，前端的 TypeScript 类型由它生成。
// 接口改了却没有重新生成时，这个测试会失败。
func TestOpenAPIFileIsUpToDate(t *testing.T) {
	want, err := OpenAPI("dev")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("../../web/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != strings.TrimSpace(string(want)) {
		t.Fatal("接口有改动，但 web/openapi.json 还是旧的。请运行 make gen，把生成的文件一起提交")
	}
}

// 接口文档是前端类型的来源：每个接口都要有唯一的 ID 和中文说明。
func TestOpenAPIIsComplete(t *testing.T) {
	raw, err := OpenAPI("test")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
			Summary     string `json:"summary"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for path, methods := range spec.Paths {
		for method, op := range methods {
			where := strings.ToUpper(method) + " " + path
			if op.OperationID == "" || op.Summary == "" {
				t.Errorf("%s 缺少 OperationID 或 Summary", where)
			}
			if other, dup := seen[op.OperationID]; dup {
				t.Errorf("%s 和 %s 的 OperationID 重复：%s", where, other, op.OperationID)
			}
			seen[op.OperationID] = where
		}
	}
	if len(seen) < 20 {
		t.Errorf("接口数量不对，只有 %d 个", len(seen))
	}
}

// 网页录音：每收到一片都记下时间（网页据此显示录音还在不在进行），结束时带上标记的重点。
func TestRecordingChunkTimeAndMarks(t *testing.T) {
	c := newServer(t)
	c.setup()
	m := c.want(c.do("POST", "/api/meetings", obj{"title": "站会", "source": "record"}), 201)
	if _, ok := m.Body["last_chunk_at"]; ok || len(m.Body["marks"].([]any)) != 0 {
		t.Errorf("新建的会议还没收到分片、没有标记：%s", m.Raw)
	}
	base := "/api/meetings/" + m.Body["id"].(string)

	c.want(c.do("PUT", base+"/chunks/0", []byte("webm 的开头")), 204)
	got := c.want(c.do("GET", base, nil), 200)
	at, err := time.Parse(time.RFC3339Nano, fmt.Sprint(got.Body["last_chunk_at"]))
	if err != nil || time.Since(at).Abs() > time.Minute {
		t.Errorf("收到分片后应该记下时间：%s", got.Raw)
	}
	list := c.want(c.do("GET", "/api/meetings", nil), 200)
	if item := list.Body["items"].([]any)[0].(map[string]any); item["last_chunk_at"] == nil {
		t.Errorf("列表里也要有最后收到分片的时间：%s", list.Raw)
	}

	// 标记的时间不对、数量太多：拒绝，会议还在等上传
	c.want(c.do("POST", base+"/complete", obj{"chunks": 1, "marks": []float64{-1}}), 422)
	c.want(c.do("POST", base+"/complete", obj{"chunks": 1, "marks": []float64{7 * 3600}}), 422)
	c.want(c.do("POST", base+"/complete", obj{"chunks": 1, "marks": make([]float64, 501)}), 422)
	if got := c.want(c.do("GET", base, nil), 200); got.Body["status"] != "uploading" {
		t.Fatalf("被拒绝后会议应该还在等上传：%s", got.Raw)
	}

	// 乱序、连着点了两下的：排好序，隔不到一秒的算一个
	done := c.want(c.do("POST", base+"/complete", obj{"chunks": 1, "marks": []float64{65.04, 3, 65.5, 3.2}}), 200)
	if marks := fmt.Sprint(done.Body["marks"]); done.Body["status"] != "queued" || marks != "[3 65]" {
		t.Errorf("应该存下 [3 65] 并开始处理：%s", done.Raw)
	}
}

// 列出接口上的模型：用网页上填的地址；Key 留空时用已保存的。
func TestListLLMModels(t *testing.T) {
	c := newServer(t)
	c.setup()
	var auth string
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[{"id":"qwen-max"},{"id":"deepseek-chat"}]}`))
	}))
	defer llm.Close()

	c.want(c.do("PUT", "/api/settings/llm", obj{"base_url": llm.URL + "/v1", "model": "qwen-max", "api_key": "sk-sample-000"}), 200)

	// 还没填模型名也能列；Key 留空用已保存的
	got := c.want(c.do("POST", "/api/settings/llm/models", obj{"base_url": llm.URL + "/v1"}), 200)
	if fmt.Sprint(got.Body["models"]) != "[deepseek-chat qwen-max]" || auth != "Bearer sk-sample-000" {
		t.Errorf("应该用已保存的 Key 列出排好序的模型：%s（Authorization: %q）", got.Raw, auth)
	}
	// 填了新 Key 就用新的
	c.want(c.do("POST", "/api/settings/llm/models", obj{"base_url": llm.URL + "/v1", "api_key": "sk-sample-111"}), 200)
	if auth != "Bearer sk-sample-111" {
		t.Errorf("应该用网页上新填的 Key，实际 %q", auth)
	}

	c.want(c.do("POST", "/api/settings/llm/models", obj{"base_url": "ftp://x"}), 422)
	// 接口不支持列模型（这里是地址少了 /v1）：返回原因，界面上照样可以手填
	notSupported := c.want(c.do("POST", "/api/settings/llm/models", obj{"base_url": llm.URL}), 422)
	if !strings.Contains(notSupported.Raw, "不支持列出模型") {
		t.Errorf("应该说明可以直接填写模型名：%s", notSupported.Raw)
	}
}

// 录音过程中保存标记：从会议页面收尾（不带标记）时也不会丢；交给后台以后不能再改。
func TestMarksSavedWhileRecording(t *testing.T) {
	c := newServer(t)
	c.setup()
	other := c.addMember("member@example.com", "李航")
	m := c.want(c.do("POST", "/api/meetings", obj{"title": "站会", "source": "record"}), 201)
	base := "/api/meetings/" + m.Body["id"].(string)

	other.want(other.do("PUT", base+"/marks", obj{"marks": []float64{5}}), 403)
	c.want(c.do("PUT", base+"/marks", obj{"marks": []float64{-1}}), 422)
	c.want(c.do("PUT", base+"/marks", obj{"marks": []float64{20, 5, 5.3}}), 204)
	if got := c.want(c.do("GET", base, nil), 200); fmt.Sprint(got.Body["marks"]) != "[5 20]" {
		t.Errorf("标记应该存下来：%s", got.Raw)
	}

	c.want(c.do("PUT", base+"/chunks/0", []byte("webm 的开头")), 204)
	done := c.want(c.do("POST", base+"/complete", obj{"chunks": 1}), 200)
	if fmt.Sprint(done.Body["marks"]) != "[5 20]" {
		t.Errorf("收尾时没带标记，应该保留录音过程中存下的：%s", done.Raw)
	}
	c.want(c.do("PUT", base+"/marks", obj{"marks": []float64{1}}), 409)
}

// processed 把一场会议直接写成「处理完了」：有逐句的转写、当前纪要、术语表，并重建搜索索引。
// 接口测试里不跑识别，用它造数据。
func (c *client) processed(title string, segs []transcript.Segment, minutes string, terms string) string {
	c.t.Helper()
	id := c.want(c.do("POST", "/api/meetings", obj{"title": title, "source": "upload"}), 201).Body["id"].(string)
	raw, err := json.Marshal(segs)
	if err != nil {
		c.t.Fatal(err)
	}
	ctx := c.t.Context()
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO transcripts (meeting_id, segments) VALUES ($1, $2)", []any{id, raw}},
		{"INSERT INTO minutes_versions (meeting_id, version, content, source) VALUES ($1, 1, $2, 'auto')", []any{id, minutes}},
		{"UPDATE meetings SET status = 'done', current_version = 1, terms = $2, audio_key = 'audio/' || id || '.m4a' WHERE id = $1", []any{id, terms}},
	} {
		if _, err := c.pool.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			c.t.Fatal(err)
		}
	}
	if err := search.Reindex(ctx, sqlc.New(c.pool), uuid.MustParse(id)); err != nil {
		c.t.Fatal(err)
	}
	return id
}

// 搜索：每场会议带上命中的句子和时间点；会议术语表里的听错写法一起搜；长词允许错一个字；
// 索引粗筛进来、但逐句其实找不到的会议不要。
func TestSearchHitsGlossaryAndFuzzy(t *testing.T) {
	c := newServer(t)
	c.setup()
	// A 的术语表记着「飞书」被听成过「非书」
	a := c.processed("产品周会", []transcript.Segment{
		{Start: 3, End: 6, Speaker: 0, Text: "飞书集成放到下一个版本。"},
	}, "## 结论\n- [00:03] 飞书集成放到下一个版本，由王悦负责调研。", `[{"term": "飞书", "variants": ["非书"]}]`)
	// B 的转写里只有听错的「非书」，纪要里也没提
	b := c.processed("技术评审", []transcript.Segment{
		{Start: 1, End: 4, Speaker: 0, Text: "今天先过一下排期。"},
		{Start: 12.5, End: 15, Speaker: 1, Text: "非书的接口文档我来看。"},
	}, "## 摘要\n过了排期。", `[]`)
	// C 里「网页录音」被识别错了一个字
	cc := c.processed("站会", []transcript.Segment{{Start: 30, End: 33, Speaker: 0, Text: "网页录因的功能做完了。"}}, "## 摘要\n好。", `[]`)
	// D 的「网页」和「录音」在相邻两句里，索引里连起来了，但没有一句话里真的有「网页录音」
	c.processed("周报", []transcript.Segment{{Start: 1, Text: "我们看了网页"}, {Start: 2, Text: "录音设备的预算。"}}, "## 摘要\n好。", `[]`)

	find := func(q string) result {
		t.Helper()
		return c.want(c.do("GET", "/api/meetings?q="+url.QueryEscape(q), nil), 200)
	}
	items := func(r result) map[string][]any {
		out := map[string][]any{}
		for _, it := range r.Body["items"].([]any) {
			m := it.(map[string]any)
			out[m["id"].(string)] = m["hits"].([]any)
		}
		return out
	}

	r := find("飞书")
	if fmt.Sprint(r.Body["expanded"]) != "[飞书 非书]" {
		t.Errorf("要用术语表扩展出「非书」：%s", r.Raw)
	}
	got := items(r)
	if len(got) != 2 || got[a] == nil || got[b] == nil {
		t.Fatalf("应该找到 A 和 B 两场：%s", r.Raw)
	}
	hit := got[b][0].(map[string]any)
	if hit["start"] != 12.5 || fmt.Sprint(hit["matches"]) != "[非书]" || hit["source"] != "transcript" || hit["speaker"] != float64(1) || hit["fuzzy"] != false {
		t.Errorf("B 里命中的是 12.5 秒那句里的「非书」：%v", hit)
	}
	if hits := got[a]; len(hits) != 2 || hits[1].(map[string]any)["source"] != "minutes" || hits[1].(map[string]any)["start"] != float64(3) {
		t.Errorf("A 里转写和纪要各命中一处，纪要那行带着时间点：%v", hits)
	}

	got = items(find("网页录音"))
	if len(got) != 1 || got[cc] == nil || fmt.Sprint(got[cc][0].(map[string]any)["matches"]) != "[网页录因]" || got[cc][0].(map[string]any)["fuzzy"] != true {
		t.Errorf("只该找到错了一个字的 C（标成近似），D 是索引里的误命中：%v", got)
	}
	if r := find("！？"); len(r.Body["items"].([]any)) != 0 {
		t.Errorf("全是标点什么也找不到：%s", r.Raw)
	}
	// 不搜索时没有命中信息
	if r := c.want(c.do("GET", "/api/meetings", nil), 200); fmt.Sprint(r.Body["expanded"]) != "[]" {
		t.Errorf("不搜索时 expanded 是空的：%s", r.Raw)
	}
}

// 搜索时中文和英文之间有没有空格都一样，几个词用空格隔开时每个词都出现就算；术语表也按这个口径扩展。
func TestSearchSpacesAndSeveralWords(t *testing.T) {
	c := newServer(t)
	c.setup()
	goMeeting := c.processed("技术评审", []transcript.Segment{
		{Start: 2, Text: "我们用Go语言写后端。"},
		{Start: 6, Text: "勾语言的并发很好用。"},
	}, "## 结论\n- [00:02] 后端用 Go 语言", `[{"term": "Go 语言", "variants": ["勾语言", "勾", "用Go语言"]}]`)
	release := c.processed("发布会", []transcript.Segment{{Start: 1, Text: "明天部署。"}, {Start: 3, Text: "周五上线。"}}, "## 摘要\n好。", `[]`)

	find := func(q string) result {
		t.Helper()
		return c.want(c.do("GET", "/api/meetings?q="+url.QueryEscape(q), nil), 200)
	}
	for _, q := range []string{"Go 语言", "Go语言", "go语言"} {
		r := find(q)
		items := r.Body["items"].([]any)
		if len(items) != 1 || items[0].(map[string]any)["id"] != goMeeting || fmt.Sprint(r.Body["expanded"]) != "["+q+" 勾语言]" {
			t.Errorf("搜「%s」要找到技术评审，并用术语表扩展出「勾语言」（「勾」只有一个字、「用Go语言」包含了要搜的词，都不用）：%s", q, r.Raw)
			continue
		}
		hits := items[0].(map[string]any)["hits"].([]any)
		if len(hits) != 3 || fmt.Sprint(hits[0].(map[string]any)["matches"]) != "[Go语言]" || fmt.Sprint(hits[1].(map[string]any)["matches"]) != "[勾语言]" {
			t.Errorf("搜「%s」的命中不对：%v", q, hits)
		}
	}
	r := find("部署 上线")
	if items := r.Body["items"].([]any); len(items) != 1 || items[0].(map[string]any)["id"] != release || len(items[0].(map[string]any)["hits"].([]any)) != 2 {
		t.Errorf("「部署」「上线」在不同的句子里也算搜到：%s", r.Raw)
	}
	if r := find("部署 下线"); len(r.Body["items"].([]any)) != 0 {
		t.Errorf("少一个词就不算：%s", r.Raw)
	}
}

// 搜索不会因为候选太多漏掉老会议：索引粗筛出的误命中再多，也要一页一页查下去。
func TestSearchPagesThroughCandidates(t *testing.T) {
	c := newServer(t)
	c.setup()
	old := c.processed("机房周会", []transcript.Segment{{Start: 4, Text: "下周开始数据中心迁移。"}}, "## 摘要\n好。", `[]`)
	// 130 场更新的会议，「数据」「据中」「中心」都有，但没有一句话里真的说了「数据中心」
	_, err := c.pool.Exec(t.Context(), `
		WITH m AS (
			INSERT INTO meetings (owner_id, title, source, status, created_at)
			SELECT (SELECT id FROM users WHERE email = $1), '周会 ' || i, 'upload', 'done', now() + i * interval '1 second'
			FROM generate_series(1, 130) AS i
			RETURNING id
		), t AS (
			INSERT INTO transcripts (meeting_id, segments)
			SELECT id, '[{"start":1,"end":2,"speaker":0,"text":"数据很多。"},{"start":3,"end":4,"speaker":0,"text":"据中方消息。"},{"start":5,"end":6,"speaker":0,"text":"中心在北京。"}]'::jsonb FROM m
		)
		INSERT INTO meeting_search (meeting_id, doc)
		SELECT id, array_to_tsvector(ARRAY['数','据','中','心','数据','据中','中心']) FROM m`, admin["email"])
	if err != nil {
		t.Fatal(err)
	}
	r := c.want(c.do("GET", "/api/meetings?limit=1&q="+url.QueryEscape("数据中心"), nil), 200)
	if items := r.Body["items"].([]any); len(items) != 1 || items[0].(map[string]any)["id"] != old || r.Body["partial"] != false {
		t.Errorf("应该翻过 130 个误命中找到老会议：%s", r.Raw)
	}
}

// 重新转写：删掉旧的转写、重新入队；录音不在、正在处理、没有权限的都不行。
func TestRetranscribe(t *testing.T) {
	c := newServer(t)
	c.setup()
	other := c.addMember("member@example.com", "李航")
	id := c.processed("产品周会", []transcript.Segment{{Start: 0, End: 2, Text: "旧的转写。"}}, "## 摘要\n好。", `[]`)
	base := "/api/meetings/" + id

	// 录音文件还没有：不能重新转写
	if r := c.want(c.do("POST", base+"/retranscribe", nil), 409); !strings.Contains(r.Raw, "录音已经不在了") {
		t.Errorf("录音文件不在时要说明：%s", r.Raw)
	}
	if err := os.MkdirAll(c.store.Path("audio"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.store.Path("audio/"+id+".m4a"), []byte("m4a"), 0o644); err != nil {
		t.Fatal(err)
	}

	other.want(other.do("POST", base+"/retranscribe", nil), 403)
	r := c.want(c.do("POST", base+"/retranscribe", nil), 200)
	if r.Body["status"] != "queued" {
		t.Errorf("应该进入排队：%s", r.Raw)
	}
	if tr := c.want(c.do("GET", base+"/transcript", nil), 200); len(tr.Body["segments"].([]any)) != 1 {
		t.Errorf("旧的转写要留到新的存好为止：%s", tr.Raw)
	}
	var flagged bool
	if err := c.pool.QueryRow(t.Context(), "SELECT retranscribe FROM meetings WHERE id = $1", id).Scan(&flagged); err != nil || !flagged {
		t.Errorf("要在会议上记下「重新识别」：%v %v", flagged, err)
	}
	var jobs int
	if err := c.pool.QueryRow(t.Context(), "SELECT count(*) FROM river_job WHERE args->>'meeting_id' = $1", id).Scan(&jobs); err != nil || jobs != 1 {
		t.Errorf("应该有一个后台任务：%d %v", jobs, err)
	}
	// 正在处理时不能再来一次
	c.want(c.do("POST", base+"/retranscribe", nil), 409)

	// 录音已经不在了
	gone := c.processed("没录音的会", []transcript.Segment{{Text: "好。"}}, "## 摘要", `[]`)
	if _, err := c.pool.Exec(t.Context(), "UPDATE meetings SET audio_deleted_at = now() WHERE id = $1", gone); err != nil {
		t.Fatal(err)
	}
	if r := c.want(c.do("POST", "/api/meetings/"+gone+"/retranscribe", nil), 409); !strings.Contains(r.Raw, "录音已经不在了") {
		t.Errorf("要说明录音不在了：%s", r.Raw)
	}
}

// loginFrom 用指定的 X-Forwarded-For 登录（模拟经过反向代理、来自某个外网 IP 的请求），返回状态码和响应。
func (c *client) loginFrom(forwarded, email, pass string) result {
	c.t.Helper()
	raw, _ := json.Marshal(obj{"email": email, "password": pass})
	req, err := http.NewRequest("POST", c.base+"/api/login", bytes.NewReader(raw))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if forwarded != "" {
		req.Header.Set("X-Forwarded-For", forwarded)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return result{Status: resp.StatusCode, Raw: string(body)}
}

// 登录限流：同一个邮箱错 5 次锁住（密码对了也不行），同一个 IP 错 20 次锁住；别人、别处不受影响。
func TestLoginRateLimit(t *testing.T) {
	c := newServer(t)
	c.setup()
	c.addMember("member@example.com", "李航")
	email, good := admin["email"].(string), admin["password"].(string)
	stranger := c.another()

	const strangerIP = "198.51.100.20"
	for i := range 5 {
		if r := stranger.loginFrom(strangerIP, email, "wrong-pass-"+fmt.Sprint(i)); r.Status != 401 {
			t.Fatalf("第 %d 次输错应该是 401：%d %s", i+1, r.Status, r.Raw)
		}
	}
	locked := stranger.loginFrom(strangerIP, email, good)
	if locked.Status != 429 || !strings.Contains(locked.Raw, "分钟后再试") {
		t.Fatalf("错满 5 次以后，密码对了也要先等一等：%d %s", locked.Status, locked.Raw)
	}
	// 同一个 IP 上别人的账号照常登录（这个 IP 才错了 5 次）
	if r := stranger.loginFrom(strangerIP, "member@example.com", "member-pass-123"); r.Status != 200 {
		t.Errorf("别的账号不该受影响：%d %s", r.Status, r.Raw)
	}

	// 锁过期以后可以登录，登录成功后这个邮箱重新计数
	if _, err := c.pool.Exec(t.Context(), "UPDATE login_failures SET locked_until = now() - interval '1 second', window_start = now() - interval '16 minutes' WHERE key = $1", "email:"+email); err != nil {
		t.Fatal(err)
	}
	if r := stranger.loginFrom("", email, good); r.Status != 200 {
		t.Fatalf("锁过期以后应该能登录：%d %s", r.Status, r.Raw)
	}
	var left int
	if err := c.pool.QueryRow(t.Context(), "SELECT count(*) FROM login_failures WHERE key = $1", "email:"+email).Scan(&left); err != nil || left != 0 {
		t.Errorf("登录成功后邮箱的计数要清掉：%d %v", left, err)
	}

	// 经过代理、来自同一个外网 IP 的请求，换着邮箱猜也会被锁
	attacker := c.another()
	for i := range 20 {
		attacker.loginFrom("203.0.113.9", fmt.Sprintf("guess%d@example.com", i), "x-wrong-pass")
	}
	if r := attacker.loginFrom("203.0.113.9", "member@example.com", "member-pass-123"); r.Status != 429 {
		t.Errorf("这个 IP 错了 20 次，应该被锁：%d %s", r.Status, r.Raw)
	}
	// 别的地方照常登录
	if r := attacker.loginFrom("198.51.100.7", "member@example.com", "member-pass-123"); r.Status != 200 {
		t.Errorf("别的 IP 不受影响：%d %s", r.Status, r.Raw)
	}

	// 登录成功不算在 IP 头上：公司出口只有一个 IP，大家都登录也不会被锁
	office := c.another()
	for i := range 25 {
		if r := office.loginFrom("198.51.100.30", "member@example.com", "member-pass-123"); r.Status != 200 {
			t.Fatalf("第 %d 次登录成功也被算进了 IP 的次数：%d %s", i+1, r.Status, r.Raw)
		}
	}
}

// 同一个邮箱同时来一大批错误的登录：只有上限那么多个去校验密码（401），其余直接拒绝（429），
// 不会因为「先查锁、再算哈希、最后才记次数」让一批请求全都溜过去。
// 锁着的时候密码对了也不行；在服务器上执行 zsmeet unlock、或者用重置密码的链接设了新密码，就能马上登录。
func TestLoginConcurrentLimit(t *testing.T) {
	c := newServer(t)
	c.setup()
	email, good := admin["email"].(string), admin["password"].(string)

	lockOut := func() {
		t.Helper()
		var wg sync.WaitGroup
		var mu sync.Mutex
		codes := map[int]int{}
		for i := range 50 {
			wg.Go(func() {
				raw, _ := json.Marshal(obj{"email": email, "password": fmt.Sprint("wrong-pass-", i)})
				resp, err := http.Post(c.base+"/api/login", "application/json", bytes.NewReader(raw))
				if err != nil {
					t.Error(err)
					return
				}
				resp.Body.Close()
				mu.Lock()
				codes[resp.StatusCode]++
				mu.Unlock()
			})
		}
		wg.Wait()
		if codes[401] != 5 || codes[429] != 45 {
			t.Fatalf("50 个同时输错的请求，应该正好 5 个 401、45 个 429：%v", codes)
		}
		if r := c.another().loginFrom("", email, good); r.Status != 429 {
			t.Fatalf("锁着的时候密码对了也要等：%d %s", r.Status, r.Raw)
		}
	}

	lockOut()
	if _, _, err := c.svc.UnlockLogin(t.Context(), " Admin@Example.com "); err != nil {
		t.Fatal(err)
	}
	if r := c.another().loginFrom("", email, good); r.Status != 200 {
		t.Fatalf("zsmeet unlock 以后应该能登录：%d %s", r.Status, r.Raw)
	}
	if _, _, err := c.svc.UnlockLogin(t.Context(), "nobody@example.com"); err == nil {
		t.Error("没有这个账号时 unlock 应该报错")
	}

	lockOut()
	token, _, err := c.svc.RecoverResetLink(t.Context(), email)
	if err != nil {
		t.Fatal(err)
	}
	visitor := c.another()
	visitor.want(visitor.do("POST", "/api/account-links/accept", obj{"token": token, "password": "brand-new-pass-1"}), 200)
	if r := c.another().loginFrom("", email, "brand-new-pass-1"); r.Status != 200 {
		t.Fatalf("用重置链接设了新密码以后应该能登录：%d %s", r.Status, r.Raw)
	}
}

// IP 被锁住以后，换着邮箱来试也不会在表里给每个邮箱留一条记录。
func TestLockedIPLeavesNoRows(t *testing.T) {
	c := newServer(t)
	c.setup()
	attacker := c.another()
	for i := range 20 {
		attacker.loginFrom("203.0.113.66", fmt.Sprintf("guess%d@example.com", i), "wrong-password-1")
	}
	var before int
	if err := c.pool.QueryRow(t.Context(), "SELECT count(*) FROM login_failures").Scan(&before); err != nil {
		t.Fatal(err)
	}
	for i := range 30 {
		if r := attacker.loginFrom("203.0.113.66", fmt.Sprintf("spray%d@example.com", i), "wrong-password-1"); r.Status != 429 {
			t.Fatalf("IP 锁住以后要直接拒绝：%d", r.Status)
		}
	}
	var after int
	if err := c.pool.QueryRow(t.Context(), "SELECT count(*) FROM login_failures").Scan(&after); err != nil || after != before {
		t.Errorf("被拒绝的登录不该再留下记录：之前 %d 条，之后 %d 条 %v", before, after, err)
	}
}

// 录音保留期：默认 90 天；只有管理员能改；范围不对时说清楚；录音删了以后会议上看得出来。
func TestRetentionSettings(t *testing.T) {
	c := newServer(t)
	c.setup()
	member := c.addMember("member@example.com", "李航")
	member.want(member.do("GET", "/api/settings/retention", nil), 403)
	if r := c.want(c.do("GET", "/api/settings/retention", nil), 200); r.Body["audio_days"] != float64(90) {
		t.Errorf("默认 90 天：%s", r.Raw)
	}
	if r := c.want(c.do("PUT", "/api/settings/retention", obj{"audio_days": 3}), 422); !strings.Contains(r.Raw, "7 到 3650") {
		t.Errorf("太短：%s", r.Raw)
	}
	if r := c.want(c.do("PUT", "/api/settings/retention", obj{"audio_days": 0}), 200); r.Body["audio_days"] != float64(0) {
		t.Errorf("一直保留：%s", r.Raw)
	}
	c.want(c.do("PUT", "/api/settings/retention", obj{"audio_days": 30}), 200)
	if r := c.want(c.do("GET", "/api/settings/retention", nil), 200); r.Body["audio_days"] != float64(30) {
		t.Errorf("保存以后读出来：%s", r.Raw)
	}

	id := c.processed("产品周会", []transcript.Segment{{Start: 0, End: 2, Text: "好。"}}, "## 摘要\n好。", `[]`)
	if _, err := c.pool.Exec(t.Context(), "UPDATE meetings SET audio_deleted_at = now() WHERE id = $1", id); err != nil {
		t.Fatal(err)
	}
	m := c.want(c.do("GET", "/api/meetings/"+id, nil), 200)
	if m.Body["has_audio"] != false || m.Body["audio_deleted_at"] == nil {
		t.Errorf("录音删了以后：%s", m.Raw)
	}
}

func TestClientIP(t *testing.T) {
	loopback, err := config.ParseTrustedProxies("")
	if err != nil {
		t.Fatal(err)
	}
	docker, err := config.ParseTrustedProxies("172.16.0.0/12, 10.0.0.2 ::1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.ParseTrustedProxies("10.0.0.0/8,not-an-ip"); err == nil {
		t.Error("写错的 ZSMEET_TRUSTED_PROXIES 应该报错")
	}
	for _, tc := range []struct {
		remote    string
		forwarded []string
		proxies   []netip.Prefix
		want      string
	}{
		{"203.0.113.9:5000", []string{"1.2.3.4"}, loopback, "203.0.113.9"},                // 不是信任的代理：不信 X-Forwarded-For
		{"192.168.1.20:5000", []string{"1.2.3.4"}, loopback, ""},                          // 内网地址带着这个头：多半是没配成可信的代理，不按 IP 计数
		{"192.168.1.20:5000", nil, loopback, "192.168.1.20"},                              // 内网直连
		{"127.0.0.1:5000", []string{"203.0.113.9"}, loopback, "203.0.113.9"},              // 同机的代理
		{"127.0.0.1:5000", []string{"1.2.3.4, 192.168.1.20"}, loopback, "192.168.1.20"},   // 左边是访问者自己填的，取代理加的最右边那个
		{"127.0.0.1:5000", []string{"1.2.3.4", "192.168.1.20"}, loopback, "192.168.1.20"}, // 多行 X-Forwarded-For 按顺序拼起来
		{"127.0.0.1:5000", []string{"203.0.113.9, 127.0.0.1"}, loopback, "203.0.113.9"},   // 跳过信任的代理
		{"127.0.0.1:5000", []string{"127.0.0.1"}, loopback, "127.0.0.1"},                  // 在本机通过代理打开
		{"127.0.0.1:5000", []string{"203.0.113.9:4711"}, loopback, "203.0.113.9"},         // 带端口的
		{"127.0.0.1:5000", nil, loopback, ""},                                             // 代理没带：不按 IP 计数
		{"127.0.0.1:5000", []string{"unknown"}, loopback, ""},                             // 认不出来的
		{"127.0.0.1:5000", []string{"198.51.100.7, unknown"}, loopback, ""},
		{"[::1]:5000", []string{"2001:db8::1"}, loopback, "2001:db8::1"},
		{"172.18.0.1:5000", []string{"192.168.1.20"}, docker, "192.168.1.20"},         // Docker 网桥的网关连过来
		{"172.18.0.1:5000", []string{"203.0.113.9, 10.0.0.2"}, docker, "203.0.113.9"}, // 两层代理
		{"172.18.0.1:5000", []string{"10.0.0.3"}, docker, "10.0.0.3"},                 // 只信写明的那一个地址，别的内网地址不跳过
		{"172.18.0.1:5000", []string{"172.20.0.5, 172.18.0.9"}, docker, "172.20.0.5"}, // 全在信任的网段里：取最左边的
		{"127.0.0.1:5000", []string{"203.0.113.9"}, docker, ""},                       // 设了 ZSMEET_TRUSTED_PROXIES 就不再默认信本机：当成没配好的代理
		{"127.0.0.1:5000", nil, docker, "127.0.0.1"},                                  // 不带这个头的本机请求按本机地址计数
		{"203.0.113.9:5000", []string{"1.2.3.4"}, docker, "203.0.113.9"},              // 外网地址带着这个头：不信，按它自己的地址计数
	} {
		if got, _ := clientIP(tc.remote, tc.forwarded, tc.proxies); got != tc.want {
			t.Errorf("clientIP(%q, %q) = %q，想要 %q", tc.remote, tc.forwarded, got, tc.want)
		}
	}
}

// 删除会议进回收站：看不到、搜不到；回收站里管理员看全部、成员看自己的；能恢复；
// 彻底删除连录音文件一起删；放满 30 天的由后台清掉；删掉的会议的术语表不再用来扩展搜索。
func TestTrash(t *testing.T) {
	c := newServer(t)
	c.setup()
	member := c.addMember("member@example.com", "李航")
	x := c.processed("产品周会", []transcript.Segment{{Start: 1, Text: "飞书集成放到下一个版本。"}}, "## 摘要\n好。", `[{"term": "飞书", "variants": ["非书"]}]`)
	y := member.want(member.do("POST", "/api/meetings", obj{"title": "李航的会", "source": "upload"}), 201).Body["id"].(string)
	// x 有一个录音文件
	if err := os.MkdirAll(filepath.Join(c.store.Path("audio")), 0o755); err != nil {
		t.Fatal(err)
	}
	audioFile := c.store.Path("audio/" + x + ".m4a")
	if err := os.WriteFile(audioFile, []byte("m4a"), 0o644); err != nil {
		t.Fatal(err)
	}

	member.want(member.do("DELETE", "/api/meetings/"+x, nil), 403)
	c.want(c.do("DELETE", "/api/meetings/"+x, nil), 204)
	c.want(c.do("GET", "/api/meetings/"+x, nil), 404)
	c.want(c.do("GET", "/api/meetings/"+x+"/transcript", nil), 404)
	if r := c.want(c.do("GET", "/api/meetings?q="+url.QueryEscape("飞书"), nil), 200); len(r.Body["items"].([]any)) != 0 || fmt.Sprint(r.Body["expanded"]) != "[飞书]" {
		t.Errorf("删掉的会议搜不到，它的术语表也不再用：%s", r.Raw)
	}
	member.want(member.do("DELETE", "/api/meetings/"+y, nil), 204)

	trash := func(cl *client) []any {
		t.Helper()
		return cl.want(cl.do("GET", "/api/trash", nil), 200).Body["items"].([]any)
	}
	all := trash(c)
	if len(all) != 2 {
		t.Fatalf("管理员看得到回收站里的全部：%v", all)
	}
	first := all[0].(map[string]any) // 最近删的在前：李航删的 y
	deletedAt, _ := time.Parse(time.RFC3339Nano, first["deleted_at"].(string))
	purgeAt, _ := time.Parse(time.RFC3339Nano, first["purge_at"].(string))
	if first["id"] != y || first["deleted_by_name"] != "李航" || purgeAt.Sub(deletedAt) != 30*24*time.Hour {
		t.Errorf("回收站条目不对：%v", first)
	}
	if mine := trash(member); len(mine) != 1 || mine[0].(map[string]any)["id"] != y {
		t.Errorf("成员只看得到自己创建的：%v", mine)
	}
	member.want(member.do("POST", "/api/trash/"+x+"/restore", nil), 403)

	// 恢复
	restored := c.want(c.do("POST", "/api/trash/"+x+"/restore", nil), 200)
	if restored.Body["id"] != x {
		t.Errorf("恢复后返回这场会议：%s", restored.Raw)
	}
	if r := c.want(c.do("GET", "/api/meetings?q="+url.QueryEscape("非书"), nil), 200); len(r.Body["items"].([]any)) != 1 {
		t.Errorf("恢复以后又能搜到（「非书」由它的术语表扩展到「飞书」）：%s", r.Raw)
	}
	c.want(c.do("POST", "/api/trash/"+x+"/restore", nil), 404)
	c.want(c.do("DELETE", "/api/trash/"+x, nil), 404) // 不在回收站里不能彻底删除

	// 彻底删除：记录和录音文件都没了
	c.want(c.do("DELETE", "/api/meetings/"+x, nil), 204)
	c.want(c.do("DELETE", "/api/trash/"+x, nil), 204)
	var n int
	if err := c.pool.QueryRow(t.Context(), "SELECT count(*) FROM meetings WHERE id = $1", x).Scan(&n); err != nil || n != 0 {
		t.Errorf("记录应该删掉：%d %v", n, err)
	}
	if _, err := os.Stat(audioFile); !os.IsNotExist(err) {
		t.Errorf("录音文件应该删掉：%v", err)
	}

	// 放满 30 天的由后台清掉，没满的留着
	if _, err := c.pool.Exec(t.Context(), "UPDATE meetings SET deleted_at = now() - interval '31 days' WHERE id = $1", y); err != nil {
		t.Fatal(err)
	}
	z := c.processed("刚删的会", nil, "## 摘要", `[]`)
	c.want(c.do("DELETE", "/api/meetings/"+z, nil), 204)
	purged, err := service.PurgeExpired(t.Context(), sqlc.New(c.pool), c.store)
	if err != nil || purged != 1 {
		t.Errorf("应该清掉 1 场：%d %v", purged, err)
	}
	if left := trash(c); len(left) != 1 || left[0].(map[string]any)["id"] != z {
		t.Errorf("只剩刚删的那场：%v", left)
	}
}

// 给说话人起名字：会议里记下名字，当前纪要里的称呼换成名字（新版本）；没权限、名字太长、编号不对的都不行。
func TestRenameSpeakers(t *testing.T) {
	c := newServer(t)
	c.setup()
	member := c.addMember("member@example.com", "李航")
	id := c.processed("产品周会", []transcript.Segment{
		{Start: 0, End: 2, Speaker: 0, Text: "一期只做网页端。"},
		{Start: 2, End: 4, Speaker: 1, Text: "同意。"},
	}, "## 摘要\n- 说话人 1 提出一期只做网页端；说话人 2（王悦）同意。说话人 12 没来。", `[]`)
	base := "/api/meetings/" + id

	member.want(member.do("PUT", base+"/speakers", obj{"names": obj{"0": "张敏"}}), 403)
	c.want(c.do("PUT", base+"/speakers", obj{"names": obj{"0": strings.Repeat("长", 21)}}), 422)
	c.want(c.do("PUT", base+"/speakers", obj{"names": obj{"x": "张敏"}}), 422)

	m := c.want(c.do("PUT", base+"/speakers", obj{"names": obj{"0": " 张敏 ", "1": "王悦"}}), 200)
	if fmt.Sprint(m.Body["speakers"]) != "map[0:张敏 1:王悦]" || m.Body["minutes_version"] != float64(2) {
		t.Fatalf("应该记下名字、纪要多一个版本：%s", m.Raw)
	}
	minutes := c.want(c.do("GET", base+"/minutes", nil), 200)
	if minutes.Body["content"] != "## 摘要\n- 张敏 提出一期只做网页端；王悦同意。说话人 12 没来。" {
		t.Errorf("纪要里的称呼要换成名字：%s", minutes.Raw)
	}
	if v := minutes.Body["versions"].([]any)[0].(map[string]any); v["source"] != "edit" || v["author"] != "张敏" {
		t.Errorf("新版本记在改名的人名下：%v", v)
	}

	// 名字没变不再产生新版本；去掉名字只改会议，不动纪要
	c.want(c.do("PUT", base+"/speakers", obj{"names": obj{"0": "张敏"}}), 200)
	m = c.want(c.do("PUT", base+"/speakers", obj{"names": obj{"1": ""}}), 200)
	if fmt.Sprint(m.Body["speakers"]) != "map[0:张敏]" || m.Body["minutes_version"] != float64(2) {
		t.Errorf("去掉名字不该产生新的纪要版本：%s", m.Raw)
	}
}

// 导出：纪要导出成 Word / Markdown，转写导出成纯文本；文件名是会议标题，说话人用起好的名字；
// 团队里的人都能导出；没有纪要时 409，删掉的会议 404。
func TestExport(t *testing.T) {
	c := newServer(t)
	c.setup()
	member := c.addMember("member@example.com", "李航")
	id := c.processed("产品周会", []transcript.Segment{
		{Start: 0, End: 2, Speaker: 0, Text: "一期只做网页端。"},
		{Start: 3, End: 5, Speaker: 1, Text: "同意。"},
	}, "## 结论\n- [00:00] 一期只做网页端。", `[]`)
	base := "/api/meetings/" + id
	c.want(c.do("PUT", base+"/speakers", obj{"names": obj{"0": "张敏"}}), 200)

	get := func(cl *client, query string) (*http.Response, []byte) {
		t.Helper()
		resp, err := cl.http.Get(c.base + base + "/export?" + query)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, body
	}

	resp, body := get(member, "what=minutes&format=docx")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/vnd.openxmlformats-officedocument.wordprocessingml.document" {
		t.Fatalf("应该导出 Word：%d %s %s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, `filename="zsmeet-export.docx"`) || !strings.Contains(cd, "filename*=UTF-8''"+url.PathEscape("产品周会-纪要.docx")) {
		t.Errorf("文件名不对：%s", cd)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("不是合法的 Word 文件：%v", err)
	}
	var doc string
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			rc, _ := f.Open()
			raw, _ := io.ReadAll(rc)
			rc.Close()
			doc = string(raw)
		}
	}
	for _, want := range []string{"产品周会", "创建者：张敏", "说话人：张敏", "纪要版本：第 1 版", "一期只做网页端"} {
		if !strings.Contains(doc, want) {
			t.Errorf("Word 里缺少 %q", want)
		}
	}

	if resp, body := get(c, "what=minutes&format=md"); resp.StatusCode != 200 || !strings.HasPrefix(string(body), "# 产品周会\n") || !strings.Contains(resp.Header.Get("Content-Type"), "text/markdown") {
		t.Errorf("Markdown 导出不对：%d %s", resp.StatusCode, body)
	}
	if resp, body := get(c, "what=transcript&format=txt"); resp.StatusCode != 200 || !strings.Contains(string(body), "[00:00] 张敏：一期只做网页端。") || !strings.Contains(string(body), "[00:03] 说话人 2：同意。") {
		t.Errorf("转写导出不对：%d %s", resp.StatusCode, body)
	}
	if resp, _ := get(c, "what=minutes&format=pdf"); resp.StatusCode != 422 {
		t.Errorf("不认识的格式要报错：%d", resp.StatusCode)
	}

	// 还没有纪要
	empty := c.want(c.do("POST", "/api/meetings", obj{"title": "空会议", "source": "upload"}), 201).Body["id"].(string)
	if resp, err := c.http.Get(c.base + "/api/meetings/" + empty + "/export?what=minutes"); err != nil || resp.StatusCode != 409 {
		t.Errorf("没有纪要时应该是 409：%v %v", resp.StatusCode, err)
	}
	// 删掉的会议
	c.want(c.do("DELETE", base, nil), 204)
	if resp, _ := get(c, "what=minutes"); resp.StatusCode != 404 {
		t.Errorf("删掉的会议不能导出：%d", resp.StatusCode)
	}
}

// 自定义纪要模板：大家都能看、都能选，只有管理员能加、改、删；内置的不能改；删掉以后不能再选。
func TestCustomTemplates(t *testing.T) {
	c := newServer(t)
	c.setup()
	member := c.addMember("member@example.com", "李航")

	list := member.want(member.do("GET", "/api/templates", nil), 200)
	if items := list.Body["items"].([]any); len(items) != 4 || items[0].(map[string]any)["custom"] != false {
		t.Fatalf("一开始只有内置的四种：%s", list.Raw)
	}
	body := obj{"name": "需求评审", "guide": "1. 摘要：评审结论\n2. 需求变更：改了什么、谁提的"}
	member.want(member.do("POST", "/api/templates", body), 403)
	c.want(c.do("POST", "/api/templates", obj{"name": " ", "guide": "x"}), 422)
	c.want(c.do("POST", "/api/templates", obj{"name": "x", "guide": strings.Repeat("长", 2001)}), 422)

	created := c.want(c.do("POST", "/api/templates", body), 201)
	id := created.Body["id"].(string)
	if created.Body["custom"] != true || created.Body["name"] != "需求评审" {
		t.Fatalf("新建的模板：%s", created.Raw)
	}
	list = member.want(member.do("GET", "/api/templates", nil), 200)
	if items := list.Body["items"].([]any); len(items) != 5 || items[4].(map[string]any)["id"] != id {
		t.Fatalf("自定义的排在内置的后面：%s", list.Raw)
	}

	// 普通成员也能用它新建会议、重新生成（这里没配大模型，过了模板检查以后停在「没有配置大模型」）
	m := member.want(member.do("POST", "/api/meetings", obj{"title": "评审会", "source": "upload", "template": id}), 201)
	if m.Body["template"] != id {
		t.Fatalf("会议记下自定义模板：%s", m.Raw)
	}
	mid := member.processed("周会", []transcript.Segment{{Start: 0, End: 2, Text: "好。"}}, "## 摘要\n好。", `[]`)
	if r := member.want(member.do("POST", "/api/meetings/"+mid+"/minutes/regenerate", obj{"template": id}), 409); !strings.Contains(r.Raw, "大模型") {
		t.Errorf("模板检查应该通过：%s", r.Raw)
	}

	// 改：只有管理员；内置的模板 ID 不是 UUID，接口直接拒绝；不存在的是 404
	member.want(member.do("PUT", "/api/templates/"+id, obj{"name": "改名", "guide": "x"}), 403)
	c.want(c.do("PUT", "/api/templates/general", obj{"name": "改名", "guide": "x"}), 422)
	c.want(c.do("PUT", "/api/templates/"+uuid.NewString(), obj{"name": "改名", "guide": "x"}), 404)
	if r := c.want(c.do("PUT", "/api/templates/"+id, obj{"name": "需求评审会", "guide": "1. 结论"}), 200); r.Body["name"] != "需求评审会" || r.Body["guide"] != "1. 结论" {
		t.Errorf("改完：%s", r.Raw)
	}

	// 删：只有管理员；删掉以后不能再选，用过它的会议还在
	member.want(member.do("DELETE", "/api/templates/"+id, nil), 403)
	c.want(c.do("DELETE", "/api/templates/general", nil), 422)
	c.want(c.do("DELETE", "/api/templates/"+id, nil), 204)
	c.want(c.do("DELETE", "/api/templates/"+id, nil), 404)
	if items := c.want(c.do("GET", "/api/templates", nil), 200).Body["items"].([]any); len(items) != 4 {
		t.Errorf("删掉以后只剩内置的：%v", items)
	}
	c.want(c.do("POST", "/api/meetings", obj{"title": "x", "source": "upload", "template": id}), 422)
	c.want(c.do("POST", "/api/meetings/"+mid+"/minutes/regenerate", obj{"template": id}), 422)
	if r := c.want(c.do("GET", "/api/meetings/"+m.Body["id"].(string), nil), 200); r.Body["template"] != id {
		t.Errorf("用过它的会议不受影响：%s", r.Raw)
	}
}

// 可见范围：只有自己能看的会议别人看不到（当作不存在），搜也搜不到，术语表也不拿来扩展搜索词；
// 指定的人能看，勾了能编辑的还能改；团队所有人能看的会议也可以单独给人编辑权限；
// 删除、改可见范围只有创建者和管理员能做。
func TestMeetingAccess(t *testing.T) {
	c := newServer(t)
	c.setup()
	owner := c.addMember("owner@example.com", "李航")
	viewer := c.addMember("viewer@example.com", "王悦")
	outsider := c.addMember("outsider@example.com", "赵雷")
	userID := func(email string) string {
		t.Helper()
		var id string
		if err := c.pool.QueryRow(t.Context(), "SELECT id FROM users WHERE email = $1", email).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	viewerID, outsiderID, ownerID := userID("viewer@example.com"), userID("outsider@example.com"), userID("owner@example.com")
	listed := func(cl *client, q string) map[string]bool {
		t.Helper()
		out := map[string]bool{}
		for _, it := range cl.want(cl.do("GET", "/api/meetings?q="+url.QueryEscape(q), nil), 200).Body["items"].([]any) {
			out[it.(map[string]any)["id"].(string)] = true
		}
		return out
	}

	// 新建时只能选团队所有人或者只有自己
	owner.want(owner.do("POST", "/api/meetings", obj{"title": "x", "source": "upload", "visibility": "members"}), 422)
	p := owner.want(owner.do("POST", "/api/meetings", obj{"title": "一对一", "source": "upload", "visibility": "private"}), 201)
	if p.Body["visibility"] != "private" || p.Body["can_manage"] != true || p.Body["owner_id"] != ownerID {
		t.Fatalf("新建的私密会议：%s", p.Raw)
	}
	private := "/api/meetings/" + p.Body["id"].(string)
	for _, path := range []string{"", "/transcript", "/minutes", "/chunks", "/access", "/export", "/feishu", "/audio"} {
		viewer.want(viewer.do("GET", private+path, nil), 404)
	}
	viewer.want(viewer.do("PATCH", private, obj{"title": "看不到也改不了"}), 404)
	if listed(viewer, "")[p.Body["id"].(string)] || !listed(c, "")[p.Body["id"].(string)] {
		t.Error("私密会议别人列不出来，管理员列得出来")
	}

	// 指定的人：王悦能看不能改，赵雷看不到；纪要里的词、术语表里的写法赵雷也搜不到
	m := owner.processed("融资讨论", []transcript.Segment{{Start: 1, End: 3, Text: "估值的事下周再谈。"}},
		"## 摘要\n讨论了蓝鲸计划的估值。", `[{"term": "蓝鲸计划", "variants": ["蓝金计划"]}]`)
	base := "/api/meetings/" + m
	viewer.want(viewer.do("PUT", base+"/access", obj{"visibility": "team"}), 403)
	owner.want(owner.do("PUT", base+"/access", obj{"visibility": "members", "members": []obj{{"user_id": uuid.NewString()}}}), 422)
	got := owner.want(owner.do("PUT", base+"/access", obj{"visibility": "members", "members": []obj{
		{"user_id": viewerID}, {"user_id": ownerID, "can_edit": true}, {"user_id": viewerID},
	}}), 200)
	if ms := got.Body["members"].([]any); len(ms) != 1 || ms[0].(map[string]any)["user_id"] != viewerID || ms[0].(map[string]any)["can_edit"] != false {
		t.Fatalf("创建者自己、重复的人不算：%s", got.Raw)
	}
	if r := viewer.want(viewer.do("GET", base, nil), 200); r.Body["can_edit"] != false || r.Body["can_manage"] != false || r.Body["visibility"] != "members" {
		t.Errorf("王悦能看不能改：%s", r.Raw)
	}
	viewer.want(viewer.do("GET", base+"/access", nil), 200)
	outsider.want(outsider.do("GET", base, nil), 404)
	viewer.want(viewer.do("PUT", base+"/minutes", obj{"content": "## 摘要\n被改了"}), 403)
	if !listed(viewer, "蓝鲸计划")[m] || listed(outsider, "蓝鲸计划")[m] || listed(outsider, "")[m] {
		t.Error("指定的人搜得到，别人搜不到")
	}
	r := outsider.want(outsider.do("GET", "/api/meetings?q="+url.QueryEscape("蓝金计划"), nil), 200)
	if exp := r.Body["expanded"].([]any); len(exp) != 1 {
		t.Errorf("看不到的会议的术语表不能拿来扩展搜索词：%s", r.Raw)
	}

	// 勾上能编辑：能改纪要、改标题，但不能删、不能改谁能看
	owner.want(owner.do("PUT", base+"/access", obj{"visibility": "members", "members": []obj{{"user_id": viewerID, "can_edit": true}}}), 200)
	if r := viewer.want(viewer.do("GET", base, nil), 200); r.Body["can_edit"] != true || r.Body["can_manage"] != false {
		t.Errorf("王悦能编辑了：%s", r.Raw)
	}
	viewer.want(viewer.do("PUT", base+"/minutes", obj{"content": "## 摘要\n王悦补充了几句。"}), 200)
	viewer.want(viewer.do("PATCH", base, obj{"title": "融资讨论（王悦改）"}), 200)
	viewer.want(viewer.do("DELETE", base, nil), 403)
	viewer.want(viewer.do("PUT", base+"/access", obj{"visibility": "team"}), 403)

	// 团队所有人能看的会议，也可以单独给人编辑权限
	team := owner.processed("周会", []transcript.Segment{{Start: 1, End: 3, Text: "好。"}}, "## 摘要\n好。", `[]`)
	owner.want(owner.do("PUT", "/api/meetings/"+team+"/access", obj{"visibility": "team", "members": []obj{{"user_id": outsiderID, "can_edit": true}}}), 200)
	outsider.want(outsider.do("PUT", "/api/meetings/"+team+"/minutes", obj{"content": "## 摘要\n赵雷改的。"}), 200)
	viewer.want(viewer.do("PUT", "/api/meetings/"+team+"/minutes", obj{"content": "## 摘要\n王悦改的。"}), 403)
	if r := viewer.want(viewer.do("GET", "/api/meetings/"+team, nil), 200); r.Body["can_edit"] != false {
		t.Errorf("没被指定的人只能看：%s", r.Raw)
	}

	// 改成只有自己：单独指定的人清掉，王悦看不到了
	got = owner.want(owner.do("PUT", base+"/access", obj{"visibility": "private", "members": []obj{{"user_id": viewerID, "can_edit": true}}}), 200)
	if len(got.Body["members"].([]any)) != 0 {
		t.Errorf("只有自己能看时不留单独指定的人：%s", got.Raw)
	}
	viewer.want(viewer.do("GET", base, nil), 404)

	// 回收站：看不到的当作不存在，看得到但不是创建者的不能恢复
	owner.want(owner.do("DELETE", "/api/meetings/"+team, nil), 204)
	viewer.want(viewer.do("POST", "/api/trash/"+team+"/restore", nil), 403)
	owner.want(owner.do("DELETE", base, nil), 204)
	viewer.want(viewer.do("POST", "/api/trash/"+m+"/restore", nil), 404)
	owner.want(owner.do("POST", "/api/trash/"+team+"/restore", nil), 200)

	// 选人的列表不列停用的人；停用的人不能新加进来，原来就在的可以留着
	if _, err := c.pool.Exec(t.Context(), "UPDATE users SET disabled = true WHERE id = $1", outsiderID); err != nil {
		t.Fatal(err)
	}
	people := viewer.want(viewer.do("GET", "/api/people", nil), 200).Body["items"].([]any)
	if len(people) != 3 {
		t.Errorf("停用的人不列：%v", people)
	}
	owner.want(owner.do("PUT", "/api/meetings/"+team+"/access", obj{"visibility": "team", "members": []obj{{"user_id": outsiderID, "can_edit": true}, {"user_id": viewerID}}}), 200)
	other := owner.processed("另一场", []transcript.Segment{{Start: 1, End: 3, Text: "好。"}}, "## 摘要\n好。", `[]`)
	owner.want(owner.do("PUT", "/api/meetings/"+other+"/access", obj{"visibility": "members", "members": []obj{{"user_id": outsiderID}}}), 422)
}

// 录音过了保留期被删了：还没有转写的失败会议不能重试（重试也只能失败），有转写的照常重试（只重写纪要）；不能重新转写。
func TestRetryAfterAudioExpired(t *testing.T) {
	c := newServer(t)
	c.setup()
	noTranscript := c.want(c.do("POST", "/api/meetings", obj{"title": "识别失败的会", "source": "upload"}), 201).Body["id"].(string)
	withTranscript := c.processed("纪要失败的会", []transcript.Segment{{Start: 0, End: 2, Text: "好。"}}, "## 摘要\n好。", `[]`)
	for _, id := range []string{noTranscript, withTranscript} {
		if _, err := c.pool.Exec(t.Context(),
			"UPDATE meetings SET status = 'failed', audio_key = 'audio/' || id || '.m4a', audio_deleted_at = now() WHERE id = $1", id); err != nil {
			t.Fatal(err)
		}
	}
	if r := c.want(c.do("POST", "/api/meetings/"+noTranscript+"/retry", nil), 409); !strings.Contains(r.Raw, "保留期") {
		t.Errorf("没有转写、录音也没了：%s", r.Raw)
	}
	c.want(c.do("POST", "/api/meetings/"+withTranscript+"/retranscribe", nil), 409)
	c.want(c.do("POST", "/api/meetings/"+withTranscript+"/retry", nil), 200)
}

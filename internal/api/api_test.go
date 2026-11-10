package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2/memstore"
	"github.com/jackc/pgx/v5/pgxpool"

	"zsmeet/internal/db/sqlc"
	"zsmeet/internal/password"
	"zsmeet/internal/queue"
	"zsmeet/internal/secret"
	"zsmeet/internal/service"
	"zsmeet/internal/settings"
	"zsmeet/internal/storage"
	"zsmeet/internal/testdb"
	"zsmeet/web"
)

// 这里的测试走真实的 HTTP：会话 Cookie、登录校验、参数校验都和线上一样，只是不启动 worker。

// client 是带 Cookie 的测试客户端，相当于一个浏览器。
type client struct {
	t    *testing.T
	base string
	http *http.Client
	pool *pgxpool.Pool
	// 直接调业务层，给那些不经过网页的操作用（服务器上的救急命令）
	svc *service.Service
}

// result 是一次请求的结果。Body 是解析后的 JSON（没有响应体时为空）。
type result struct {
	Status int
	Body   map[string]any
	Raw    string
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
	// 和线上同一份会话配置，只是存在内存里
	sess := NewSessions(memstore.New(), false)

	server := httptest.NewServer(NewHandler(svc, sess, web.FS(), "test"))
	t.Cleanup(server.Close)
	c := &client{t: t, base: server.URL, pool: pool, svc: svc}
	return c.another()
}

// another 返回连着同一个服务、但没有登录的另一个客户端（另一个人的浏览器）。
func (c *client) another() *client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		c.t.Fatal(err)
	}
	return &client{t: c.t, base: c.base, http: &http.Client{Jar: jar}, pool: c.pool, svc: c.svc}
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
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	r := result{Status: resp.StatusCode, Raw: string(raw)}
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

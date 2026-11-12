package api

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/danielgtaylor/huma/v2"
)

// problem 是错误响应（problem+json）里用得到的部分。
type problem struct {
	Status int    `json:"status"`
	Detail string `json:"detail"`
	Errors []struct {
		Message  string `json:"message"`
		Location string `json:"location"`
		Value    any    `json:"value"`
	} `json:"errors"`
}

func hasHan(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return unicode.Is(unicode.Han, r) })
}

// 进到处理函数之前就被 huma 挡下的请求（参数校验没通过、请求内容不是 JSON、太大……），报错也是中文：
// 网页和桌面版直接显示 detail，它要是一句说清楚哪一项不对、怎么改的话；
// errors 里每一项的 message 是中文的原因；状态码和 location 给程序看，和原来一样。
// 提交的值不放回去（huma 原本放在 errors 的 value 里）：整个请求出错时就是提交的全部内容，可能带着密码。
func TestRequestErrorsInChinese(t *testing.T) {
	c := newServer(t)
	c.setup()
	meeting := "/api/meetings/" + c.want(c.do("POST", "/api/meetings", obj{"title": "周会", "source": "upload"}), 201).Body["id"].(string)
	const password = "correct-horse-battery"

	for _, tc := range []struct {
		name   string
		r      result
		status int
		// 第一处错误的位置；为空表示响应里没有 errors（不是哪一项的问题）
		location string
		detail   string
	}{
		{"地址里的 ID 不是 UUID", c.do("GET", "/api/meetings/不是合法的ID", nil), 422, "path.id",
			"地址里的 id 格式不对，应该是一串 UUID，请检查是否完整"},
		{"标题太长", c.do("PATCH", meeting, obj{"title": strings.Repeat("长", 201)}), 422, "body.title",
			"标题太长了，最多 200 个字"},
		{"标题是空的", c.do("PATCH", meeting, obj{"title": ""}), 422, "body.title",
			"标题至少要 1 个字"},
		{"少了必填的字段", c.do("POST", "/api/meetings", obj{"title": "周会"}), 422, "body",
			"提交的内容缺少「source」，请补上"},
		{"不是可选的值", c.do("POST", "/api/meetings", obj{"title": "周会", "source": "feishu"}), 422, "body.source",
			"「source」取值不对，只能是 upload, record, desktop 中的一个"},
		// 位置按字符数，不按字节数：前面的「周会」两个字占 6 个字节
		{"不是合法的 JSON", c.post("/api/meetings", "application/json", `{"title": "周会", }`), 400, "body",
			"提交的内容不是合法的 JSON，第 17 个字符附近有错，请检查格式"},
		// 校验只看是不是整数，放不进 int32 是解析的时候才发现的
		{"数字超出范围", c.do("POST", meeting+"/feishu", obj{"chat_id": "oc_team", "minutes_version": 1e10}), 422, "body",
			"提交的内容有一项类型不对或者超出了范围：「minutes_version」，请检查"},
		{"不是 JSON", c.post("/api/meetings", "text/plain", `{"title": "周会", "source": "upload"}`), 415, "body",
			"提交的内容格式不支持（Content-Type: text/plain），请用 JSON 提交（Content-Type: application/json）"},
		{"地址里的参数不是整数", c.do("GET", "/api/meetings?limit=abc", nil), 422, "query.limit",
			"地址里的 limit 应该是整数"},
		{"搜索词太长", c.do("GET", "/api/meetings?q="+url.QueryEscape(strings.Repeat("词", 101)), nil), 422, "query.q",
			"搜索词太长了，最多 100 个字"},
		{"好几处不对", c.do("POST", meeting+"/complete", obj{"chunks": 0, "marks": []any{"三秒"}}), 422, "body.chunks",
			"「chunks」太小了，不能小于 1（另外还有 1 处不对）"},
		{"空的分片", c.do("PUT", meeting+"/chunks/0", []byte{}), 400, "",
			"没有收到要提交的内容，请重新提交"},
		{"纪要太大", c.do("PUT", meeting+"/minutes", obj{"content": strings.Repeat("纪", 1<<20)}), 413, "",
			"提交的内容太大了，最多 2 MB，请删减一些再提交"},
		{"登录时没填邮箱", c.do("POST", "/api/login", obj{"password": password}), 422, "body",
			"提交的内容缺少「email」，请补上"},
		{"登录的内容不是合法的 JSON", c.post("/api/login", "application/json", `{"email": "lihang@example.com", "password": "`+password+`",}`), 400, "body",
			"提交的内容不是合法的 JSON，第 69 个字符附近有错，请检查格式"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.r.Status != tc.status {
				t.Fatalf("状态码应该是 %d，得到 %d：%s", tc.status, tc.r.Status, tc.r.Raw)
			}
			var p problem
			if err := json.Unmarshal([]byte(tc.r.Raw), &p); err != nil {
				t.Fatalf("错误响应应该是 JSON：%s", tc.r.Raw)
			}
			if p.Status != tc.status || p.Detail != tc.detail {
				t.Errorf("detail 应该是「%s」：%s", tc.detail, tc.r.Raw)
			}
			if strings.Contains(tc.r.Raw, password) {
				t.Errorf("错误响应里不能有提交的密码：%s", tc.r.Raw)
			}
			if tc.location == "" {
				if len(p.Errors) != 0 {
					t.Errorf("不应该有 errors：%s", tc.r.Raw)
				}
				return
			}
			if len(p.Errors) == 0 || p.Errors[0].Location != tc.location {
				t.Fatalf("第一处错误的位置应该还是 %s：%s", tc.location, tc.r.Raw)
			}
			for _, e := range p.Errors {
				if !hasHan(e.Message) {
					t.Errorf("errors 里的原因应该是中文：%s", tc.r.Raw)
				}
				if e.Value != nil {
					t.Errorf("errors 里不该放回提交的值：%s", tc.r.Raw)
				}
			}
		})
	}
}

// huma 的校验提示要一条不漏都有中文，升级 huma 以后新加的也是：从 huma 的源码里读出 validation 包的全部 Msg*，
// 和 localizeValidation 换掉的英文原文对一遍。中文的占位符要和原文一一对应，不然拼出来是 %!d(MISSING) 这样的乱码。
func TestValidationMessagesAllChinese(t *testing.T) {
	en := localizeValidation()

	dir, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/danielgtaylor/huma/v2").Output()
	if err != nil {
		t.Fatalf("找不到 huma 的源码：%v", err)
	}
	path := filepath.Join(strings.TrimSpace(string(dir)), "validation", "messages.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var source []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				var lit *ast.BasicLit
				if i < len(vs.Values) {
					lit, _ = vs.Values[i].(*ast.BasicLit)
				}
				if lit == nil || lit.Kind != token.STRING {
					t.Errorf("看不懂 huma 的 %s，请对照 %s 检查 validationMessages", name.Name, path)
					continue
				}
				msg, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				source = append(source, msg)
			}
		}
	}
	if len(source) == 0 {
		t.Fatalf("没从 %s 里读到校验提示", path)
	}
	for _, msg := range source {
		if !slices.Contains(en, msg) {
			t.Errorf("huma 的校验提示 %q 还没有中文，请加到 problem.go 的 validationMessages 里", msg)
		}
	}
	for i, m := range validationMessages {
		if !hasHan(m.zh) {
			t.Errorf("%q 应该是中文", m.zh)
		}
		if verbs(m.zh) != verbs(en[i]) {
			t.Errorf("「%s」的占位符和原文 %q 对不上", m.zh, en[i])
		}
	}
}

// verbs 按顺序取出格式串里占位符的动词：%.0v 算 v，%% 不算。
func verbs(format string) string {
	var out []byte
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		i++
		for i < len(format) && strings.IndexByte("+-# 0123456789.[]*", format[i]) >= 0 {
			i++
		}
		if i < len(format) && format[i] != '%' {
			out = append(out, format[i])
		}
	}
	return string(out)
}

// 处理函数返回了不认识的错误时，huma 会把 Go 的报错原文放进 errors。不能给用户看：换成笼统的一句，原文只进日志。
func TestUnexpectedErrorIsHidden(t *testing.T) {
	out, err := localizeProblem(nil, "500", huma.NewError(http.StatusInternalServerError, "unexpected error occurred", errors.New("dial tcp 10.0.0.5:5432: connection refused")))
	if err != nil {
		t.Fatal(err)
	}
	p := out.(*huma.ErrorModel)
	if p.Status != http.StatusInternalServerError || p.Detail != "服务器内部出错了，请稍后再试" || len(p.Errors) != 0 {
		t.Errorf("内部报错不能出现在响应里：%+v", p)
	}
}

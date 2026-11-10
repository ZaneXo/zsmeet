// newapi 生成一个新接口的骨架，用法见 make api：
//
//	make api NAME=DeleteMeeting METHOD=DELETE ROUTE=/api/meetings/{id}
//
// 它会新建 internal/api/delete_meeting.go（输入输出结构体、接口说明、空的处理函数）
// 和对应的测试文件，并把接口注册进 routes.go。生成后接口就能访问，返回 501，
// 接下来填字段、在 service 里写业务逻辑、补测试，最后 make gen 更新前端类型。
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/template"
	"unicode"
)

const (
	apiDir = "internal/api"
	routes = "internal/api/routes.go"
	marker = "// gen:routes"
)

type param struct {
	Name  string // 路径里的名字，如 seq
	Field string // Go 字段名，如 Seq
}

type data struct {
	Name       string // RenameMeeting
	Var        string // renameMeeting
	Kebab      string // rename-meeting
	HTTPMethod string // PATCH
	GoMethod   string // Patch，对应 http.MethodPatch
	Route      string
	Example    string // 测试里用的示例地址
	// 路径以 /api/meetings/{id} 开头时，直接嵌入已有的 MeetingID
	MeetingID bool
	Params    []param
	HasBody   bool
}

var (
	nameRE  = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)
	paramRE = regexp.MustCompile(`\{(\w+)\}`)
)

func main() {
	name := flag.String("name", "", "接口名，大写开头的驼峰，如 RenameMeeting")
	method := flag.String("method", "", "HTTP 方法：GET、POST、PUT、PATCH、DELETE")
	route := flag.String("route", "", "路径，如 /api/meetings/{id}/title")
	flag.Parse()
	if err := run(*name, strings.ToUpper(*method), *route); err != nil {
		fmt.Fprintln(os.Stderr, "✘", err)
		fmt.Fprintln(os.Stderr, "用法：make api NAME=RenameMeeting METHOD=PATCH ROUTE=/api/meetings/{id}/title")
		os.Exit(1)
	}
}

func run(name, method, route string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("NAME 要是大写开头的驼峰（如 RenameMeeting），得到 %q", name)
	}
	if !slices.Contains([]string{"GET", "POST", "PUT", "PATCH", "DELETE"}, method) {
		return fmt.Errorf("METHOD 要是 GET、POST、PUT、PATCH、DELETE 之一，得到 %q", method)
	}
	if !strings.HasPrefix(route, "/api/") {
		return fmt.Errorf("ROUTE 要以 /api/ 开头，得到 %q", route)
	}
	if _, err := os.Stat(routes); err != nil {
		return fmt.Errorf("找不到 %s，请在仓库根目录运行", routes)
	}

	d := data{
		Name:       name,
		Var:        strings.ToLower(name[:1]) + name[1:],
		Kebab:      separate(name, '-'),
		HTTPMethod: method,
		GoMethod:   method[:1] + strings.ToLower(method[1:]),
		Route:      route,
		MeetingID:  strings.HasPrefix(route, "/api/meetings/{id}"),
		HasBody:    method != "GET" && method != "DELETE",
	}
	d.Example = paramRE.ReplaceAllStringFunc(route, func(p string) string {
		if p == "{id}" {
			return "00000000-0000-0000-0000-000000000001"
		}
		return "1"
	})
	for _, m := range paramRE.FindAllStringSubmatch(route, -1) {
		if d.MeetingID && m[1] == "id" {
			continue
		}
		d.Params = append(d.Params, param{Name: m[1], Field: strings.ToUpper(m[1][:1]) + m[1][1:]})
	}

	base := filepath.Join(apiDir, separate(name, '_'))
	for _, f := range []string{base + ".go", base + "_test.go"} {
		if _, err := os.Stat(f); err == nil {
			return fmt.Errorf("%s 已经存在，换个名字或者先删掉它", f)
		}
	}
	if taken, err := handlerExists(name); err != nil {
		return err
	} else if taken {
		return fmt.Errorf("已经有一个叫 %s 的接口了", name)
	}

	registered, err := register(d)
	if err != nil {
		return err
	}
	handler, err := render(handlerTpl, d)
	if err != nil {
		return err
	}
	test, err := render(testTpl, d)
	if err != nil {
		return err
	}
	// 三个文件都准备好了再落盘，中途出错不会留下半截
	for file, content := range map[string][]byte{base + ".go": handler, base + "_test.go": test, routes: registered} {
		if err := os.WriteFile(file, content, 0o644); err != nil {
			return err
		}
	}

	fmt.Printf("✔ 新建 %s.go\n✔ 新建 %s_test.go\n✔ 已在 %s 注册\n", base, base, routes)
	fmt.Println("接下来：填请求和返回字段 → 在 internal/service 里写业务逻辑 → 补测试 → make gen 更新前端类型")
	return nil
}

// separate 把驼峰拆开：RenameMeeting → rename-meeting 或 rename_meeting。
func separate(name string, sep rune) string {
	var b strings.Builder
	for i, r := range name {
		if i > 0 && unicode.IsUpper(r) {
			b.WriteRune(sep)
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

func handlerExists(name string) (bool, error) {
	files, err := filepath.Glob(filepath.Join(apiDir, "*.go"))
	if err != nil {
		return false, err
	}
	needle := []byte("func (h *Handlers) " + name + "(")
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			return false, err
		}
		if bytes.Contains(src, needle) {
			return true, nil
		}
	}
	return false, nil
}

// register 返回在标记行上方插入了注册语句的 routes.go。
func register(d data) ([]byte, error) {
	src, err := os.ReadFile(routes)
	if err != nil {
		return nil, err
	}
	if bytes.Count(src, []byte(marker)) != 1 {
		return nil, fmt.Errorf("%s 里应该正好有一行 %q，注册语句要插在它上面", routes, marker)
	}
	line := fmt.Sprintf("huma.Register(api, %sOp, h.%s)\n\t%s", d.Var, d.Name, marker)
	return format.Source(bytes.Replace(src, []byte(marker), []byte(line), 1))
}

func render(tpl string, d data) ([]byte, error) {
	var buf bytes.Buffer
	if err := template.Must(template.New("").Parse(tpl)).Execute(&buf, d); err != nil {
		return nil, err
	}
	return format.Source(buf.Bytes())
}

const handlerTpl = `package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

type {{.Name}}Input struct {
{{- if .MeetingID}}
	MeetingID
{{- end}}
{{- range .Params}}
	{{.Field}} string ` + "`path:\"{{.Name}}\"`" + `
{{- end}}
{{- if .HasBody}}
	Body struct {
		// TODO: 请求字段，用 json、minLength、enum、doc 等标签写清楚约束
	}
{{- end}}
}

type {{.Name}}Output struct {
	Body struct {
		// TODO: 返回字段；想返回已有的结构（如 Meeting）就把 Body 换成它
	}
}

var {{.Var}}Op = huma.Operation{
	OperationID: "{{.Kebab}}",
	Method:      http.Method{{.GoMethod}},
	Path:        "{{.Route}}",
	Summary:     "TODO: 一句话说明这个接口做什么",
	Tags:        []string{"TODO"},
	// 需要登录；只有管理员能用的接口换成 adminOnly
	Security: loggedIn,
}

func (h *Handlers) {{.Name}}(ctx context.Context, in *{{.Name}}Input) (*{{.Name}}Output, error) {
	// TODO: 调用 h.svc 里的业务方法；出错时 return nil, toHTTP(err)
	return nil, huma.Error501NotImplemented("{{.Name}} 还没有实现")
}
`

const testTpl = `package api

import (
	"net/http"
	"testing"
)

func Test{{.Name}}(t *testing.T) {
	c := newServer(t)
	c.setup()

	// TODO: 准备数据，再把下面换成真正的断言
	r := c.do("{{.HTTPMethod}}", "{{.Example}}", {{if .HasBody}}obj{}{{else}}nil{{end}})
	c.want(r, http.StatusNotImplemented)
}
`

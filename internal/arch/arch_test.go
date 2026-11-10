// Package arch 用测试守住代码的分层规则。
package arch

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// 每条规则：import 路径以 prefix 开头的包，只允许出现在 allowed 这些目录里。
var rules = []struct {
	prefix  string
	allowed []string
	why     string
}{
	{
		prefix:  "github.com/danielgtaylor/huma/",
		allowed: []string{"internal/api/"},
		why:     "接口框架只能出现在接口层。业务逻辑写在 internal/service，这样以后换框架只用重写 internal/api",
	},
	{
		prefix:  "github.com/k2-fsa/sherpa-onnx-go",
		allowed: []string{"internal/asr/"},
		why:     "识别库只能通过 internal/asr 使用，别处不直接碰 cgo",
	},
	{
		prefix:  "github.com/openai/openai-go",
		allowed: []string{"internal/llm/"},
		why:     "大模型 SDK 只能通过 internal/llm 使用",
	},
	{
		prefix:  "github.com/larksuite/oapi-sdk-go",
		allowed: []string{"internal/feishu/"},
		why:     "飞书 SDK 只用在 internal/feishu 的长连接里，别处通过 internal/feishu 调用飞书",
	},
	{
		prefix:  "github.com/ZaneXo/zsmeet/internal/api",
		allowed: []string{"cmd/"},
		why:     "接口层在最外面，内部的包不能反过来依赖它",
	},
}

func TestLayering(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(strings.TrimPrefix(path, root+string(filepath.Separator)))
		if d.IsDir() {
			if rel == "web/node_modules" || strings.HasPrefix(d.Name(), ".") && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range file.Imports {
			name, _ := strconv.Unquote(imp.Path.Value)
			for _, rule := range rules {
				if !strings.HasPrefix(name, rule.prefix) || inAny(rel, rule.allowed) {
					continue
				}
				t.Errorf("%s 引用了 %s。%s", rel, name, rule.why)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func inAny(rel string, dirs []string) bool {
	for _, dir := range dirs {
		if strings.HasPrefix(rel, dir) {
			return true
		}
	}
	return false
}

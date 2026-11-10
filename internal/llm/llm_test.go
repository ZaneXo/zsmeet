package llm

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZaneXo/zsmeet/internal/settings"
)

func TestModels(t *testing.T) {
	var auth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[{"id":"qwen-max","object":"model"},{"id":"deepseek-chat","object":"model"},{"id":"qwen-max","object":"model"}]}`))
	}))
	defer server.Close()

	models, err := Models(t.Context(), settings.LLM{BaseURL: server.URL + "/v1", APIKey: "sk-sample-000"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(models, ",") != "deepseek-chat,qwen-max" {
		t.Errorf("应该按名字排好序、去掉重复：%v", models)
	}
	if auth != "Bearer sk-sample-000" {
		t.Errorf("请求里应该带上 Key，实际 %q", auth)
	}

	// 接口地址少了 /v1，或者接口不支持列模型：告诉管理员可以直接手填
	_, err = Models(t.Context(), settings.LLM{BaseURL: server.URL, APIKey: "sk-sample-000"})
	if err == nil || !strings.Contains(err.Error(), "不支持列出模型") {
		t.Errorf("404 时应该说明可以手填模型名：%v", err)
	}
}

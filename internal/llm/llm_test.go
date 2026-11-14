package llm

import (
	"encoding/json"
	"fmt"
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

// 推理模型先长时间思考：流式接口一边想一边传，回答按段拼起来，思考过程不要
func TestChatStreams(t *testing.T) {
	var stream bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream bool `json:"stream"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		stream = body.Stream
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range []string{
			`{"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"先想一想"}}]}`,
			`{"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"<think>还在想</think>收"}}]}`,
			`{"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"到"},"finish_reason":"stop"}]}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", chunk)
			w.(http.Flusher).Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	got, err := Chat(t.Context(), settings.LLM{BaseURL: server.URL + "/v1", Model: "m"}, "系统", "你好")
	if err != nil {
		t.Fatal(err)
	}
	if !stream {
		t.Error("请求里应该要求流式返回")
	}
	if got != "收到" {
		t.Errorf("回答 = %q", got)
	}
}

// 不支持流式、一次返回整个回答的接口也能用
func TestChatAcceptsWholeAnswer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"收到"}}]}`))
	}))
	defer server.Close()

	got, err := Chat(t.Context(), settings.LLM{BaseURL: server.URL + "/v1", Model: "m"}, "系统", "你好")
	if err != nil || got != "收到" {
		t.Errorf("Chat = %q, %v", got, err)
	}
}

// 回答到一半上游断了（中转服务在流里发一个错误事件）：报错，不能把半截回答当成完整的
func TestChatStreamError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"{\"fixes\": ["}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"error":{"code":"upstream_stream_read_error","message":"Upstream response stream was interrupted","type":"upstream_error"}}`+"\n\n")
	}))
	defer server.Close()

	_, err := Chat(t.Context(), settings.LLM{BaseURL: server.URL + "/v1", Model: "m"}, "系统", "你好")
	if err == nil || !strings.Contains(err.Error(), "中途断了") {
		t.Errorf("应该报回答中途断了：%v", err)
	}
}

// 接口直接报错（Key 不对）：和以前一样给出能照着处理的提示
func TestChatAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"Incorrect API key provided","type":"invalid_request_error"}}`))
	}))
	defer server.Close()

	_, err := Chat(t.Context(), settings.LLM{BaseURL: server.URL + "/v1", Model: "m", APIKey: "sk-bad"}, "系统", "你好")
	if err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "检查 API Key") {
		t.Errorf("401 时应该提示检查 Key：%v", err)
	}
}

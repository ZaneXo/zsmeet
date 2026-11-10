// Package llm 调用 OpenAI 兼容的对话接口（OpenAI、Ollama、vLLM、各家云服务）。
package llm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/ZaneXo/zsmeet/internal/settings"
)

// 一次请求最长等多久。长会议的纪要在慢模型上要几分钟。
const requestTimeout = 10 * time.Minute

// Chat 发一轮对话，返回模型的回答（去掉了思考过程）。
func Chat(ctx context.Context, cfg settings.LLM, system, user string) (string, error) {
	if !cfg.Ready() {
		return "", errors.New("还没有配置大模型")
	}
	client := newClient(cfg)
	resp, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: cfg.Model,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(system),
			openai.UserMessage(user),
		},
	})
	if err != nil {
		return "", explain(err)
	}
	if len(resp.Choices) == 0 {
		return "", errors.New("大模型没有返回内容")
	}
	text := stripThinking(resp.Choices[0].Message.Content)
	if text == "" {
		return "", errors.New("大模型返回了空内容")
	}
	return text, nil
}

func newClient(cfg settings.LLM) openai.Client {
	return openai.NewClient(
		option.WithBaseURL(cfg.BaseURL),
		// 本地模型不校验 Key，但 SDK 要求有值
		option.WithAPIKey(orDefault(cfg.APIKey, "none")),
		option.WithMaxRetries(2),
		option.WithRequestTimeout(requestTimeout),
	)
}

// 列模型时最多取这么多个。有的中转服务挂着几百个模型，全列出来也没法选
const maxModels = 1000

// Models 列出接口上的模型（请求 {接口地址}/models），按名字排序。
// 不是所有 OpenAI 兼容接口都支持；不支持时返回的错误会告诉管理员可以直接手填模型名。
func Models(ctx context.Context, cfg settings.LLM) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	client := newClient(cfg)
	pager := client.Models.ListAutoPaging(ctx)
	var ids []string
	for len(ids) < maxModels && pager.Next() {
		if id := strings.TrimSpace(pager.Current().ID); id != "" {
			ids = append(ids, id)
		}
	}
	if err := pager.Err(); err != nil {
		if apiErr, ok := errors.AsType[*openai.Error](err); ok && (apiErr.StatusCode == 404 || apiErr.StatusCode == 405) {
			return nil, fmt.Errorf("这个接口不支持列出模型（返回 %d），请直接填写模型名。也检查一下接口地址，一般以 /v1 结尾", apiErr.StatusCode)
		}
		return nil, explain(err)
	}
	if len(ids) == 0 {
		return nil, errors.New("接口没有列出任何模型，请直接填写模型名")
	}
	slices.Sort(ids)
	return slices.Compact(ids), nil
}

// Test 发一个最短的请求，确认地址、Key、模型名都对。
func Test(ctx context.Context, cfg settings.LLM) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	_, err := Chat(ctx, cfg, "你是连通性测试。", "只回复两个字：收到")
	return err
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

var thinking = regexp.MustCompile(`(?s)<think>.*?</think>`)

// stripThinking 去掉推理模型混在正文里的思考过程。
func stripThinking(s string) string {
	return strings.TrimSpace(thinking.ReplaceAllString(s, ""))
}

// explain 把 SDK 的错误换成用户能看懂、能照着处理的话。
func explain(err error) error {
	if apiErr, ok := errors.AsType[*openai.Error](err); ok {
		msg := apiErr.Message
		if msg == "" {
			msg = strings.TrimSpace(apiErr.RawJSON())
		}
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		hint := ""
		switch apiErr.StatusCode {
		case 401, 403:
			hint = "（检查 API Key，以及这个 Key 能不能用这个模型）"
		case 404:
			hint = "（检查接口地址和模型名；接口地址一般以 /v1 结尾）"
		case 429:
			hint = "（请求太频繁或额度用完了）"
		}
		return fmt.Errorf("大模型返回 %d：%s%s", apiErr.StatusCode, msg, hint)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("大模型超时没有响应")
	}
	if _, ok := errors.AsType[net.Error](err); ok {
		return fmt.Errorf("连不上大模型接口，检查接口地址和网络：%v", err)
	}
	return fmt.Errorf("调用大模型失败：%w", err)
}

package service

import (
	"context"
	"net/url"
	"strings"

	"zsmeet/internal/llm"
	"zsmeet/internal/settings"
)

// LLMSettings 返回大模型配置。API Key 不会传到接口层之外，那里只告诉网页「填没填」。
func (s *Service) LLMSettings(ctx context.Context) (settings.LLM, error) {
	return s.settings.LLM(ctx)
}

// LLMInput 是网页提交的大模型配置。APIKey 为空表示沿用已保存的。
type LLMInput struct {
	BaseURL string
	Model   string
	APIKey  string
}

func (s *Service) resolveLLM(ctx context.Context, in LLMInput) (settings.LLM, error) {
	cfg, err := s.resolveEndpoint(ctx, in)
	if err != nil {
		return settings.LLM{}, err
	}
	if cfg.Model == "" {
		return settings.LLM{}, invalid("模型名不能为空")
	}
	return cfg, nil
}

// resolveEndpoint 检查接口地址，Key 留空时用已保存的。模型名可以为空（列模型时还不知道）。
func (s *Service) resolveEndpoint(ctx context.Context, in LLMInput) (settings.LLM, error) {
	cfg := settings.LLM{
		BaseURL: strings.TrimRight(strings.TrimSpace(in.BaseURL), "/"),
		Model:   strings.TrimSpace(in.Model),
		APIKey:  strings.TrimSpace(in.APIKey),
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return settings.LLM{}, invalid("接口地址要以 http:// 或 https:// 开头，例如 https://api.openai.com/v1")
	}
	if cfg.APIKey == "" {
		saved, err := s.settings.LLM(ctx)
		if err != nil {
			return settings.LLM{}, err
		}
		cfg.APIKey = saved.APIKey
	}
	return cfg, nil
}

func (s *Service) SaveLLM(ctx context.Context, in LLMInput) (settings.LLM, error) {
	cfg, err := s.resolveLLM(ctx, in)
	if err != nil {
		return settings.LLM{}, err
	}
	return cfg, s.settings.SaveLLM(ctx, cfg)
}

// TestLLM 用网页上填的值（还没保存也可以）发一个最短的请求。
// 连不上、Key 不对这些都算业务错误，把原因原样告诉管理员。
func (s *Service) TestLLM(ctx context.Context, in LLMInput) error {
	cfg, err := s.resolveLLM(ctx, in)
	if err != nil {
		return err
	}
	if err := llm.Test(ctx, cfg); err != nil {
		return &Error{Invalid, err.Error()}
	}
	return nil
}

// LLMModels 用网页上填的接口地址和 Key（还没保存也可以）列出接口上的模型，给管理员挑。
func (s *Service) LLMModels(ctx context.Context, in LLMInput) ([]string, error) {
	cfg, err := s.resolveEndpoint(ctx, in)
	if err != nil {
		return nil, err
	}
	models, err := llm.Models(ctx, cfg)
	if err != nil {
		return nil, &Error{Invalid, err.Error()}
	}
	return models, nil
}

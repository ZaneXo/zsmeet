package service

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/ZaneXo/zsmeet/internal/cloudasr"
	"github.com/ZaneXo/zsmeet/internal/llm"
	"github.com/ZaneXo/zsmeet/internal/secret"
	"github.com/ZaneXo/zsmeet/internal/settings"
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
	// 模型的上下文长度（token 数），只能是 settings.ContextChoices 里的一档；0 表示按默认的 32K
	ContextTokens int
}

func (s *Service) resolveLLM(ctx context.Context, in LLMInput) (settings.LLM, error) {
	cfg, err := s.resolveEndpoint(ctx, in)
	if err != nil {
		return settings.LLM{}, err
	}
	if cfg.Model == "" {
		return settings.LLM{}, invalid("模型名不能为空")
	}
	if in.ContextTokens != 0 && !slices.Contains(settings.ContextChoices, in.ContextTokens) {
		return settings.LLM{}, invalid("上下文长度只能从列表里选（8K 到 200K），不知道的话选 32K")
	}
	cfg.ContextTokens = in.ContextTokens
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

// ---- 语音识别 ----

// ASRSettings 是网页上看到的语音识别配置和最近一次运行错误。API Key 本身不传出接口层。
type ASRSettings struct {
	settings.ASR
	LastError   string
	LastErrorAt *time.Time
}

// ASRInput 是管理员提交的语音识别配置。APIKey 留空表示沿用已保存的；ClearAPIKey 为 true 时清掉已保存的 Key。
type ASRInput struct {
	Provider    string
	BaseURL     string
	APIKey      string
	ClearAPIKey bool
}

// ASRSettings 返回语音识别配置和最近一次错误。主密钥改过解不开旧 Key 时不报 500，把原因放在 LastError 里让管理员重新填。
func (s *Service) ASRSettings(ctx context.Context) (ASRSettings, error) {
	cfg, err := s.settings.ASR(ctx)
	var decryptErr bool
	if errors.Is(err, secret.ErrDecrypt) {
		decryptErr = true
		err = nil
	}
	if err != nil {
		return ASRSettings{}, err
	}
	st, err := s.settings.ASRStatus(ctx)
	if err != nil {
		return ASRSettings{}, err
	}
	out := ASRSettings{ASR: cfg, LastError: st.LastError}
	if !st.UpdatedAt.IsZero() {
		t := st.UpdatedAt
		out.LastErrorAt = &t
	}
	if decryptErr && out.LastError == "" {
		out.LastError = secret.ErrDecrypt.Error()
	}
	return out, nil
}

func checkASRURL(raw string) (string, error) {
	cleaned := strings.TrimRight(strings.TrimSpace(raw), "/")
	if cleaned == "" {
		cleaned = settings.DefaultASRBaseURL
	}
	u, err := url.Parse(cleaned)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return "", invalid("接口地址要以 http:// 或 https:// 开头，例如 %s", settings.DefaultASRBaseURL)
	}
	return cleaned, nil
}

func (s *Service) resolveASRKey(ctx context.Context, key string, clearKey bool) (string, error) {
	if clearKey {
		return "", nil
	}
	key = strings.TrimSpace(key)
	if key != "" {
		return key, nil
	}
	saved, err := s.settings.ASR(ctx)
	if err != nil && !errors.Is(err, secret.ErrDecrypt) {
		return "", err
	}
	return saved.APIKey, nil
}

// SaveASR 保存语音识别配置。
func (s *Service) SaveASR(ctx context.Context, in ASRInput) (ASRSettings, error) {
	provider := strings.TrimSpace(in.Provider)
	if provider != settings.ASRLocal && provider != settings.ASRDeepSeek {
		return ASRSettings{}, invalid("识别方式只能选「本机识别」或「DeepSeek 在线识别」")
	}
	baseURL, err := checkASRURL(in.BaseURL)
	if err != nil {
		return ASRSettings{}, err
	}
	apiKey, err := s.resolveASRKey(ctx, in.APIKey, in.ClearAPIKey)
	if err != nil {
		return ASRSettings{}, err
	}
	if provider == settings.ASRDeepSeek && apiKey == "" {
		return ASRSettings{}, invalid("打开 DeepSeek 在线识别需要填写 API Key")
	}
	cfg := settings.ASR{Provider: provider, BaseURL: baseURL, APIKey: apiKey}
	if err := s.settings.SaveASR(ctx, cfg); err != nil {
		return ASRSettings{}, err
	}
	return ASRSettings{ASR: cfg}, nil
}

// TestASR 用提交的接口地址和 API Key（不需要先保存）发 1 秒静音测试能不能用。
func (s *Service) TestASR(ctx context.Context, in ASRInput) error {
	baseURL, err := checkASRURL(in.BaseURL)
	if err != nil {
		return err
	}
	apiKey, err := s.resolveASRKey(ctx, in.APIKey, false)
	if err != nil {
		return err
	}
	if apiKey == "" {
		return invalid("请先填写 DeepSeek 的 API Key")
	}
	if err := cloudasr.Test(ctx, baseURL, apiKey); err != nil {
		return &Error{Invalid, err.Error()}
	}
	return nil
}

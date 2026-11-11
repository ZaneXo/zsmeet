package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ZaneXo/zsmeet/internal/service"
	"github.com/ZaneXo/zsmeet/internal/settings"
)

type LLMSettings struct {
	BaseURL string `json:"base_url"`
	Model   string `json:"model"`
	// 已保存的 Key 不会返回给网页，只说有没有
	APIKeySet     bool `json:"api_key_set" doc:"是否已经保存了 API Key；Key 本身不会返回"`
	ContextTokens int  `json:"context_tokens" doc:"模型的上下文长度（token 数）。没设置过时是默认的 32768"`
}

type LLMSettingsOutput struct {
	Body LLMSettings
}

func toLLMSettings(cfg settings.LLM) *LLMSettingsOutput {
	return &LLMSettingsOutput{Body: LLMSettings{BaseURL: cfg.BaseURL, Model: cfg.Model, APIKeySet: cfg.APIKey != "", ContextTokens: cfg.Context()}}
}

var getLLMSettingsOp = huma.Operation{
	OperationID: "get-llm-settings",
	Method:      http.MethodGet,
	Path:        "/api/settings/llm",
	Summary:     "大模型配置",
	Tags:        []string{"设置"},
	Security:    adminOnly,
}

func (h *Handlers) GetLLMSettings(ctx context.Context, _ *struct{}) (*LLMSettingsOutput, error) {
	cfg, err := h.svc.LLMSettings(ctx)
	if err != nil {
		return nil, toHTTP(err)
	}
	return toLLMSettings(cfg), nil
}

type LLMSettingsInput struct {
	Body struct {
		BaseURL       string `json:"base_url" maxLength:"500" doc:"OpenAI 兼容接口的地址，一般以 /v1 结尾"`
		Model         string `json:"model" minLength:"1" maxLength:"200"`
		APIKey        string `json:"api_key,omitempty" maxLength:"500" doc:"留空表示沿用已保存的 Key"`
		ContextTokens int    `json:"context_tokens,omitempty" doc:"模型的上下文长度（token 数）：8192、16384、32768、65536、131072 或 200000（200K 及以上都选它）。不填或 0 表示默认的 32768。长会议按它分段交给大模型"`
	}
}

func (in *LLMSettingsInput) toService() service.LLMInput {
	return service.LLMInput{BaseURL: in.Body.BaseURL, Model: in.Body.Model, APIKey: in.Body.APIKey, ContextTokens: in.Body.ContextTokens}
}

var putLLMSettingsOp = huma.Operation{
	OperationID: "put-llm-settings",
	Method:      http.MethodPut,
	Path:        "/api/settings/llm",
	Summary:     "保存大模型配置",
	Description: "全团队共用一份。API Key 加密后保存。",
	Tags:        []string{"设置"},
	Security:    adminOnly,
}

func (h *Handlers) PutLLMSettings(ctx context.Context, in *LLMSettingsInput) (*LLMSettingsOutput, error) {
	cfg, err := h.svc.SaveLLM(ctx, in.toService())
	if err != nil {
		return nil, toHTTP(err)
	}
	return toLLMSettings(cfg), nil
}

var testLLMSettingsOp = huma.Operation{
	OperationID:   "test-llm-settings",
	Method:        http.MethodPost,
	Path:          "/api/settings/llm/test",
	Summary:       "测试大模型能不能连上",
	Description:   "用提交的值（不需要先保存）发一个最短的请求。连不上时返回 422，detail 里是原因。",
	Tags:          []string{"设置"},
	Security:      adminOnly,
	DefaultStatus: http.StatusNoContent,
}

func (h *Handlers) TestLLMSettings(ctx context.Context, in *LLMSettingsInput) (*struct{}, error) {
	if err := h.svc.TestLLM(ctx, in.toService()); err != nil {
		return nil, toHTTP(err)
	}
	return nil, nil
}

type LLMModelsInput struct {
	Body struct {
		BaseURL string `json:"base_url" maxLength:"500" doc:"OpenAI 兼容接口的地址，一般以 /v1 结尾"`
		APIKey  string `json:"api_key,omitempty" maxLength:"500" doc:"留空表示沿用已保存的 Key"`
	}
}

type LLMModelsOutput struct {
	Body struct {
		Models []string `json:"models" nullable:"false" doc:"模型名，按字母排序"`
	}
}

var listLLMModelsOp = huma.Operation{
	OperationID: "list-llm-models",
	Method:      http.MethodPost,
	Path:        "/api/settings/llm/models",
	Summary:     "列出接口上可用的模型",
	Description: "用提交的接口地址和 Key（不需要先保存）请求「接口地址/models」。接口不支持列出模型、连不上时返回 422，detail 里是原因，这时请直接填写模型名。",
	Tags:        []string{"设置"},
	Security:    adminOnly,
}

func (h *Handlers) ListLLMModels(ctx context.Context, in *LLMModelsInput) (*LLMModelsOutput, error) {
	models, err := h.svc.LLMModels(ctx, service.LLMInput{BaseURL: in.Body.BaseURL, APIKey: in.Body.APIKey})
	if err != nil {
		return nil, toHTTP(err)
	}
	out := &LLMModelsOutput{}
	out.Body.Models = models
	return out, nil
}

// ---- 录音保留期 ----

type RetentionOutput struct {
	Body struct {
		AudioDays int `json:"audio_days" doc:"录音保留多少天，0 表示一直保留。过了保留期只删录音文件，转写和纪要一直留着"`
	}
}

var getRetentionOp = huma.Operation{
	OperationID: "get-retention",
	Method:      http.MethodGet,
	Path:        "/api/settings/retention",
	Summary:     "录音保留多久",
	Tags:        []string{"设置"},
	Security:    adminOnly,
}

func (h *Handlers) GetRetention(ctx context.Context, _ *struct{}) (*RetentionOutput, error) {
	r, err := h.svc.Retention(ctx)
	if err != nil {
		return nil, toHTTP(err)
	}
	out := &RetentionOutput{}
	out.Body.AudioDays = r.AudioDays
	return out, nil
}

type RetentionInput struct {
	Body struct {
		AudioDays int `json:"audio_days" minimum:"0" maximum:"3650" doc:"录音保留多少天，7 到 3650，或者 0 表示一直保留"`
	}
}

var putRetentionOp = huma.Operation{
	OperationID: "put-retention",
	Method:      http.MethodPut,
	Path:        "/api/settings/retention",
	Summary:     "保存录音保留多久",
	Description: "改短以后，后台巡检时（每分钟一次）会删掉已经过期的录音文件，删了不能恢复。转写和纪要不受影响。",
	Tags:        []string{"设置"},
	Security:    adminOnly,
}

func (h *Handlers) PutRetention(ctx context.Context, in *RetentionInput) (*RetentionOutput, error) {
	r, err := h.svc.SaveRetention(ctx, in.Body.AudioDays)
	if err != nil {
		return nil, toHTTP(err)
	}
	out := &RetentionOutput{}
	out.Body.AudioDays = r.AudioDays
	return out, nil
}

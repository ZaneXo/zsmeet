// Package settings 读写管理员在网页里填的团队配置（存在 settings 表里）。
// 密钥字段落库前加密，读出来时解密。
package settings

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/secret"
)

// LLM 是写纪要用的大模型，任意 OpenAI 兼容接口。
type LLM struct {
	BaseURL string `json:"base_url"`
	Model   string `json:"model"`
	// 自己部署的 Ollama、vLLM 可以不填
	APIKey string `json:"api_key"`
}

// Ready 表示填了能发请求的最少信息。
func (l LLM) Ready() bool {
	return l.BaseURL != "" && l.Model != ""
}

type Store struct {
	q   *sqlc.Queries
	box *secret.Box
}

func New(q *sqlc.Queries, box *secret.Box) *Store {
	return &Store{q: q, box: box}
}

const keyLLM = "llm"

// LLM 返回大模型配置；还没配置时返回零值。
func (s *Store) LLM(ctx context.Context) (LLM, error) {
	raw, err := s.q.GetSetting(ctx, keyLLM)
	if errors.Is(err, pgx.ErrNoRows) {
		return LLM{}, nil
	}
	if err != nil {
		return LLM{}, err
	}
	var l LLM
	if err := json.Unmarshal(raw, &l); err != nil {
		return LLM{}, err
	}
	l.APIKey, err = s.box.Open(l.APIKey)
	return l, err
}

func (s *Store) SaveLLM(ctx context.Context, l LLM) error {
	l.BaseURL = strings.TrimRight(strings.TrimSpace(l.BaseURL), "/")
	l.Model = strings.TrimSpace(l.Model)
	l.APIKey = s.box.Seal(strings.TrimSpace(l.APIKey))
	raw, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return s.q.PutSetting(ctx, sqlc.PutSettingParams{Key: keyLLM, Value: raw})
}

// Feishu 是飞书（或 Lark）自建应用的配置：用它的机器人往群里发纪要、导入飞书文档、导入飞书会议的录制。
type Feishu struct {
	// "feishu" 或 "lark"（飞书国际版）
	Domain string `json:"domain"`
	AppID  string `json:"app_id"`
	// 加密后保存
	AppSecret string `json:"app_secret"`
	// zsmeet 的访问地址，例如 https://meet.example.com。卡片上「查看纪要」的链接用它拼
	SiteURL string `json:"site_url"`

	// 默认发到的群
	ChatID   string `json:"chat_id"`
	ChatName string `json:"chat_name"`
	// 纪要第一次生成后自动发到默认群
	AutoPush bool `json:"auto_push"`
	// 发到群里时同时导入成飞书文档，卡片上带文档的链接
	WithDoc bool `json:"with_doc"`
	// 文档放在哪个云空间文件夹（token）；为空时放在应用自己的云空间
	DocFolder string `json:"doc_folder"`
	// 公司里拿到文档链接的人都能看（不开时只有发到的群里的人能看）
	LinkShare bool `json:"link_share"`

	// 自动导入飞书会议的云录制（组织者是 zsmeet 成员的会议）
	ImportRecordings bool `json:"import_recordings"`
}

// Ready 表示填了凭证，可以调用飞书。
func (f Feishu) Ready() bool {
	return f.AppID != "" && f.AppSecret != ""
}

const keyFeishu = "feishu"

// Feishu 返回飞书配置；还没配置时返回零值。
func (s *Store) Feishu(ctx context.Context) (Feishu, error) {
	raw, err := s.q.GetSetting(ctx, keyFeishu)
	if errors.Is(err, pgx.ErrNoRows) {
		return Feishu{Domain: "feishu"}, nil
	}
	if err != nil {
		return Feishu{}, err
	}
	var f Feishu
	if err := json.Unmarshal(raw, &f); err != nil {
		return Feishu{}, err
	}
	f.AppSecret, err = s.box.Open(f.AppSecret)
	return f, err
}

func (s *Store) SaveFeishu(ctx context.Context, f Feishu) error {
	f.AppSecret = s.box.Seal(f.AppSecret)
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return s.q.PutSetting(ctx, sqlc.PutSettingParams{Key: keyFeishu, Value: raw})
}

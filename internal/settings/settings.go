// Package settings 读写管理员在网页里填的团队配置（存在 settings 表里）。
// 密钥字段落库前加密，读出来时解密。
package settings

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"zsmeet/internal/db/sqlc"
	"zsmeet/internal/secret"
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

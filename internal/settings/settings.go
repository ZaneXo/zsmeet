// Package settings 读写管理员在网页里填的团队配置（存在 settings 表里）。
// 密钥字段落库前加密，读出来时解密。
package settings

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

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
	// 模型的上下文长度（token 数），只能是 ContextChoices 里的一档。0 表示没设置过（以前保存的配置里没有这一项），
	// 按 DefaultContextTokens 算。长会议按它分段交给大模型（见 llm.TranscriptChars）
	ContextTokens int `json:"context_tokens,omitempty"`
}

// Ready 表示填了能发请求的最少信息。
func (l LLM) Ready() bool {
	return l.BaseURL != "" && l.Model != ""
}

// DefaultContextTokens 是管理员没设置上下文长度时按多长算：32K，照顾只开了 32K 的本地模型。
const DefaultContextTokens = 32_768

// ContextChoices 是上下文长度能选的几档（token 数）。最后一档表示「200K 及以上」，再长的也按它算。
var ContextChoices = []int{8_192, 16_384, 32_768, 65_536, 131_072, 200_000}

// Context 返回按多长的上下文来安排每次交给大模型的内容：没设置过时是 DefaultContextTokens。
func (l LLM) Context() int {
	if l.ContextTokens <= 0 {
		return DefaultContextTokens
	}
	return l.ContextTokens
}

type Store struct {
	q   *sqlc.Queries
	box *secret.Box
	// 管理员还没在网页上保存过语音识别设置时用的默认识别语言（来自环境变量 ZSMEET_ASR_LANGUAGE）；为空时按 DefaultASRLanguage
	DefaultASRLanguage string
}

func New(q *sqlc.Queries, box *secret.Box) *Store {
	return &Store{q: q, box: box}
}

// WithTx 返回在事务里读写的 Store（q 是 sqlc.Queries.WithTx 得到的）：拿着锁读、写设置时用，读到的和锁保护的改动在同一个事务里。
func (s *Store) WithTx(q *sqlc.Queries) *Store {
	return &Store{q: q, box: s.box, DefaultASRLanguage: s.DefaultASRLanguage}
}

// Box 是加密凭证的那把钥匙。用飞书登录时，暂存在浏览器里的 state 也用它加密（见 secret.Box.SealFor）。
func (s *Store) Box() *secret.Box { return s.box }

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

	// 允许成员用飞书登录（扫码，或者在飞书网页版已登录时点一下）
	Login bool `json:"login"`
	// 飞书账号还不是成员时自动创建一个成员账号；不开的话只有已经是成员的人能用飞书登录
	LoginAutoJoin bool `json:"login_auto_join"`
}

// LoginReady 表示可以用飞书登录：打开了飞书登录，填了凭证和 zsmeet 的访问地址（飞书登录完要跳回这个地址）。
func (f Feishu) LoginReady() bool {
	return f.Login && f.Ready() && f.SiteURL != ""
}

// Ready 表示填了凭证，可以调用飞书。
func (f Feishu) Ready() bool {
	return f.AppID != "" && f.AppSecret != ""
}

const keyFeishu = "feishu"

// Feishu 返回飞书配置；还没配置时返回零值。
// Feishu 返回飞书配置。访问地址只存一份（见 SiteURL）：SiteURL 字段填的是通用的那个，没有时才是以前存在飞书配置里的旧值。
// App Secret 解不开（主密钥改过）时返回 secret.ErrDecrypt，其他字段照样有。
func (s *Store) Feishu(ctx context.Context) (Feishu, error) {
	f, err := s.feishuStored(ctx)
	if err != nil && !errors.Is(err, secret.ErrDecrypt) {
		return f, err
	}
	site, serr := s.Site(ctx)
	if serr != nil {
		return Feishu{}, serr
	}
	if site.URL != "" {
		f.SiteURL = site.URL
	}
	return f, err
}

// feishuStored 是配置表里存的飞书配置原样。
func (s *Store) feishuStored(ctx context.Context) (Feishu, error) {
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

// SaveFeishu 保存飞书配置。访问地址存进通用设置（和「设置 → 群机器人」里的是同一个），飞书配置里不再存；
// 以前存在飞书配置里的旧地址，在通用设置还空着时先搬过去，免得清掉飞书配置时一起丢了。
func (s *Store) SaveFeishu(ctx context.Context, f Feishu) error {
	site, err := s.Site(ctx)
	if err != nil {
		return err
	}
	testHookAfterSiteRead()
	switch {
	case f.SiteURL != "" && f.SiteURL != site.URL:
		// 管理员在飞书设置里填了新地址
		if err := s.SaveSite(ctx, Site{URL: f.SiteURL}); err != nil {
			return err
		}
	case f.SiteURL == "" && site.URL == "":
		old, err := s.feishuStored(ctx)
		if err != nil && !errors.Is(err, secret.ErrDecrypt) {
			return err
		}
		// 搬旧地址时通用设置可能刚被别人填上（上面读到的已经过时了）：只在它还空着时写，不盖掉新填的
		if old.SiteURL != "" {
			raw, err := json.Marshal(Site{URL: cleanURL(old.SiteURL)})
			if err != nil {
				return err
			}
			if err := s.q.PutSettingIfNoURL(ctx, sqlc.PutSettingIfNoURLParams{Key: keySite, Value: raw}); err != nil {
				return err
			}
		}
	}
	f.SiteURL = ""
	f.AppSecret = s.box.Seal(f.AppSecret)
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return s.q.PutSetting(ctx, sqlc.PutSettingParams{Key: keyFeishu, Value: raw})
}

// Retention 是录音保留多久。过了保留期只删录音文件，转写和纪要一直留着。
type Retention struct {
	// 录音保留多少天，0 表示一直保留
	AudioDays int `json:"audio_days"`
}

// DefaultAudioDays 是管理员没设置过时录音保留的天数。
const DefaultAudioDays = 90

const keyRetention = "retention"

// Retention 返回录音保留多久；没设置过时是 DefaultAudioDays 天。
func (s *Store) Retention(ctx context.Context) (Retention, error) {
	raw, err := s.q.GetSetting(ctx, keyRetention)
	if errors.Is(err, pgx.ErrNoRows) {
		return Retention{AudioDays: DefaultAudioDays}, nil
	}
	if err != nil {
		return Retention{}, err
	}
	var r Retention
	return r, json.Unmarshal(raw, &r)
}

func (s *Store) SaveRetention(ctx context.Context, r Retention) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return s.q.PutSetting(ctx, sqlc.PutSettingParams{Key: keyRetention, Value: raw})
}

// Voiceprints 是「记住说话人」：有人在会议里给说话人改了名字以后，之后的会议按声音认出这个人、自动用这个名字。
// 声音特征属于生物特征信息，默认关着；关着的时候一点声音特征都不存。
type Voiceprints struct {
	Enabled bool `json:"enabled"`
}

const keyVoiceprints = "voiceprints"

// Voiceprints 返回声纹的设置；没设置过时是关着的。
func (s *Store) Voiceprints(ctx context.Context) (Voiceprints, error) {
	raw, err := s.q.GetSetting(ctx, keyVoiceprints)
	if errors.Is(err, pgx.ErrNoRows) {
		return Voiceprints{}, nil
	}
	if err != nil {
		return Voiceprints{}, err
	}
	var v Voiceprints
	return v, json.Unmarshal(raw, &v)
}

func (s *Store) SaveVoiceprints(ctx context.Context, v Voiceprints) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.q.PutSetting(ctx, sqlc.PutSettingParams{Key: keyVoiceprints, Value: raw})
}

// Site 是 zsmeet 自己的配置：发到群里的消息要链接回 zsmeet，得知道同事们打开它用的地址。
type Site struct {
	// zsmeet 的访问地址，例如 https://meet.example.com，结尾不带斜杠
	URL string `json:"url"`
}

const keySite = "site"

// Site 返回 zsmeet 的访问地址；还没设置时返回零值。
func (s *Store) Site(ctx context.Context) (Site, error) {
	raw, err := s.q.GetSetting(ctx, keySite)
	if errors.Is(err, pgx.ErrNoRows) {
		return Site{}, nil
	}
	if err != nil {
		return Site{}, err
	}
	var site Site
	return site, json.Unmarshal(raw, &site)
}

// testHookAfterSiteRead 在 SaveFeishu 读完访问地址之后调用，测试用它模拟这时有人同时保存了访问地址。
var testHookAfterSiteRead = func() {}

func cleanURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

func (s *Store) SaveSite(ctx context.Context, site Site) error {
	site.URL = cleanURL(site.URL)
	raw, err := json.Marshal(site)
	if err != nil {
		return err
	}
	return s.q.PutSetting(ctx, sqlc.PutSettingParams{Key: keySite, Value: raw})
}

// SiteURL 返回拼链接用的 zsmeet 访问地址（飞书的卡片和文档、飞书登录的回调、群机器人的消息都用它）。
// 飞书设置和「设置 → 群机器人」里改的是同一个；还没在新版本里保存过时，用以前存在飞书配置里的旧值。都没填时为空。
func (s *Store) SiteURL(ctx context.Context) (string, error) {
	site, err := s.Site(ctx)
	if err != nil || site.URL != "" {
		return site.URL, err
	}
	// 只用飞书配置里的地址：App Secret 解不开（主密钥改过）也不影响
	f, err := s.feishuStored(ctx)
	if err != nil && !errors.Is(err, secret.ErrDecrypt) {
		return "", err
	}
	return strings.TrimRight(f.SiteURL, "/"), nil
}

// Seal 用主密钥加密不在这张配置表里的凭证（比如群机器人的 Webhook 地址）。空字符串原样返回。
func (s *Store) Seal(plain string) string {
	return s.box.Seal(plain)
}

// Open 解开 Seal 加密的凭证。
func (s *Store) Open(sealed string) (string, error) {
	return s.box.Open(sealed)
}

// 语音识别的两种方式和可选语言。
const (
	ASRLocal    = "local"
	ASRDeepSeek = "deepseek"

	// DefaultASRLanguage 是默认的识别语言：自动识别多语种与混合语音。
	DefaultASRLanguage = "auto"

	// DefaultASRBaseURL 是 DeepSeek 在线语音识别接口的默认地址。
	DefaultASRBaseURL = "https://api.deepseek.com/v1/tools/asr"
)

// ASRLanguages 是支持的识别语言：auto 是自动识别多语种与混合语音，其余是固定单种语言。
var ASRLanguages = []string{"auto", "zh", "en", "yue", "ja", "ko"}

// ASR 是团队级的语音识别配置。默认只用服务器本机识别（Provider 为 "local"），录音不外发；
// 打开 "deepseek" 后，录音切成小段发给 DeepSeek 在线识别，本机 SenseVoice 仍同时运行以提供字级时间并在失败时兜底。
type ASR struct {
	Provider string `json:"provider"`
	Language string `json:"language,omitempty"`
	BaseURL  string `json:"base_url,omitempty"`
	// 加密后保存
	APIKey string `json:"api_key,omitempty"`
}

// EffectiveLanguage 返回团队默认的识别语言。
func (a ASR) EffectiveLanguage() string {
	if slices.Contains(ASRLanguages, a.Language) {
		return a.Language
	}
	return DefaultASRLanguage
}

func (s *Store) defaultLanguage() string {
	if slices.Contains(ASRLanguages, s.DefaultASRLanguage) {
		return s.DefaultASRLanguage
	}
	return DefaultASRLanguage
}

// EffectiveURL 返回在线识别实际请求的接口地址。
func (a ASR) EffectiveURL() string {
	if strings.TrimSpace(a.BaseURL) != "" {
		return strings.TrimSpace(a.BaseURL)
	}
	return DefaultASRBaseURL
}

// OnlineReady 表示管理员打开了在线识别且填了凭证。
func (a ASR) OnlineReady() bool {
	return a.Provider == ASRDeepSeek && a.APIKey != ""
}

// ASRStatus 是后台处理会议调用在线识别时记下的最近一次错误（比如 API Key 失效、欠费），单独存一行以免和管理员改配置互相覆盖。
type ASRStatus struct {
	LastError string    `json:"last_error,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

const (
	keyASR       = "asr"
	keyASRStatus = "asr_status"
)

// ASR 返回语音识别配置；还没设置过时返回本机识别和默认的接口地址。
// API Key 解不开（主密钥改过）时返回 secret.ErrDecrypt，其余字段照样有。
func (s *Store) ASR(ctx context.Context) (ASR, error) {
	cfg, _, err := s.ASRWithTime(ctx)
	return cfg, err
}

// ASRWithTime 返回语音识别配置和这一行最后一次保存的时间（后台写错误状态时用来核对配置期间没被改过）。
func (s *Store) ASRWithTime(ctx context.Context) (ASR, time.Time, error) {
	row, err := s.q.GetSettingWithTime(ctx, keyASR)
	if errors.Is(err, pgx.ErrNoRows) {
		return ASR{Provider: ASRLocal, Language: s.defaultLanguage(), BaseURL: DefaultASRBaseURL}, time.Time{}, nil
	}
	if err != nil {
		return ASR{}, time.Time{}, err
	}
	var a ASR
	if err := json.Unmarshal(row.Value, &a); err != nil {
		return ASR{}, time.Time{}, err
	}
	if a.Provider == "" {
		a.Provider = ASRLocal
	}
	if !slices.Contains(ASRLanguages, a.Language) {
		a.Language = s.defaultLanguage()
	}
	if a.BaseURL == "" {
		a.BaseURL = DefaultASRBaseURL
	}
	a.APIKey, err = s.box.Open(a.APIKey)
	return a, row.UpdatedAt, err
}

// SaveASR 保存语音识别配置，并清掉旧的最近一次错误。
func (s *Store) SaveASR(ctx context.Context, a ASR) error {
	if a.Provider == "" {
		a.Provider = ASRLocal
	}
	if !slices.Contains(ASRLanguages, a.Language) {
		a.Language = s.defaultLanguage()
	}
	a.BaseURL = strings.TrimRight(strings.TrimSpace(a.BaseURL), "/")
	if a.BaseURL == "" {
		a.BaseURL = DefaultASRBaseURL
	}
	a.APIKey = s.box.Seal(strings.TrimSpace(a.APIKey))
	raw, err := json.Marshal(a)
	if err != nil {
		return err
	}
	if err := s.q.PutSetting(ctx, sqlc.PutSettingParams{Key: keyASR, Value: raw}); err != nil {
		return err
	}
	return s.q.DeleteSetting(ctx, keyASRStatus)
}

// ASRStatus 返回在线识别最近一次记下的错误；没有时返回零值。
func (s *Store) ASRStatus(ctx context.Context) (ASRStatus, error) {
	raw, err := s.q.GetSetting(ctx, keyASRStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return ASRStatus{}, nil
	}
	if err != nil {
		return ASRStatus{}, err
	}
	var st ASRStatus
	return st, json.Unmarshal(raw, &st)
}

// RecordASRError 记下在线识别最近一次错误；只在这项配置自 asrUpdatedAt 以来没被管理员改过时写入。
func (s *Store) RecordASRError(ctx context.Context, asrUpdatedAt time.Time, msg string) error {
	if asrUpdatedAt.IsZero() || msg == "" {
		return nil
	}
	raw, err := json.Marshal(ASRStatus{LastError: msg, UpdatedAt: time.Now().UTC()})
	if err != nil {
		return err
	}
	return s.q.PutASRStatusIfUnchanged(ctx, sqlc.PutASRStatusIfUnchangedParams{
		Value:        raw,
		AsrUpdatedAt: asrUpdatedAt,
	})
}

// ClearASRError 在在线识别恢复正常后清掉之前记下的错误（同样只在配置没被改过时动它）。
func (s *Store) ClearASRError(ctx context.Context, asrUpdatedAt time.Time) error {
	if asrUpdatedAt.IsZero() {
		return nil
	}
	return s.q.ClearASRStatusIfUnchanged(ctx, asrUpdatedAt)
}

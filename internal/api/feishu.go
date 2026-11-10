package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/feishu"
	"github.com/ZaneXo/zsmeet/internal/service"
	"github.com/ZaneXo/zsmeet/internal/settings"
)

// ---- 飞书：管理员的配置 ----

type FeishuSettings struct {
	Domain string `json:"domain" enum:"feishu,lark" doc:"飞书，或者飞书国际版 Lark"`
	AppID  string `json:"app_id"`
	// 已保存的 Secret 不会返回给网页，只说有没有
	AppSecretSet     bool   `json:"app_secret_set" doc:"是否已经保存了 App Secret；Secret 本身不会返回"`
	SiteURL          string `json:"site_url" doc:"zsmeet 的访问地址，卡片上的链接用它拼"`
	ChatID           string `json:"chat_id" doc:"默认发到的群"`
	ChatName         string `json:"chat_name"`
	AutoPush         bool   `json:"auto_push" doc:"纪要第一次生成后自动发到默认的群"`
	WithDoc          bool   `json:"with_doc" doc:"发到群里时同时导入成飞书文档"`
	DocFolder        string `json:"doc_folder" doc:"文档放在哪个云空间文件夹（token）；为空时放在应用自己的云空间"`
	LinkShare        bool   `json:"link_share" doc:"公司里拿到文档链接的人都能看"`
	ImportRecordings bool   `json:"import_recordings" doc:"自动导入飞书会议的云录制"`
	ConsoleURL       string `json:"console_url" doc:"这个应用在飞书开发者后台的页面，没填 App ID 时为空"`
}

type FeishuSettingsOutput struct {
	Body FeishuSettings
}

func toFeishuSettings(cfg settings.Feishu) *FeishuSettingsOutput {
	out := &FeishuSettingsOutput{Body: FeishuSettings{
		Domain: cfg.Domain, AppID: cfg.AppID, AppSecretSet: cfg.AppSecret != "", SiteURL: cfg.SiteURL,
		ChatID: cfg.ChatID, ChatName: cfg.ChatName, AutoPush: cfg.AutoPush, WithDoc: cfg.WithDoc,
		DocFolder: cfg.DocFolder, LinkShare: cfg.LinkShare, ImportRecordings: cfg.ImportRecordings,
	}}
	if out.Body.Domain == "" {
		out.Body.Domain = "feishu"
	}
	if cfg.AppID != "" {
		out.Body.ConsoleURL = feishu.ConsoleURL(cfg.Domain, cfg.AppID)
	}
	return out
}

var getFeishuSettingsOp = huma.Operation{
	OperationID: "get-feishu-settings",
	Method:      http.MethodGet,
	Path:        "/api/settings/feishu",
	Summary:     "飞书配置",
	Tags:        []string{"设置"},
	Security:    adminOnly,
}

func (h *Handlers) GetFeishuSettings(ctx context.Context, _ *struct{}) (*FeishuSettingsOutput, error) {
	cfg, err := h.svc.FeishuSettings(ctx)
	if err != nil {
		return nil, toHTTP(err)
	}
	return toFeishuSettings(cfg), nil
}

type FeishuSettingsInput struct {
	Body struct {
		Domain           string `json:"domain,omitempty" enum:"feishu,lark"`
		AppID            string `json:"app_id" maxLength:"100" doc:"清空表示不用飞书了，其他配置一起清掉"`
		AppSecret        string `json:"app_secret,omitempty" maxLength:"200" doc:"留空表示沿用已保存的（App ID 没变时）"`
		SiteURL          string `json:"site_url,omitempty" maxLength:"500"`
		ChatID           string `json:"chat_id,omitempty" maxLength:"100"`
		ChatName         string `json:"chat_name,omitempty" maxLength:"200"`
		AutoPush         bool   `json:"auto_push,omitempty"`
		WithDoc          bool   `json:"with_doc,omitempty"`
		DocFolder        string `json:"doc_folder,omitempty" maxLength:"500" doc:"云空间文件夹的链接或者 token"`
		LinkShare        bool   `json:"link_share,omitempty"`
		ImportRecordings bool   `json:"import_recordings,omitempty"`
	}
}

func (in *FeishuSettingsInput) toService() service.FeishuInput {
	b := in.Body
	return service.FeishuInput{
		Domain: b.Domain, AppID: b.AppID, AppSecret: b.AppSecret, SiteURL: b.SiteURL, ChatID: b.ChatID, ChatName: b.ChatName,
		AutoPush: b.AutoPush, WithDoc: b.WithDoc, DocFolder: b.DocFolder, LinkShare: b.LinkShare, ImportRecordings: b.ImportRecordings,
	}
}

var putFeishuSettingsOp = huma.Operation{
	OperationID: "put-feishu-settings",
	Method:      http.MethodPut,
	Path:        "/api/settings/feishu",
	Summary:     "保存飞书配置",
	Description: "全团队共用一个飞书自建应用。App Secret 加密后保存。",
	Tags:        []string{"设置"},
	Security:    adminOnly,
}

func (h *Handlers) PutFeishuSettings(ctx context.Context, in *FeishuSettingsInput) (*FeishuSettingsOutput, error) {
	cfg, err := h.svc.SaveFeishu(ctx, in.toService())
	if err != nil {
		return nil, toHTTP(err)
	}
	return toFeishuSettings(cfg), nil
}

type FeishuChat struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func toFeishuChats(chats []feishu.Chat) []FeishuChat {
	out := make([]FeishuChat, len(chats))
	for i, c := range chats {
		out[i] = FeishuChat{ID: c.ID, Name: c.Name}
	}
	return out
}

type FeishuTestInput struct {
	Body struct {
		Domain    string `json:"domain,omitempty" enum:"feishu,lark"`
		AppID     string `json:"app_id" maxLength:"100"`
		AppSecret string `json:"app_secret,omitempty" maxLength:"200" doc:"留空表示沿用已保存的（App ID 没变时）"`
	}
}

type FeishuTestOutput struct {
	Body struct {
		BotName string       `json:"bot_name" doc:"应用机器人的名字"`
		Chats   []FeishuChat `json:"chats" nullable:"false" doc:"机器人所在的群"`
	}
}

var testFeishuOp = huma.Operation{
	OperationID: "test-feishu",
	Method:      http.MethodPost,
	Path:        "/api/settings/feishu/test",
	Summary:     "测试飞书应用能不能连上",
	Description: "用提交的凭证（不需要先保存）读机器人的信息和它所在的群。凭证不对、没开机器人能力、缺权限时返回 422，detail 里是原因。",
	Tags:        []string{"设置"},
	Security:    adminOnly,
}

func (h *Handlers) TestFeishu(ctx context.Context, in *FeishuTestInput) (*FeishuTestOutput, error) {
	check, err := h.svc.TestFeishu(ctx, service.FeishuInput{Domain: in.Body.Domain, AppID: in.Body.AppID, AppSecret: in.Body.AppSecret})
	if err != nil {
		return nil, toHTTP(err)
	}
	out := &FeishuTestOutput{}
	out.Body.BotName, out.Body.Chats = check.BotName, toFeishuChats(check.Chats)
	return out, nil
}

// FeishuImport 是一场自动导入的飞书会议。
type FeishuImport struct {
	FeishuMeetingID string    `json:"feishu_meeting_id" doc:"飞书的会议 ID"`
	Topic           string    `json:"topic"`
	Status          string    `json:"status" enum:"waiting,imported,skipped,failed" doc:"waiting 等飞书生成录制；imported 已导入；skipped 没有录制；failed 出错了"`
	Detail          string    `json:"detail" doc:"没导入的原因"`
	OwnerName       string    `json:"owner_name" doc:"会议组织者（导入后的会议记在他名下）"`
	MeetingID       string    `json:"meeting_id,omitempty" doc:"导入后的会议"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type FeishuStatusOutput struct {
	Body struct {
		State       string         `json:"state" enum:"off,connecting,connected,error" doc:"接收飞书会议事件的长连接：没开、连接中、已连上、出错了"`
		Detail      string         `json:"detail" doc:"出错的原因"`
		UpdatedAt   *time.Time     `json:"updated_at,omitempty"`
		LastEventAt *time.Time     `json:"last_event_at,omitempty" doc:"最近一次处理飞书会议的时间"`
		LastEvent   string         `json:"last_event" doc:"最近一次处理飞书会议的情况"`
		Imports     []FeishuImport `json:"imports" nullable:"false" doc:"最近自动导入的飞书会议（只有组织者是成员的会议）"`
	}
}

var getFeishuStatusOp = huma.Operation{
	OperationID: "get-feishu-status",
	Method:      http.MethodGet,
	Path:        "/api/settings/feishu/status",
	Summary:     "飞书运行情况",
	Description: "接收会议事件的长连接连上没有、最近一次收到的会议、最近自动导入的会议。",
	Tags:        []string{"设置"},
	Security:    adminOnly,
}

func (h *Handlers) GetFeishuStatus(ctx context.Context, _ *struct{}) (*FeishuStatusOutput, error) {
	st, err := h.svc.FeishuStatus(ctx)
	if err != nil {
		return nil, toHTTP(err)
	}
	out := &FeishuStatusOutput{}
	b := &out.Body
	b.State, b.Detail, b.LastEventAt, b.LastEvent = st.Link.State, st.Link.Detail, st.Link.LastEventAt, st.Link.LastEvent
	if !st.Link.UpdatedAt.IsZero() {
		b.UpdatedAt = &st.Link.UpdatedAt
	}
	b.Imports = make([]FeishuImport, len(st.Imports))
	for i, r := range st.Imports {
		imp := r.FeishuImport
		b.Imports[i] = FeishuImport{
			FeishuMeetingID: imp.FeishuMeetingID, Topic: imp.Topic, Status: imp.Status, Detail: imp.Detail, OwnerName: r.OwnerName,
			CreatedAt: imp.CreatedAt, UpdatedAt: imp.UpdatedAt,
		}
		if imp.MeetingID != nil {
			b.Imports[i].MeetingID = imp.MeetingID.String()
		}
	}
	return out, nil
}

// ---- 飞书：把纪要发出去 ----

type FeishuChatsOutput struct {
	Body struct {
		Items         []FeishuChat `json:"items" nullable:"false" doc:"应用机器人所在的群"`
		DefaultChatID string       `json:"default_chat_id" doc:"管理员设的默认群"`
		WithDoc       bool         `json:"with_doc" doc:"默认是否同时导入成飞书文档"`
	}
}

var listFeishuChatsOp = huma.Operation{
	OperationID: "list-feishu-chats",
	Method:      http.MethodGet,
	Path:        "/api/feishu/chats",
	Summary:     "可以发纪要的飞书群",
	Description: "应用机器人所在的群。没有配置飞书时返回 409。",
	Tags:        []string{"飞书"},
	Security:    loggedIn,
}

func (h *Handlers) ListFeishuChats(ctx context.Context, _ *struct{}) (*FeishuChatsOutput, error) {
	chats, cfg, err := h.svc.FeishuChats(ctx)
	if err != nil {
		return nil, toHTTP(err)
	}
	out := &FeishuChatsOutput{}
	out.Body.Items, out.Body.DefaultChatID, out.Body.WithDoc = toFeishuChats(chats), cfg.ChatID, cfg.WithDoc
	return out, nil
}

// FeishuShare 是一次「发到飞书」。
type FeishuShare struct {
	ID              string     `json:"id"`
	ChatID          string     `json:"chat_id" doc:"发到的群，只导入文档时为空"`
	ChatName        string     `json:"chat_name"`
	WithDoc         bool       `json:"with_doc" doc:"是否同时导入成飞书文档"`
	MinutesVersion  int32      `json:"minutes_version" doc:"发的是第几版纪要"`
	Status          string     `json:"status" enum:"pending,done,failed" doc:"pending 发送中；done 已发送；failed 失败"`
	Error           string     `json:"error" doc:"失败的原因"`
	DocURL          string     `json:"doc_url" doc:"导入的飞书文档"`
	RequestedByName string     `json:"requested_by_name" doc:"谁发的；纪要生成后自动发的为空"`
	CreatedAt       time.Time  `json:"created_at"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
}

func toFeishuShare(s sqlc.FeishuShare, by string) FeishuShare {
	return FeishuShare{
		ID: s.ID.String(), ChatID: s.ChatID, ChatName: s.ChatName, WithDoc: s.WithDoc, MinutesVersion: s.MinutesVersion,
		Status: s.Status, Error: s.Error, DocURL: s.DocUrl, RequestedByName: by, CreatedAt: s.CreatedAt, FinishedAt: s.FinishedAt,
	}
}

type ShareToFeishuInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		ChatID  string `json:"chat_id,omitempty" maxLength:"100" doc:"发到哪个群；为空表示只导入成文档"`
		WithDoc bool   `json:"with_doc,omitempty" doc:"同时导入成飞书文档"`
	}
}

type FeishuShareOutput struct {
	Body FeishuShare
}

var shareToFeishuOp = huma.Operation{
	OperationID: "share-to-feishu",
	Method:      http.MethodPost,
	Path:        "/api/meetings/{id}/feishu",
	Summary:     "把纪要发到飞书",
	Description: "把当前版本的纪要推到飞书群（卡片上是摘要、待办和链接），可以同时导入成飞书文档。后台发送，状态看 GET 同一个地址。创建者和管理员可以发。",
	Tags:        []string{"飞书"},
	Security:    loggedIn,
}

func (h *Handlers) ShareToFeishu(ctx context.Context, in *ShareToFeishuInput) (*FeishuShareOutput, error) {
	id, err := parseID(in.ID)
	if err != nil {
		return nil, err
	}
	user := currentUser(ctx)
	share, err := h.svc.ShareToFeishu(ctx, user, id, in.Body.ChatID, in.Body.WithDoc)
	if err != nil {
		return nil, toHTTP(err)
	}
	return &FeishuShareOutput{Body: toFeishuShare(share, user.Name)}, nil
}

type FeishuSharesOutput struct {
	Body struct {
		Items []FeishuShare `json:"items" nullable:"false" doc:"最新的在前，最多 20 条"`
	}
}

var listFeishuSharesOp = huma.Operation{
	OperationID: "list-feishu-shares",
	Method:      http.MethodGet,
	Path:        "/api/meetings/{id}/feishu",
	Summary:     "这场会议发到飞书的记录",
	Tags:        []string{"飞书"},
	Security:    loggedIn,
}

func (h *Handlers) ListFeishuShares(ctx context.Context, in *MeetingID) (*FeishuSharesOutput, error) {
	id, err := parseID(in.ID)
	if err != nil {
		return nil, err
	}
	rows, err := h.svc.FeishuShares(ctx, id)
	if err != nil {
		return nil, toHTTP(err)
	}
	out := &FeishuSharesOutput{}
	out.Body.Items = make([]FeishuShare, len(rows))
	for i, r := range rows {
		out.Body.Items[i] = toFeishuShare(r.FeishuShare, r.RequestedByName)
	}
	return out, nil
}

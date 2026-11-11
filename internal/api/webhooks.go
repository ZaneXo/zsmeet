package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/service"
)

// ---- zsmeet 的访问地址 ----

type SiteSettingsOutput struct {
	Body struct {
		URL       string `json:"url" doc:"zsmeet 的访问地址，发到群里的消息用它链接回会议；为空表示没填"`
		FeishuURL string `json:"feishu_url" doc:"飞书配置里填的访问地址。上面没填时用它"`
	}
}

func toSiteSettings(s service.SiteSettings) *SiteSettingsOutput {
	out := &SiteSettingsOutput{}
	out.Body.URL, out.Body.FeishuURL = s.Site.URL, s.FeishuURL
	return out
}

var getSiteSettingsOp = huma.Operation{
	OperationID: "get-site-settings",
	Method:      http.MethodGet,
	Path:        "/api/settings/site",
	Summary:     "zsmeet 的访问地址",
	Tags:        []string{"设置"},
	Security:    adminOnly,
}

func (h *Handlers) GetSiteSettings(ctx context.Context, _ *struct{}) (*SiteSettingsOutput, error) {
	s, err := h.svc.SiteSettings(ctx)
	if err != nil {
		return nil, toHTTP(err)
	}
	return toSiteSettings(s), nil
}

type SiteSettingsInput struct {
	Body struct {
		URL string `json:"url" maxLength:"500" doc:"例如 https://meet.example.com。为空表示不填"`
	}
}

var putSiteSettingsOp = huma.Operation{
	OperationID: "put-site-settings",
	Method:      http.MethodPut,
	Path:        "/api/settings/site",
	Summary:     "保存 zsmeet 的访问地址",
	Tags:        []string{"设置"},
	Security:    adminOnly,
}

func (h *Handlers) PutSiteSettings(ctx context.Context, in *SiteSettingsInput) (*SiteSettingsOutput, error) {
	s, err := h.svc.SaveSite(ctx, in.Body.URL)
	if err != nil {
		return nil, toHTTP(err)
	}
	return toSiteSettings(s), nil
}

// ---- 群机器人：管理员的配置 ----

// Webhook 是一个群机器人。Webhook 地址和加签的密钥不会返回，只给打了码的地址和「有没有加签」。
type Webhook struct {
	ID        string    `json:"id"`
	Platform  string    `json:"platform" enum:"dingtalk,wecom,slack" doc:"钉钉、企业微信或 Slack"`
	Name      string    `json:"name"`
	URLMasked string    `json:"url_masked" doc:"打了码的 Webhook 地址，密钥只留最后 4 位"`
	SecretSet bool      `json:"secret_set" doc:"钉钉是否开了加签；密钥本身不会返回"`
	AutoPush  bool      `json:"auto_push" doc:"纪要第一次生成后自动发到这个群（只发团队里所有人都能看的会议）"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func toWebhook(w sqlc.Webhook) Webhook {
	return Webhook{
		ID: w.ID.String(), Platform: w.Platform, Name: w.Name, URLMasked: w.UrlMasked, SecretSet: w.Secret != "",
		AutoPush: w.AutoPush, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt,
	}
}

type WebhooksOutput struct {
	Body struct {
		Items []Webhook `json:"items" nullable:"false"`
	}
}

var listWebhooksOp = huma.Operation{
	OperationID: "list-webhooks",
	Method:      http.MethodGet,
	Path:        "/api/settings/webhooks",
	Summary:     "群机器人",
	Description: "钉钉、企业微信、Slack 群里的自定义机器人，纪要可以发到这些群里。",
	Tags:        []string{"设置"},
	Security:    adminOnly,
}

func (h *Handlers) ListWebhooks(ctx context.Context, _ *struct{}) (*WebhooksOutput, error) {
	hooks, err := h.svc.Webhooks(ctx)
	if err != nil {
		return nil, toHTTP(err)
	}
	out := &WebhooksOutput{}
	out.Body.Items = make([]Webhook, len(hooks))
	for i, w := range hooks {
		out.Body.Items[i] = toWebhook(w)
	}
	return out, nil
}

// WebhookBody 是网页提交的群机器人。
type WebhookBody struct {
	Platform    string `json:"platform" enum:"dingtalk,wecom,slack"`
	Name        string `json:"name" maxLength:"100"`
	URL         string `json:"url,omitempty" maxLength:"1000" doc:"Webhook 地址。改的时候留空表示沿用已保存的"`
	Secret      string `json:"secret,omitempty" maxLength:"200" doc:"钉钉「加签」的密钥（SEC 开头）。留空表示沿用已保存的"`
	ClearSecret bool   `json:"clear_secret,omitempty" doc:"钉钉不再加签，清掉保存的密钥"`
	AutoPush    bool   `json:"auto_push,omitempty"`
}

type WebhookInput struct {
	Body WebhookBody
}

func (b WebhookBody) toService() service.WebhookInput {
	return service.WebhookInput{Platform: b.Platform, Name: b.Name, URL: b.URL, Secret: b.Secret, ClearSecret: b.ClearSecret, AutoPush: b.AutoPush}
}

type WebhookOutput struct {
	Body Webhook
}

var createWebhookOp = huma.Operation{
	OperationID:   "create-webhook",
	Method:        http.MethodPost,
	Path:          "/api/settings/webhooks",
	Summary:       "加一个群机器人",
	Description:   "只接受各平台官方的 Webhook 地址（https://oapi.dingtalk.com、https://qyapi.weixin.qq.com、https://hooks.slack.com）。地址和加签的密钥加密后保存。",
	Tags:          []string{"设置"},
	Security:      adminOnly,
	DefaultStatus: http.StatusCreated,
}

func (h *Handlers) CreateWebhook(ctx context.Context, in *WebhookInput) (*WebhookOutput, error) {
	w, err := h.svc.CreateWebhook(ctx, currentUser(ctx), in.Body.toService())
	if err != nil {
		return nil, toHTTP(err)
	}
	return &WebhookOutput{Body: toWebhook(w)}, nil
}

type WebhookID struct {
	ID string `path:"id"`
}

func parseWebhookID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, huma.Error404NotFound("群机器人不存在，可能已被删除")
	}
	return id, nil
}

type UpdateWebhookInput struct {
	ID   string `path:"id"`
	Body WebhookBody
}

var updateWebhookOp = huma.Operation{
	OperationID: "update-webhook",
	Method:      http.MethodPut,
	Path:        "/api/settings/webhooks/{id}",
	Summary:     "改一个群机器人",
	Tags:        []string{"设置"},
	Security:    adminOnly,
}

func (h *Handlers) UpdateWebhook(ctx context.Context, in *UpdateWebhookInput) (*WebhookOutput, error) {
	id, err := parseWebhookID(in.ID)
	if err != nil {
		return nil, err
	}
	w, err := h.svc.UpdateWebhook(ctx, id, in.Body.toService())
	if err != nil {
		return nil, toHTTP(err)
	}
	return &WebhookOutput{Body: toWebhook(w)}, nil
}

var deleteWebhookOp = huma.Operation{
	OperationID:   "delete-webhook",
	Method:        http.MethodDelete,
	Path:          "/api/settings/webhooks/{id}",
	Summary:       "删掉一个群机器人",
	Description:   "已经发到群里的消息不会撤回。发送记录留着。",
	Tags:          []string{"设置"},
	Security:      adminOnly,
	DefaultStatus: http.StatusNoContent,
}

func (h *Handlers) DeleteWebhook(ctx context.Context, in *WebhookID) (*struct{}, error) {
	id, err := parseWebhookID(in.ID)
	if err != nil {
		return nil, err
	}
	if err := h.svc.DeleteWebhook(ctx, id); err != nil {
		return nil, toHTTP(err)
	}
	return nil, nil
}

var testWebhookOp = huma.Operation{
	OperationID:   "test-webhook",
	Method:        http.MethodPost,
	Path:          "/api/settings/webhooks/{id}/test",
	Summary:       "往群机器人发一条测试消息",
	Description:   "等发完才返回。地址失效、关键词或加签不对时返回 422，detail 里是原因和改法。",
	Tags:          []string{"设置"},
	Security:      adminOnly,
	DefaultStatus: http.StatusNoContent,
}

func (h *Handlers) TestWebhook(ctx context.Context, in *WebhookID) (*struct{}, error) {
	id, err := parseWebhookID(in.ID)
	if err != nil {
		return nil, err
	}
	if err := h.svc.TestWebhook(ctx, currentUser(ctx), id); err != nil {
		return nil, toHTTP(err)
	}
	return nil, nil
}

// ---- 把纪要发到群机器人 ----

// WebhookSend 是一次「发到群里」。
type WebhookSend struct {
	ID              string     `json:"id"`
	WebhookID       string     `json:"webhook_id" doc:"群机器人已经删掉时为空"`
	WebhookName     string     `json:"webhook_name" doc:"发送时群机器人的名字"`
	Platform        string     `json:"platform" enum:"dingtalk,wecom,slack"`
	MinutesVersion  int32      `json:"minutes_version" doc:"发的是第几版纪要"`
	Status          string     `json:"status" enum:"pending,done,failed" doc:"pending 发送中；done 已发送；failed 失败。发送中超过半小时的（后台任务丢了）也报成 failed，可以重新发"`
	Error           string     `json:"error" doc:"失败的原因；发送中时是上一次没发出去的原因"`
	RequestedByName string     `json:"requested_by_name" doc:"谁发的；纪要生成后自动发的为空"`
	CreatedAt       time.Time  `json:"created_at"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
}

// toWebhookSend 转成网页看的发送记录。now 是现在：发送中太久的（后台任务丢了）报成失败，网页上就能重新发，也不再一直刷新。
// 判断用的期限和 SendToWebhooks 允许重新发的是同一个（service.WebhookPendingWindow）。
func toWebhookSend(s sqlc.WebhookSend, by string, now time.Time) WebhookSend {
	out := WebhookSend{
		ID: s.ID.String(), WebhookName: s.WebhookName, Platform: s.Platform, MinutesVersion: s.MinutesVersion,
		Status: s.Status, Error: s.Error, RequestedByName: by, CreatedAt: s.CreatedAt, FinishedAt: s.FinishedAt,
	}
	if service.WebhookSendStale(s, now) {
		out.Status, out.Error = "failed", service.WebhookStaleMessage
	}
	if s.WebhookID != nil {
		out.WebhookID = s.WebhookID.String()
	}
	return out
}

// MeetingWebhook 是会议页上能选的一个群机器人。
type MeetingWebhook struct {
	ID       string       `json:"id"`
	Name     string       `json:"name"`
	Platform string       `json:"platform" enum:"dingtalk,wecom,slack"`
	Latest   *WebhookSend `json:"latest,omitempty" doc:"这场会议最近一次发到这个群的记录，没发过时没有"`
}

type MeetingWebhooksOutput struct {
	Body struct {
		Items []MeetingWebhook `json:"items" nullable:"false" doc:"管理员加的群机器人；一个都没有时会议页上不显示「发到群里」"`
	}
}

var listMeetingWebhooksOp = huma.Operation{
	OperationID: "list-meeting-webhooks",
	Method:      http.MethodGet,
	Path:        "/api/meetings/{id}/webhooks",
	Summary:     "能发到的群机器人，和这场会议最近一次发到每个群的情况",
	Tags:        []string{"群机器人"},
	Security:    loggedIn,
}

func (h *Handlers) ListMeetingWebhooks(ctx context.Context, in *MeetingID) (*MeetingWebhooksOutput, error) {
	id, err := parseID(in.ID)
	if err != nil {
		return nil, err
	}
	rows, err := h.svc.MeetingWebhooks(ctx, currentUser(ctx), id)
	if err != nil {
		return nil, toHTTP(err)
	}
	out := &MeetingWebhooksOutput{}
	out.Body.Items = make([]MeetingWebhook, len(rows))
	now := time.Now()
	for i, r := range rows {
		item := MeetingWebhook{ID: r.Webhook.ID.String(), Name: r.Webhook.Name, Platform: r.Webhook.Platform}
		if r.Latest != nil {
			s := toWebhookSend(r.Latest.WebhookSend, r.Latest.RequestedByName, now)
			item.Latest = &s
		}
		out.Body.Items[i] = item
	}
	return out, nil
}

type SendToWebhooksInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		WebhookIDs     []string `json:"webhook_ids" minItems:"1" maxItems:"50" doc:"发到哪几个群机器人"`
		MinutesVersion int32    `json:"minutes_version,omitempty" minimum:"0" doc:"网页上看到的纪要版本：当前版本已经不是它时返回 409、不发。不传表示发当前版本，不检查"`
	}
}

type WebhookSendsOutput struct {
	Body struct {
		Items []WebhookSend `json:"items" nullable:"false"`
	}
}

var sendToWebhooksOp = huma.Operation{
	OperationID: "send-to-webhooks",
	Method:      http.MethodPost,
	Path:        "/api/meetings/{id}/webhooks",
	Summary:     "把纪要发到群里",
	Description: "把当前版本的纪要（摘要、待办和链接）发到选的钉钉、企业微信、Slack 群机器人。后台发送，状态看 GET 同一个地址。能编辑这场会议的人可以发。",
	Tags:        []string{"群机器人"},
	Security:    loggedIn,
}

func (h *Handlers) SendToWebhooks(ctx context.Context, in *SendToWebhooksInput) (*WebhookSendsOutput, error) {
	id, err := parseID(in.ID)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, 0, len(in.Body.WebhookIDs))
	for _, raw := range in.Body.WebhookIDs {
		hid, err := uuid.Parse(raw)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity("选的群里有已经被管理员删掉的，请关掉窗口重新选")
		}
		ids = append(ids, hid)
	}
	user := currentUser(ctx)
	sends, err := h.svc.SendToWebhooks(ctx, user, id, ids, in.Body.MinutesVersion)
	if err != nil {
		return nil, toHTTP(err)
	}
	out := &WebhookSendsOutput{}
	out.Body.Items = make([]WebhookSend, len(sends))
	now := time.Now()
	for i, s := range sends {
		out.Body.Items[i] = toWebhookSend(s, user.Name, now)
	}
	return out, nil
}

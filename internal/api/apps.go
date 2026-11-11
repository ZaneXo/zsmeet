package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
)

// ---- App 登录（桌面版） ----
//
// 流程见 docs/desktop.md 和 service/apps.go：网页上点「允许」拿到一次性的授权码（authorize），
// App 用授权码和 PKCE 的 code_verifier 换令牌（token），之后请求带 Authorization: Bearer zsm_…。

type AuthorizeAppInput struct {
	Body struct {
		Client        string `json:"client" doc:"哪种 App，目前只有 desktop"`
		DeviceName    string `json:"device_name" doc:"设备名（电脑名），账号页上显示。去掉首尾空白后不能为空，最长 100 个字"`
		CodeChallenge string `json:"code_challenge" doc:"base64url(SHA-256(code_verifier))，不补 =，43 个字符"`
	}
}

type AuthorizeAppOutput struct {
	Body struct {
		Code string `json:"code" doc:"一次性的授权码，2 分钟内有效。网页把它带在 zsmeet://auth?code=…&state=… 里交给 App"`
	}
}

var authorizeAppOp = huma.Operation{
	OperationID: "authorize-app",
	Method:      http.MethodPost,
	Path:        "/api/apps/authorize",
	Summary:     "允许一个 App 登录自己的账号",
	Description: "网页上的授权页点「允许」时调用，返回一次性的授权码（2 分钟内有效，只能用一次，和 code_challenge 绑定）。" +
		"只能在网页上登录后调用，带 App 令牌调用返回 403：App 不能自己给自己发新的令牌。",
	Tags:     []string{"App"},
	Security: browserOnly,
}

func (h *Handlers) AuthorizeApp(ctx context.Context, in *AuthorizeAppInput) (*AuthorizeAppOutput, error) {
	code, err := h.svc.AuthorizeApp(ctx, currentUser(ctx), in.Body.Client, in.Body.DeviceName, in.Body.CodeChallenge)
	if err != nil {
		return nil, toHTTP(err)
	}
	out := &AuthorizeAppOutput{}
	out.Body.Code = code
	return out, nil
}

type AppTokenInput struct {
	Body struct {
		Code         string `json:"code" doc:"授权页给的授权码"`
		CodeVerifier string `json:"code_verifier" doc:"PKCE 的 code_verifier（43 到 128 个字符），它的 S256 要和授权时的 code_challenge 一样"`
	}
}

type AppTokenOutput struct {
	Body struct {
		Token string `json:"token" doc:"App 令牌，形如 zsm_…。之后的请求放在 Authorization: Bearer 里。只在这里出现一次，服务器上只存它的哈希"`
		User  User   `json:"user"`
	}
}

var appTokenOp = huma.Operation{
	OperationID: "app-token",
	Method:      http.MethodPost,
	Path:        "/api/apps/token",
	Summary:     "用授权码换 App 令牌",
	Description: "不用登录。授权码不管换没换成功都用掉了。授权码不对、过期、用过了、和 code_verifier 对不上，都返回同一个 422。",
	Tags:        []string{"App"},
}

func (h *Handlers) AppToken(ctx context.Context, in *AppTokenInput) (*AppTokenOutput, error) {
	token, user, err := h.svc.ExchangeAppCode(ctx, in.Body.Code, in.Body.CodeVerifier)
	if err != nil {
		return nil, toHTTP(err)
	}
	out := &AppTokenOutput{}
	out.Body.Token, out.Body.User = token, toUser(user)
	return out, nil
}

var appLogoutOp = huma.Operation{
	OperationID:   "app-logout",
	Method:        http.MethodPost,
	Path:          "/api/apps/logout",
	Summary:       "App 退出登录",
	Description:   "作废请求里带的这个 App 令牌。只能带 App 令牌调用。",
	Tags:          []string{"App"},
	Security:      appOnly,
	DefaultStatus: http.StatusNoContent,
}

func (h *Handlers) AppLogout(ctx context.Context, _ *struct{}) (*struct{}, error) {
	if err := h.svc.RevokeAppToken(ctx, currentUser(ctx), currentAppToken(ctx).ID); err != nil {
		return nil, toHTTP(err)
	}
	return nil, nil
}

// ---- 账号页：登录着的 App ----

type Device struct {
	ID         string     `json:"id" format:"uuid"`
	Client     string     `json:"client" enum:"desktop" doc:"哪种 App：desktop 是桌面版"`
	DeviceName string     `json:"device_name" doc:"设备名（电脑名）"`
	CreatedAt  time.Time  `json:"created_at" doc:"什么时候登录的"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty" doc:"上次使用的时间，大约精确到分钟。登录以后还没用过时没有这一项"`
	Current    bool       `json:"current" doc:"是不是发这个请求的这台 App"`
}

type DevicesOutput struct {
	Body struct {
		Items []Device `json:"items" nullable:"false"`
	}
}

var listDevicesOp = huma.Operation{
	OperationID: "list-devices",
	Method:      http.MethodGet,
	Path:        "/api/me/devices",
	Summary:     "登录着的 App",
	Description: "自己的账号在哪些 App 上登录着，新登录的在前。",
	Tags:        []string{"App"},
	Security:    loggedIn,
}

func (h *Handlers) ListDevices(ctx context.Context, _ *struct{}) (*DevicesOutput, error) {
	list, err := h.svc.AppDevices(ctx, currentUser(ctx))
	if err != nil {
		return nil, toHTTP(err)
	}
	current := uuid.Nil
	if t := currentAppToken(ctx); t != nil {
		current = t.ID
	}
	out := &DevicesOutput{}
	out.Body.Items = make([]Device, len(list))
	for i, t := range list {
		out.Body.Items[i] = Device{
			ID: t.ID.String(), Client: t.Client, DeviceName: t.DeviceName, CreatedAt: t.CreatedAt, LastUsedAt: t.LastUsedAt,
			Current: t.ID == current,
		}
	}
	return out, nil
}

type DeviceID struct {
	ID string `path:"id" format:"uuid"`
}

var deleteDeviceOp = huma.Operation{
	OperationID:   "delete-device",
	Method:        http.MethodDelete,
	Path:          "/api/me/devices/{id}",
	Summary:       "让一台 App 退出登录",
	Description:   "只能是自己的。那台 App 的令牌立刻失效，要再用得重新登录。",
	Tags:          []string{"App"},
	Security:      loggedIn,
	DefaultStatus: http.StatusNoContent,
}

func (h *Handlers) DeleteDevice(ctx context.Context, in *DeviceID) (*struct{}, error) {
	id, err := uuid.Parse(in.ID)
	if err != nil {
		return nil, huma.Error404NotFound("没有这台设备，可能已经退出登录了")
	}
	if err := h.svc.RevokeAppToken(ctx, currentUser(ctx), id); err != nil {
		return nil, toHTTP(err)
	}
	return nil, nil
}

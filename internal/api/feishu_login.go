package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ZaneXo/zsmeet/internal/service"
)

// 用飞书登录：网页上的按钮是一个普通链接，指向 /api/auth/feishu/start，浏览器整页跳过去，
// 再被带到飞书的授权页；飞书带着授权码跳回 /api/auth/feishu/callback，登录成功后回到首页。
// 登录着的人在「设置 → 账号」里输入密码、点「关联飞书」（POST /api/me/feishu/link）以后，回调也走这一套，完成后回到账号页。
//
// 发起时的 state 和 PKCE 的 code_verifier 加密后放在一个 10 分钟的 Cookie 里（只发给 /api/auth/feishu），
// 不放进服务器的会话：没登录的人点一下按钮，不该在服务器上留一条 30 天的会话。整个过程只有登录成功那一下写会话。
// 出错时跳回登录页（关联时是账号页），地址里带一个固定的错误代号（feishu_error），网页按它显示说明。
// 不带任意文字：那样谁都能编一个带着任意说明的 zsmeet 链接发给别人。

// feishuStateCookie 是放 state 的 Cookie。只发给 feishuCookiePath 下的地址
const (
	feishuStateCookie = "zsmeet_feishu_state"
	feishuCookiePath  = "/api/auth/feishu"
)

// 飞书登录完回到哪里
const (
	loginPage   = "/"
	accountPage = "/settings"
)

// RedirectOutput 让浏览器跳到另一个地址。
type RedirectOutput struct {
	Status       int
	Location     string        `header:"Location"`
	CacheControl string        `header:"Cache-Control"`
	SetCookie    []http.Cookie `header:"Set-Cookie"`
}

func redirect(to string) *RedirectOutput {
	return &RedirectOutput{Status: http.StatusFound, Location: to, CacheControl: "no-store"}
}

// feishuFailed 跳回 page，地址里带上失败的原因。不是 service.FeishuAuthError 的错误（连不上飞书、数据库出错）
// 只给一个笼统的代号，细节进日志；日志里没有授权码和凭证。
func feishuFailed(page string, err error) *RedirectOutput {
	code := service.FeishuFailed
	if e, ok := errors.AsType[*service.FeishuAuthError](err); ok {
		code = e.Code
	} else {
		slog.Error("用飞书登录失败", "err", err)
	}
	return redirect(page + "?feishu_error=" + url.QueryEscape(code))
}

// stateCookie 是放 state 的 Cookie。maxAge 小于 0 时是删掉它。
func (h *Handlers) stateCookie(value string, maxAge int) http.Cookie {
	return http.Cookie{
		Name: feishuStateCookie, Value: value, Path: feishuCookiePath, MaxAge: maxAge,
		// 飞书跳回来是从别的网站整页跳转过来的 GET，Lax 的 Cookie 会带上
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: h.sess.Cookie.Secure,
	}
}

type FeishuStartInput struct {
	// 不加 enum 之类的约束：校验失败时 huma 回的是 JSON，用户在浏览器里只会看到一串看不懂的东西
	Mode string `query:"mode" doc:"不用填。以前的 link（关联飞书）不再从这里发起，要在账号页上输入密码（POST /api/me/feishu/link）"`
}

var feishuLoginStartOp = huma.Operation{
	OperationID: "feishu-login-start",
	Method:      http.MethodGet,
	Path:        "/api/auth/feishu/start",
	Summary:     "用飞书登录，或者关联飞书账号",
	Description: "浏览器整页打开这个地址，会被带到飞书的授权页（扫码，或者在飞书网页版已登录时点一下）。" +
		"管理员没打开飞书登录、系统还没初始化时直接跳回登录页（关联时是账号页），地址里的 feishu_error 是原因。不写会话。",
	Tags:          []string{"账号"},
	DefaultStatus: http.StatusFound,
}

func (h *Handlers) FeishuLoginStart(ctx context.Context, in *FeishuStartInput) (*RedirectOutput, error) {
	// 关联飞书要先输入密码，不能用一个地址直接发起
	if in.Mode == service.FeishuModeLink {
		return feishuFailed(accountPage, &service.FeishuAuthError{Code: service.FeishuLinkPassword}), nil
	}
	authorize, sealed, err := h.svc.StartFeishuAuth(ctx, service.FeishuModeLogin, nil)
	if err != nil {
		return feishuFailed(loginPage, err), nil
	}
	// 每次发起都覆盖上一次的：同一个浏览器里只有最近点的那一次能用
	out := redirect(authorize)
	out.SetCookie = []http.Cookie{h.stateCookie(sealed, int(service.FeishuStateTTL/time.Second))}
	return out, nil
}

type FeishuLinkStartInput struct {
	Body struct {
		Password string `json:"password" maxLength:"200" doc:"当前的密码"`
	}
}

type FeishuLinkStartOutput struct {
	SetCookie []http.Cookie `header:"Set-Cookie"`
	Body      struct {
		URL   string `json:"url,omitempty" doc:"飞书授权页的地址，浏览器整页打开它"`
		Error string `json:"error,omitempty" doc:"发起不了时的原因代号（和回调地址里的 feishu_error 一样），比如管理员没打开飞书登录"`
	}
}

var feishuLinkStartOp = huma.Operation{
	OperationID: "feishu-link-start",
	Method:      http.MethodPost,
	Path:        "/api/me/feishu/link",
	Summary:     "关联飞书：输入密码后发起",
	Description: "核对当前密码，通过后返回飞书授权页的地址（浏览器整页打开），同时在 Cookie 里存下这次关联的 state。" +
		"密码不对时返回 422。要求输入密码，是为了只拿到登录状态的人没法把自己的飞书账号关联上来。",
	Tags:     []string{"账号"},
	Security: loggedIn,
}

func (h *Handlers) FeishuLinkStart(ctx context.Context, in *FeishuLinkStartInput) (*FeishuLinkStartOutput, error) {
	authorize, sealed, err := h.svc.StartFeishuLink(ctx, currentUser(ctx), in.Body.Password)
	out := &FeishuLinkStartOutput{}
	if fe, ok := errors.AsType[*service.FeishuAuthError](err); ok {
		out.Body.Error = fe.Code
		return out, nil
	}
	if err != nil {
		return nil, toHTTP(err)
	}
	out.Body.URL = authorize
	out.SetCookie = []http.Cookie{h.stateCookie(sealed, int(service.FeishuStateTTL/time.Second))}
	return out, nil
}

type FeishuCallbackInput struct {
	// 不加长度之类的约束，理由同上
	Code        string `query:"code" doc:"飞书给的授权码"`
	State       string `query:"state" doc:"发起登录时的 state，原样带回来"`
	Error       string `query:"error" doc:"用户在授权页上拒绝时是 access_denied"`
	StateCookie string `cookie:"zsmeet_feishu_state" doc:"发起时存下的 state（加密的）"`
}

var feishuLoginCallbackOp = huma.Operation{
	OperationID: "feishu-login-callback",
	Method:      http.MethodGet,
	Path:        service.FeishuCallbackPath,
	Summary:     "飞书登录完跳回来",
	Description: "飞书的授权页跳回这里。核对 state（只能用一次，10 分钟内有效），用授权码换出飞书账号：登录时对应到 zsmeet 的账号后登录，回到首页；" +
		"关联时关联到当前登录的账号，回到账号页（地址里带 feishu=linked）。失败时跳回登录页或者账号页，地址里的 feishu_error 是原因。",
	Tags:          []string{"账号"},
	DefaultStatus: http.StatusFound,
}

func (h *Handlers) FeishuLoginCallback(ctx context.Context, in *FeishuCallbackInput) (*RedirectOutput, error) {
	out, err := h.feishuCallback(ctx, in)
	if out != nil {
		// 不管成没成功，这次的 state 都用掉了
		out.SetCookie = []http.Cookie{h.stateCookie("", -1)}
	}
	return out, err
}

func (h *Handlers) feishuCallback(ctx context.Context, in *FeishuCallbackInput) (*RedirectOutput, error) {
	st, err := h.svc.OpenFeishuState(in.StateCookie)
	if err == nil && (in.State == "" || subtle.ConstantTimeCompare([]byte(in.State), []byte(st.State)) != 1) {
		err = &service.FeishuAuthError{Code: service.FeishuExpired}
	}
	if err != nil {
		// 不知道是登录还是关联：登录着的话回账号页，否则回登录页
		page := loginPage
		if _, err := h.sessionUser(ctx); err == nil {
			page = accountPage
		}
		return feishuFailed(page, err), nil
	}

	if st.Mode == service.FeishuModeLink {
		if in.Error != "" || in.Code == "" {
			return feishuFailed(accountPage, &service.FeishuAuthError{Code: service.FeishuDenied}), nil
		}
		// 关联到发起时登录着的那个人：中间退出了、换了账号都不行
		user, err := h.sessionUser(ctx)
		if err != nil || user.ID != st.UserID {
			return feishuFailed(accountPage, &service.FeishuAuthError{Code: service.FeishuSignedOut}), nil
		}
		if err := h.svc.LinkFeishu(ctx, user, in.Code, st); err != nil {
			return feishuFailed(accountPage, err), nil
		}
		return redirect(accountPage + "?feishu=linked"), nil
	}

	if in.Error != "" || in.Code == "" {
		return feishuFailed(loginPage, &service.FeishuAuthError{Code: service.FeishuDenied}), nil
	}
	user, err := h.svc.FeishuLogin(ctx, in.Code, st.Verifier)
	if err != nil {
		return feishuFailed(loginPage, err), nil
	}
	if _, err := h.startSession(ctx, user); err != nil {
		return nil, err
	}
	return redirect(loginPage), nil
}

// ---- 账号页：关联的飞书账号 ----

type FeishuLinkOutput struct {
	Body struct {
		Enabled  bool       `json:"enabled" doc:"管理员打开了飞书登录（能关联飞书账号）"`
		Linked   bool       `json:"linked" doc:"是否关联了飞书账号"`
		LinkedAt *time.Time `json:"linked_at,omitempty" doc:"什么时候关联的"`
	}
}

var getFeishuLinkOp = huma.Operation{
	OperationID: "get-feishu-link",
	Method:      http.MethodGet,
	Path:        "/api/me/feishu",
	Summary:     "我关联的飞书账号",
	Description: "要关联飞书账号，先用 POST /api/me/feishu/link 核对当前密码，再在浏览器里整页打开返回的飞书授权页地址。",
	Tags:        []string{"账号"},
	Security:    loggedIn,
}

func (h *Handlers) GetFeishuLink(ctx context.Context, _ *struct{}) (*FeishuLinkOutput, error) {
	at, err := h.svc.FeishuLink(ctx, currentUser(ctx).ID)
	if err != nil {
		return nil, toHTTP(err)
	}
	out := &FeishuLinkOutput{}
	if out.Body.Enabled, err = h.svc.FeishuLoginEnabled(ctx); err != nil {
		return nil, toHTTP(err)
	}
	out.Body.Linked, out.Body.LinkedAt = at != nil, at
	return out, nil
}

var unlinkFeishuOp = huma.Operation{
	OperationID:   "unlink-feishu",
	Method:        http.MethodDelete,
	Path:          "/api/me/feishu",
	Summary:       "解除和飞书账号的关联",
	Description:   "换了飞书账号时用：解除以后再关联新的飞书账号。",
	Tags:          []string{"账号"},
	Security:      loggedIn,
	DefaultStatus: http.StatusNoContent,
}

func (h *Handlers) UnlinkFeishu(ctx context.Context, _ *struct{}) (*struct{}, error) {
	if err := h.svc.UnlinkFeishu(ctx, currentUser(ctx).ID); err != nil {
		return nil, toHTTP(err)
	}
	return nil, nil
}

var unlinkMemberFeishuOp = huma.Operation{
	OperationID: "unlink-member-feishu",
	Method:      http.MethodDelete,
	Path:        "/api/members/{id}/feishu",
	Summary:     "解除成员和飞书账号的关联",
	Description: "成员换了飞书账号、自己又登录不了（比如用飞书自动创建的账号不知道密码）时，管理员在这里解除。" +
		"解除以后他用新的飞书账号登录时，重新按企业邮箱对应。",
	Tags:          []string{"成员"},
	Security:      adminOnly,
	DefaultStatus: http.StatusNoContent,
}

func (h *Handlers) UnlinkMemberFeishu(ctx context.Context, in *InviteID) (*struct{}, error) {
	id, err := parseMemberID(in.ID)
	if err != nil {
		return nil, err
	}
	if err := h.svc.UnlinkFeishu(ctx, id); err != nil {
		return nil, toHTTP(err)
	}
	return nil, nil
}

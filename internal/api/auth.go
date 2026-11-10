package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"zsmeet/internal/db/sqlc"
)

type StatusOutput struct {
	Body struct {
		Initialized bool   `json:"initialized" doc:"是否已经创建过管理员；没有的话显示初始化向导"`
		User        *User  `json:"user,omitempty" doc:"当前登录的用户，没登录时没有这个字段"`
		LLMReady    bool   `json:"llm_ready" doc:"是否配置了大模型；只对登录用户有意义"`
		Version     string `json:"version"`
	}
}

var statusOp = huma.Operation{
	OperationID: "get-status",
	Method:      http.MethodGet,
	Path:        "/api/status",
	Summary:     "系统状态和当前登录用户",
	Description: "网页打开时第一个调用的接口，决定显示初始化向导、登录页还是主界面。",
	Tags:        []string{"账号"},
}

func (h *Handlers) Status(ctx context.Context, _ *struct{}) (*StatusOutput, error) {
	out := &StatusOutput{}
	out.Body.Version = h.api.OpenAPI().Info.Version
	initialized, err := h.svc.Initialized(ctx)
	if err != nil {
		return nil, toHTTP(err)
	}
	out.Body.Initialized = initialized
	if user, err := h.sessionUser(ctx); err == nil {
		out.Body.User = new(toUser(user))
		cfg, err := h.svc.LLMSettings(ctx)
		if err != nil {
			return nil, toHTTP(err)
		}
		out.Body.LLMReady = cfg.Ready()
	}
	return out, nil
}

type UserOutput struct {
	Body User
}

type SetupInput struct {
	Body struct {
		Email    string `json:"email" format:"email"`
		Name     string `json:"name" minLength:"1" maxLength:"50"`
		Password string `json:"password" minLength:"8" doc:"8 到 200 位"`
	}
}

var setupOp = huma.Operation{
	OperationID: "setup",
	Method:      http.MethodPost,
	Path:        "/api/setup",
	Summary:     "创建第一个管理员",
	Description: "只在系统里还没有任何用户时可用，成功后直接登录。",
	Tags:        []string{"账号"},
}

func (h *Handlers) Setup(ctx context.Context, in *SetupInput) (*UserOutput, error) {
	user, err := h.svc.Setup(ctx, in.Body.Email, in.Body.Name, in.Body.Password)
	if err != nil {
		return nil, toHTTP(err)
	}
	return h.startSession(ctx, user)
}

type LoginInput struct {
	Body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
}

var loginOp = huma.Operation{
	OperationID: "login",
	Method:      http.MethodPost,
	Path:        "/api/login",
	Summary:     "登录",
	Tags:        []string{"账号"},
}

func (h *Handlers) Login(ctx context.Context, in *LoginInput) (*UserOutput, error) {
	user, err := h.svc.Login(ctx, in.Body.Email, in.Body.Password)
	if err != nil {
		return nil, toHTTP(err)
	}
	return h.startSession(ctx, user)
}

// startSession 登录成功后换一个新的会话令牌（防止会话固定攻击），再记下用户。
func (h *Handlers) startSession(ctx context.Context, user sqlc.User) (*UserOutput, error) {
	if err := h.sess.RenewToken(ctx); err != nil {
		return nil, toHTTP(err)
	}
	h.sess.Put(ctx, sessionUserKey, user.ID.String())
	h.sess.Put(ctx, sessionEpochKey, int(user.SessionEpoch))
	return &UserOutput{Body: toUser(user)}, nil
}

var logoutOp = huma.Operation{
	OperationID:   "logout",
	Method:        http.MethodPost,
	Path:          "/api/logout",
	Summary:       "退出登录",
	Tags:          []string{"账号"},
	DefaultStatus: http.StatusNoContent,
}

func (h *Handlers) Logout(ctx context.Context, _ *struct{}) (*struct{}, error) {
	if err := h.sess.Destroy(ctx); err != nil {
		return nil, toHTTP(err)
	}
	return nil, nil
}

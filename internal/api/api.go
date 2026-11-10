// Package api 是 HTTP 接口层。只做三件事：取参数、调 service、把结果转成响应。
// 业务规则写在 internal/service 里；huma 只允许出现在这个包里。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/google/uuid"

	"zsmeet/internal/db/sqlc"
	"zsmeet/internal/service"
)

// CookieName 是会话 Cookie 的名字。
const CookieName = "zsmeet_session"

// 会话里存的两样东西：用户 ID，和登录时这个用户的 session_epoch（改密码后会变，用来让旧会话失效）
const (
	sessionUserKey  = "uid"
	sessionEpochKey = "epoch"
)

type userKey struct{}

// 接口对登录状态的要求，写在 Operation.Security 里，文档里也看得到
var (
	loggedIn  = []map[string][]string{{"session": {}}}
	adminOnly = []map[string][]string{{"session": {"admin"}}}
)

// NewSessions 返回登录会话的配置，线上和测试共用这一份。
//
// 会话从登录起 30 天有效，到期重新登录。没有设「闲置多久自动退出」（scs 的 IdleTimeout）：
// 开了它，scs 会在每一个请求结束时把请求开始时读到的会话原样写回去，并重新下发 Cookie。
// 这样一个进行中的慢请求（比如正在上传的分片）会把刚换好的会话又换回旧的：
// 改完密码，当前设备反而被退出；退出登录后，会话又被写了回来。
func NewSessions(store scs.Store, secureCookie bool) *scs.SessionManager {
	sess := scs.New()
	sess.Store = store
	sess.Lifetime = 30 * 24 * time.Hour
	sess.Cookie.Name = CookieName
	sess.Cookie.HttpOnly = true
	sess.Cookie.SameSite = http.SameSiteLaxMode
	sess.Cookie.Secure = secureCookie
	sess.ErrorFunc = func(w http.ResponseWriter, _ *http.Request, err error) {
		slog.Error("会话读写失败", "err", err)
		http.Error(w, "服务器内部出错了，请稍后再试", http.StatusInternalServerError)
	}
	return sess
}

type Handlers struct {
	svc  *service.Service
	sess *scs.SessionManager
	api  huma.API
}

// newHandlers 在 mux 上注册所有接口。svc 和 sess 传 nil 时只能用来导出接口文档。
func newHandlers(mux *http.ServeMux, svc *service.Service, sess *scs.SessionManager, version string) *Handlers {
	cfg := huma.DefaultConfig("zsmeet", version)
	cfg.Info.Description = "zsmeet 会议纪要系统的接口。除了 /api/status、/api/setup、/api/login 以外都需要先登录。"
	cfg.OpenAPIPath = "/api/openapi"
	cfg.DocsPath = "/api/docs"
	cfg.SchemasPath = "/api/schemas"
	// 默认会在每个响应里加一个 $schema 字段，前端用不上
	cfg.CreateHooks = nil
	cfg.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		"session": {Type: "apiKey", In: "cookie", Name: CookieName},
	}

	h := &Handlers{svc: svc, sess: sess}
	h.api = humago.New(mux, cfg)
	h.api.UseMiddleware(h.authenticate)
	h.Register(h.api)
	return h
}

// OpenAPI 返回接口文档（OpenAPI 3.1 JSON），不需要数据库。前端的 TypeScript 类型由它生成。
func OpenAPI(version string) ([]byte, error) {
	h := newHandlers(http.NewServeMux(), nil, nil, version)
	return json.MarshalIndent(h.api.OpenAPI(), "", "  ")
}

// NewHandler 组装完整的 HTTP 服务：接口、录音回放、网页。
func NewHandler(svc *service.Service, sess *scs.SessionManager, web fs.FS, version string) http.Handler {
	mux := http.NewServeMux()
	h := newHandlers(mux, svc, sess, version)
	// 录音要支持拖动进度（HTTP Range），用标准库直接发文件
	mux.HandleFunc("GET /api/meetings/{id}/audio", h.audio)
	mux.Handle("/", spa(web))
	return securityHeaders(sess.LoadAndSave(mux))
}

// authenticate 给需要登录的接口查出当前用户，放进 context。
func (h *Handlers) authenticate(ctx huma.Context, next func(huma.Context)) {
	need := ctx.Operation().Security
	if len(need) == 0 {
		next(ctx)
		return
	}
	user, err := h.sessionUser(ctx.Context())
	if err != nil {
		writeErr(h.api, ctx, err)
		return
	}
	if slices.Contains(need[0]["session"], "admin") && user.Role != "admin" {
		huma.WriteErr(h.api, ctx, http.StatusForbidden, "只有管理员能做这个操作")
		return
	}
	next(huma.WithValue(ctx, userKey{}, user))
}

var errNotLoggedIn = &service.Error{Kind: service.Unauthorized, Msg: "请先登录"}

// sessionUser 从会话里找出当前用户。每次都查数据库，账号被停用后立刻失效。
func (h *Handlers) sessionUser(ctx context.Context) (sqlc.User, error) {
	id, err := uuid.Parse(h.sess.GetString(ctx, sessionUserKey))
	if err != nil {
		return sqlc.User{}, errNotLoggedIn
	}
	return h.svc.SessionUser(ctx, id, h.sess.GetInt(ctx, sessionEpochKey))
}

// currentUser 取出 authenticate 放进去的用户。只能在需要登录的接口里用。
func currentUser(ctx context.Context) sqlc.User {
	return ctx.Value(userKey{}).(sqlc.User)
}

// status 把业务错误的种类对应到 HTTP 状态码；不认识的错误一律 500，细节只进日志。
func status(err error) (int, string) {
	if e, ok := errors.AsType[*service.Error](err); ok {
		switch e.Kind {
		case service.NotFound:
			return http.StatusNotFound, e.Msg
		case service.Invalid:
			return http.StatusUnprocessableEntity, e.Msg
		case service.Conflict:
			return http.StatusConflict, e.Msg
		case service.Forbidden:
			return http.StatusForbidden, e.Msg
		case service.Unauthorized:
			return http.StatusUnauthorized, e.Msg
		}
	}
	if !errors.Is(err, context.Canceled) {
		slog.Error("接口内部错误", "err", err)
	}
	return http.StatusInternalServerError, "服务器内部出错了，请稍后再试"
}

// toHTTP 把 service 返回的错误转成 huma 的错误响应。
func toHTTP(err error) error {
	code, msg := status(err)
	return huma.NewError(code, msg)
}

func writeErr(api huma.API, ctx huma.Context, err error) {
	code, msg := status(err)
	huma.WriteErr(api, ctx, code, msg)
}

func parseID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, huma.Error404NotFound("会议不存在，可能已被删除")
	}
	return id, nil
}

// audio 回放录音。
func (h *Handlers) audio(w http.ResponseWriter, r *http.Request) {
	if _, err := h.sessionUser(r.Context()); err != nil {
		code, msg := status(err)
		http.Error(w, msg, code)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	path, err := h.svc.AudioPath(r.Context(), id)
	if err != nil {
		code, msg := status(err)
		http.Error(w, msg, code)
		return
	}
	w.Header().Set("Content-Type", "audio/mp4")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeFile(w, r, path)
}

// spa 提供打包好的网页。前端用的是浏览器路由，所以找不到的路径都返回 index.html。
func spa(web fs.FS) http.Handler {
	files := http.FileServerFS(web)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.Error(w, "没有这个接口", http.StatusNotFound)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		if info, err := fs.Stat(web, name); name != "" && err == nil && !info.IsDir() {
			if strings.HasPrefix(name, "assets/") {
				// 文件名里带内容哈希，可以永久缓存
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			files.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, web, "index.html")
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := w.Header()
		hdr.Set("X-Content-Type-Options", "nosniff")
		hdr.Set("X-Frame-Options", "DENY")
		hdr.Set("Referrer-Policy", "same-origin")
		// 接口文档页要从 CDN 加载脚本，所以内容安全策略只加在网页上
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			hdr.Set("Content-Security-Policy",
				"default-src 'self'; img-src 'self' data:; media-src 'self' blob:; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'")
		}
		next.ServeHTTP(w, r)
	})
}

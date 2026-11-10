// Package feishutest 是测试用的假飞书开放平台：实现 zsmeet 用到的那些接口，记下收到的请求，
// 可以设置某些接口返回错误。数据都是编的，不连真的飞书。
package feishutest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
)

// 假应用的凭证
const (
	AppID     = "cli_test_app"
	AppSecret = "test-app-secret"
	token     = "t-test-token"
)

// Message 是机器人发出去的一条消息。
type Message struct {
	ChatID  string
	Content string // 卡片的 JSON
	UUID    string
}

// Import 是一次「Markdown 导入成文档」。
type Import struct {
	Folder   string // 放进哪个文件夹
	Name     string // 文档名
	Markdown string
	Token    string // 生成的文档 token
	polls    int
}

// Grant 是给文档加的一个协作者。
type Grant struct {
	Doc, MemberType, MemberID, Perm string
}

// Recording 是一场飞书会议的云录制。
type Recording struct {
	MinuteToken string
	// 还要返回几次「录制还在生成」
	Processing int
}

// Media 是一个妙记的音视频文件。
type Media struct {
	Data []byte
	// 还要返回几次「妙记还没生成好」
	NotReady int
	// 返回「没有权限」
	Deny bool
}

// User 是通讯录里的一个人。
type User struct {
	Name, Email string
}

// LoginUser 是在授权页上登录着飞书的人（用飞书登录时用）。
type LoginUser struct {
	OpenID, UnionID, Name, Email, EnterpriseEmail string
}

// grant 是授权页发出去的一个授权码。
type grant struct {
	user           LoginUser
	redirect, pkce string
	// 打开授权页时申请的权限（scope）
	scopes []string
	used   bool
}

// userGrant 是换出来的一个用户访问凭证：是谁，申请过哪些权限。
type userGrant struct {
	user   LoginUser
	scopes []string
}

// 读邮箱要申请的权限：和真的飞书一样，没申请的话用户信息里没有对应的邮箱
const (
	scopeEmail      = "contact:user.email:readonly"
	scopeEnterprise = "contact:user.employee:readonly"
)

// Server 是假的开放平台。
type Server struct {
	*httptest.Server
	t *testing.T

	mu         sync.Mutex
	Chats      [][2]string // [chat_id, name]
	Messages   []Message
	Imports    []Import
	Grants     []Grant
	PublicDocs []string
	// 文件夹：父文件夹 → 子文件夹名 → token
	Folders    map[string]map[string]string
	Recordings map[string]*Recording
	Media      map[string]*Media
	Users      map[string]User
	// 往这些群发消息时返回的错误码
	FailChats map[string]int
	// 每个导入任务头几次查询返回「还在处理」
	ImportPolls int
	// 用飞书登录：授权页上登录着飞书的是谁。为 nil 时相当于用户在授权页上点了拒绝
	LoginAs *LoginUser
	// 换用户凭证时返回的错误码（模拟授权码过期、重定向 URL 对不上这类）
	FailToken int
	// 每个接口被调用的次数，键是「方法 路径」，路径里的变量换成 *
	Calls      map[string]int
	tasks      map[string]*Import
	grants     map[string]*grant
	userTokens map[string]userGrant
	seq        int
}

// New 启动假的开放平台，测试结束时自动关掉。
func New(t *testing.T) *Server {
	s := &Server{
		t:          t,
		Folders:    map[string]map[string]string{},
		Recordings: map[string]*Recording{},
		Media:      map[string]*Media{},
		Users:      map[string]User{},
		FailChats:  map[string]int{},
		Calls:      map[string]int{},
		tasks:      map[string]*Import{},
		grants:     map[string]*grant{},
		userTokens: map[string]userGrant{},
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// Count 返回某个接口被调用了几次。
func (s *Server) Count(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Calls[key]
}

// Sent 返回发出去的消息（复制一份）。
func (s *Server) Sent() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.Messages...)
}

// Docs 返回导入的文档（复制一份）。
func (s *Server) Docs() []Import {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Import(nil), s.Imports...)
}

func (s *Server) next(prefix string) string {
	s.seq++
	return fmt.Sprintf("%s%04d", prefix, s.seq)
}

func reply(w http.ResponseWriter, code int, msg string, data any) {
	w.Header().Set("Content-Type", "application/json")
	body := map[string]any{"code": code, "msg": msg}
	if data != nil {
		body["data"] = data
	}
	json.NewEncoder(w).Encode(body)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := r.URL.Path
	key := r.Method + " " + pattern(path)
	s.Calls[key]++

	if path == "/open-apis/auth/v3/tenant_access_token/internal" {
		var in struct {
			AppID     string `json:"app_id"`
			AppSecret string `json:"app_secret"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		w.Header().Set("Content-Type", "application/json")
		if in.AppID != AppID || in.AppSecret != AppSecret {
			json.NewEncoder(w).Encode(map[string]any{"code": 10014, "msg": "app secret invalid"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "ok", "tenant_access_token": token, "expire": 7200})
		return
	}
	if strings.HasPrefix(path, "/download/") {
		m := s.Media[strings.TrimPrefix(path, "/download/")]
		if m == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Write(m.Data)
		return
	}
	switch key {
	case "GET /open-apis/authen/v1/authorize":
		s.authorize(w, r)
		return
	case "POST /open-apis/authen/v2/oauth/token":
		s.userToken(w, r)
		return
	case "GET /open-apis/authen/v1/user_info":
		ug, ok := s.userTokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if !ok {
			reply(w, 99991668, "invalid user access token", nil)
			return
		}
		u := ug.user
		data := map[string]string{"name": u.Name, "open_id": u.OpenID, "union_id": u.UnionID, "tenant_key": "tenant_test"}
		// 名字和 ID 不用权限；邮箱、企业邮箱只有申请了对应的权限才给
		if u.Email != "" && slices.Contains(ug.scopes, scopeEmail) {
			data["email"] = u.Email
		}
		if u.EnterpriseEmail != "" && slices.Contains(ug.scopes, scopeEnterprise) {
			data["enterprise_email"] = u.EnterpriseEmail
		}
		reply(w, 0, "success", data)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+token {
		reply(w, 99991661, "missing access token", nil)
		return
	}

	switch {
	case key == "GET /open-apis/bot/v3/info":
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "ok", "bot": map[string]any{"app_name": "会议助手", "open_id": "ou_bot"}})

	case key == "GET /open-apis/im/v1/chats":
		items := []map[string]string{}
		for _, c := range s.Chats {
			items = append(items, map[string]string{"chat_id": c[0], "name": c[1]})
		}
		// 每页两个，测试翻页
		start := 0
		if p := r.URL.Query().Get("page_token"); p != "" {
			fmt.Sscan(p, &start)
		}
		end := min(start+2, len(items))
		data := map[string]any{"items": items[start:end], "has_more": end < len(items)}
		if end < len(items) {
			data["page_token"] = fmt.Sprint(end)
		}
		reply(w, 0, "ok", data)

	case key == "POST /open-apis/im/v1/messages":
		var in struct {
			ReceiveID string `json:"receive_id"`
			MsgType   string `json:"msg_type"`
			Content   string `json:"content"`
			UUID      string `json:"uuid"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		if r.URL.Query().Get("receive_id_type") != "chat_id" || in.MsgType != "interactive" || !json.Valid([]byte(in.Content)) {
			reply(w, 230001, "bad request", nil)
			return
		}
		if code := s.FailChats[in.ReceiveID]; code != 0 {
			reply(w, code, "fail", nil)
			return
		}
		// 一小时内同一个 uuid 只发一条
		for _, m := range s.Messages {
			if in.UUID != "" && m.UUID == in.UUID {
				reply(w, 0, "ok", map[string]string{"message_id": "om_dup"})
				return
			}
		}
		s.Messages = append(s.Messages, Message{ChatID: in.ReceiveID, Content: in.Content, UUID: in.UUID})
		reply(w, 0, "ok", map[string]string{"message_id": s.next("om_")})

	case key == "POST /open-apis/drive/v1/medias/upload_all":
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			reply(w, 1061002, "params error", nil)
			return
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			reply(w, 1061002, "no file", nil)
			return
		}
		data, _ := io.ReadAll(f)
		var extra struct {
			ObjType string `json:"obj_type"`
			Ext     string `json:"file_extension"`
		}
		json.Unmarshal([]byte(r.FormValue("extra")), &extra)
		if r.FormValue("parent_type") != "ccm_import_open" || extra.ObjType != "docx" || extra.Ext != "md" ||
			!strings.HasSuffix(r.FormValue("file_name"), ".md") || r.FormValue("size") != fmt.Sprint(len(data)) {
			reply(w, 1061002, "params error", nil)
			return
		}
		ft := s.next("boxmedia")
		s.tasks[ft] = &Import{Markdown: string(data)}
		reply(w, 0, "ok", map[string]string{"file_token": ft})

	case key == "POST /open-apis/drive/v1/import_tasks":
		var in struct {
			FileExtension string `json:"file_extension"`
			FileToken     string `json:"file_token"`
			Type          string `json:"type"`
			FileName      string `json:"file_name"`
			Point         struct {
				MountType int    `json:"mount_type"`
				MountKey  string `json:"mount_key"`
			} `json:"point"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		imp := s.tasks[in.FileToken]
		if imp == nil || in.FileExtension != "md" || in.Type != "docx" || in.Point.MountType != 1 {
			reply(w, 1069910, "bad import", nil)
			return
		}
		imp.Folder, imp.Name = in.Point.MountKey, in.FileName
		ticket := s.next("ticket")
		s.tasks[ticket] = imp
		reply(w, 0, "ok", map[string]string{"ticket": ticket})

	case key == "GET /open-apis/drive/v1/import_tasks/*":
		ticket := strings.TrimPrefix(path, "/open-apis/drive/v1/import_tasks/")
		imp := s.tasks[ticket]
		if imp == nil {
			reply(w, 1069902, "no ticket", nil)
			return
		}
		// 头 ImportPolls 次查询还在处理
		imp.polls++
		if imp.Token == "" && imp.polls <= s.ImportPolls {
			reply(w, 0, "ok", map[string]any{"result": map[string]any{"ticket": ticket, "job_status": 2}})
			return
		}
		if imp.Token == "" {
			imp.Token = s.next("doxcn")
			s.Imports = append(s.Imports, *imp)
		}
		reply(w, 0, "ok", map[string]any{"result": map[string]any{
			"ticket": ticket, "job_status": 0, "token": imp.Token, "url": s.URL + "/docx/" + imp.Token,
		}})

	case key == "POST /open-apis/drive/v1/permissions/*/members":
		var in struct {
			MemberType string `json:"member_type"`
			MemberID   string `json:"member_id"`
			Perm       string `json:"perm"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		doc := strings.TrimSuffix(strings.TrimPrefix(path, "/open-apis/drive/v1/permissions/"), "/members")
		s.Grants = append(s.Grants, Grant{Doc: doc, MemberType: in.MemberType, MemberID: in.MemberID, Perm: in.Perm})
		reply(w, 0, "ok", map[string]any{"member": in})

	case key == "PATCH /open-apis/drive/v2/permissions/*/public":
		var in struct {
			LinkShareEntity string `json:"link_share_entity"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		if in.LinkShareEntity == "tenant_readable" {
			s.PublicDocs = append(s.PublicDocs, strings.TrimSuffix(strings.TrimPrefix(path, "/open-apis/drive/v2/permissions/"), "/public"))
		}
		reply(w, 0, "ok", map[string]any{})

	case key == "GET /open-apis/drive/explorer/v2/root_folder/meta":
		reply(w, 0, "ok", map[string]string{"token": "fldroot"})

	case key == "GET /open-apis/drive/v1/files":
		parent := r.URL.Query().Get("folder_token")
		files := []map[string]string{}
		for name, tok := range s.Folders[parent] {
			files = append(files, map[string]string{"token": tok, "name": name, "type": "folder"})
		}
		reply(w, 0, "ok", map[string]any{"files": files, "has_more": false})

	case key == "POST /open-apis/drive/v1/files/create_folder":
		var in struct {
			Name        string `json:"name"`
			FolderToken string `json:"folder_token"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		if s.Folders[in.FolderToken] == nil {
			s.Folders[in.FolderToken] = map[string]string{}
		}
		tok := s.next("fld")
		s.Folders[in.FolderToken][in.Name] = tok
		reply(w, 0, "ok", map[string]string{"token": tok, "url": s.URL + "/drive/folder/" + tok})

	case key == "GET /open-apis/vc/v1/meetings/*/recording":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/open-apis/vc/v1/meetings/"), "/recording")
		rec := s.Recordings[id]
		switch {
		case rec == nil:
			reply(w, 121004, "data not exist", nil)
		case rec.Processing > 0:
			rec.Processing--
			reply(w, 124002, "record processing", nil)
		default:
			reply(w, 0, "ok", map[string]any{"recording": map[string]string{
				"url": "https://meetings.feishu.cn/minutes/" + rec.MinuteToken, "duration": "60000",
			}})
		}

	case key == "GET /open-apis/minutes/v1/minutes/*/media":
		tok := strings.TrimSuffix(strings.TrimPrefix(path, "/open-apis/minutes/v1/minutes/"), "/media")
		m := s.Media[tok]
		switch {
		case m == nil:
			reply(w, 2091002, "not found", nil)
		case m.Deny:
			reply(w, 2091005, "permission deny", nil)
		case m.NotReady > 0:
			m.NotReady--
			reply(w, 2091003, "minute not ready", nil)
		default:
			reply(w, 0, "ok", map[string]string{"download_url": s.URL + "/download/" + tok})
		}

	case key == "GET /open-apis/contact/v3/users/*":
		u, ok := s.Users[strings.TrimPrefix(path, "/open-apis/contact/v3/users/")]
		if !ok || r.URL.Query().Get("user_id_type") != "open_id" {
			reply(w, 41050, "no user authority error", nil)
			return
		}
		reply(w, 0, "ok", map[string]any{"user": map[string]string{"name": u.Name, "email": u.Email}})

	default:
		s.t.Errorf("假飞书收到了没实现的请求：%s %s", r.Method, path)
		reply(w, 404, "not found", nil)
	}
}

// authorize 是飞书的授权页：用户已经登录着飞书（LoginAs），直接同意授权，带着授权码跳回 redirect_uri。
// 参数不对时像飞书一样停在授权页上报错（这里返回 400），不跳回去。
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	back, err := url.Parse(q.Get("redirect_uri"))
	if q.Get("client_id") != AppID || q.Get("response_type") != "code" || err != nil || back.Host == "" ||
		q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		http.Error(w, "授权页参数不对", http.StatusBadRequest)
		return
	}
	for _, scope := range strings.Fields(q.Get("scope")) {
		if !strings.HasPrefix(scope, "contact:user.") {
			http.Error(w, "20027 应用没有开通权限 "+scope, http.StatusBadRequest)
			return
		}
	}
	params := back.Query()
	if q.Has("state") {
		params.Set("state", q.Get("state"))
	}
	if s.LoginAs == nil {
		params.Set("error", "access_denied")
	} else {
		code := s.next("code_")
		s.grants[code] = &grant{user: *s.LoginAs, redirect: q.Get("redirect_uri"), pkce: q.Get("code_challenge"), scopes: strings.Fields(q.Get("scope"))}
		params.Set("code", code)
	}
	back.RawQuery = params.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

// userToken 用授权码换用户的访问凭证：授权码只能用一次，redirect_uri 和 PKCE 都要对得上。
func (s *Server) userToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		GrantType    string `json:"grant_type"`
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		Code         string `json:"code"`
		RedirectURI  string `json:"redirect_uri"`
		CodeVerifier string `json:"code_verifier"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	fail := func(status, code int, desc string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]any{"code": code, "error": "invalid_grant", "error_description": desc})
	}
	g := s.grants[in.Code]
	sum := sha256.Sum256([]byte(in.CodeVerifier))
	switch {
	case s.FailToken != 0:
		fail(http.StatusBadRequest, s.FailToken, "mock failure")
	case in.GrantType != "authorization_code" || in.ClientID != AppID || in.ClientSecret != AppSecret:
		fail(http.StatusBadRequest, 20002, "client authentication failed")
	case g == nil || g.used:
		fail(http.StatusBadRequest, 20003, "the code is invalid")
	case in.RedirectURI != g.redirect:
		fail(http.StatusBadRequest, 20071, "redirect_uri mismatch")
	case base64.RawURLEncoding.EncodeToString(sum[:]) != g.pkce:
		fail(http.StatusBadRequest, 20049, "PKCE verification failed")
	default:
		g.used = true
		tok := s.next("u-token-")
		s.userTokens[tok] = userGrant{user: g.user, scopes: g.scopes}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"code": 0, "access_token": tok, "expires_in": 7200, "token_type": "Bearer",
			"scope": strings.Join(g.scopes, " "),
		})
	}
}

// pattern 把路径里的变量（token、ID）换成 *，用来区分是哪个接口。
func pattern(path string) string {
	for _, p := range [][2]string{
		{"/open-apis/drive/v1/import_tasks/", ""},
		{"/open-apis/drive/v1/permissions/", "/members"},
		{"/open-apis/drive/v2/permissions/", "/public"},
		{"/open-apis/vc/v1/meetings/", "/recording"},
		{"/open-apis/minutes/v1/minutes/", "/media"},
		{"/open-apis/contact/v3/users/", ""},
	} {
		if strings.HasPrefix(path, p[0]) && strings.HasSuffix(path, p[1]) && len(path) > len(p[0])+len(p[1]) {
			return p[0] + "*" + p[1]
		}
	}
	return path
}

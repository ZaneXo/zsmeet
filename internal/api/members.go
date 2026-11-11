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

// 成员管理的接口。系统不发邮件：邀请和重置密码都是生成一次性链接，由管理员自己发给对方。
// 接口只返回链接里的 token，完整的链接由网页用自己的访问地址拼出来（服务端不知道自己对外的地址）。

type Member struct {
	ID        string    `json:"id" format:"uuid"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	Role      string    `json:"role" enum:"admin,member"`
	Disabled  bool      `json:"disabled" doc:"已停用的成员不能登录"`
	CreatedAt time.Time `json:"created_at"`
	// 拿到重置链接的人能直接接管这个账号，所以列表里要看得到谁名下有一个
	ResetLink    *PendingReset `json:"reset_link,omitempty" doc:"这个成员名下还没用过的重置密码链接；没有的话没有这个字段"`
	FeishuLinked bool          `json:"feishu_linked" doc:"关联了飞书账号，能用飞书登录"`
}

type PendingReset struct {
	ExpiresAt  time.Time `json:"expires_at"`
	CreatedBy  string    `json:"created_by" doc:"生成这个链接的管理员的姓名；from_server 为 true 时为空"`
	FromServer bool      `json:"from_server" doc:"是在服务器上用 zsmeet reset-link 命令生成的，不是哪位管理员在网页上生成的"`
}

func toMember(u sqlc.User) Member {
	return Member{ID: u.ID.String(), Email: u.Email, Name: u.Name, Role: u.Role, Disabled: u.Disabled, CreatedAt: u.CreatedAt}
}

type Invite struct {
	ID        string    `json:"id" format:"uuid"`
	Email     string    `json:"email"`
	Role      string    `json:"role" enum:"admin,member"`
	InvitedBy string    `json:"invited_by" doc:"发出邀请的管理员的姓名"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Expired   bool      `json:"expired" doc:"链接已经用不了了（过期，或者发出邀请的人已经不是管理员），重新生成一个就能接着用"`
}

func toInvite(i service.Invite) Invite {
	return Invite{
		ID: i.ID.String(), Email: i.Email, Role: i.Role, InvitedBy: i.InvitedBy,
		CreatedAt: i.CreatedAt, ExpiresAt: i.ExpiresAt, Expired: i.Expired,
	}
}

// ---- 成员列表 ----

type MembersOutput struct {
	Body struct {
		Members []Member `json:"members"`
		Invites []Invite `json:"invites" doc:"还没被接受的邀请"`
	}
}

var listMembersOp = huma.Operation{
	OperationID: "list-members",
	Method:      http.MethodGet,
	Path:        "/api/members",
	Summary:     "成员和待接受的邀请",
	Tags:        []string{"成员"},
	Security:    adminOnly,
}

func (h *Handlers) ListMembers(ctx context.Context, _ *struct{}) (*MembersOutput, error) {
	users, invites, err := h.svc.Members(ctx)
	if err != nil {
		return nil, toHTTP(err)
	}
	out := &MembersOutput{}
	out.Body.Members = make([]Member, len(users))
	for i, u := range users {
		m := toMember(u.User)
		m.FeishuLinked = u.FeishuLinked
		if u.Reset != nil {
			m.ResetLink = &PendingReset{ExpiresAt: u.Reset.ExpiresAt, CreatedBy: u.Reset.CreatedBy, FromServer: u.Reset.FromServer}
		}
		out.Body.Members[i] = m
	}
	out.Body.Invites = make([]Invite, len(invites))
	for i, inv := range invites {
		out.Body.Invites[i] = toInvite(inv)
	}
	return out, nil
}

// ---- 修改成员 ----

type UpdateMemberInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		Role     *string `json:"role,omitempty" enum:"admin,member" doc:"不传表示不改"`
		Disabled *bool   `json:"disabled,omitempty" doc:"true 停用，false 启用；不传表示不改"`
	}
}

type MemberOutput struct {
	Body Member
}

var updateMemberOp = huma.Operation{
	OperationID: "update-member",
	Method:      http.MethodPatch,
	Path:        "/api/members/{id}",
	Summary:     "修改成员的角色，或者停用、启用",
	Description: "立刻生效：被停用的人下一次请求就会被退出登录，之后重新启用也要重新登录。管理员不能修改自己。" +
		"一位管理员被降级或停用时，他发出去还没用的邀请会立刻过期（重新生成后可以接着用），他给别人生成的重置密码链接作废。",
	Tags:     []string{"成员"},
	Security: adminOnly,
}

func (h *Handlers) UpdateMember(ctx context.Context, in *UpdateMemberInput) (*MemberOutput, error) {
	id, err := parseMemberID(in.ID)
	if err != nil {
		return nil, err
	}
	user, err := h.svc.UpdateMember(ctx, currentUser(ctx), id, service.MemberChange{Role: in.Body.Role, Disabled: in.Body.Disabled})
	if err != nil {
		return nil, toHTTP(err)
	}
	return &MemberOutput{Body: toMember(user)}, nil
}

// ---- 邀请 ----

type InviteOutput struct {
	Body struct {
		Invite Invite `json:"invite"`
		Token  string `json:"token" doc:"链接里的一次性凭证，只在这一次返回。完整链接是 <站点地址>/invite#<token>"`
	}
}

func inviteOutput(invite service.Invite, token string) *InviteOutput {
	out := &InviteOutput{}
	out.Body.Invite, out.Body.Token = toInvite(invite), token
	return out
}

type CreateInviteInput struct {
	Body struct {
		Email string `json:"email" format:"email" maxLength:"200"`
		Role  string `json:"role" enum:"admin,member"`
	}
}

var createInviteOp = huma.Operation{
	OperationID:   "create-invite",
	Method:        http.MethodPost,
	Path:          "/api/invites",
	Summary:       "邀请一个邮箱加入",
	Description:   "返回一次性的邀请链接凭证，7 天内有效。同一个邮箱再次邀请时，之前的链接作废。",
	Tags:          []string{"成员"},
	Security:      adminOnly,
	DefaultStatus: http.StatusCreated,
}

func (h *Handlers) CreateInvite(ctx context.Context, in *CreateInviteInput) (*InviteOutput, error) {
	invite, token, err := h.svc.CreateInvite(ctx, currentUser(ctx), in.Body.Email, in.Body.Role)
	if err != nil {
		return nil, toHTTP(err)
	}
	return inviteOutput(invite, token), nil
}

type InviteID struct {
	ID string `path:"id" format:"uuid"`
}

var renewInviteOp = huma.Operation{
	OperationID: "renew-invite",
	Method:      http.MethodPost,
	Path:        "/api/invites/{id}/renew",
	Summary:     "重新生成邀请链接",
	Description: "链接弄丢了或者过期了时用。旧链接随即失效，有效期重新算 7 天。",
	Tags:        []string{"成员"},
	Security:    adminOnly,
}

func (h *Handlers) RenewInvite(ctx context.Context, in *InviteID) (*InviteOutput, error) {
	id, err := parseMemberID(in.ID)
	if err != nil {
		return nil, err
	}
	invite, token, err := h.svc.RenewInvite(ctx, currentUser(ctx), id)
	if err != nil {
		return nil, toHTTP(err)
	}
	return inviteOutput(invite, token), nil
}

var revokeInviteOp = huma.Operation{
	OperationID:   "revoke-invite",
	Method:        http.MethodDelete,
	Path:          "/api/invites/{id}",
	Summary:       "撤销邀请",
	Tags:          []string{"成员"},
	Security:      adminOnly,
	DefaultStatus: http.StatusNoContent,
}

func (h *Handlers) RevokeInvite(ctx context.Context, in *InviteID) (*struct{}, error) {
	id, err := parseMemberID(in.ID)
	if err != nil {
		return nil, err
	}
	if err := h.svc.RevokeInvite(ctx, id); err != nil {
		return nil, toHTTP(err)
	}
	return nil, nil
}

// ---- 重置密码 ----

type ResetLinkOutput struct {
	Body struct {
		Token     string    `json:"token" doc:"链接里的一次性凭证，只在这一次返回。完整链接是 <站点地址>/reset#<token>"`
		ExpiresAt time.Time `json:"expires_at"`
	}
}

var createResetLinkOp = huma.Operation{
	OperationID: "create-reset-link",
	Method:      http.MethodPost,
	Path:        "/api/members/{id}/reset-link",
	Summary:     "给成员生成重置密码的链接",
	Description: "成员忘了密码时用。链接 24 小时内有效、只能用一次；再生成一次，之前的就作废。不能给自己生成，自己的密码在「修改自己的密码」里改。",
	Tags:        []string{"成员"},
	Security:    adminOnly,
}

func (h *Handlers) CreateResetLink(ctx context.Context, in *InviteID) (*ResetLinkOutput, error) {
	id, err := parseMemberID(in.ID)
	if err != nil {
		return nil, err
	}
	token, expires, err := h.svc.CreateResetLink(ctx, currentUser(ctx), id)
	if err != nil {
		return nil, toHTTP(err)
	}
	out := &ResetLinkOutput{}
	out.Body.Token, out.Body.ExpiresAt = token, expires
	return out, nil
}

var revokeResetLinkOp = huma.Operation{
	OperationID:   "revoke-reset-link",
	Method:        http.MethodDelete,
	Path:          "/api/members/{id}/reset-link",
	Summary:       "撤销成员还没用过的重置密码链接",
	Description:   "链接发错了人，或者不确定是谁生成的时候用。撤销后那个链接立刻失效。",
	Tags:          []string{"成员"},
	Security:      adminOnly,
	DefaultStatus: http.StatusNoContent,
}

func (h *Handlers) RevokeResetLink(ctx context.Context, in *InviteID) (*struct{}, error) {
	id, err := parseMemberID(in.ID)
	if err != nil {
		return nil, err
	}
	if err := h.svc.RevokeResetLink(ctx, id); err != nil {
		return nil, toHTTP(err)
	}
	return nil, nil
}

// ---- 使用链接（不需要登录，token 就是凭证）----

type LinkTokenInput struct {
	Body struct {
		Token string `json:"token"`
	}
}

type LinkInfoOutput struct {
	Body struct {
		Kind      string `json:"kind" enum:"invite,reset" doc:"invite 是邀请加入，reset 是重置密码"`
		Email     string `json:"email" doc:"这个链接对应的邮箱"`
		Name      string `json:"name" doc:"重置密码时是账号主人的姓名；邀请时为空，由对方自己填"`
		InvitedBy string `json:"invited_by" doc:"邀请时是发出邀请的人的姓名"`
	}
}

var lookupLinkOp = huma.Operation{
	OperationID: "lookup-account-link",
	Method:      http.MethodPost,
	Path:        "/api/account-links/lookup",
	Summary:     "查看一个邀请或重置链接",
	Description: "落地页打开时调用，用来显示「谁邀请你」「给哪个账号重置密码」。链接无效返回 404；用过了、过期了、或者发出它的人已经不是管理员，返回 409；要重置密码的账号已被停用，返回 403。",
	Tags:        []string{"账号"},
}

func (h *Handlers) LookupLink(ctx context.Context, in *LinkTokenInput) (*LinkInfoOutput, error) {
	info, err := h.svc.LookupLink(ctx, in.Body.Token)
	if err != nil {
		return nil, toHTTP(err)
	}
	out := &LinkInfoOutput{}
	out.Body.Kind, out.Body.Email, out.Body.Name, out.Body.InvitedBy = info.Kind, info.Email, info.Name, info.InvitedBy
	return out, nil
}

type AcceptLinkInput struct {
	Body struct {
		Token    string `json:"token"`
		Name     string `json:"name,omitempty" maxLength:"50" doc:"接受邀请时必填；重置密码时不用传"`
		Password string `json:"password" minLength:"8" doc:"8 到 200 位"`
	}
}

var acceptLinkOp = huma.Operation{
	OperationID: "accept-account-link",
	Method:      http.MethodPost,
	Path:        "/api/account-links/accept",
	Summary:     "接受邀请，或者设置新密码",
	Description: "成功后链接作废，并且直接登录。重置密码成功后，这个账号在其他设备上的登录全部失效。",
	Tags:        []string{"账号"},
}

func (h *Handlers) AcceptLink(ctx context.Context, in *AcceptLinkInput) (*UserOutput, error) {
	user, err := h.svc.AcceptLink(ctx, in.Body.Token, in.Body.Name, in.Body.Password)
	if err != nil {
		return nil, toHTTP(err)
	}
	return h.startSession(ctx, user)
}

// ---- 修改自己的密码 ----

type ChangePasswordInput struct {
	Body struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password" minLength:"8" doc:"8 到 200 位"`
	}
}

var changePasswordOp = huma.Operation{
	OperationID:   "change-password",
	Method:        http.MethodPost,
	Path:          "/api/me/password",
	Summary:       "修改自己的密码",
	Description:   "成功后当前设备保持登录，其他设备上的登录全部失效。",
	Tags:          []string{"账号"},
	Security:      loggedIn,
	DefaultStatus: http.StatusNoContent,
}

func (h *Handlers) ChangePassword(ctx context.Context, in *ChangePasswordInput) (*struct{}, error) {
	user, err := h.svc.ChangePassword(ctx, currentUser(ctx), in.Body.CurrentPassword, in.Body.NewPassword)
	if err != nil {
		return nil, toHTTP(err)
	}
	// 当前会话换一个令牌，并记下新的 epoch：这台设备继续登录，别的设备被退出
	if _, err := h.startSession(ctx, user); err != nil {
		return nil, err
	}
	return nil, nil
}

func parseMemberID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, huma.Error404NotFound("没有这个成员或邀请")
	}
	return id, nil
}

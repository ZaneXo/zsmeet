package service

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/feishu"
	"github.com/ZaneXo/zsmeet/internal/password"
	"github.com/ZaneXo/zsmeet/internal/settings"
)

// 用飞书登录。成员在飞书的授权页上扫码（或者在飞书网页版已经登录时点一下），飞书跳回 zsmeet，
// zsmeet 拿到这个人在飞书的 ID 和邮箱，按下面的顺序对应到 zsmeet 的账号上：
//
//  1. 已经关联过的（user_identities），按关联找，邮箱改过也不影响；
//  2. 企业邮箱（enterprise_email，公司发的飞书邮箱，飞书验证过）和某个成员的邮箱一样：记下关联，登录；
//  3. 企业邮箱有一个还能用的邀请：按邀请的角色建账号，邀请算用掉了，记下关联，登录；
//  4. 通讯录里的邮箱（email）和某个成员一样：不关联，拒绝。飞书说这个邮箱是企业管理员填的，
//     没有经过本人验证，不能当登录凭证：飞书里谁的通讯录邮箱被填成了管理员的邮箱，就能登录成管理员。
//     请他先用邮箱和密码登录，再在「设置 → 账号」里自己关联飞书；
//  5. 通讯录里的邮箱有一个邀请：不用掉这个邀请，请他打开邀请链接；
//  6. 都没有：管理员打开了「自动加入」就新建一个成员账号，否则拒绝，请管理员先邀请。
//
// 登录着的人也可以在「设置 → 账号」里主动关联（LinkFeishu）：那时已经用密码证明了自己是谁，不看邮箱。

// FeishuCallbackPath 是飞书登录完跳回来的地址（接在 zsmeet 的访问地址后面），要原样填进飞书应用的「重定向 URL」。
const FeishuCallbackPath = "/api/auth/feishu/callback"

// 关联表里飞书账号的 provider
const providerFeishu = "feishu"

// FeishuRedirectURL 是飞书应用里要填的重定向 URL。
func FeishuRedirectURL(siteURL string) string {
	return strings.TrimRight(siteURL, "/") + FeishuCallbackPath
}

// FeishuAuthError 是用飞书登录、关联飞书没有成功的原因。Code 是下面几个固定的值之一，
// 网页按它显示说明（错误放在跳回来的地址里，只能是固定的代号，不能是任意的文字）。
type FeishuAuthError struct{ Code string }

func (e *FeishuAuthError) Error() string { return "用飞书登录没有成功：" + e.Code }

// 飞书登录失败的原因
const (
	// 管理员没有打开飞书登录、没配置好，或者系统还没初始化
	FeishuUnavailable = "unavailable"
	// 发起时记下的 state 没有了、过期了、对不上（超过 10 分钟，或者换了浏览器）
	FeishuExpired = "expired"
	// 关联飞书时已经不是发起时登录着的那个人了（退出了，或者换了账号）
	FeishuSignedOut = "signed_out"
	// 关联飞书要先在账号页上输入密码（用地址直接发起关联不行）
	FeishuLinkPassword = "link_password"
	// 在飞书的授权页上没有同意
	FeishuDenied = "denied"
	// 账号被停用了
	FeishuAccountDisabled = "account_disabled"
	// 企业邮箱对应的成员已经关联了另一个飞书账号
	FeishuUserLinkedOther = "user_linked_other"
	// 企业邮箱有邀请，但邀请已经用不了了（过期、发邀请的人不再是管理员）
	FeishuInviteUnusable = "invite_unusable"
	// 通讯录里的邮箱和一个成员一样（没有验证过，不能直接登录）
	FeishuContactEmailMember = "contact_email_member"
	// 通讯录里的邮箱有一个邀请
	FeishuContactEmailInvite = "contact_email_invite"
	// 还不是成员，没打开自动加入
	FeishuNotMember = "not_member"
	// 打开了自动加入，但飞书没给出邮箱
	FeishuNoEmail = "no_email"
	// 关联：这个飞书账号已经关联了别的 zsmeet 账号
	FeishuLinkedElsewhere = "linked_elsewhere"
	// 关联：自己已经关联了另一个飞书账号
	FeishuAlreadyLinked = "already_linked"
	// 同一个飞书账号同时来了两次，另一次刚处理完
	FeishuBusy = "busy"
	// 换取用户凭证失败：App ID / App Secret 不对
	FeishuAppCredentials = "app_credentials"
	// 换取用户凭证失败：重定向 URL 对不上
	FeishuRedirectMismatch = "redirect_mismatch"
	// 换取用户凭证失败：不在应用的可用范围里
	FeishuOutOfScope = "out_of_scope"
	// 换取用户凭证失败：授权码用过了、过期了、PKCE 对不上，重新来一次就好
	FeishuRetry = "retry"
	// 其他（连不上飞书、数据库出错……），细节只进日志
	FeishuFailed = "failed"
)

func authErr(code string) error { return &FeishuAuthError{Code: code} }

// feishuLoginConfig 返回能用来登录的飞书配置。系统还没初始化、没打开飞书登录时返回 FeishuUnavailable。
func (s *Service) feishuLoginConfig(ctx context.Context) (settings.Feishu, error) {
	initialized, err := s.Initialized(ctx)
	if err != nil {
		return settings.Feishu{}, err
	}
	if !initialized {
		return settings.Feishu{}, authErr(FeishuUnavailable)
	}
	cfg, err := s.settings.Feishu(ctx)
	if err != nil {
		return settings.Feishu{}, err
	}
	if !cfg.LoginReady() {
		return settings.Feishu{}, authErr(FeishuUnavailable)
	}
	return cfg, nil
}

// FeishuLoginEnabled 表示登录页上要不要显示「用飞书登录」、账号页上能不能关联飞书。
func (s *Service) FeishuLoginEnabled(ctx context.Context) (bool, error) {
	_, err := s.feishuLoginConfig(ctx)
	if _, ok := errors.AsType[*FeishuAuthError](err); ok {
		return false, nil
	}
	return err == nil, err
}

// 发起飞书登录的两种用途
const (
	// 用飞书登录
	FeishuModeLogin = "login"
	// 登录着的人把飞书账号关联到自己的账号上
	FeishuModeLink = "link"
)

// FeishuStateTTL 是从点「用飞书登录」到飞书跳回来最多多久。飞书的授权码本身 5 分钟就过期，
// 这里多留一点扫码、确认的时间。
const FeishuStateTTL = 10 * time.Minute

// state 加密时的用途，见 secret.Box.SealFor
const feishuStatePurpose = "zsmeet/feishu-oauth-state/v1"

// FeishuState 是发起一次飞书登录时记下的东西。加密后放在这个浏览器的一个短期 Cookie 里，
// 不放进服务器的会话：那样每个没登录的人点一下按钮，服务器就要存一条 30 天的会话。
type FeishuState struct {
	// 随机的 state，飞书原样带回来，核对一致才认（防止别人把他的授权码塞给你）
	State string `json:"s"`
	// PKCE 的 code_verifier：授权码被别人截到也换不出凭证
	Verifier string `json:"v"`
	Mode     string `json:"m"`
	// 关联飞书时，发起的是哪个 zsmeet 账号
	UserID uuid.UUID `json:"u,omitzero"`
	// 什么时候过期（Unix 秒）
	Expires int64 `json:"e"`
}

// StartFeishuAuth 发起一次飞书登录（mode 为 FeishuModeLogin）或者关联（FeishuModeLink，user 是登录着的人）。
// 返回飞书授权页的地址，和要原样存进这个浏览器 Cookie 的加密过的 state（见 OpenFeishuState）。
func (s *Service) StartFeishuAuth(ctx context.Context, mode string, user *sqlc.User) (authorizeURL, sealed string, err error) {
	cfg, err := s.feishuLoginConfig(ctx)
	if err != nil {
		return "", "", err
	}
	st := FeishuState{State: randomString(), Verifier: randomString(), Mode: FeishuModeLogin, Expires: time.Now().Add(FeishuStateTTL).Unix()}
	if mode == FeishuModeLink {
		if user == nil {
			return "", "", authErr(FeishuSignedOut)
		}
		st.Mode, st.UserID = FeishuModeLink, user.ID
	}
	return feishu.AuthorizeURL(cfg.Domain, s.FeishuBaseURL, cfg.AppID, FeishuRedirectURL(cfg.SiteURL), st.State, feishu.PKCEChallenge(st.Verifier)),
		s.SealFeishuState(st), nil
}

// StartFeishuLink 在账号页上关联飞书：先核对当前密码，再发起关联。只拿到了登录状态的人（比如别人的电脑上没退出），
// 没有密码就没法把自己的飞书账号关联上来，留下一个改了密码都还能进来的后门。
func (s *Service) StartFeishuLink(ctx context.Context, user sqlc.User, pass string) (authorizeURL, sealed string, err error) {
	me, err := s.q.GetUser(ctx, user.ID)
	if err != nil {
		return "", "", err
	}
	ok, err := password.Verify(pass, me.PasswordHash)
	if err != nil {
		return "", "", err
	}
	if !ok {
		return "", "", invalid("密码不对。用飞书自动创建的账号没有密码：请管理员在「设置 → 成员」里给你生成重置密码的链接，设好密码再关联")
	}
	return s.StartFeishuAuth(ctx, FeishuModeLink, &me)
}

// SealFeishuState 把 state 加密成能放进 Cookie 的一串字。
func (s *Service) SealFeishuState(st FeishuState) string {
	raw, _ := json.Marshal(st)
	return s.settings.Box().SealFor(feishuStatePurpose, string(raw))
}

// OpenFeishuState 解开 StartFeishuAuth 给出的 state。没有、被改过、过期了都返回 FeishuExpired。
func (s *Service) OpenFeishuState(sealed string) (FeishuState, error) {
	var st FeishuState
	raw, err := s.settings.Box().OpenFor(feishuStatePurpose, sealed)
	if err != nil || json.Unmarshal([]byte(raw), &st) != nil || st.State == "" || st.Verifier == "" ||
		time.Now().Unix() > st.Expires || (st.Mode != FeishuModeLogin && st.Mode != FeishuModeLink) ||
		(st.Mode == FeishuModeLink) == (st.UserID == uuid.Nil) {
		return FeishuState{}, authErr(FeishuExpired)
	}
	return st, nil
}

// randomString 是 32 字节的随机数，URL 安全的 base64（43 个字符，也正好满足 PKCE 对 code_verifier 的要求）。
func randomString() string {
	raw := make([]byte, 32)
	rand.Read(raw)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// feishuIdentity 用授权码换出飞书账号。subject 是关联用的 ID：union_id（同一个企业里不随应用变），没有时用 open_id。
func (s *Service) feishuIdentity(ctx context.Context, cfg settings.Feishu, code, verifier string) (feishu.LoginUser, string, error) {
	fu, err := s.feishuClient(cfg).LoginUser(ctx, code, FeishuRedirectURL(cfg.SiteURL), verifier)
	if oe, ok := errors.AsType[*feishu.OAuthError](err); ok {
		switch oe.Code {
		case 20002:
			return feishu.LoginUser{}, "", authErr(FeishuAppCredentials)
		case 20003, 20004, 20049:
			return feishu.LoginUser{}, "", authErr(FeishuRetry)
		case 20010:
			return feishu.LoginUser{}, "", authErr(FeishuOutOfScope)
		case 20071:
			return feishu.LoginUser{}, "", authErr(FeishuRedirectMismatch)
		}
		// 别的错误码：原样（不带授权码和凭证）进日志，用户看到笼统的说明
		return feishu.LoginUser{}, "", err
	}
	if err != nil {
		return feishu.LoginUser{}, "", err
	}
	subject := fu.UnionID
	if subject == "" {
		subject = fu.OpenID
	}
	return fu, subject, nil
}

// FeishuLogin 用飞书跳回来时带的授权码登录，返回对应的 zsmeet 账号。verifier 是 StartFeishuAuth 记下的。
// 调用方已经核对过 state。对应的顺序见这个文件开头。
func (s *Service) FeishuLogin(ctx context.Context, code, verifier string) (sqlc.User, error) {
	cfg, err := s.feishuLoginConfig(ctx)
	if err != nil {
		return sqlc.User{}, err
	}
	fu, subject, err := s.feishuIdentity(ctx, cfg, code, verifier)
	if err != nil {
		return sqlc.User{}, err
	}

	// 1. 已经关联过
	link, err := s.q.GetIdentity(ctx, sqlc.GetIdentityParams{Provider: providerFeishu, Subject: subject})
	switch {
	case err == nil:
		user, err := s.q.GetUser(ctx, link.UserID)
		if err != nil {
			return sqlc.User{}, err
		}
		if user.Disabled {
			return sqlc.User{}, authErr(FeishuAccountDisabled)
		}
		return user, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return sqlc.User{}, err
	}

	// 格式不对的邮箱当作没有
	enterprise, _ := cleanEmail(fu.EnterpriseEmail)
	contact, _ := cleanEmail(fu.Email)

	if enterprise != "" {
		// 2. 企业邮箱是成员
		user, err := s.q.GetUserByEmail(ctx, enterprise)
		if err == nil {
			return s.linkByEmail(ctx, user.ID, subject)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return sqlc.User{}, err
		}
		// 3. 企业邮箱有邀请
		invite, err := s.q.GetPendingInviteByEmail(ctx, &enterprise)
		if err == nil {
			return s.joinByInvite(ctx, invite.ID, fu.Name, subject)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return sqlc.User{}, err
		}
	}

	if contact != "" {
		// 4. 通讯录里的邮箱是成员：没验证过，不能凭它登录
		user, err := s.q.GetUserByEmail(ctx, contact)
		if err == nil {
			if user.Disabled {
				return sqlc.User{}, authErr(FeishuAccountDisabled)
			}
			return sqlc.User{}, authErr(FeishuContactEmailMember)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return sqlc.User{}, err
		}
		// 5. 通讯录里的邮箱有邀请：邀请链接才是凭证，不能凭一个没验证过的邮箱用掉它
		_, err = s.q.GetPendingInviteByEmail(ctx, &contact)
		if err == nil {
			return sqlc.User{}, authErr(FeishuContactEmailInvite)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return sqlc.User{}, err
		}
	}

	// 6. 还不是成员
	if !cfg.LoginAutoJoin {
		return sqlc.User{}, authErr(FeishuNotMember)
	}
	email := enterprise
	if email == "" {
		email = contact
	}
	if email == "" {
		return sqlc.User{}, authErr(FeishuNoEmail)
	}
	return s.joinFromFeishu(ctx, fu.Name, email, subject)
}

// linkByEmail 记下 zsmeet 账号和飞书账号的关联（第一次用飞书登录、按企业邮箱找到成员时）。
func (s *Service) linkByEmail(ctx context.Context, userID uuid.UUID, subject string) (sqlc.User, error) {
	var user sqlc.User
	err := s.tx(ctx, func(_ pgx.Tx, q *sqlc.Queries) error {
		// 锁住这个人：同时停用他、或者同时有两个飞书账号来关联时排队
		var err error
		if user, err = q.LockUser(ctx, userID); err != nil {
			return err
		}
		if user.Disabled {
			return authErr(FeishuAccountDisabled)
		}
		_, err = q.GetUserIdentity(ctx, sqlc.GetUserIdentityParams{Provider: providerFeishu, UserID: userID})
		switch {
		case err == nil:
			// 已经关联了另一个飞书账号（是同一个的话，第 1 步就找到了）：不能被按邮箱接管
			return authErr(FeishuUserLinkedOther)
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		return createIdentity(ctx, q, subject, userID)
	})
	return user, err
}

func createIdentity(ctx context.Context, q *sqlc.Queries, subject string, userID uuid.UUID) error {
	err := q.CreateIdentity(ctx, sqlc.CreateIdentityParams{Provider: providerFeishu, Subject: subject, UserID: userID})
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
		// 同一个飞书账号同时来了两次，另一次刚刚关联好
		return authErr(FeishuBusy)
	}
	return err
}

// feishuName 把飞书上的名字整理成 zsmeet 的姓名（不能为空，最长 50 个字）：没有名字时用邮箱 @ 前面的部分。
func feishuName(name, email string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name, _, _ = strings.Cut(email, "@")
	}
	if utf8.RuneCountInString(name) > 50 {
		name = string([]rune(name)[:50])
	}
	return name
}

// joinByInvite 用发给企业邮箱的邀请建账号：角色按邀请的，邀请算用掉了，记下关联。
// 和打开邀请链接（AcceptLink）一样锁住邀请、用 usable 检查它还能用。密码设成一个谁都不知道的随机串，
// 想用密码登录的话，请管理员生成重置密码的链接。
func (s *Service) joinByInvite(ctx context.Context, inviteID uuid.UUID, name, subject string) (sqlc.User, error) {
	hash := password.Hash(randomString())
	var user sqlc.User
	err := s.tx(ctx, func(_ pgx.Tx, q *sqlc.Queries) error {
		// 锁住邀请这一行：同时有人打开邀请链接时，后到的那个会看到「已经用过了」。
		// 刚才那一眼之后邀请可能被撤销、用掉了
		link, err := q.LockLinkByID(ctx, inviteID)
		if errors.Is(err, pgx.ErrNoRows) {
			return authErr(FeishuNotMember)
		}
		if err != nil {
			return err
		}
		if link.UsedAt != nil {
			// 刚刚被用掉了：再登录一次就会按成员找到
			return authErr(FeishuBusy)
		}
		if err := usable(ctx, q, link); err != nil {
			if _, ok := errors.AsType[*Error](err); ok {
				return authErr(FeishuInviteUnusable)
			}
			return err
		}
		user, err = q.CreateUser(ctx, sqlc.CreateUserParams{
			Email: *link.Email, Name: feishuName(name, *link.Email), PasswordHash: hash, Role: *link.Role,
		})
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
			return authErr(FeishuBusy)
		}
		if err != nil {
			return err
		}
		if err := createIdentity(ctx, q, subject, user.ID); err != nil {
			return err
		}
		return q.MarkLinkUsed(ctx, link.ID)
	})
	return user, err
}

// joinFromFeishu 给还不是成员的飞书账号新建一个成员账号（管理员打开了「自动加入」时）。
// 密码设成一个谁都不知道的随机串：想用密码登录的话，请管理员生成重置密码的链接。
func (s *Service) joinFromFeishu(ctx context.Context, name, email, subject string) (sqlc.User, error) {
	hash := password.Hash(randomString())
	var user sqlc.User
	err := s.tx(ctx, func(_ pgx.Tx, q *sqlc.Queries) error {
		var err error
		user, err = q.CreateUser(ctx, sqlc.CreateUserParams{Email: email, Name: feishuName(name, email), PasswordHash: hash, Role: "member"})
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
			return authErr(FeishuBusy)
		}
		if err != nil {
			return err
		}
		return createIdentity(ctx, q, subject, user.ID)
	})
	return user, err
}

// LinkFeishu 把飞书账号关联到登录着的这个人（在「设置 → 账号」里点「关联飞书」）。
// 他已经登录了，所以不看邮箱。这个飞书账号已经关联了别人、或者他自己已经关联了另一个飞书账号时拒绝。
func (s *Service) LinkFeishu(ctx context.Context, user sqlc.User, code, verifier string) error {
	cfg, err := s.feishuLoginConfig(ctx)
	if err != nil {
		return err
	}
	_, subject, err := s.feishuIdentity(ctx, cfg, code, verifier)
	if err != nil {
		return err
	}
	return s.tx(ctx, func(_ pgx.Tx, q *sqlc.Queries) error {
		me, err := q.LockUser(ctx, user.ID)
		if err != nil {
			return err
		}
		if me.Disabled {
			return authErr(FeishuAccountDisabled)
		}
		link, err := q.GetIdentity(ctx, sqlc.GetIdentityParams{Provider: providerFeishu, Subject: subject})
		switch {
		case err == nil && link.UserID == me.ID:
			return nil
		case err == nil:
			return authErr(FeishuLinkedElsewhere)
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		_, err = q.GetUserIdentity(ctx, sqlc.GetUserIdentityParams{Provider: providerFeishu, UserID: me.ID})
		switch {
		case err == nil:
			return authErr(FeishuAlreadyLinked)
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		return createIdentity(ctx, q, subject, me.ID)
	})
}

// FeishuLink 返回这个人关联的飞书账号是什么时候关联的；没关联过时返回 nil。
func (s *Service) FeishuLink(ctx context.Context, userID uuid.UUID) (*time.Time, error) {
	link, err := s.q.GetUserIdentity(ctx, sqlc.GetUserIdentityParams{Provider: providerFeishu, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &link.CreatedAt, nil
}

// UnlinkFeishu 解除这个人和飞书账号的关联：他自己在账号页上解除，或者管理员在成员列表里解除
// （换了飞书账号、自动创建的账号不知道密码时）。之后用飞书登录时重新按上面的顺序对应。
func (s *Service) UnlinkFeishu(ctx context.Context, userID uuid.UUID) error {
	n, err := s.q.DeleteUserIdentity(ctx, sqlc.DeleteUserIdentityParams{Provider: providerFeishu, UserID: userID})
	if err == nil && n == 0 {
		return notFound("这个账号没有关联飞书账号，可能已经解除了")
	}
	return err
}

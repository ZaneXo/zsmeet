package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/password"
)

// 成员管理：邀请、角色、停用、重置密码。
//
// 系统不发邮件。邀请和重置密码都是生成一个一次性的链接，由管理员自己发给对方。
// 链接里的随机串（token）就是凭证：数据库里只存它的哈希，所以链接只在生成的那一刻看得到。

const (
	// 邀请链接的有效期
	inviteTTL = 7 * 24 * time.Hour
	// 重置密码的链接能直接接管一个已有的账号，有效期短一些
	resetTTL = 24 * time.Hour
	// 成员锁：见 withMembersLock
	membersLock = 7_368_823
	minPassword = 8
	// 密码长度的上限。只是为了挡住离谱的输入，正常的密码（包括密码管理器生成的）远远到不了
	maxPassword = 200
)

// newToken 生成链接里的随机串，返回原文和存进数据库的哈希。
func newToken() (token string, hash []byte) {
	raw := make([]byte, 32)
	rand.Read(raw)
	token = base64.RawURLEncoding.EncodeToString(raw)
	return token, hashToken(token)
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func cleanEmail(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if addr, err := mail.ParseAddress(email); err != nil || addr.Address != email {
		return "", invalid("邮箱格式不对")
	}
	return email, nil
}

// checkPassword 检查一个新设的密码。长度在这里查而不是写进接口的参数约束里：
// 那样超长时返回的是框架生成的英文提示；也不让网页的输入框限制长度，粘贴进去的长密码会被悄悄截断。
func checkPassword(pass string) error {
	n := utf8.RuneCountInString(pass)
	if n < minPassword {
		return invalid("密码至少 %d 位", minPassword)
	}
	if n > maxPassword {
		return invalid("密码最长 %d 位，现在是 %d 位", maxPassword, n)
	}
	return nil
}

func checkRole(role string) error {
	if role != "admin" && role != "member" {
		return invalid("角色只能是管理员或成员")
	}
	return nil
}

// Invite 是一个还没被接受的邀请。
type Invite struct {
	ID        uuid.UUID
	Email     string
	Role      string
	InvitedBy string
	CreatedAt time.Time
	ExpiresAt time.Time
	// 链接已经用不了了：过期了，或者发出邀请的人已经不是管理员。重新生成一个就能接着用
	Expired bool
}

// toInvite 把数据库里的一行变成邀请。issuerIsAdmin 是发出邀请的人现在是不是还在任的管理员。
func toInvite(l sqlc.AccountLink, invitedBy string, issuerIsAdmin bool) Invite {
	return Invite{
		ID: l.ID, Email: *l.Email, Role: *l.Role, InvitedBy: invitedBy, CreatedAt: l.CreatedAt, ExpiresAt: l.ExpiresAt,
		Expired: !issuerIsAdmin || time.Now().After(l.ExpiresAt),
	}
}

// Member 是一个成员，加上他名下还没用过的重置密码链接（没有就是 nil）。
type Member struct {
	sqlc.User
	Reset *PendingReset
}

// PendingReset 是一个已经发出去、还能用的重置密码链接。
// 拿到它的人能直接接管这个账号，所以要让管理员们看得到、撤得掉。
type PendingReset struct {
	ExpiresAt time.Time
	// 是哪位管理员生成的；FromServer 为 true 时没有意义
	CreatedBy string
	// 是在服务器上用 zsmeet reset-link 命令生成的，不是哪位管理员在网页上生成的
	FromServer bool
}

// Members 返回全部成员和还没被接受的邀请。
func (s *Service) Members(ctx context.Context) ([]Member, []Invite, error) {
	users, err := s.q.ListUsers(ctx)
	if err != nil {
		return nil, nil, err
	}
	resets, err := s.q.ListPendingResets(ctx)
	if err != nil {
		return nil, nil, err
	}
	pending := make(map[uuid.UUID]*PendingReset, len(resets))
	for _, r := range resets {
		reset := &PendingReset{ExpiresAt: r.ExpiresAt, FromServer: r.FromServer}
		if !r.FromServer {
			reset.CreatedBy = r.CreatedByName
		}
		pending[*r.UserID] = reset
	}
	members := make([]Member, len(users))
	for i, u := range users {
		members[i] = Member{User: u, Reset: pending[u.ID]}
	}

	rows, err := s.q.ListPendingInvites(ctx)
	if err != nil {
		return nil, nil, err
	}
	invites := make([]Invite, len(rows))
	for i, r := range rows {
		invites[i] = toInvite(r.AccountLink, r.InvitedBy, r.IssuerIsAdmin)
	}
	return members, invites, nil
}

// CreateInvite 邀请一个邮箱加入，返回邀请和链接里的 token。
// 这个邮箱已经有没用过的邀请时，旧链接作废，换成新的。
func (s *Service) CreateInvite(ctx context.Context, admin sqlc.User, email, role string) (Invite, string, error) {
	email, err := cleanEmail(email)
	if err != nil {
		return Invite{}, "", err
	}
	if err := checkRole(role); err != nil {
		return Invite{}, "", err
	}
	token, hash := newToken()
	var link sqlc.AccountLink
	err = s.withMembersLock(ctx, func(q *sqlc.Queries) error {
		if err := stillAdmin(ctx, q, admin.ID); err != nil {
			return err
		}
		existing, err := q.GetUserByEmail(ctx, email)
		switch {
		case err == nil && existing.Disabled:
			return conflict("这个邮箱的账号已被停用。在成员列表里重新启用就行，不需要再邀请")
		case err == nil:
			return conflict("这个邮箱已经是成员了")
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		link, err = q.UpsertInvite(ctx, sqlc.UpsertInviteParams{
			TokenHash: hash, Email: &email, Role: &role, CreatedBy: admin.ID, ExpiresAt: time.Now().Add(inviteTTL),
		})
		return err
	})
	if err != nil {
		return Invite{}, "", err
	}
	return toInvite(link, admin.Name, true), token, nil
}

var errNoInvite = notFound("这个邀请不存在，可能已经被接受或者撤销了")

// RenewInvite 给一个邀请换新链接（管理员把链接弄丢了，或者链接过期了）。旧链接随即失效。
func (s *Service) RenewInvite(ctx context.Context, admin sqlc.User, id uuid.UUID) (Invite, string, error) {
	token, hash := newToken()
	var link sqlc.AccountLink
	err := s.withMembersLock(ctx, func(q *sqlc.Queries) error {
		if err := stillAdmin(ctx, q, admin.ID); err != nil {
			return err
		}
		var err error
		link, err = q.RenewInvite(ctx, sqlc.RenewInviteParams{
			ID: id, TokenHash: hash, CreatedBy: admin.ID, ExpiresAt: time.Now().Add(inviteTTL),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return errNoInvite
		}
		return err
	})
	if err != nil {
		return Invite{}, "", err
	}
	return toInvite(link, admin.Name, true), token, nil
}

// RevokeInvite 撤销一个还没被接受的邀请。
func (s *Service) RevokeInvite(ctx context.Context, id uuid.UUID) error {
	n, err := s.q.DeleteInvite(ctx, id)
	if err == nil && n == 0 {
		return errNoInvite
	}
	return err
}

// MemberChange 是对一个成员的修改，nil 的字段不动。
type MemberChange struct {
	Role     *string
	Disabled *bool
}

var (
	errNoMember        = notFound("没有这个成员")
	errAccountDisabled = &Error{Forbidden, "这个账号已被停用，请联系管理员"}
)

// UpdateMember 改成员的角色，或者停用、启用。
//
// 管理员不能改自己：这样做操作的人一定还是管理员，系统里就永远至少有一个管理员。
// 两个管理员同时互相降级时，这个保证靠锁来守住：操作一个一个来，轮到后面那个时他已经不是管理员了。
func (s *Service) UpdateMember(ctx context.Context, actor sqlc.User, id uuid.UUID, change MemberChange) (sqlc.User, error) {
	if change.Role != nil {
		if err := checkRole(*change.Role); err != nil {
			return sqlc.User{}, err
		}
	}
	if id == actor.ID {
		return sqlc.User{}, invalid("不能修改自己的角色，也不能停用自己。需要的话请让另一位管理员操作")
	}
	var out sqlc.User
	err := s.withMembersLock(ctx, func(q *sqlc.Queries) error {
		if err := stillAdmin(ctx, q, actor.ID); err != nil {
			return err
		}
		before, err := q.LockUser(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNoMember
		}
		if err != nil {
			return err
		}
		out = before
		if change.Role != nil {
			if out, err = q.SetUserRole(ctx, sqlc.SetUserRoleParams{ID: id, Role: *change.Role}); err != nil {
				return err
			}
		}
		if change.Disabled != nil {
			if out, err = q.SetUserDisabled(ctx, sqlc.SetUserDisabledParams{ID: id, Disabled: *change.Disabled}); err != nil {
				return err
			}
		}
		// 他发出去的邀请和重置链接是凭管理员身份发的，只在他这一任管理员期间有效。
		// 卸任（降级或停用）时，还没用掉的跟着失效：否则一个刚被撤掉权限的人，
		// 还能用手里留着的链接拿回一个管理员账号。
		// 重新上任时也清一次，保证上一任留下的链接不会跟着身份一起复活。正常情况下这时已经没有可清的了
		// （卸任时清过，卸任期间也发不出新的），这是多一道保险
		if activeAdmin(before) != activeAdmin(out) {
			if err := expireIssuedLinks(ctx, q, id); err != nil {
				return err
			}
		}
		// 停用、重新启用时，这个人名下还没用的重置密码链接都作废：
		// 停用期间不该留着能改他密码的链接，启用以后需要的话再生成一个
		if before.Disabled != out.Disabled {
			if _, err := q.DeletePendingReset(ctx, &id); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

func activeAdmin(u sqlc.User) bool {
	return u.Role == "admin" && !u.Disabled
}

// withMembersLock 在一个事务里执行 fn，全程拿着成员锁（一把全局的 Postgres advisory lock），
// 所以这些事务一个一个来。两类操作要在这把锁里做：
//
//   - 改变「谁是在任管理员、谁被停用」的：改角色、停用、启用；
//   - 凭管理员身份发链接的：邀请、重新生成邀请、生成重置密码链接。
//
// 这样后一类在锁里确认过「我还是管理员」之后，直到事务提交这个结论都不会变；
// 前一类清理某个人发出去的链接时，也不会漏掉一条正写到一半的。
// 没有这把锁的话，管理员被降级的那一瞬间正在发的链接会躲过清理，等他哪天恢复身份就又能用了。
//
// 还要锁用户、链接的行时，顺序固定是：成员锁 → 用户 → 链接。
// AcceptLink、ChangePassword 不拿成员锁，后两步的顺序和这里相同，所以不会互相卡死。
func (s *Service) withMembersLock(ctx context.Context, fn func(q *sqlc.Queries) error) error {
	return s.tx(ctx, func(tx pgx.Tx, q *sqlc.Queries) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", membersLock); err != nil {
			return err
		}
		return fn(q)
	})
}

// stillAdmin 在成员锁里重新确认操作者还是在任的管理员。
// 请求刚进来时查过一次，但那是在锁外面：从那时到现在，另一位管理员可能已经把他降级或停用了。
func stillAdmin(ctx context.Context, q *sqlc.Queries, id uuid.UUID) error {
	me, err := q.GetUser(ctx, id)
	if err != nil {
		return err
	}
	if !activeAdmin(me) {
		return &Error{Forbidden, "你已经不是管理员了，刷新页面看看"}
	}
	return nil
}

// expireIssuedLinks 让某个人凭管理员身份发出去、还没用掉的链接全部失效。
// 邀请只是让它过期、不删：别的管理员在列表里还看得到，重新生成一个链接就能接着用。
// 在服务器上用命令给他本人生成的重置链接不受影响（见 usable）。
func expireIssuedLinks(ctx context.Context, q *sqlc.Queries, issuer uuid.UUID) error {
	if err := q.ExpireInvitesCreatedBy(ctx, issuer); err != nil {
		return err
	}
	return q.DeleteResetsCreatedBy(ctx, issuer)
}

// CreateResetLink 让管理员给另一个成员生成重置密码的链接，返回 token 和过期时间。
// 这个人已经有没用过的重置链接时，旧的作废。
func (s *Service) CreateResetLink(ctx context.Context, admin sqlc.User, userID uuid.UUID) (string, time.Time, error) {
	if userID == admin.ID {
		// 「生成的人就是账号本人」这个特征留给服务器上的命令用（见 usable）
		return "", time.Time{}, invalid("要改自己的密码，请到「设置 → 账号」里修改")
	}
	return s.issueReset(ctx, &admin.ID, userID)
}

// issueReset 给 userID 生成重置密码的链接。issuer 是生成它的管理员；
// nil 表示是在服务器上用命令生成的，这时把生成者记成账号本人（见 usable）。
func (s *Service) issueReset(ctx context.Context, issuer *uuid.UUID, userID uuid.UUID) (string, time.Time, error) {
	token, hash := newToken()
	var link sqlc.AccountLink
	err := s.withMembersLock(ctx, func(q *sqlc.Queries) error {
		createdBy := userID
		if issuer != nil {
			if err := stillAdmin(ctx, q, *issuer); err != nil {
				return err
			}
			createdBy = *issuer
		}
		// 先锁用户、再动链接（顺序见 withMembersLock）。锁住以后，「没被停用」在写完链接之前也不会变
		target, err := q.LockUser(ctx, userID)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNoMember
		}
		if err != nil {
			return err
		}
		if target.Disabled {
			return conflict("这个账号已被停用，先启用再重置密码")
		}
		link, err = q.UpsertReset(ctx, sqlc.UpsertResetParams{
			TokenHash: hash, UserID: &userID, CreatedBy: createdBy, ExpiresAt: time.Now().Add(resetTTL),
		})
		return err
	})
	if err != nil {
		return "", time.Time{}, err
	}
	return token, link.ExpiresAt, nil
}

// RevokeResetLink 撤销某个成员还没用过的重置密码链接。
func (s *Service) RevokeResetLink(ctx context.Context, userID uuid.UUID) error {
	n, err := s.q.DeletePendingReset(ctx, &userID)
	if err == nil && n == 0 {
		return notFound("这个成员没有还没用过的重置密码链接，可能已经用掉或者撤销了")
	}
	return err
}

// LinkInfo 是打开一个账号链接时，落地页要显示的内容。
type LinkInfo struct {
	// invite 或 reset
	Kind  string
	Email string
	// reset：账号主人的姓名
	Name string
	// invite：谁发的邀请
	InvitedBy string
}

var errNoLink = notFound("链接无效。请确认链接复制完整了；如果管理员重新生成过链接，旧的就不能用了")

// usable 检查链接还能不能用：没用过、没过期，而且发出它的人现在仍然是管理员。
//
// 最后一条是这类链接的根本规则：链接的效力来自发出者的管理员身份。
// UpdateMember 在降级、停用时会把那个人发的链接删掉，这里再查一次，
// 是为了不依赖「每一处改角色的代码都记得去删」。
// 例外是服务器上用 zsmeet reset-link 给自己生成的链接（created_by 就是账号本人），
// 它的效力来自能登录服务器，和角色无关。
func usable(ctx context.Context, q *sqlc.Queries, link sqlc.AccountLink) error {
	if link.UsedAt != nil {
		return conflict("这个链接已经用过了。如果是你自己用的，直接登录就行")
	}
	if time.Now().After(link.ExpiresAt) {
		return conflict("这个链接已经过期了，请让管理员重新发一个")
	}
	if link.UserID != nil && *link.UserID == link.CreatedBy {
		return nil
	}
	issuer, err := q.GetUser(ctx, link.CreatedBy)
	if err != nil {
		return err
	}
	if !activeAdmin(issuer) {
		return conflict("发出这个链接的人已经不是管理员了，链接随之失效。请让现在的管理员重新发一个")
	}
	return nil
}

// LookupLink 返回一个链接对应的是哪个邮箱、哪种操作，给落地页显示。
func (s *Service) LookupLink(ctx context.Context, token string) (LinkInfo, error) {
	link, err := s.q.GetLinkByToken(ctx, hashToken(token))
	if errors.Is(err, pgx.ErrNoRows) {
		return LinkInfo{}, errNoLink
	}
	if err != nil {
		return LinkInfo{}, err
	}
	if err := usable(ctx, s.q, link); err != nil {
		return LinkInfo{}, err
	}
	info := LinkInfo{Kind: link.Kind}
	if link.Kind == "invite" {
		info.Email = *link.Email
		if inviter, err := s.q.GetUser(ctx, link.CreatedBy); err == nil {
			info.InvitedBy = inviter.Name
		}
		return info, nil
	}
	user, err := s.q.GetUser(ctx, *link.UserID)
	if err != nil {
		return LinkInfo{}, err
	}
	if user.Disabled {
		return LinkInfo{}, errAccountDisabled
	}
	info.Email, info.Name = user.Email, user.Name
	return info, nil
}

// AcceptLink 使用一个账号链接：邀请链接会建一个新账号，重置链接会给已有的账号换密码。
// 成功后链接作废，返回的用户由调用方登录。
func (s *Service) AcceptLink(ctx context.Context, token, name, pass string) (sqlc.User, error) {
	if err := checkPassword(pass); err != nil {
		return sqlc.User{}, err
	}
	var user sqlc.User
	err := s.tx(ctx, func(_ pgx.Tx, q *sqlc.Queries) error {
		hash := hashToken(token)
		// 加锁的顺序和停用成员、修改密码保持一致：先锁用户，再锁链接。顺序相反的话，
		// 「有人在用重置链接」和「这个人同时被停用或者在改密码」会互相等对方放手。
		// 所以先不加锁地看一眼链接是给谁的，锁住那个用户，然后才锁链接
		peek, err := q.GetLinkByToken(ctx, hash)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNoLink
		}
		if err != nil {
			return err
		}
		var target sqlc.User
		if peek.Kind == "reset" {
			if target, err = q.LockUser(ctx, *peek.UserID); err != nil {
				return err
			}
		}
		// 锁住链接这一行：两个人同时用同一个链接时，后到的那个会看到「已经用过了」。
		// 刚才那一眼之后链接可能被撤销或者重新生成了，那样这里就找不到了
		link, err := q.LockLinkByToken(ctx, hash)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNoLink
		}
		if err != nil {
			return err
		}
		if err := usable(ctx, q, link); err != nil {
			return err
		}

		switch link.Kind {
		case "invite":
			email, name, err := checkAccount(*link.Email, name, pass)
			if err != nil {
				return err
			}
			user, err = q.CreateUser(ctx, sqlc.CreateUserParams{
				Email: email, Name: name, PasswordHash: password.Hash(pass), Role: *link.Role,
			})
			if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
				return conflict("这个邮箱已经注册过了，请直接登录")
			}
			if err != nil {
				return err
			}
		case "reset":
			// 停用时这个人的重置链接已经删掉了，生成时也会拒绝停用的账号，正常走不到这里。
			// 这是最后一道：不管链接是怎么留下来的，都不能用它给停用的账号换密码
			if target.Disabled {
				return errAccountDisabled
			}
			user, err = q.SetUserPassword(ctx, sqlc.SetUserPasswordParams{ID: target.ID, PasswordHash: password.Hash(pass)})
			if err != nil {
				return err
			}
		}
		return q.MarkLinkUsed(ctx, link.ID)
	})
	return user, err
}

// ChangePassword 让登录的人修改自己的密码。成功后他在其他设备上的登录全部失效，
// 返回的用户带着新的 session_epoch，调用方要把它记进当前会话，当前设备才不会被一起踢掉。
func (s *Service) ChangePassword(ctx context.Context, user sqlc.User, current, next string) (sqlc.User, error) {
	if err := checkPassword(next); err != nil {
		return sqlc.User{}, err
	}
	var out sqlc.User
	err := s.tx(ctx, func(_ pgx.Tx, q *sqlc.Queries) error {
		me, err := q.LockUser(ctx, user.ID)
		if err != nil {
			return err
		}
		ok, err := password.Verify(current, me.PasswordHash)
		if err != nil {
			return err
		}
		if !ok {
			return invalid("当前密码不对")
		}
		if out, err = q.SetUserPassword(ctx, sqlc.SetUserPasswordParams{ID: me.ID, PasswordHash: password.Hash(next)}); err != nil {
			return err
		}
		// 改密码常常是因为担心账号被别人动过。之前发出去的重置链接如果还留着，
		// 拿到它的人照样能进来，所以一起作废
		_, err = q.DeletePendingReset(ctx, &me.ID)
		return err
	})
	return out, err
}

// 下面两个方法只给服务器上的命令行用（zsmeet reset-link、zsmeet make-admin），不经过网页。
// 系统不发邮件，所以唯一的管理员忘了密码、或者管理员离职没有交接时，没有任何人能在网页上救回来，
// 只能由能登录服务器的人来处理。能在服务器上执行命令的人本来就拥有整个部署，这里不再校验身份。

var errNoAccount = notFound("没有这个邮箱的账号")

// RecoverResetLink 给某个邮箱的账号生成重置密码的链接。
func (s *Service) RecoverResetLink(ctx context.Context, email string) (string, time.Time, error) {
	email, err := cleanEmail(email)
	if err != nil {
		return "", time.Time{}, err
	}
	user, err := s.q.GetUserByEmail(ctx, email)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, errNoAccount
	}
	if err != nil {
		return "", time.Time{}, err
	}
	return s.issueReset(ctx, nil, user.ID)
}

// RecoverAdmin 把某个邮箱的账号设为管理员；账号被停用的话顺便启用。
func (s *Service) RecoverAdmin(ctx context.Context, email string) (sqlc.User, error) {
	email, err := cleanEmail(email)
	if err != nil {
		return sqlc.User{}, err
	}
	var out sqlc.User
	err = s.withMembersLock(ctx, func(q *sqlc.Queries) error {
		user, err := q.GetUserByEmail(ctx, email)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNoAccount
		}
		if err != nil {
			return err
		}
		if out, err = q.SetUserRole(ctx, sqlc.SetUserRoleParams{ID: user.ID, Role: "admin"}); err != nil {
			return err
		}
		if out, err = q.SetUserDisabled(ctx, sqlc.SetUserDisabledParams{ID: user.ID, Disabled: false}); err != nil {
			return err
		}
		// 下面两条和 UpdateMember 一样：重新启用时，他名下还没用的重置密码链接作废……
		if user.Disabled {
			if _, err := q.DeletePendingReset(ctx, &user.ID); err != nil {
				return err
			}
		}
		// ……重新上任时，上一任留下的链接不跟着复活
		if !activeAdmin(user) {
			return expireIssuedLinks(ctx, q, user.ID)
		}
		return nil
	})
	return out, err
}

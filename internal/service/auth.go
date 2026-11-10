package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/password"
)

// 初始化时用的 Postgres advisory lock，防止两个人同时创建第一个管理员
const setupLock = 7_368_822

// Initialized 表示是否已经创建过管理员。还没有的话网页显示初始化向导。
func (s *Service) Initialized(ctx context.Context) (bool, error) {
	n, err := s.q.CountUsers(ctx)
	return n > 0, err
}

// Setup 创建第一个管理员。只在系统里一个用户都没有的时候能用。
func (s *Service) Setup(ctx context.Context, email, name, pass string) (sqlc.User, error) {
	email, name, err := checkAccount(email, name, pass)
	if err != nil {
		return sqlc.User{}, err
	}
	var user sqlc.User
	err = s.tx(ctx, func(tx pgx.Tx, q *sqlc.Queries) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", setupLock); err != nil {
			return err
		}
		n, err := q.CountUsers(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			return conflict("系统已经初始化过了，请直接登录")
		}
		user, err = q.CreateUser(ctx, sqlc.CreateUserParams{
			Email: email, Name: name, PasswordHash: password.Hash(pass), Role: "admin",
		})
		return err
	})
	return user, err
}

// checkAccount 校验新账号的邮箱、姓名和密码，返回整理过的邮箱和姓名。
func checkAccount(email, name, pass string) (string, string, error) {
	email, err := cleanEmail(email)
	if err != nil {
		return "", "", err
	}
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 50 {
		return "", "", invalid("姓名不能为空，最长 50 个字")
	}
	if err := checkPassword(pass); err != nil {
		return "", "", err
	}
	return email, name, nil
}

var errLogin = &Error{Unauthorized, "邮箱或密码不对"}

// 登录限流：同一个邮箱 15 分钟里输错 5 次，或者同一个 IP 输错 20 次，就锁 15 分钟。
// 锁着的时候密码对了也不让进，不然锁就没有意义了。锁要等到时间才开，
// 或者在服务器上执行 zsmeet unlock，或者用重置密码的链接设了新密码。
const (
	loginWindow       = 15 * time.Minute
	maxEmailFailures  = 5
	maxClientFailures = 20
)

// loginLimit 是登录限流的一个计数：一个邮箱，或者一个 IP。
type loginLimit struct {
	key string
	max int
}

// loginKey 是一个邮箱在 login_failures 里的 key。
func loginKey(email string) string { return "email:" + email }

// Login 用邮箱和密码登录。邮箱不存在和密码不对返回同一个错误，不泄露哪些邮箱注册过。
// clientIP 是发请求的人的 IP（接口层取的），可以为空，为空时只按邮箱计数。
//
// 校验密码之前先在每个计数上占一次尝试，占不上（锁着，或者已经到上限）就直接拒绝，不算哈希。
// 判断和占位是一条语句，同时来的一大批请求也只有上限那么多个能去校验密码；
// 算哈希的时候不占着数据库连接。密码不对，占上的就算一次失败；密码对了，这个邮箱重新计数，
// 这次也不算在 IP 头上。
func (s *Service) Login(ctx context.Context, email, pass, clientIP string) (sqlc.User, error) {
	// 已经占上的次数要照常结算，所以后面不跟着请求取消（用户关掉页面也一样）
	ctx = context.WithoutCancel(ctx)
	email = strings.ToLower(strings.TrimSpace(email))
	emailKey := loginKey(email)
	// 先占 IP 的：IP 已经锁住时直接拒绝，不会为每个新邮箱都在表里留一条记录
	var limits []loginLimit
	if clientIP != "" {
		limits = append(limits, loginLimit{"ip:" + clientIP, maxClientFailures})
	}
	limits = append(limits, loginLimit{emailKey, maxEmailFailures})
	for i, l := range limits {
		_, err := s.q.ReserveLoginAttempt(ctx, sqlc.ReserveLoginAttemptParams{
			Key: l.key, WindowSecs: loginWindow.Seconds(), MaxFailures: int32(l.max),
		})
		if err == nil {
			continue
		}
		// 这次不校验了，前面已经占上的退回去
		if rerr := s.releaseLogin(ctx, limits[:i]); rerr != nil {
			return sqlc.User{}, rerr
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return sqlc.User{}, s.tooManyLogins(ctx, l.key)
		}
		return sqlc.User{}, err
	}

	failed := func() (sqlc.User, error) {
		for _, l := range limits {
			if err := s.q.LockLoginIfOverLimit(ctx, sqlc.LockLoginIfOverLimitParams{
				Key: l.key, WindowSecs: loginWindow.Seconds(), MaxFailures: int32(l.max),
			}); err != nil {
				return sqlc.User{}, err
			}
		}
		// 偶尔顺手清掉早就过期的记录，免得表越来越大
		if rand.IntN(50) == 0 {
			if err := s.q.DeleteStaleLoginFailures(ctx); err != nil {
				return sqlc.User{}, err
			}
		}
		return sqlc.User{}, errLogin
	}
	user, err := s.q.GetUserByEmail(ctx, email)
	if errors.Is(err, pgx.ErrNoRows) {
		// 邮箱不存在时也算一次哈希，让两种情况的响应时间差不多
		password.Hash(pass)
		return failed()
	}
	if err != nil {
		return sqlc.User{}, err
	}
	ok, err := password.Verify(pass, user.PasswordHash)
	if err != nil {
		return sqlc.User{}, err
	}
	if !ok {
		return failed()
	}
	// 密码对了：这个邮箱重新计数；IP 上只退回这一次，不清零：拿一个自己知道密码的账号登录一下，
	// 不能把猜别人密码的次数也清掉
	if err := s.q.ClearLoginFailures(ctx, emailKey); err != nil {
		return sqlc.User{}, err
	}
	if err := s.releaseLogin(ctx, limits[:len(limits)-1]); err != nil {
		return sqlc.User{}, err
	}
	if user.Disabled {
		return sqlc.User{}, errAccountDisabled
	}
	return user, nil
}

// releaseLogin 把这几个计数上占的一次退回去。
func (s *Service) releaseLogin(ctx context.Context, limits []loginLimit) error {
	for _, l := range limits {
		if err := s.q.ReleaseLoginAttempt(ctx, l.key); err != nil {
			return err
		}
		// 退回到 0 又没锁着的记录删掉，表不会越来越大
		if err := s.q.DeleteEmptyLoginFailure(ctx, l.key); err != nil {
			return err
		}
	}
	return nil
}

// tooManyLogins 返回「错太多次」的错误，告诉对方大概多久以后能再试。
func (s *Service) tooManyLogins(ctx context.Context, key string) error {
	minutes := 1
	at, err := s.q.LoginRetryAt(ctx, sqlc.LoginRetryAtParams{Key: key, WindowSecs: loginWindow.Seconds()})
	if err == nil {
		minutes = max(int(math.Ceil(time.Until(at).Minutes())), 1)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	return &Error{TooManyRequests, fmt.Sprintf("密码输错的次数太多，为了账号安全暂时不能登录，请 %d 分钟后再试", minutes)}
}

// UnlockLogin 清掉一个账号的登录失败次数，锁着的话马上解开；同时解开所有被锁住的 IP
// （反向代理没配好时所有人共用一个 IP，它被锁住就谁都登录不了）。返回解开了几个 IP。
// 只给服务器上的 zsmeet unlock 用：有人故意不停地输错某个账号（比如管理员）的密码，让他一直登录不了的时候。
func (s *Service) UnlockLogin(ctx context.Context, email string) (sqlc.User, int64, error) {
	email, err := cleanEmail(email)
	if err != nil {
		return sqlc.User{}, 0, err
	}
	user, err := s.q.GetUserByEmail(ctx, email)
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlc.User{}, 0, errNoAccount
	}
	if err != nil {
		return sqlc.User{}, 0, err
	}
	if err := s.q.ClearLoginFailures(ctx, loginKey(user.Email)); err != nil {
		return sqlc.User{}, 0, err
	}
	ips, err := s.q.ClearLockedIPs(ctx)
	return user, ips, err
}

// SessionUser 返回会话对应的用户。epoch 是登录时记在会话里的 users.session_epoch。
// 用户不存在、已停用，或者登录之后改过密码（epoch 对不上）时返回 Unauthorized，这个会话就不能再用了。
func (s *Service) SessionUser(ctx context.Context, id uuid.UUID, epoch int) (sqlc.User, error) {
	user, err := s.q.GetUser(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (user.Disabled || int(user.SessionEpoch) != epoch)) {
		return sqlc.User{}, &Error{Unauthorized, "登录已失效，请重新登录"}
	}
	return user, err
}

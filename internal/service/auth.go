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

// Login 校验邮箱和密码。邮箱不存在和密码不对返回同一个错误，不泄露哪些邮箱注册过。
// 登录限流：同一个邮箱 15 分钟里输错 5 次，或者同一个 IP 输错 20 次，就锁 15 分钟。
// 锁着的时候密码对了也不让进，不然锁就没有意义了。
const (
	loginWindow       = 15 * time.Minute
	maxEmailFailures  = 5
	maxClientFailures = 20
)

// Login 用邮箱和密码登录。clientIP 是发请求的人的 IP（接口层取的），可以为空。
func (s *Service) Login(ctx context.Context, email, pass, clientIP string) (sqlc.User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	emailKey := "email:" + email
	keys := []string{emailKey}
	if clientIP != "" {
		keys = append(keys, "ip:"+clientIP)
	}
	locked, err := s.q.LoginLockedUntil(ctx, keys)
	if err != nil {
		return sqlc.User{}, err
	}
	if len(locked) > 0 && locked[0] != nil {
		minutes := int(math.Ceil(time.Until(*locked[0]).Minutes()))
		return sqlc.User{}, &Error{TooManyRequests, fmt.Sprintf("密码输错的次数太多，为了账号安全暂时不能登录，请 %d 分钟后再试", max(minutes, 1))}
	}

	failed := func() (sqlc.User, error) {
		if err := s.recordLoginFailure(ctx, emailKey, clientIP); err != nil {
			return sqlc.User{}, err
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
	if user.Disabled {
		return sqlc.User{}, errAccountDisabled
	}
	// 登录成功，这个邮箱重新计数。IP 的不清：拿一个自己知道密码的账号登录一下，不能把猜别人密码的次数也清掉
	if err := s.q.ClearLoginFailures(ctx, emailKey); err != nil {
		return sqlc.User{}, err
	}
	return user, nil
}

func (s *Service) recordLoginFailure(ctx context.Context, emailKey, clientIP string) error {
	window := loginWindow.Seconds()
	if err := s.q.RecordLoginFailure(ctx, sqlc.RecordLoginFailureParams{Key: emailKey, WindowSecs: window, MaxFailures: maxEmailFailures}); err != nil {
		return err
	}
	if clientIP != "" {
		if err := s.q.RecordLoginFailure(ctx, sqlc.RecordLoginFailureParams{Key: "ip:" + clientIP, WindowSecs: window, MaxFailures: maxClientFailures}); err != nil {
			return err
		}
	}
	// 偶尔顺手清掉早就过期的记录，免得表越来越大
	if rand.IntN(50) == 0 {
		return s.q.DeleteStaleLoginFailures(ctx)
	}
	return nil
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

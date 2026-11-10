package service

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zsmeet/internal/db/sqlc"
	"zsmeet/internal/password"
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
func (s *Service) Login(ctx context.Context, email, pass string) (sqlc.User, error) {
	user, err := s.q.GetUserByEmail(ctx, strings.ToLower(strings.TrimSpace(email)))
	if errors.Is(err, pgx.ErrNoRows) {
		// 邮箱不存在时也算一次哈希，让两种情况的响应时间差不多
		password.Hash(pass)
		return sqlc.User{}, errLogin
	}
	if err != nil {
		return sqlc.User{}, err
	}
	ok, err := password.Verify(pass, user.PasswordHash)
	if err != nil {
		return sqlc.User{}, err
	}
	if !ok {
		return sqlc.User{}, errLogin
	}
	if user.Disabled {
		return sqlc.User{}, errAccountDisabled
	}
	return user, nil
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

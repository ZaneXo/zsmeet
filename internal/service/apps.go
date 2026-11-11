package service

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
)

// App 登录（目前只有桌面版），设计见 docs/desktop.md：
//
// App 不收密码。它生成 PKCE 的 code_verifier，把 S256(code_verifier) 带在地址里打开网页上的授权页；
// 用户在浏览器里登录（密码、飞书都行）、点「允许」以后，网页调用 AuthorizeApp 拿到一次性的授权码，
// 跳回 App（zsmeet://auth?code=…）。App 用授权码和 code_verifier 调用 ExchangeAppCode 换出令牌。
// 只拿到授权码、没有 code_verifier 的人（比如截获了跳转地址的别的程序）换不出令牌。
//
// 令牌形如 zsm_ 加 32 字节随机数，请求时放在 Authorization: Bearer 里，和网页的登录状态一样有效。
// 数据库里只存令牌和授权码的 SHA-256。退出、被吊销的令牌直接删掉。

const (
	// ClientDesktop 是桌面版。以后有安卓再加
	ClientDesktop = "desktop"
	// AppTokenPrefix 是 App 令牌的前缀，一眼认得出是 zsmeet 的令牌（比如被误贴到了别处）
	AppTokenPrefix = "zsm_"
	// 授权码的有效期：网页跳回 App、App 拿它换令牌，正常一两秒就完了
	appCodeTTL = 2 * time.Minute
	// 令牌的「上次使用时间」多久才记一次。每个请求都写的话，App 开着就一直在写数据库
	appTouchEvery = time.Minute
	maxDeviceName = 100
)

var (
	// S256 的 code_challenge：32 字节的 SHA-256 用 base64url（不补 =）写出来正好 43 个字符
	codeChallengeRe = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	// RFC 7636 规定的 code_verifier：43 到 128 个字符
	codeVerifierRe = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

	// 授权码不对、过期、用过了、和 code_verifier 对不上，都是这一句：不让人试出是哪一种
	errAppCode = invalid("登录没有完成：授权码无效或者已经过期。请回到 zsmeet 桌面版，重新点「在浏览器里登录」")
	// 令牌不对、被吊销了、账号停用了
	errAppToken = &Error{Unauthorized, "这台设备的登录已失效，请在 App 里重新登录"}
)

// AppToken 是一台登录着的 App。
type AppToken = sqlc.AppToken

// AuthorizeApp 在网页上点了「允许」以后调用：给登录着的 user 发一个一次性的授权码，
// 2 分钟内有效，和 code_challenge 绑定。返回授权码原文（数据库里只存哈希）。
func (s *Service) AuthorizeApp(ctx context.Context, user sqlc.User, client, deviceName, challenge string) (string, error) {
	if client != ClientDesktop {
		return "", invalid("不认识的 App 类型：%s。请确认用的是 zsmeet 桌面版", client)
	}
	deviceName, err := cleanDeviceName(deviceName)
	if err != nil {
		return "", err
	}
	if !codeChallengeRe.MatchString(challenge) {
		return "", invalid("登录链接不完整（code_challenge 不对）。请回到 zsmeet 桌面版，重新点「在浏览器里登录」")
	}
	// 32 字节随机数，猜不出来，不用另外限制换令牌的次数
	code, hash := newToken()
	err = s.tx(ctx, func(_ pgx.Tx, q *sqlc.Queries) error {
		// 顺手清掉早就过期的授权码，免得表越来越大
		if err := q.DeleteStaleAppAuthCodes(ctx); err != nil {
			return err
		}
		return q.CreateAppAuthCode(ctx, sqlc.CreateAppAuthCodeParams{
			CodeHash: hash, UserID: user.ID, Client: client, DeviceName: deviceName,
			CodeChallenge: challenge, ExpiresAt: time.Now().Add(appCodeTTL),
		})
	})
	if err != nil {
		return "", err
	}
	return code, nil
}

// cleanDeviceName 整理 App 报上来的设备名：去掉首尾空白，不能为空，最长 100 个字。
func cleanDeviceName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxDeviceName {
		return "", invalid("设备名不能为空，最长 %d 个字", maxDeviceName)
	}
	if !utf8.ValidString(name) || strings.ContainsFunc(name, unicode.IsControl) {
		return "", invalid("设备名里有不能显示的字符")
	}
	return name, nil
}

// ExchangeAppCode 用授权码和 code_verifier 换 App 令牌，返回令牌原文和它的主人。
// 授权码不管换没换成功都用掉了：拿到授权码的人不能一个个试 code_verifier。
func (s *Service) ExchangeAppCode(ctx context.Context, code, verifier string) (string, sqlc.User, error) {
	if code == "" {
		return "", sqlc.User{}, errAppCode
	}
	hash := hashToken(code)
	token, tokenHash := newAppToken()
	var (
		user     sqlc.User
		mismatch bool
	)
	err := s.tx(ctx, func(_ pgx.Tx, q *sqlc.Queries) error {
		// 加锁的顺序和改密码、停用账号一样：先锁用户，再动授权码。顺序相反的话，
		// 「正在换令牌」和「这个人同时在改密码」会互相等对方放手。所以先不加锁地看一眼授权码是谁的
		owner, err := q.GetAppAuthCodeUser(ctx, hash)
		if errors.Is(err, pgx.ErrNoRows) {
			return errAppCode
		}
		if err != nil {
			return err
		}
		if user, err = q.LockUser(ctx, owner); err != nil {
			return err
		}
		// 判断「没用过、没过期」和标记用掉在同一条语句里，同时来的两个请求只有一个能拿到
		row, err := q.UseAppAuthCode(ctx, hash)
		if errors.Is(err, pgx.ErrNoRows) {
			return errAppCode
		}
		if err != nil {
			return err
		}
		if !verifierMatches(verifier, row.CodeChallenge) {
			// 授权码照样算用掉：提交事务，再返回错误
			mismatch = true
			return nil
		}
		if user.Disabled {
			return errAppCode
		}
		_, err = q.CreateAppToken(ctx, sqlc.CreateAppTokenParams{
			UserID: user.ID, Client: row.Client, DeviceName: row.DeviceName, TokenHash: tokenHash,
		})
		return err
	})
	if err == nil && mismatch {
		err = errAppCode
	}
	if err != nil {
		return "", sqlc.User{}, err
	}
	return token, user, nil
}

// verifierMatches 检查 base64url(SHA-256(verifier)) 是不是 challenge。
func verifierMatches(verifier, challenge string) bool {
	if !codeVerifierRe.MatchString(verifier) {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(want), []byte(challenge)) == 1
}

// newAppToken 生成一个 App 令牌，返回原文和存进数据库的哈希。
func newAppToken() (string, []byte) {
	raw, _ := newToken()
	token := AppTokenPrefix + raw
	return token, hashToken(token)
}

// AppTokenUser 按请求里带的 App 令牌找出用户和令牌。令牌不对、被吊销、账号停用时返回 Unauthorized。
// 顺便记下这台设备的上次使用时间，大约一分钟才写一次。
func (s *Service) AppTokenUser(ctx context.Context, token string) (sqlc.User, AppToken, error) {
	if !strings.HasPrefix(token, AppTokenPrefix) {
		return sqlc.User{}, AppToken{}, errAppToken
	}
	row, err := s.q.GetAppTokenUser(ctx, hashToken(token))
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlc.User{}, AppToken{}, errAppToken
	}
	if err != nil {
		return sqlc.User{}, AppToken{}, err
	}
	// 停用账号时令牌已经删掉了，这里是最后一道
	if row.User.Disabled {
		return sqlc.User{}, AppToken{}, errAppToken
	}
	if row.AppToken.LastUsedAt == nil || time.Since(*row.AppToken.LastUsedAt) >= appTouchEvery {
		// 请求被取消也照样记下：这个请求确实用过这个令牌
		if err := s.q.TouchAppToken(context.WithoutCancel(ctx), row.AppToken.ID); err != nil {
			return sqlc.User{}, AppToken{}, err
		}
	}
	return row.User, row.AppToken, nil
}

// AppDevices 返回 user 登录着的 App，新登录的在前。
func (s *Service) AppDevices(ctx context.Context, user sqlc.User) ([]AppToken, error) {
	return s.q.ListAppTokens(ctx, user.ID)
}

// RevokeAppToken 让 user 的一台 App 退出登录。只能退出自己的。
func (s *Service) RevokeAppToken(ctx context.Context, user sqlc.User, id uuid.UUID) error {
	n, err := s.q.DeleteAppToken(ctx, sqlc.DeleteAppTokenParams{ID: id, UserID: user.ID})
	if err == nil && n == 0 {
		return notFound("没有这台设备，可能已经退出登录了")
	}
	return err
}

// revokeAppAccess 让一个人的 App 全部退出登录，还没换成令牌的授权码也作废。
// keep 不为 nil 时留下这一个令牌（在 App 里改密码时，留下这台 App）。
// 调用方要已经锁住了这个人（LockUser），和换令牌排队。
func revokeAppAccess(ctx context.Context, q *sqlc.Queries, userID uuid.UUID, keep *uuid.UUID) error {
	if err := q.DeleteUserAppAuthCodes(ctx, userID); err != nil {
		return err
	}
	if keep != nil {
		return q.DeleteUserAppTokensExcept(ctx, sqlc.DeleteUserAppTokensExceptParams{UserID: userID, ID: *keep})
	}
	return q.DeleteUserAppTokens(ctx, userID)
}

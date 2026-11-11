-- name: CreateAppAuthCode :exec
INSERT INTO app_auth_codes (code_hash, user_id, client, device_name, code_challenge, expires_at)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: DeleteStaleAppAuthCodes :exec
-- 过期一段时间的授权码清掉，免得表越来越大
DELETE FROM app_auth_codes WHERE expires_at < now() - interval '1 hour';

-- name: UseAppAuthCode :one
-- 用掉一个授权码：没用过、没过期才能用。判断和标记在同一条语句里，同时来的两个请求只有一个能拿到
UPDATE app_auth_codes SET used_at = now()
WHERE code_hash = $1 AND used_at IS NULL AND expires_at > now()
RETURNING *;

-- name: CreateAppToken :one
INSERT INTO app_tokens (user_id, client, device_name, token_hash)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetAppTokenUser :one
-- 按令牌的哈希找出令牌和它的主人
SELECT sqlc.embed(t), sqlc.embed(u)
FROM app_tokens t JOIN users u ON u.id = t.user_id
WHERE t.token_hash = $1;

-- name: TouchAppToken :exec
-- 记下最近一次使用的时间。上一次记的还不到一分钟就不改，同时来的一批请求只写一次
UPDATE app_tokens SET last_used_at = now()
WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute');

-- name: ListAppTokens :many
SELECT * FROM app_tokens WHERE user_id = $1 ORDER BY created_at DESC;

-- name: DeleteAppToken :execrows
DELETE FROM app_tokens WHERE id = $1 AND user_id = $2;

-- name: DeleteUserAppTokens :exec
-- 这个人的 App 令牌全部作废（改密码、重置密码、停用账号时）
DELETE FROM app_tokens WHERE user_id = $1;

-- name: DeleteUserAppTokensExcept :exec
-- 同上，但留下一个：在 App 里改密码时，留下这台 App 的令牌
DELETE FROM app_tokens WHERE user_id = $1 AND id <> $2;

-- name: GetAppAuthCodeUser :one
-- 授权码是谁的（不加锁，只是看一眼，好先锁住这个人）
SELECT user_id FROM app_auth_codes WHERE code_hash = $1;

-- name: DeleteUserAppAuthCodes :exec
-- 这个人还没换成令牌的授权码全部作废
DELETE FROM app_auth_codes WHERE user_id = $1;

-- name: CountUsers :one
SELECT count(*) FROM users;

-- name: CreateUser :one
INSERT INTO users (email, name, password_hash, role)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetUser :one
SELECT * FROM users WHERE id = $1;

-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = $1;

-- name: GetSetting :one
SELECT value FROM settings WHERE key = $1;

-- name: PutSetting :exec
INSERT INTO settings (key, value) VALUES ($1, $2)
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now();

-- name: LoginLockedUntil :many
-- 这几个 key 里还锁着的，锁到什么时候（最晚的排第一）
SELECT locked_until FROM login_failures
WHERE key = ANY(sqlc.arg('keys')::text[]) AND locked_until > now()
ORDER BY locked_until DESC;

-- name: RecordLoginFailure :exec
-- 记一次登录失败。上一次计数已经超出时间窗口就从 1 重新数；数到上限就锁住一段时间
INSERT INTO login_failures (key, failures, window_start) VALUES (sqlc.arg('key'), 1, now())
ON CONFLICT (key) DO UPDATE SET
    failures     = CASE WHEN login_failures.window_start < now() - make_interval(secs => sqlc.arg('window_secs')::float8)
                        THEN 1 ELSE login_failures.failures + 1 END,
    window_start = CASE WHEN login_failures.window_start < now() - make_interval(secs => sqlc.arg('window_secs')::float8)
                        THEN now() ELSE login_failures.window_start END,
    locked_until = CASE WHEN login_failures.window_start >= now() - make_interval(secs => sqlc.arg('window_secs')::float8)
                         AND login_failures.failures + 1 >= sqlc.arg('max_failures')::int
                        THEN now() + make_interval(secs => sqlc.arg('window_secs')::float8)
                        ELSE login_failures.locked_until END;

-- name: ClearLoginFailures :exec
DELETE FROM login_failures WHERE key = $1;

-- name: DeleteStaleLoginFailures :exec
-- 一天前就不再计数、也没有锁着的记录清掉
DELETE FROM login_failures
WHERE window_start < now() - interval '1 day' AND (locked_until IS NULL OR locked_until < now());

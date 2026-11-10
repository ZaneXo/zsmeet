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

-- name: ReserveLoginAttempt :one
-- 校验密码之前先占一次尝试：在时间窗口里把计数加一（上一次计数已经超出窗口就从 1 重新数）。
-- 锁着、或者窗口里的次数已经到上限（包括正在校验、还没出结果的）时什么都不改，也不返回行，这次登录直接拒绝。
-- 判断和加一在同一条语句里，同时来的请求在这一行上排队，不会都看到「还没到上限」
INSERT INTO login_failures (key, failures, window_start) VALUES (sqlc.arg('key'), 1, now())
ON CONFLICT (key) DO UPDATE SET
    failures     = CASE WHEN login_failures.window_start < now() - make_interval(secs => sqlc.arg('window_secs')::float8)
                        THEN 1 ELSE login_failures.failures + 1 END,
    window_start = CASE WHEN login_failures.window_start < now() - make_interval(secs => sqlc.arg('window_secs')::float8)
                        THEN now() ELSE login_failures.window_start END,
    locked_until = CASE WHEN login_failures.window_start < now() - make_interval(secs => sqlc.arg('window_secs')::float8)
                        THEN NULL ELSE login_failures.locked_until END
WHERE (login_failures.locked_until IS NULL OR login_failures.locked_until <= now())
  AND (login_failures.window_start < now() - make_interval(secs => sqlc.arg('window_secs')::float8)
       OR login_failures.failures < sqlc.arg('max_failures')::int)
RETURNING failures;

-- name: LockLoginIfOverLimit :exec
-- 密码不对：占上的那次就算一次失败。窗口里的次数到了上限就锁住一段时间
UPDATE login_failures
SET locked_until = now() + make_interval(secs => sqlc.arg('window_secs')::float8)
WHERE key = sqlc.arg('key')
  AND failures >= sqlc.arg('max_failures')::int
  AND window_start >= now() - make_interval(secs => sqlc.arg('window_secs')::float8)
  AND (locked_until IS NULL OR locked_until <= now());

-- name: ReleaseLoginAttempt :exec
-- 把占上的一次退回去（密码对了，或者这次登录没真正校验）
UPDATE login_failures SET failures = greatest(failures - 1, 0) WHERE key = $1;

-- name: DeleteEmptyLoginFailure :exec
-- 退回到 0、又没锁着的记录直接删掉，表不会越来越大
DELETE FROM login_failures WHERE key = $1 AND failures = 0 AND (locked_until IS NULL OR locked_until <= now());

-- name: LoginRetryAt :one
-- 被拒绝的登录什么时候能再试：锁着的看锁到什么时候，还没锁（有校验还没出结果）的看窗口什么时候结束
SELECT greatest(locked_until, window_start + make_interval(secs => sqlc.arg('window_secs')::float8))::timestamptz
FROM login_failures WHERE key = sqlc.arg('key');

-- name: ClearLoginFailures :exec
DELETE FROM login_failures WHERE key = $1;

-- name: ClearLockedIPs :execrows
-- 解开所有被锁住的 IP（zsmeet unlock 用：反向代理没配好时所有人共用一个 IP，被锁住就谁都登录不了）
DELETE FROM login_failures WHERE key LIKE 'ip:%' AND locked_until > now();

-- name: DeleteStaleLoginFailures :exec
-- 一天前就不再计数、也没有锁着的记录清掉
DELETE FROM login_failures
WHERE window_start < now() - interval '1 day' AND (locked_until IS NULL OR locked_until < now());

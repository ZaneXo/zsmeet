-- name: ListUsers :many
SELECT * FROM users ORDER BY created_at, email;

-- name: LockUser :one
SELECT * FROM users WHERE id = $1 FOR UPDATE;

-- name: SetUserRole :one
UPDATE users SET role = $2 WHERE id = $1 RETURNING *;

-- name: SetUserDisabled :one
-- 停用状态每变一次（停用、重新启用），这个人之前的登录全部作废（session_epoch 加一）。
-- 只靠 disabled 挡着的话，以后一重新启用，停用前的登录（比如丢了的那台电脑上的）就又能用了
UPDATE users
SET disabled = $2,
    session_epoch = session_epoch + CASE WHEN disabled <> $2 THEN 1 ELSE 0 END
WHERE id = $1
RETURNING *;

-- name: SetUserPassword :one
-- 换密码的同时让这个人在别处的登录失效
UPDATE users SET password_hash = $2, session_epoch = session_epoch + 1
WHERE id = $1
RETURNING *;

-- name: UpsertInvite :one
-- 这个邮箱已经有没用过的邀请时，覆盖它：旧链接作废，有效期重新算
INSERT INTO account_links (kind, token_hash, email, role, created_by, expires_at)
VALUES ('invite', $1, $2, $3, $4, $5)
ON CONFLICT (email) WHERE kind = 'invite' AND used_at IS NULL
DO UPDATE SET token_hash = EXCLUDED.token_hash, role = EXCLUDED.role,
              created_by = EXCLUDED.created_by, created_at = now(), expires_at = EXCLUDED.expires_at
RETURNING *;

-- name: RenewInvite :one
-- 重新生成链接：换一个 token，有效期重新算
UPDATE account_links
SET token_hash = $2, created_by = $3, created_at = now(), expires_at = $4
WHERE id = $1 AND kind = 'invite' AND used_at IS NULL
RETURNING *;

-- name: DeleteInvite :execrows
DELETE FROM account_links WHERE id = $1 AND kind = 'invite' AND used_at IS NULL;

-- name: ListPendingInvites :many
-- issuer_is_admin：发出邀请的人现在是不是还在任的管理员。不是的话这个邀请已经用不了了
SELECT sqlc.embed(l), u.name AS invited_by, (u.role = 'admin' AND NOT u.disabled)::boolean AS issuer_is_admin
FROM account_links l JOIN users u ON u.id = l.created_by
WHERE l.kind = 'invite' AND l.used_at IS NULL
ORDER BY l.created_at DESC;

-- name: UpsertReset :one
INSERT INTO account_links (kind, token_hash, user_id, created_by, expires_at)
VALUES ('reset', $1, $2, $3, $4)
ON CONFLICT (user_id) WHERE kind = 'reset' AND used_at IS NULL
DO UPDATE SET token_hash = EXCLUDED.token_hash,
              created_by = EXCLUDED.created_by, created_at = now(), expires_at = EXCLUDED.expires_at
RETURNING *;

-- name: GetLinkByToken :one
SELECT * FROM account_links WHERE token_hash = $1;

-- name: LockLinkByToken :one
SELECT * FROM account_links WHERE token_hash = $1 FOR UPDATE;

-- name: MarkLinkUsed :exec
UPDATE account_links SET used_at = now() WHERE id = $1;

-- name: ListPendingResets :many
-- 还能用的重置密码链接：给成员列表显示「谁有一个没用过的重置链接」。
-- from_server：在服务器上用命令给账号本人生成的（created_by 就是本人），不是哪位管理员在网页上生成的
SELECT l.user_id, l.expires_at, u.name AS created_by_name, (l.created_by = l.user_id)::boolean AS from_server
FROM account_links l JOIN users u ON u.id = l.created_by
WHERE l.kind = 'reset' AND l.used_at IS NULL AND l.expires_at > now();

-- name: DeletePendingReset :execrows
-- 作废某个人还没用过的重置密码链接
DELETE FROM account_links WHERE kind = 'reset' AND user_id = $1 AND used_at IS NULL;

-- name: ExpireInvitesCreatedBy :exec
-- 某个人不再是管理员（被降级或停用）时，他发出去还没用的邀请立刻过期。
-- 行留着不删：别的管理员在列表里还看得到，重新生成一个链接就能接着用
UPDATE account_links SET expires_at = now()
WHERE created_by = $1 AND kind = 'invite' AND used_at IS NULL AND expires_at > now();

-- name: DeleteResetsCreatedBy :exec
-- ……他给别人生成的重置密码链接直接作废。
-- 在服务器上用命令给账号本人生成的那种不算：它的效力来自能登录服务器，和角色无关
DELETE FROM account_links
WHERE created_by = $1 AND kind = 'reset' AND used_at IS NULL AND user_id <> created_by;

-- name: GetIdentity :one
SELECT * FROM user_identities WHERE provider = $1 AND subject = $2;

-- name: GetUserIdentity :one
-- 这个人关联的第三方账号（每种最多一个）
SELECT * FROM user_identities WHERE provider = $1 AND user_id = $2;

-- name: CreateIdentity :exec
INSERT INTO user_identities (provider, subject, user_id) VALUES ($1, $2, $3);

-- name: DeleteUserIdentity :execrows
DELETE FROM user_identities WHERE provider = $1 AND user_id = $2;

-- name: ListIdentityUsers :many
-- 关联了这种第三方账号的人，成员列表上显示
SELECT user_id FROM user_identities WHERE provider = $1;

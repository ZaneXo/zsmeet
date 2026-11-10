-- +goose Up

-- 改密码、重置密码时加一。会话里记着登录时的值，对不上就算没登录，
-- 所以改了密码以后，这个人在其他设备上的登录全部失效。
ALTER TABLE users ADD COLUMN session_epoch integer NOT NULL DEFAULT 0;

-- 一次性的账号链接：邀请同事加入（invite），或者给忘了密码的人重置密码（reset）。
-- 系统不发邮件，链接由管理员生成后自己发给对方。
CREATE TABLE account_links (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind       text NOT NULL CHECK (kind IN ('invite', 'reset')),
    -- 链接里那串随机数的 SHA-256。不存原文：数据库泄露了也没法拿去用
    token_hash bytea NOT NULL UNIQUE,
    -- 邀请：被邀请人的邮箱（小写）和加入后的角色
    email      text,
    role       text CHECK (role IN ('admin', 'member')),
    -- 重置：给谁重置
    user_id    uuid REFERENCES users (id) ON DELETE CASCADE,
    created_by uuid NOT NULL REFERENCES users (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    -- 用过的链接留着不删，对方再点开时能告诉他「已经用过了」
    used_at    timestamptz,
    CHECK ((kind = 'invite' AND email IS NOT NULL AND role IS NOT NULL AND user_id IS NULL)
        OR (kind = 'reset' AND user_id IS NOT NULL AND email IS NULL AND role IS NULL))
);

-- 同一个邮箱同时只有一个没用过的邀请，同一个人同时只有一个没用过的重置链接。
-- 重新生成时覆盖原来那一行，旧链接随即失效。
CREATE UNIQUE INDEX account_links_pending_invite ON account_links (email)
    WHERE kind = 'invite' AND used_at IS NULL;
CREATE UNIQUE INDEX account_links_pending_reset ON account_links (user_id)
    WHERE kind = 'reset' AND used_at IS NULL;

-- +goose Down
DROP TABLE account_links;
ALTER TABLE users DROP COLUMN session_epoch;

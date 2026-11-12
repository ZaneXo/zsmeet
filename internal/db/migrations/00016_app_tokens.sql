-- +goose Up

-- App 令牌：桌面版等 App 登录以后拿到的凭证，请求时放在 Authorization: Bearer 里。
-- 令牌形如 zsm_ 加 32 字节随机数，数据库里只存它的 SHA-256。退出、被吊销时直接删掉这一行
CREATE TABLE app_tokens (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    client       text NOT NULL CHECK (client IN ('desktop')),
    -- App 自己报的设备名（电脑名），账号页上显示，让人认得出是哪一台
    device_name  text NOT NULL,
    token_hash   bytea NOT NULL UNIQUE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    -- 最近一次用它访问的时间。大约一分钟才写一次，不是每个请求都写
    last_used_at timestamptz
);
CREATE INDEX app_tokens_user_idx ON app_tokens (user_id);

-- App 登录用的一次性授权码：网页上点了「允许」以后发给 App，App 拿它和 PKCE 的 code_verifier 换令牌。
-- 只存授权码的 SHA-256；2 分钟内有效，只能用一次
CREATE TABLE app_auth_codes (
    code_hash      bytea PRIMARY KEY,
    user_id        uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    client         text NOT NULL,
    device_name    text NOT NULL,
    -- base64url(SHA-256(code_verifier))，只支持 S256
    code_challenge text NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    expires_at     timestamptz NOT NULL,
    used_at        timestamptz
);
CREATE INDEX app_auth_codes_expires_idx ON app_auth_codes (expires_at);

-- 会议来源多一种：桌面版录的电脑声音和麦克风
ALTER TABLE meetings DROP CONSTRAINT meetings_source_check;
ALTER TABLE meetings ADD CONSTRAINT meetings_source_check CHECK (source IN ('record', 'upload', 'feishu', 'desktop'));

-- +goose Down
ALTER TABLE meetings DROP CONSTRAINT meetings_source_check;
-- 回滚以后没有 desktop 这种来源了：桌面版录的会议算成录音，不然加不回旧的约束
UPDATE meetings SET source = 'record' WHERE source = 'desktop';
ALTER TABLE meetings ADD CONSTRAINT meetings_source_check CHECK (source IN ('record', 'upload', 'feishu'));
DROP TABLE app_auth_codes;
DROP TABLE app_tokens;

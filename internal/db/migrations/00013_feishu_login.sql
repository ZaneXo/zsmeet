-- +goose Up

-- 用第三方账号登录时，第三方账号和 zsmeet 账号的对应关系。目前只有飞书：
-- subject 是飞书的 union_id（同一个企业里不随应用变），拿不到时用 open_id。
-- 第一次用飞书登录、按邮箱找到成员时记下来，以后按它找人，成员在 zsmeet 或者飞书里改了邮箱也不受影响。
-- 一个 zsmeet 账号只关联一个飞书账号：飞书那边有人被改成了同一个邮箱，也接管不了已经关联过的账号
CREATE TABLE user_identities (
    provider   text NOT NULL CHECK (provider IN ('feishu')),
    subject    text NOT NULL,
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, subject)
);
CREATE UNIQUE INDEX user_identities_user ON user_identities (provider, user_id);

-- +goose Down
DROP TABLE user_identities;

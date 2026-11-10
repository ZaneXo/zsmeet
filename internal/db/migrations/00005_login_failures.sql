-- +goose Up

-- 登录失败的次数，按邮箱（key = "email:…"）和来源 IP（key = "ip:…"）各记一份。
-- 一段时间里错太多次就暂时锁住，挡住猜密码。
CREATE TABLE login_failures (
    key          text PRIMARY KEY,
    failures     integer NOT NULL,
    window_start timestamptz NOT NULL,
    locked_until timestamptz
);

-- +goose Down
DROP TABLE login_failures;

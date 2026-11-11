-- +goose Up

-- 群机器人：钉钉、企业微信、Slack 群里加的自定义机器人（Webhook）。管理员在设置里加，纪要可以发到这些群里
CREATE TABLE webhooks (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    platform    text NOT NULL CHECK (platform IN ('dingtalk', 'wecom', 'slack')),
    name        text NOT NULL,
    -- Webhook 地址，加密后保存：拿到地址的人就能往群里发消息
    url         text NOT NULL,
    -- 网页上显示的地址：密钥部分只留最后几位，认得出是哪一个就够了
    url_masked  text NOT NULL,
    -- 钉钉「加签」的密钥（SEC 开头），加密后保存；没开加签的为空
    secret      text NOT NULL DEFAULT '',
    -- 纪要第一次生成后自动发到这个群（只发团队里所有人都能看的会议）
    auto_push   boolean NOT NULL DEFAULT false,
    created_by  uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- 发到群机器人的记录。每发一次一条，后台任务照着它去做
CREATE TABLE webhook_sends (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    meeting_id      uuid NOT NULL REFERENCES meetings (id) ON DELETE CASCADE,
    -- 群机器人删掉以后记录还留着，名字和平台记的是发送时的
    webhook_id      uuid REFERENCES webhooks (id) ON DELETE SET NULL,
    webhook_name    text NOT NULL,
    platform        text NOT NULL,
    -- 发的是第几版纪要
    minutes_version integer NOT NULL,
    status          text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'done', 'failed')),
    error           text NOT NULL DEFAULT '',
    -- 谁发的；纪要生成后自动发的为空
    requested_by    uuid REFERENCES users (id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    finished_at     timestamptz
);
CREATE INDEX webhook_sends_meeting_idx ON webhook_sends (meeting_id, created_at DESC);

-- +goose Down
DROP TABLE webhook_sends;
DROP TABLE webhooks;

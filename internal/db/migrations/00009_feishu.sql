-- +goose Up

-- 发到飞书的记录：推到群里的纪要卡片、导入的飞书文档。每发一次一条，后台任务照着它去做
CREATE TABLE feishu_shares (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    meeting_id      uuid NOT NULL REFERENCES meetings (id) ON DELETE CASCADE,
    -- 发到哪个群；为空表示只导入成文档，不发群
    chat_id         text NOT NULL DEFAULT '',
    chat_name       text NOT NULL DEFAULT '',
    -- 同时导入成飞书文档
    with_doc        boolean NOT NULL DEFAULT false,
    -- 发的是第几版纪要
    minutes_version integer NOT NULL,
    status          text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'done', 'failed')),
    error           text NOT NULL DEFAULT '',
    -- 导入文档的任务号：导入要等飞书处理一会儿，中途出错重试时接着等这个任务，不再导入一遍
    import_ticket   text NOT NULL DEFAULT '',
    doc_token       text NOT NULL DEFAULT '',
    doc_url         text NOT NULL DEFAULT '',
    message_id      text NOT NULL DEFAULT '',
    -- 谁发的；纪要生成后自动发的为空
    requested_by    uuid REFERENCES users (id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    finished_at     timestamptz
);
CREATE INDEX feishu_shares_meeting_idx ON feishu_shares (meeting_id, created_at DESC);

-- 自动导入的飞书会议录制。飞书的会议 ID 是主键：同一场会议只导入一次（飞书可能重复推送同一个事件）。
-- 只记组织者是 zsmeet 成员的会议，别人的会议连主题都不存
CREATE TABLE feishu_imports (
    feishu_meeting_id text PRIMARY KEY,
    topic             text NOT NULL DEFAULT '',
    -- waiting：等录制生成；imported：已经变成 zsmeet 的会议；skipped：没有录制，不导入；failed：出错了
    status            text NOT NULL DEFAULT 'waiting' CHECK (status IN ('waiting', 'imported', 'skipped', 'failed')),
    detail            text NOT NULL DEFAULT '',
    owner_id          uuid NOT NULL REFERENCES users (id),
    meeting_id        uuid REFERENCES meetings (id) ON DELETE SET NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX feishu_imports_created_idx ON feishu_imports (created_at DESC);

-- 飞书长连接现在的状态：worker 写，设置页面读。只有一行
CREATE TABLE feishu_link (
    id            boolean PRIMARY KEY DEFAULT true CHECK (id),
    state         text NOT NULL DEFAULT 'off' CHECK (state IN ('off', 'connecting', 'connected', 'error')),
    detail        text NOT NULL DEFAULT '',
    updated_at    timestamptz NOT NULL DEFAULT now(),
    -- 最近一次收到会议结束事件的时间和处理结果
    last_event_at timestamptz,
    last_event    text NOT NULL DEFAULT ''
);

-- +goose Down
DROP TABLE feishu_link;
DROP TABLE feishu_imports;
DROP TABLE feishu_shares;

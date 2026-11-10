-- +goose Up

CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email         text NOT NULL UNIQUE,
    name          text NOT NULL,
    password_hash text NOT NULL,
    role          text NOT NULL DEFAULT 'member' CHECK (role IN ('admin', 'member')),
    disabled      boolean NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- scs 会话（pgxstore 要求的表结构）
CREATE TABLE sessions (
    token  text PRIMARY KEY,
    data   bytea NOT NULL,
    expiry timestamptz NOT NULL
);
CREATE INDEX sessions_expiry_idx ON sessions (expiry);

-- 团队级设置：大模型、识别、飞书、保留策略……value 里的密钥字段是加密后的
CREATE TABLE settings (
    key        text PRIMARY KEY,
    value      jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE meetings (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id         uuid NOT NULL REFERENCES users (id),
    title            text NOT NULL,
    source           text NOT NULL CHECK (source IN ('record', 'upload', 'feishu')),
    -- uploading: 等分片；queued: 已入队；processing: 流水线进行中；done / failed
    status           text NOT NULL DEFAULT 'uploading'
                     CHECK (status IN ('uploading', 'queued', 'processing', 'done', 'failed')),
    stage            text NOT NULL DEFAULT '',
    progress         real NOT NULL DEFAULT 0,
    error            text NOT NULL DEFAULT '',
    -- 每次入队换一个 run_id。worker 的所有写入都带着它，被新一轮顶替的旧任务写不进去
    run_id           uuid,
    -- 这一轮是谁触发的（重新生成纪要时记为新版本的作者）
    run_by           uuid REFERENCES users (id),
    -- worker 处理期间定时刷新；长时间没刷新说明 worker 挂了，由巡检任务重新入队
    heartbeat_at     timestamptz,
    -- 因为 worker 异常退出而被重新入队的次数，超过上限就判失败
    requeues         integer NOT NULL DEFAULT 0,
    template         text NOT NULL DEFAULT 'general',
    hotwords         text NOT NULL DEFAULT '',
    duration_sec     integer NOT NULL DEFAULT 0,
    audio_key        text NOT NULL DEFAULT '',
    audio_deleted_at timestamptz,
    visibility       text NOT NULL DEFAULT 'team' CHECK (visibility IN ('private', 'members', 'team')),
    current_version  integer NOT NULL DEFAULT 0,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX meetings_created_idx ON meetings (created_at DESC);
CREATE INDEX meetings_stalled_idx ON meetings (heartbeat_at) WHERE status = 'processing';

-- 标题 + 当前纪要 + 转写的分词结果，由程序写入（中文分词在 Go 里做）
CREATE TABLE meeting_search (
    meeting_id uuid PRIMARY KEY REFERENCES meetings (id) ON DELETE CASCADE,
    doc        tsvector NOT NULL
);
CREATE INDEX meeting_search_doc_idx ON meeting_search USING gin (doc);

-- 已收到的上传分片，用来判断是否收齐、支持断点续传
CREATE TABLE upload_chunks (
    meeting_id uuid NOT NULL REFERENCES meetings (id) ON DELETE CASCADE,
    seq        integer NOT NULL,
    size       bigint NOT NULL,
    PRIMARY KEY (meeting_id, seq)
);

CREATE TABLE transcripts (
    meeting_id uuid PRIMARY KEY REFERENCES meetings (id) ON DELETE CASCADE,
    -- [{"start":0.0,"end":3.2,"speaker":0,"text":"…"}]
    segments   jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- 纪要的每一次变化都是新版本，只增不改
CREATE TABLE minutes_versions (
    meeting_id uuid NOT NULL REFERENCES meetings (id) ON DELETE CASCADE,
    version    integer NOT NULL,
    content    text NOT NULL,
    source     text NOT NULL CHECK (source IN ('auto', 'edit', 'regenerate')),
    template   text NOT NULL DEFAULT '',
    author_id  uuid REFERENCES users (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (meeting_id, version)
);

-- +goose Down
DROP TABLE minutes_versions;
DROP TABLE transcripts;
DROP TABLE upload_chunks;
DROP TABLE meeting_search;
DROP TABLE meetings;
DROP TABLE settings;
DROP TABLE sessions;
DROP TABLE users;

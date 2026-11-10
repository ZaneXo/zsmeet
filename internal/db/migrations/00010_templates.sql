-- +goose Up

-- 管理员自己定义的纪要模板（内置的四种写在代码里）。会议里记的是模板的 ID：内置的是英文单词，自定义的是这里的 UUID
CREATE TABLE minutes_templates (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL,
    -- 告诉大模型纪要要有哪几部分、每部分写什么
    guide      text NOT NULL,
    created_by uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE minutes_templates;

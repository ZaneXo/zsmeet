-- +goose Up

-- 这场会议指定用的识别语言：'' 是跟随团队默认设置，'auto' 是自动识别多语种与混合语音，其余是固定某种语言（'zh'、'en'、'yue'、'ja'、'ko'）
ALTER TABLE meetings
    ADD COLUMN language text NOT NULL DEFAULT '' CHECK (language IN ('', 'auto', 'zh', 'en', 'yue', 'ja', 'ko'));

-- +goose Down
ALTER TABLE meetings
    DROP COLUMN language;

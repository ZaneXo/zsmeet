-- +goose Up

-- 这场会议的转写实际用了哪种识别方式：'' 是还没转写过（或升级前的会议），'local' 是本机 SenseVoice，'deepseek' 是 DeepSeek 在线识别。
-- asr_warning 是在线识别整场没用上、退回本机识别时给用户看的说明（比如 API Key 不对），正常时为空
ALTER TABLE meetings
    ADD COLUMN asr_engine text NOT NULL DEFAULT '' CHECK (asr_engine IN ('', 'local', 'deepseek')),
    ADD COLUMN asr_warning text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE meetings
    DROP COLUMN asr_warning,
    DROP COLUMN asr_engine;

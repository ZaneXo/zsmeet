-- +goose Up

-- 录音过程中边录边识出的实时字幕。和正式的 transcripts 分开存：
-- 录音结束后后台仍按完整流程转码、区分说话人、认声纹、校对并生成纪要，正式转写存好时清掉这一行。
CREATE TABLE live_transcripts (
    meeting_id    uuid PRIMARY KEY REFERENCES meetings(id) ON DELETE CASCADE,
    -- 已经连续处理到第几片（不含，即已处理 0..chunks-1）
    chunks        int NOT NULL DEFAULT 0,
    -- 已经定稿的音频位置（秒）：结束时间早于它的句子不再重算
    finalized_sec double precision NOT NULL DEFAULT 0,
    -- 已经定稿的句子（transcript.Segment 数组）
    finalized     jsonb NOT NULL DEFAULT '[]',
    -- 当前全部可见句子（定稿句子 + 末尾可能跨分片的暂定句，transcript.Segment 数组）
    segments      jsonb NOT NULL DEFAULT '[]',
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS live_transcripts;

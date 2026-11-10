-- +goose Up

-- 要求后台从录音重新识别（用新的识别模型）。旧的转写在新的存好之前一直保留，
-- 重新识别失败时纪要和转写都还在；标记存在会议上，worker 挂掉被巡检重新入队时也不会丢
ALTER TABLE meetings ADD COLUMN retranscribe boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE meetings DROP COLUMN retranscribe;

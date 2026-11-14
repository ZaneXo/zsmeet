-- +goose Up

-- 这场会议有几个人说话。0 是不知道，按声音的相似度自动区分；1 到 30 是用户填的人数，区分说话人时就分成这么多人。
-- 远场录音（一台电脑放在会议桌上）按相似度分常常把一个人拆成好几个，声音相近的人又可能被并成一个，知道人数时直接按人数分最准
ALTER TABLE meetings ADD COLUMN speaker_count integer NOT NULL DEFAULT 0 CHECK (speaker_count BETWEEN 0 AND 30);

-- +goose Down
ALTER TABLE meetings DROP COLUMN speaker_count;

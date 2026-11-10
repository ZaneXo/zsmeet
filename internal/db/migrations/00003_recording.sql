-- +goose Up

-- 最后一次收到上传分片的时间。网页录音时用它判断录音还在不在进行：
-- 一直有新的片传上来就是在录；很久没有新片，说明录音的页面关掉了、断网了，或者暂停着
ALTER TABLE meetings ADD COLUMN last_chunk_at timestamptz;

-- 录音时点「标记重点」的时间点（录音开始后的秒数，不算暂停的时间），写纪要时交给大模型
ALTER TABLE meetings ADD COLUMN marks real[] NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE meetings DROP COLUMN marks;
ALTER TABLE meetings DROP COLUMN last_chunk_at;

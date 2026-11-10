-- +goose Up

-- 删除会议先放进回收站：30 天内可以恢复，过期后由后台连同录音文件一起彻底删掉。
-- 放进回收站的会议在列表、搜索、详情里都当作不存在。
ALTER TABLE meetings ADD COLUMN deleted_at timestamptz;
ALTER TABLE meetings ADD COLUMN deleted_by uuid REFERENCES users (id);
CREATE INDEX meetings_trash_idx ON meetings (deleted_at) WHERE deleted_at IS NOT NULL;

-- +goose Down
DROP INDEX meetings_trash_idx;
ALTER TABLE meetings DROP COLUMN deleted_by;
ALTER TABLE meetings DROP COLUMN deleted_at;

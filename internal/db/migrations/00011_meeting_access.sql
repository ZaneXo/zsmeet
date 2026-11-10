-- +goose Up

-- 会议的可见范围（meetings.visibility）：
--   team     团队里所有人能看（默认）
--   members  只有创建者、管理员和下面这张表里的人能看
--   private  只有创建者和管理员能看
-- 这张表里 can_edit 为 true 的人还能编辑（改标题、编辑纪要、重新生成、给说话人改名、发到飞书……），
-- 可见范围是 team 或 members 时生效，private 时不生效。删除会议、改可见范围只有创建者和管理员能做
CREATE TABLE meeting_members (
    meeting_id uuid NOT NULL REFERENCES meetings (id) ON DELETE CASCADE,
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    can_edit   boolean NOT NULL DEFAULT false,
    added_by   uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (meeting_id, user_id)
);

-- 列会议时按「我在不在这场会议的成员里」过滤
CREATE INDEX meeting_members_user ON meeting_members (user_id);

-- +goose Down
DROP TABLE meeting_members;

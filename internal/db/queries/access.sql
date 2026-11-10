-- name: MeetingMember :one
-- 这个人是不是这场会议单独指定的成员、能不能编辑。不是的话没有结果
SELECT can_edit FROM meeting_members WHERE meeting_id = @meeting_id AND user_id = @user_id;

-- name: ListMeetingMembers :many
SELECT mm.user_id, mm.can_edit, u.name, u.email, u.disabled
FROM meeting_members mm JOIN users u ON u.id = mm.user_id
WHERE mm.meeting_id = @meeting_id
ORDER BY mm.created_at, u.name;

-- name: SetMeetingVisibility :exec
UPDATE meetings SET visibility = @visibility, updated_at = now() WHERE id = @id;

-- name: ReplaceMeetingMembers :exec
-- 把这场会议单独指定的成员换成传进来的这些：不在里面的删掉，已经在的更新能不能编辑
WITH input AS (
    SELECT unnest(@user_ids::uuid[]) AS user_id, unnest(@can_edit::bool[]) AS can_edit
), removed AS (
    DELETE FROM meeting_members mm WHERE mm.meeting_id = @meeting_id AND NOT (mm.user_id = ANY (@user_ids::uuid[]))
)
INSERT INTO meeting_members (meeting_id, user_id, can_edit, added_by)
SELECT @meeting_id, i.user_id, i.can_edit, @added_by FROM input i
ON CONFLICT (meeting_id, user_id) DO UPDATE SET can_edit = excluded.can_edit;

-- name: ActiveUsers :many
-- 选会议成员时列出的人：没停用的
SELECT id, name, email FROM users WHERE NOT disabled ORDER BY name, email;

-- name: UsersByIDs :many
SELECT id, disabled FROM users WHERE id = ANY (@ids::uuid[]);

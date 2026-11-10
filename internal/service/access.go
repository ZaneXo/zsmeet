package service

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
)

// 会议的可见范围
const (
	// 团队里所有人能看（默认）
	VisibilityTeam = "team"
	// 只有创建者、管理员和单独指定的人能看
	VisibilityMembers = "members"
	// 只有创建者和管理员能看
	VisibilityPrivate = "private"
)

// 一场会议最多单独指定多少人
const maxMeetingMembers = 200

// Access 是一个人对一场会议能做什么。管理员和创建者什么都能做。
type Access struct {
	// 看会议、纪要、转写，听录音，导出
	View bool
	// 改标题、编辑纪要、重新生成、重试、重新转写、给说话人改名、发到飞书
	Edit bool
	// 删除、恢复、彻底删除，改谁能看、谁能编辑；网页录音、上传文件也只有创建者（和管理员）能往里传
	Manage bool
}

var fullAccess = Access{View: true, Edit: true, Manage: true}

// accessFor 算出 user 对会议 m 的权限。isMember 表示 user 是这场会议单独指定的人，canEdit 表示指定他能编辑。
func accessFor(user sqlc.User, m sqlc.Meeting, isMember, canEdit bool) Access {
	if user.Role == "admin" || m.OwnerID == user.ID {
		return fullAccess
	}
	switch m.Visibility {
	case VisibilityTeam:
		return Access{View: true, Edit: isMember && canEdit}
	case VisibilityMembers:
		return Access{View: isMember, Edit: isMember && canEdit}
	}
	return Access{}
}

// accessIn 查出 user 对会议 m 的权限。q 可以是事务里的。
func accessIn(ctx context.Context, q *sqlc.Queries, user sqlc.User, m sqlc.Meeting) (Access, error) {
	if user.Role == "admin" || m.OwnerID == user.ID || m.Visibility == VisibilityPrivate {
		return accessFor(user, m, false, false), nil
	}
	canEdit, err := q.MeetingMember(ctx, sqlc.MeetingMemberParams{MeetingID: m.ID, UserID: user.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return accessFor(user, m, false, false), nil
	}
	if err != nil {
		return Access{}, err
	}
	return accessFor(user, m, true, canEdit), nil
}

var (
	errNotAllowed = &Error{Forbidden, "你不能修改这场会议：只有创建者、管理员和被指定能编辑的人能改"}
	errNotManager = &Error{Forbidden, "只有这场会议的创建者和管理员能做这件事"}
)

// GetMeeting 取出一场会议，带上 user 对它的权限。看不到的会议和回收站里的会议都当作不存在。
func (s *Service) GetMeeting(ctx context.Context, user sqlc.User, id uuid.UUID) (Meeting, error) {
	m, err := s.getMeeting(ctx, id)
	if err != nil {
		return Meeting{}, err
	}
	if m.Access, err = accessIn(ctx, s.q, user, m.Meeting); err != nil {
		return Meeting{}, err
	}
	if !m.Access.View {
		return Meeting{}, errNoMeeting
	}
	return m, nil
}

// editable 取出会议，并确认 user 能编辑它。
func (s *Service) editable(ctx context.Context, user sqlc.User, id uuid.UUID) (Meeting, error) {
	m, err := s.GetMeeting(ctx, user, id)
	if err == nil && !m.Access.Edit {
		err = errNotAllowed
	}
	return m, err
}

// manageable 取出会议，并确认 user 是创建者或者管理员。
func (s *Service) manageable(ctx context.Context, user sqlc.User, id uuid.UUID) (Meeting, error) {
	m, err := s.GetMeeting(ctx, user, id)
	if err == nil && !m.Access.Manage {
		err = errNotManager
	}
	return m, err
}

// MeetingAccess 是一场会议谁能看、谁能编辑。
type MeetingAccess struct {
	Visibility string
	// 单独指定的人
	Members []sqlc.ListMeetingMembersRow
}

// GetMeetingAccess 返回一场会议谁能看、谁能编辑。能看这场会议的人都能看。
func (s *Service) GetMeetingAccess(ctx context.Context, user sqlc.User, id uuid.UUID) (MeetingAccess, error) {
	m, err := s.GetMeeting(ctx, user, id)
	if err != nil {
		return MeetingAccess{}, err
	}
	members, err := s.q.ListMeetingMembers(ctx, id)
	if err != nil {
		return MeetingAccess{}, err
	}
	return MeetingAccess{Visibility: m.Visibility, Members: members}, nil
}

// MemberInput 是单独指定的一个人。
type MemberInput struct {
	UserID  uuid.UUID
	CanEdit bool
}

// SetMeetingAccess 改一场会议谁能看、谁能编辑。只有创建者和管理员能改。
// 可见范围是「只有我」时不用单独指定的人，一起清掉；创建者自己不用指定，传进来也跳过。
func (s *Service) SetMeetingAccess(ctx context.Context, user sqlc.User, id uuid.UUID, visibility string, members []MemberInput) (MeetingAccess, error) {
	switch visibility {
	case VisibilityTeam, VisibilityMembers, VisibilityPrivate:
	default:
		return MeetingAccess{}, invalid("不认识的可见范围：%s", visibility)
	}
	if len(members) > maxMeetingMembers {
		return MeetingAccess{}, invalid("一场会议最多单独指定 %d 个人", maxMeetingMembers)
	}
	m, err := s.manageable(ctx, user, id)
	if err != nil {
		return MeetingAccess{}, err
	}

	ids, edits := []uuid.UUID{}, []bool{}
	if visibility != VisibilityPrivate {
		seen := map[uuid.UUID]bool{}
		for _, mi := range members {
			if mi.UserID == m.OwnerID || seen[mi.UserID] {
				continue
			}
			seen[mi.UserID] = true
			ids, edits = append(ids, mi.UserID), append(edits, mi.CanEdit)
		}
	}
	if err := s.checkMembers(ctx, id, ids); err != nil {
		return MeetingAccess{}, err
	}
	err = s.tx(ctx, func(_ pgx.Tx, q *sqlc.Queries) error {
		if _, err := q.LockMeeting(ctx, id); err != nil {
			return err
		}
		if err := q.SetMeetingVisibility(ctx, sqlc.SetMeetingVisibilityParams{ID: id, Visibility: visibility}); err != nil {
			return err
		}
		return q.ReplaceMeetingMembers(ctx, sqlc.ReplaceMeetingMembersParams{MeetingID: id, AddedBy: &user.ID, UserIds: ids, CanEdit: edits})
	})
	if err != nil {
		return MeetingAccess{}, err
	}
	return s.GetMeetingAccess(ctx, user, id)
}

// checkMembers 确认要指定的人都在团队里。已经停用的人不能新加进来，原来就在的可以留着（反正他登录不了）。
func (s *Service) checkMembers(ctx context.Context, id uuid.UUID, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	users, err := s.q.UsersByIDs(ctx, ids)
	if err != nil {
		return err
	}
	current, err := s.q.ListMeetingMembers(ctx, id)
	if err != nil {
		return err
	}
	already := map[uuid.UUID]bool{}
	for _, c := range current {
		already[c.UserID] = true
	}
	ok := map[uuid.UUID]bool{}
	for _, u := range users {
		ok[u.ID] = !u.Disabled || already[u.ID]
	}
	for _, uid := range ids {
		if !ok[uid] {
			return invalid("选的人里有不在团队里、或者已经停用了的，请刷新页面重新选")
		}
	}
	return nil
}

// People 列出团队里没停用的人，给会议指定成员时选。
func (s *Service) People(ctx context.Context) ([]sqlc.ActiveUsersRow, error) {
	return s.q.ActiveUsers(ctx)
}

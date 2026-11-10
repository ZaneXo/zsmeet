package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/ZaneXo/zsmeet/internal/service"
)

// MeetingMember 是一场会议单独指定的一个人。
type MeetingMember struct {
	UserID   string `json:"user_id" format:"uuid"`
	Name     string `json:"name"`
	Email    string `json:"email"`
	CanEdit  bool   `json:"can_edit" doc:"除了看，还能编辑"`
	Disabled bool   `json:"disabled" doc:"这个人已经停用了，登录不了"`
}

type MeetingAccess struct {
	Visibility string          `json:"visibility" enum:"team,members,private" doc:"谁能看：团队所有人、指定的人、只有创建者（管理员总能看）"`
	Members    []MeetingMember `json:"members" nullable:"false" doc:"单独指定的人。可见范围是团队所有人时，这里是还能编辑的人；是指定的人时，这里是能看的人（can_edit 的还能编辑）"`
}

type MeetingAccessOutput struct {
	Body MeetingAccess
}

func toMeetingAccess(a service.MeetingAccess) MeetingAccess {
	out := MeetingAccess{Visibility: a.Visibility, Members: make([]MeetingMember, len(a.Members))}
	for i, m := range a.Members {
		out.Members[i] = MeetingMember{UserID: m.UserID.String(), Name: m.Name, Email: m.Email, CanEdit: m.CanEdit, Disabled: m.Disabled}
	}
	return out
}

var getMeetingAccessOp = huma.Operation{
	OperationID: "get-meeting-access",
	Method:      http.MethodGet,
	Path:        "/api/meetings/{id}/access",
	Summary:     "谁能看、谁能编辑这场会议",
	Description: "能看这场会议的人都能看。创建者和管理员总能看、能编辑，不在列表里。",
	Tags:        []string{"会议"},
	Security:    loggedIn,
}

func (h *Handlers) GetMeetingAccess(ctx context.Context, in *MeetingID) (*MeetingAccessOutput, error) {
	id, err := parseID(in.ID)
	if err != nil {
		return nil, err
	}
	a, err := h.svc.GetMeetingAccess(ctx, currentUser(ctx), id)
	if err != nil {
		return nil, toHTTP(err)
	}
	return &MeetingAccessOutput{Body: toMeetingAccess(a)}, nil
}

type SetMeetingAccessInput struct {
	MeetingID
	Body struct {
		Visibility string `json:"visibility" enum:"team,members,private"`
		Members    []struct {
			UserID  string `json:"user_id" format:"uuid"`
			CanEdit bool   `json:"can_edit,omitempty" doc:"除了看，还能编辑；不传是只能看"`
		} `json:"members,omitempty" maxItems:"200" doc:"单独指定的人，整份替换；不传表示一个都没有。可见范围是只有自己时忽略（清空）"`
	}
}

var setMeetingAccessOp = huma.Operation{
	OperationID: "set-meeting-access",
	Method:      http.MethodPut,
	Path:        "/api/meetings/{id}/access",
	Summary:     "改谁能看、谁能编辑这场会议",
	Description: "只有创建者和管理员能改。已经发到飞书群的卡片、导入的飞书文档不受影响。",
	Tags:        []string{"会议"},
	Security:    loggedIn,
}

func (h *Handlers) SetMeetingAccess(ctx context.Context, in *SetMeetingAccessInput) (*MeetingAccessOutput, error) {
	id, err := parseID(in.ID)
	if err != nil {
		return nil, err
	}
	members := make([]service.MemberInput, len(in.Body.Members))
	for i, m := range in.Body.Members {
		uid, err := uuid.Parse(m.UserID)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity("选的人不对，请刷新页面重新选")
		}
		members[i] = service.MemberInput{UserID: uid, CanEdit: m.CanEdit}
	}
	a, err := h.svc.SetMeetingAccess(ctx, currentUser(ctx), id, in.Body.Visibility, members)
	if err != nil {
		return nil, toHTTP(err)
	}
	return &MeetingAccessOutput{Body: toMeetingAccess(a)}, nil
}

// Person 是团队里的一个人，给会议指定成员时选。
type Person struct {
	ID    string `json:"id" format:"uuid"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type PeopleOutput struct {
	Body struct {
		Items []Person `json:"items" nullable:"false"`
	}
}

var listPeopleOp = huma.Operation{
	OperationID: "list-people",
	Method:      http.MethodGet,
	Path:        "/api/people",
	Summary:     "团队里的人",
	Description: "没停用的成员，给会议指定谁能看、谁能编辑时选。所有登录的人都能看。",
	Tags:        []string{"成员"},
	Security:    loggedIn,
}

func (h *Handlers) ListPeople(ctx context.Context, _ *struct{}) (*PeopleOutput, error) {
	rows, err := h.svc.People(ctx)
	if err != nil {
		return nil, toHTTP(err)
	}
	out := &PeopleOutput{}
	out.Body.Items = make([]Person, len(rows))
	for i, r := range rows {
		out.Body.Items[i] = Person{ID: r.ID.String(), Name: r.Name, Email: r.Email}
	}
	return out, nil
}

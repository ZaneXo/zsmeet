package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ZaneXo/zsmeet/internal/service"
)

// ---- 删除会议和回收站 ----

var deleteMeetingOp = huma.Operation{
	OperationID:   "delete-meeting",
	Method:        http.MethodDelete,
	Path:          "/api/meetings/{id}",
	Summary:       "删除会议（放进回收站）",
	Description:   "创建者和管理员可以删。删除后 30 天内可以在回收站恢复，过期后连同录音一起彻底删除。",
	Tags:          []string{"会议"},
	Security:      loggedIn,
	DefaultStatus: http.StatusNoContent,
}

func (h *Handlers) DeleteMeeting(ctx context.Context, in *MeetingID) (*struct{}, error) {
	id, err := parseID(in.ID)
	if err != nil {
		return nil, err
	}
	if err := h.svc.DeleteMeeting(ctx, currentUser(ctx), id); err != nil {
		return nil, toHTTP(err)
	}
	return nil, nil
}

// TrashedMeeting 是回收站里的一场会议。
type TrashedMeeting struct {
	Meeting
	DeletedAt     time.Time `json:"deleted_at" doc:"什么时候删的"`
	DeletedByName string    `json:"deleted_by_name" doc:"谁删的"`
	PurgeAt       time.Time `json:"purge_at" doc:"到这个时间会被彻底删除，连同录音"`
}

type TrashOutput struct {
	Body struct {
		Items []TrashedMeeting `json:"items" nullable:"false"`
		Days  int              `json:"days" doc:"会议在回收站里放多少天后彻底删除"`
	}
}

var listTrashOp = huma.Operation{
	OperationID: "list-trash",
	Method:      http.MethodGet,
	Path:        "/api/trash",
	Summary:     "回收站",
	Description: "最近删的在前。管理员看到全部，其他人只看到自己创建的会议。",
	Tags:        []string{"会议"},
	Security:    loggedIn,
}

func (h *Handlers) ListTrash(ctx context.Context, _ *struct{}) (*TrashOutput, error) {
	viewer := currentUser(ctx)
	items, err := h.svc.ListTrash(ctx, viewer)
	if err != nil {
		return nil, toHTTP(err)
	}
	out := &TrashOutput{}
	out.Body.Days = service.TrashDays
	out.Body.Items = make([]TrashedMeeting, len(items))
	for i, t := range items {
		out.Body.Items[i] = TrashedMeeting{
			Meeting:       toMeeting(t.Meeting, viewer),
			DeletedAt:     *t.DeletedAt,
			DeletedByName: t.DeletedByName,
			PurgeAt:       t.PurgeAt(),
		}
	}
	return out, nil
}

type TrashID struct {
	ID string `path:"id" format:"uuid"`
}

var restoreMeetingOp = huma.Operation{
	OperationID: "restore-meeting",
	Method:      http.MethodPost,
	Path:        "/api/trash/{id}/restore",
	Summary:     "从回收站恢复",
	Tags:        []string{"会议"},
	Security:    loggedIn,
}

func (h *Handlers) RestoreMeeting(ctx context.Context, in *TrashID) (*MeetingOutput, error) {
	id, err := parseID(in.ID)
	if err != nil {
		return nil, err
	}
	m, err := h.svc.RestoreMeeting(ctx, currentUser(ctx), id)
	if err != nil {
		return nil, toHTTP(err)
	}
	return &MeetingOutput{Body: toMeeting(m, currentUser(ctx))}, nil
}

var purgeMeetingOp = huma.Operation{
	OperationID:   "purge-meeting",
	Method:        http.MethodDelete,
	Path:          "/api/trash/{id}",
	Summary:       "彻底删除",
	Description:   "立即删掉回收站里的这场会议，连同录音、转写和纪要，不能恢复。",
	Tags:          []string{"会议"},
	Security:      loggedIn,
	DefaultStatus: http.StatusNoContent,
}

func (h *Handlers) PurgeMeeting(ctx context.Context, in *TrashID) (*struct{}, error) {
	id, err := parseID(in.ID)
	if err != nil {
		return nil, err
	}
	if err := h.svc.PurgeMeeting(ctx, currentUser(ctx), id); err != nil {
		return nil, toHTTP(err)
	}
	return nil, nil
}

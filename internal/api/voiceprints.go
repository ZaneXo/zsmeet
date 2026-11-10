package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
)

// ---- 声纹（记住说话人） ----

type VoiceprintSettingsOutput struct {
	Body struct {
		Enabled bool `json:"enabled" doc:"是否记住说话人：给说话人改过名字以后，之后的会议按声音认出这个人、自动用这个名字。默认关着"`
	}
}

var getVoiceprintSettingsOp = huma.Operation{
	OperationID: "get-voiceprint-settings",
	Method:      http.MethodGet,
	Path:        "/api/settings/voiceprints",
	Summary:     "声纹设置",
	Tags:        []string{"设置"},
	Security:    adminOnly,
}

func (h *Handlers) GetVoiceprintSettings(ctx context.Context, _ *struct{}) (*VoiceprintSettingsOutput, error) {
	v, err := h.svc.VoiceprintSettings(ctx)
	if err != nil {
		return nil, toHTTP(err)
	}
	out := &VoiceprintSettingsOutput{}
	out.Body.Enabled = v.Enabled
	return out, nil
}

type VoiceprintSettingsInput struct {
	Body struct {
		Enabled bool `json:"enabled"`
	}
}

var putVoiceprintSettingsOp = huma.Operation{
	OperationID: "put-voiceprint-settings",
	Method:      http.MethodPut,
	Path:        "/api/settings/voiceprints",
	Summary:     "打开或关掉声纹",
	Description: "关掉以后不再收集、不再比对声音特征，已经记下的声纹留着，要删的话调用「删除全部声纹」。",
	Tags:        []string{"设置"},
	Security:    adminOnly,
}

func (h *Handlers) PutVoiceprintSettings(ctx context.Context, in *VoiceprintSettingsInput) (*VoiceprintSettingsOutput, error) {
	v, err := h.svc.SaveVoiceprintSettings(ctx, in.Body.Enabled)
	if err != nil {
		return nil, toHTTP(err)
	}
	out := &VoiceprintSettingsOutput{}
	out.Body.Enabled = v.Enabled
	return out, nil
}

type Voiceprint struct {
	ID        string    `json:"id" format:"uuid"`
	Name      string    `json:"name"`
	Meetings  int       `json:"meetings" doc:"从几场会议里学到的"`
	Seconds   float64   `json:"seconds" doc:"学到的说话一共多少秒"`
	UpdatedAt time.Time `json:"updated_at" doc:"最近一次学到或去掉样本的时间"`
}

type VoiceprintsOutput struct {
	Body struct {
		Items []Voiceprint `json:"items" nullable:"false"`
	}
}

var listVoiceprintsOp = huma.Operation{
	OperationID: "list-voiceprints",
	Method:      http.MethodGet,
	Path:        "/api/voiceprints",
	Summary:     "声纹库里的人",
	Description: "最近更新的在前。只说从几场会议里学到的，不列是哪几场。",
	Tags:        []string{"设置"},
	Security:    adminOnly,
}

func (h *Handlers) ListVoiceprints(ctx context.Context, _ *struct{}) (*VoiceprintsOutput, error) {
	list, err := h.svc.ListVoiceprints(ctx)
	if err != nil {
		return nil, toHTTP(err)
	}
	out := &VoiceprintsOutput{}
	out.Body.Items = make([]Voiceprint, len(list))
	for i, v := range list {
		out.Body.Items[i] = Voiceprint{ID: v.ID.String(), Name: v.Name, Meetings: v.Meetings, Seconds: v.Seconds, UpdatedAt: v.UpdatedAt}
	}
	return out, nil
}

type VoiceprintID struct {
	ID string `path:"id" format:"uuid"`
}

var deleteVoiceprintOp = huma.Operation{
	OperationID:   "delete-voiceprint",
	Method:        http.MethodDelete,
	Path:          "/api/voiceprints/{id}",
	Summary:       "删除一个人的声纹",
	Description:   "连同学到的全部样本一起删除，不能恢复。会议里已经起好的名字不变；以后有人再把说话人改成这个名字，会重新开始学。",
	Tags:          []string{"设置"},
	Security:      adminOnly,
	DefaultStatus: http.StatusNoContent,
}

func (h *Handlers) DeleteVoiceprint(ctx context.Context, in *VoiceprintID) (*struct{}, error) {
	id, err := uuid.Parse(in.ID)
	if err != nil {
		return nil, huma.Error404NotFound("没有这个声纹，可能已经删掉了")
	}
	if err := h.svc.DeleteVoiceprint(ctx, id); err != nil {
		return nil, toHTTP(err)
	}
	return nil, nil
}

var deleteAllVoiceprintsOp = huma.Operation{
	OperationID:   "delete-all-voiceprints",
	Method:        http.MethodDelete,
	Path:          "/api/voiceprints",
	Summary:       "删除全部声纹",
	Description:   "删除声纹库，以及每场会议里存的说话人声音特征，不能恢复。会议里已经起好的名字不变。",
	Tags:          []string{"设置"},
	Security:      adminOnly,
	DefaultStatus: http.StatusNoContent,
}

func (h *Handlers) DeleteAllVoiceprints(ctx context.Context, _ *struct{}) (*struct{}, error) {
	if err := h.svc.DeleteAllVoiceprints(ctx); err != nil {
		return nil, toHTTP(err)
	}
	return nil, nil
}

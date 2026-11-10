package api

import (
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/sse"
)

// Register 注册所有接口。新接口用 make api 生成骨架时会自动加到这里。
func (h *Handlers) Register(api huma.API) {
	huma.Register(api, statusOp, h.Status)
	huma.Register(api, setupOp, h.Setup)
	huma.Register(api, loginOp, h.Login)
	huma.Register(api, logoutOp, h.Logout)

	huma.Register(api, listTemplatesOp, h.ListTemplates)
	huma.Register(api, listMeetingsOp, h.ListMeetings)
	huma.Register(api, createMeetingOp, h.CreateMeeting)
	huma.Register(api, getMeetingOp, h.GetMeeting)
	huma.Register(api, renameMeetingOp, h.RenameMeeting)
	huma.Register(api, retryMeetingOp, h.RetryMeeting)
	sse.Register(api, meetingEventsOp, map[string]any{"meeting": Meeting{}}, h.MeetingEvents)

	huma.Register(api, putChunkOp, h.PutChunk)
	huma.Register(api, listChunksOp, h.ListChunks)
	huma.Register(api, saveMarksOp, h.SaveMarks)
	huma.Register(api, completeUploadOp, h.CompleteUpload)

	huma.Register(api, getTranscriptOp, h.GetTranscript)
	huma.Register(api, getMinutesOp, h.GetMinutes)
	huma.Register(api, editMinutesOp, h.EditMinutes)
	huma.Register(api, regenerateMinutesOp, h.RegenerateMinutes)

	huma.Register(api, lookupLinkOp, h.LookupLink)
	huma.Register(api, acceptLinkOp, h.AcceptLink)
	huma.Register(api, changePasswordOp, h.ChangePassword)

	huma.Register(api, listMembersOp, h.ListMembers)
	huma.Register(api, updateMemberOp, h.UpdateMember)
	huma.Register(api, createResetLinkOp, h.CreateResetLink)
	huma.Register(api, revokeResetLinkOp, h.RevokeResetLink)
	huma.Register(api, createInviteOp, h.CreateInvite)
	huma.Register(api, renewInviteOp, h.RenewInvite)
	huma.Register(api, revokeInviteOp, h.RevokeInvite)

	huma.Register(api, getLLMSettingsOp, h.GetLLMSettings)
	huma.Register(api, putLLMSettingsOp, h.PutLLMSettings)
	huma.Register(api, testLLMSettingsOp, h.TestLLMSettings)
	huma.Register(api, listLLMModelsOp, h.ListLLMModels)
	// gen:routes —— make api 会在这一行上面插入新接口，别删
}

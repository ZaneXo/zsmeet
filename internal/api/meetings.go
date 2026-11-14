package api

import (
	"bytes"
	"context"
	"net/http"
	"reflect"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/sse"
	"github.com/google/uuid"

	"github.com/ZaneXo/zsmeet/internal/service"
)

// ---- 模板 ----

type Template struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Guide  string `json:"guide" doc:"纪要要有哪几部分、每部分写什么（交给大模型的说明）"`
	Custom bool   `json:"custom" doc:"管理员自己定义的模板（可以改、可以删）；内置的模板是 false"`
}

func toTemplate(t service.Template) Template {
	return Template{ID: t.ID, Name: t.Name, Guide: t.Guide, Custom: t.Custom}
}

type TemplatesOutput struct {
	Body struct {
		Items []Template `json:"items"`
	}
}

var listTemplatesOp = huma.Operation{
	OperationID: "list-templates",
	Method:      http.MethodGet,
	Path:        "/api/templates",
	Summary:     "可选的纪要模板",
	Description: "先是内置的四种，再是管理员自己定义的。",
	Tags:        []string{"会议"},
	Security:    loggedIn,
}

func (h *Handlers) ListTemplates(ctx context.Context, _ *struct{}) (*TemplatesOutput, error) {
	items, err := h.svc.Templates(ctx)
	if err != nil {
		return nil, toHTTP(err)
	}
	out := &TemplatesOutput{}
	out.Body.Items = make([]Template, len(items))
	for i, t := range items {
		out.Body.Items[i] = toTemplate(t)
	}
	return out, nil
}

type TemplateBody struct {
	Name  string `json:"name" maxLength:"30" doc:"模板的名字，选模板时显示"`
	Guide string `json:"guide" maxLength:"2000" doc:"纪要要有哪几部分、每部分写什么，一行一部分"`
}

type TemplateInput struct {
	Body TemplateBody
}

type TemplateOutput struct {
	Body Template
}

var createTemplateOp = huma.Operation{
	OperationID:   "create-template",
	Method:        http.MethodPost,
	Path:          "/api/templates",
	Summary:       "新建纪要模板",
	Tags:          []string{"会议"},
	Security:      adminOnly,
	DefaultStatus: http.StatusCreated,
}

func (h *Handlers) CreateTemplate(ctx context.Context, in *TemplateInput) (*TemplateOutput, error) {
	t, err := h.svc.CreateTemplate(ctx, currentUser(ctx), in.Body.Name, in.Body.Guide)
	if err != nil {
		return nil, toHTTP(err)
	}
	return &TemplateOutput{Body: toTemplate(t)}, nil
}

type TemplateID struct {
	ID string `path:"id" format:"uuid"`
}

type UpdateTemplateInput struct {
	TemplateID
	Body TemplateBody
}

var updateTemplateOp = huma.Operation{
	OperationID: "update-template",
	Method:      http.MethodPut,
	Path:        "/api/templates/{id}",
	Summary:     "修改纪要模板",
	Description: "只能改自定义的模板。以后用它写的纪要按新的写法，已经写好的不变。",
	Tags:        []string{"会议"},
	Security:    adminOnly,
}

func (h *Handlers) UpdateTemplate(ctx context.Context, in *UpdateTemplateInput) (*TemplateOutput, error) {
	id, err := uuid.Parse(in.ID)
	if err != nil {
		return nil, huma.Error404NotFound("没有这个自定义模板")
	}
	t, err := h.svc.UpdateTemplate(ctx, id, in.Body.Name, in.Body.Guide)
	if err != nil {
		return nil, toHTTP(err)
	}
	return &TemplateOutput{Body: toTemplate(t)}, nil
}

var deleteTemplateOp = huma.Operation{
	OperationID:   "delete-template",
	Method:        http.MethodDelete,
	Path:          "/api/templates/{id}",
	Summary:       "删除纪要模板",
	Description:   "只能删自定义的模板。已经写好的纪要不受影响；还选着它的会议以后重新生成时用通用模板。",
	Tags:          []string{"会议"},
	Security:      adminOnly,
	DefaultStatus: http.StatusNoContent,
}

func (h *Handlers) DeleteTemplate(ctx context.Context, in *TemplateID) (*struct{}, error) {
	id, err := uuid.Parse(in.ID)
	if err != nil {
		return nil, huma.Error404NotFound("没有这个自定义模板")
	}
	if err := h.svc.DeleteTemplate(ctx, id); err != nil {
		return nil, toHTTP(err)
	}
	return nil, nil
}

// ---- 会议 ----

type ListMeetingsInput struct {
	Q     string `query:"q" maxLength:"100" doc:"在标题、纪要和转写原文里搜索"`
	Mine  bool   `query:"mine" doc:"只列我创建的"`
	Limit int    `query:"limit" default:"100" minimum:"1" maximum:"200"`
}

// SearchHit 是搜索时一场会议里命中的地方：转写里的一句话，或者纪要里的一行。
type SearchHit struct {
	Start   float64  `json:"start" doc:"从录音开头算的秒数，点它从这附近开始播放；纪要里没写时间点的行是 -1"`
	Speaker int      `json:"speaker" doc:"说话人编号，从 0 开始；纪要里的行是 -1"`
	Text    string   `json:"text" doc:"命中的那句话（或者纪要里的那一行）"`
	Matches []string `json:"matches" nullable:"false" doc:"text 里命中的词，原样（大小写不变），用来高亮"`
	Source  string   `json:"source" enum:"transcript,minutes" doc:"在转写里还是纪要里"`
	Fuzzy   bool     `json:"fuzzy" doc:"只是错了一个字的近似命中（转写可能把这个词听错了一个字）"`
}

// MeetingListItem 是列表里的一场会议，搜索时带上命中的地方。
type MeetingListItem struct {
	Meeting
	Hits []SearchHit `json:"hits" nullable:"false" doc:"搜索时命中的地方：先是转写里的句子（最相关的在前，错一个字的在最后），再是纪要里的行。不是搜索时为空"`
}

type ListMeetingsOutput struct {
	Body struct {
		Items    []MeetingListItem `json:"items"`
		Expanded []string          `json:"expanded" nullable:"false" doc:"实际搜了哪几种写法：输入的词，加上会议术语表里它对应的写法（比如同一个词被听错的写法）。不是搜索时为空"`
		Partial  bool              `json:"partial" doc:"「错一个字」的近似匹配只检查了最近的一部分会议（完全命中的会议不受影响）"`
	}
}

var listMeetingsOp = huma.Operation{
	OperationID: "list-meetings",
	Method:      http.MethodGet,
	Path:        "/api/meetings",
	Summary:     "会议列表",
	Description: "按创建时间倒序。团队里所有人都能看到全部会议。搜索时每场会议带上命中的句子和时间点（hits）。",
	Tags:        []string{"会议"},
	Security:    loggedIn,
}

func (h *Handlers) ListMeetings(ctx context.Context, in *ListMeetingsInput) (*ListMeetingsOutput, error) {
	viewer := currentUser(ctx)
	var owner *uuid.UUID
	if in.Mine {
		owner = &viewer.ID
	}
	list, err := h.svc.ListMeetings(ctx, viewer, in.Q, owner, in.Limit)
	if err != nil {
		return nil, toHTTP(err)
	}
	out := &ListMeetingsOutput{}
	out.Body.Items = make([]MeetingListItem, len(list.Items))
	for i, m := range list.Items {
		hits := make([]SearchHit, len(m.Hits))
		for j, h := range m.Hits {
			hits[j] = SearchHit(h)
		}
		out.Body.Items[i] = MeetingListItem{Meeting: toMeeting(m), Hits: hits}
	}
	out.Body.Expanded = list.Spellings
	if out.Body.Expanded == nil {
		out.Body.Expanded = []string{}
	}
	out.Body.Partial = list.Partial
	return out, nil
}

type CreateMeetingInput struct {
	Body struct {
		Title    string `json:"title,omitempty" maxLength:"200" doc:"留空会用当前时间起一个"`
		Source   string `json:"source" enum:"upload,record,desktop" doc:"upload 是上传文件，record 是网页录音，desktop 是桌面版录音"`
		Template string `json:"template,omitempty" doc:"纪要模板的 ID，留空用通用模板"`
		Hotwords string `json:"hotwords,omitempty" maxLength:"500" doc:"人名、产品名等专有名词，用逗号分开"`
		// 指定具体的人要在建好以后用「谁能看」接口设置
		Visibility   string `json:"visibility,omitempty" enum:"team,private" doc:"谁能看：团队所有人（默认）或者只有自己（管理员也能看）"`
		SpeakerCount int    `json:"speaker_count,omitzero" minimum:"0" maximum:"30" doc:"有几个人说话：0 或不填是按声音自动区分；1 到 30 是按这个人数区分说话人"`
	}
}

var createMeetingOp = huma.Operation{
	OperationID:   "create-meeting",
	Method:        http.MethodPost,
	Path:          "/api/meetings",
	Summary:       "新建会议",
	Description:   "新建后会议处于 uploading 状态：接着逐片上传录音，传完调用「上传完成」。",
	Tags:          []string{"会议"},
	Security:      loggedIn,
	DefaultStatus: http.StatusCreated,
}

func (h *Handlers) CreateMeeting(ctx context.Context, in *CreateMeetingInput) (*MeetingOutput, error) {
	m, err := h.svc.CreateMeeting(ctx, currentUser(ctx), service.NewMeeting{
		Title: in.Body.Title, Source: in.Body.Source, Template: in.Body.Template, Hotwords: in.Body.Hotwords,
		Visibility: in.Body.Visibility, SpeakerCount: in.Body.SpeakerCount,
	})
	if err != nil {
		return nil, toHTTP(err)
	}
	return &MeetingOutput{Body: toMeeting(m)}, nil
}

var getMeetingOp = huma.Operation{
	OperationID: "get-meeting",
	Method:      http.MethodGet,
	Path:        "/api/meetings/{id}",
	Summary:     "会议详情",
	Tags:        []string{"会议"},
	Security:    loggedIn,
}

func (h *Handlers) GetMeeting(ctx context.Context, in *MeetingID) (*MeetingOutput, error) {
	id, err := parseID(in.ID)
	if err != nil {
		return nil, err
	}
	m, err := h.svc.GetMeeting(ctx, currentUser(ctx), id)
	if err != nil {
		return nil, toHTTP(err)
	}
	return &MeetingOutput{Body: toMeeting(m)}, nil
}

type RenameMeetingInput struct {
	MeetingID
	Body struct {
		Title string `json:"title" minLength:"1" maxLength:"200"`
	}
}

var renameMeetingOp = huma.Operation{
	OperationID: "rename-meeting",
	Method:      http.MethodPatch,
	Path:        "/api/meetings/{id}",
	Summary:     "修改会议标题",
	Tags:        []string{"会议"},
	Security:    loggedIn,
}

func (h *Handlers) RenameMeeting(ctx context.Context, in *RenameMeetingInput) (*MeetingOutput, error) {
	id, err := parseID(in.ID)
	if err != nil {
		return nil, err
	}
	m, err := h.svc.RenameMeeting(ctx, currentUser(ctx), id, in.Body.Title)
	if err != nil {
		return nil, toHTTP(err)
	}
	return &MeetingOutput{Body: toMeeting(m)}, nil
}

// ---- 上传 ----

// 单个分片的大小上限。网页录音每片几十 KB，上传文件按 4 MB 切
const maxChunkBytes = 8 << 20

type PutChunkInput struct {
	MeetingID
	Seq     int    `path:"seq" minimum:"0" maximum:"3999" doc:"分片序号，从 0 开始"`
	RawBody []byte `contentType:"application/octet-stream"`
}

var putChunkOp = huma.Operation{
	OperationID: "put-chunk",
	Method:      http.MethodPut,
	Path:        "/api/meetings/{id}/chunks/{seq}",
	Summary:     "上传一个分片",
	Description: "录音或文件按顺序切成若干片，每片单独上传。同一片可以重复上传，所以断网后重传是安全的。",
	Tags:        []string{"上传"},
	Security:    loggedIn,
	// 默认只允许 1 MB、5 秒读完，分片比这个大，网络差的时候也更慢
	MaxBodyBytes:    maxChunkBytes,
	BodyReadTimeout: 2 * time.Minute,
	DefaultStatus:   http.StatusNoContent,
}

func (h *Handlers) PutChunk(ctx context.Context, in *PutChunkInput) (*struct{}, error) {
	id, err := parseID(in.ID)
	if err != nil {
		return nil, err
	}
	if err := h.svc.PutChunk(ctx, currentUser(ctx), id, in.Seq, bytes.NewReader(in.RawBody)); err != nil {
		return nil, toHTTP(err)
	}
	return nil, nil
}

type ChunksOutput struct {
	Body struct {
		Received []int `json:"received" doc:"已经收到的分片序号"`
	}
}

var listChunksOp = huma.Operation{
	OperationID: "list-chunks",
	Method:      http.MethodGet,
	Path:        "/api/meetings/{id}/chunks",
	Summary:     "已经收到哪些分片",
	Description: "断点续传时用：网页刷新或断网恢复后，跳过已经传上来的分片。",
	Tags:        []string{"上传"},
	Security:    loggedIn,
}

func (h *Handlers) ListChunks(ctx context.Context, in *MeetingID) (*ChunksOutput, error) {
	id, err := parseID(in.ID)
	if err != nil {
		return nil, err
	}
	seqs, err := h.svc.ReceivedChunks(ctx, currentUser(ctx), id)
	if err != nil {
		return nil, toHTTP(err)
	}
	out := &ChunksOutput{}
	out.Body.Received = seqs
	return out, nil
}

type SaveMarksInput struct {
	MeetingID
	Body struct {
		Marks []float64 `json:"marks" maxItems:"500" doc:"全部标记（整份替换）：录音开始后的秒数，不算暂停的时间"`
	}
}

var saveMarksOp = huma.Operation{
	OperationID:   "save-marks",
	Method:        http.MethodPut,
	Path:          "/api/meetings/{id}/marks",
	Summary:       "保存录音时标记的重点",
	Description:   "网页录音过程中每标记一次就调用一次，整份替换。会议交给后台处理以后再调用返回 409。",
	Tags:          []string{"上传"},
	Security:      loggedIn,
	DefaultStatus: http.StatusNoContent,
}

func (h *Handlers) SaveMarks(ctx context.Context, in *SaveMarksInput) (*struct{}, error) {
	id, err := parseID(in.ID)
	if err != nil {
		return nil, err
	}
	if err := h.svc.SaveMarks(ctx, currentUser(ctx), id, in.Body.Marks); err != nil {
		return nil, toHTTP(err)
	}
	return nil, nil
}

type CompleteUploadInput struct {
	MeetingID
	Body struct {
		Chunks int       `json:"chunks" minimum:"1" maximum:"4000" doc:"一共有多少片"`
		Marks  []float64 `json:"marks,omitempty" maxItems:"500" doc:"网页录音时标记的重点：录音开始后的秒数，不算暂停的时间。写纪要时会重点写这些时间点前后的内容"`
	}
}

var completeUploadOp = huma.Operation{
	OperationID: "complete-upload",
	Method:      http.MethodPost,
	Path:        "/api/meetings/{id}/complete",
	Summary:     "上传完成，开始处理",
	Description: "确认所有分片都收到后，会议进入 queued 状态，由后台转写并生成纪要。缺片时返回 409 并指出缺哪一片。",
	Tags:        []string{"上传"},
	Security:    loggedIn,
}

func (h *Handlers) CompleteUpload(ctx context.Context, in *CompleteUploadInput) (*MeetingOutput, error) {
	id, err := parseID(in.ID)
	if err != nil {
		return nil, err
	}
	m, err := h.svc.CompleteUpload(ctx, currentUser(ctx), id, in.Body.Chunks, in.Body.Marks)
	if err != nil {
		return nil, toHTTP(err)
	}
	return &MeetingOutput{Body: toMeeting(m)}, nil
}

// ---- 处理 ----

var retryMeetingOp = huma.Operation{
	OperationID: "retry-meeting",
	Method:      http.MethodPost,
	Path:        "/api/meetings/{id}/retry",
	Summary:     "重试处理失败的会议",
	Description: "已经有转写的话只重新生成纪要，否则从录音开始。",
	Tags:        []string{"会议"},
	Security:    loggedIn,
}

func (h *Handlers) RetryMeeting(ctx context.Context, in *MeetingID) (*MeetingOutput, error) {
	id, err := parseID(in.ID)
	if err != nil {
		return nil, err
	}
	m, err := h.svc.Retry(ctx, currentUser(ctx), id)
	if err != nil {
		return nil, toHTTP(err)
	}
	return &MeetingOutput{Body: toMeeting(m)}, nil
}

var meetingEventsOp = huma.Operation{
	OperationID: "meeting-events",
	Method:      http.MethodGet,
	Path:        "/api/meetings/{id}/events",
	Summary:     "订阅处理进度（SSE）",
	Description: "会议状态或进度变化时推送一条 meeting 事件，内容和「会议详情」一样。处理结束（done 或 failed）后连接关闭。",
	Tags:        []string{"会议"},
	Security:    loggedIn,
}

// MeetingEvents 每秒查一次数据库，有变化就推给浏览器。
// 进度由 worker 进程写进数据库，这里读出来，api 和 worker 之间不需要别的通道。
func (h *Handlers) MeetingEvents(ctx context.Context, in *MeetingID, send sse.Sender) {
	id, err := parseID(in.ID)
	if err != nil {
		return
	}
	viewer := currentUser(ctx)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var last Meeting
	idle := 0
	for {
		// 每次都重新检查：看着看着被改成看不到了，就断开
		m, err := h.svc.GetMeeting(ctx, viewer, id)
		if err != nil {
			return
		}
		cur := toMeeting(m)
		switch {
		case meetingChanged(cur, last):
			if send.Data(cur) != nil {
				return
			}
			last, idle = cur, 0
		case idle >= 15:
			// 长时间没有变化时发一行注释，免得中间的代理把空闲连接掐掉
			if send.Comment("keepalive") != nil {
				return
			}
			idle = 0
		default:
			idle++
		}
		if cur.Status == "done" || cur.Status == "failed" {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// meetingChanged 看会议和上次推送的比有没有变化。Meeting 里有切片和指针，要比较内容。
// upload_idle_sec 不算：等录音传完时它每秒都在变，只为它不推（新收到分片时 last_chunk_at 会变，照样推）。
func meetingChanged(cur, last Meeting) bool {
	cur.UploadIdleSec, last.UploadIdleSec = nil, nil
	return !reflect.DeepEqual(cur, last)
}

type RetranscribeInput struct {
	MeetingID
	// 可以不带：照原来的人数（或自动区分）转写
	Body *struct {
		SpeakerCount *int `json:"speaker_count,omitempty" minimum:"0" maximum:"30" doc:"这次按几个人区分说话人：0 是按声音自动区分，1 到 30 是按这个人数；不填就照原来的设置"`
	}
}

var retranscribeOp = huma.Operation{
	OperationID: "retranscribe-meeting",
	Method:      http.MethodPost,
	Path:        "/api/meetings/{id}/retranscribe",
	Summary:     "重新转写",
	Description: "用现在的识别模型从录音重新转写，转完重新生成纪要（新版本，之前的版本保留）。录音已经不在、或者正在处理时返回 409。",
	Tags:        []string{"会议"},
	Security:    loggedIn,
}

func (h *Handlers) Retranscribe(ctx context.Context, in *RetranscribeInput) (*MeetingOutput, error) {
	id, err := parseID(in.ID)
	if err != nil {
		return nil, err
	}
	var speakerCount *int
	if in.Body != nil {
		speakerCount = in.Body.SpeakerCount
	}
	m, err := h.svc.Retranscribe(ctx, currentUser(ctx), id, speakerCount)
	if err != nil {
		return nil, toHTTP(err)
	}
	return &MeetingOutput{Body: toMeeting(m)}, nil
}

type RenameSpeakersInput struct {
	MeetingID
	Body struct {
		Names map[string]string `json:"names" doc:"说话人编号（从 0 开始，写成字符串）→ 名字；名字给空字符串表示去掉名字。只改这里面列出的说话人"`
	}
}

var renameSpeakersOp = huma.Operation{
	OperationID: "rename-speakers",
	Method:      http.MethodPut,
	Path:        "/api/meetings/{id}/speakers",
	Summary:     "给说话人起名字",
	Description: "转写和搜索结果马上显示新名字；当前纪要里的「说话人 N」换成名字，作为一个新版本保存（旧版本保留）。以后重新生成纪要时也用名字。" +
		"管理员打开了声纹时，名字连同这位说话人的声音特征记进声纹库，之后的会议按声音认出他；按声音认出的名字照原样提交一次算确认。",
	Tags:     []string{"会议"},
	Security: loggedIn,
}

func (h *Handlers) RenameSpeakers(ctx context.Context, in *RenameSpeakersInput) (*MeetingOutput, error) {
	id, err := parseID(in.ID)
	if err != nil {
		return nil, err
	}
	names := make(map[int]string, len(in.Body.Names))
	for key, name := range in.Body.Names {
		idx, err := strconv.Atoi(key)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity("说话人编号要是数字：" + key)
		}
		names[idx] = name
	}
	m, err := h.svc.RenameSpeakers(ctx, currentUser(ctx), id, names)
	if err != nil {
		return nil, toHTTP(err)
	}
	return &MeetingOutput{Body: toMeeting(m)}, nil
}

// ---- 转写 ----

type TranscriptOutput struct {
	Body struct {
		Segments []Segment `json:"segments"`
	}
}

var getTranscriptOp = huma.Operation{
	OperationID: "get-transcript",
	Method:      http.MethodGet,
	Path:        "/api/meetings/{id}/transcript",
	Summary:     "会议转写",
	Description: "还没转写完时返回空列表。",
	Tags:        []string{"会议"},
	Security:    loggedIn,
}

func (h *Handlers) GetTranscript(ctx context.Context, in *MeetingID) (*TranscriptOutput, error) {
	id, err := parseID(in.ID)
	if err != nil {
		return nil, err
	}
	segs, err := h.svc.Transcript(ctx, currentUser(ctx), id)
	if err != nil {
		return nil, toHTTP(err)
	}
	out := &TranscriptOutput{}
	out.Body.Segments = toSegments(segs)
	return out, nil
}

// ---- 纪要 ----

type MinutesOutput struct {
	Body Minutes
}

type GetMinutesInput struct {
	MeetingID
	Version int `query:"version" minimum:"0" doc:"要看哪个版本，不传或传 0 表示当前版本"`
}

var getMinutesOp = huma.Operation{
	OperationID: "get-minutes",
	Method:      http.MethodGet,
	Path:        "/api/meetings/{id}/minutes",
	Summary:     "会议纪要",
	Description: "返回指定版本的内容和全部版本记录。还没有纪要时 version 为 0。",
	Tags:        []string{"纪要"},
	Security:    loggedIn,
}

func (h *Handlers) GetMinutes(ctx context.Context, in *GetMinutesInput) (*MinutesOutput, error) {
	id, err := parseID(in.ID)
	if err != nil {
		return nil, err
	}
	m, err := h.svc.GetMinutes(ctx, currentUser(ctx), id, in.Version)
	if err != nil {
		return nil, toHTTP(err)
	}
	return &MinutesOutput{Body: toMinutes(m)}, nil
}

type EditMinutesInput struct {
	MeetingID
	Body struct {
		Content string `json:"content" minLength:"1" doc:"Markdown"`
	}
}

var editMinutesOp = huma.Operation{
	OperationID: "edit-minutes",
	Method:      http.MethodPut,
	Path:        "/api/meetings/{id}/minutes",
	Summary:     "保存手动修改的纪要",
	Description: "不覆盖原文，而是生成一个新版本，旧版本保留。",
	Tags:        []string{"纪要"},
	Security:    loggedIn,
	// 默认的 1 MB 对一份长纪要来说偏紧
	MaxBodyBytes: 2 << 20,
}

func (h *Handlers) EditMinutes(ctx context.Context, in *EditMinutesInput) (*MinutesOutput, error) {
	id, err := parseID(in.ID)
	if err != nil {
		return nil, err
	}
	m, err := h.svc.EditMinutes(ctx, currentUser(ctx), id, in.Body.Content)
	if err != nil {
		return nil, toHTTP(err)
	}
	return &MinutesOutput{Body: toMinutes(m)}, nil
}

type RegenerateMinutesInput struct {
	MeetingID
	Body struct {
		Template string `json:"template" doc:"用哪个纪要模板"`
	}
}

var regenerateMinutesOp = huma.Operation{
	OperationID: "regenerate-minutes",
	Method:      http.MethodPost,
	Path:        "/api/meetings/{id}/minutes/regenerate",
	Summary:     "用大模型重新生成纪要",
	Description: "会议回到处理中，完成后多出一个新版本，旧版本（包括手动改过的）保留。",
	Tags:        []string{"纪要"},
	Security:    loggedIn,
}

func (h *Handlers) RegenerateMinutes(ctx context.Context, in *RegenerateMinutesInput) (*MeetingOutput, error) {
	id, err := parseID(in.ID)
	if err != nil {
		return nil, err
	}
	m, err := h.svc.Regenerate(ctx, currentUser(ctx), id, in.Body.Template)
	if err != nil {
		return nil, toHTTP(err)
	}
	return &MeetingOutput{Body: toMeeting(m)}, nil
}

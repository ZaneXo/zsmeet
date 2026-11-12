package api

import (
	"strconv"
	"time"

	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/service"
	"github.com/ZaneXo/zsmeet/internal/transcript"
)

// 这个文件是接口的数据结构，也就是前后端的约定。
// 改了以后跑 make gen，接口文档和前端的 TypeScript 类型会一起更新。

type User struct {
	ID    string `json:"id" format:"uuid"`
	Email string `json:"email"`
	Name  string `json:"name"`
	Role  string `json:"role" enum:"admin,member"`
}

func toUser(u sqlc.User) User {
	return User{ID: u.ID.String(), Email: u.Email, Name: u.Name, Role: u.Role}
}

type Meeting struct {
	ID     string `json:"id" format:"uuid"`
	Title  string `json:"title"`
	Source string `json:"source" enum:"record,upload,feishu,desktop" doc:"录音从哪来：网页录音、上传文件、飞书会议、桌面版录音（电脑录音）"`
	Status string `json:"status" enum:"uploading,queued,processing,done,failed"`
	// 只在 status 为 processing 时有意义
	Stage    string  `json:"stage" enum:",models,assemble,transcribe,diarize,minutes" doc:"正在进行的处理步骤"`
	Progress float64 `json:"progress" minimum:"0" maximum:"1" doc:"当前步骤的进度"`
	Error    string  `json:"error" doc:"status 为 failed 时的失败原因，可以直接展示"`

	Template    string `json:"template" doc:"纪要模板的 ID"`
	Hotwords    string `json:"hotwords"`
	DurationSec int    `json:"duration_sec"`
	HasAudio    bool   `json:"has_audio" doc:"录音还在不在（过了保留期会被删除）"`
	// 录音过了保留期被删除的时间；没删时没有这一项
	AudioDeletedAt *time.Time `json:"audio_deleted_at,omitempty" doc:"录音过了保留期被删除的时间，转写和纪要还在。没删时没有这一项"`
	// 0 表示还没有纪要
	MinutesVersion int               `json:"minutes_version" doc:"当前纪要的版本号，0 表示还没有纪要"`
	Visibility     string            `json:"visibility" enum:"team,members,private" doc:"谁能看：团队所有人、指定的人、只有创建者（管理员总能看）"`
	CanEdit        bool              `json:"can_edit" doc:"当前登录的人能不能修改这场会议（改标题、编辑纪要、重新生成、给说话人改名、发到飞书）：创建者、管理员和被指定能编辑的人可以。界面据此决定显不显示编辑入口"`
	CanManage      bool              `json:"can_manage" doc:"当前登录的人能不能删除这场会议、改谁能看谁能编辑：创建者和管理员可以"`
	Marks          []float64         `json:"marks" doc:"网页录音时标记的重点：录音开始后的秒数"`
	Speakers       map[string]string `json:"speakers" nullable:"false" doc:"给说话人起的名字，键是说话人编号（从 0 开始，写成字符串）。没起名字的说话人不在里面，显示成「说话人 N」"`
	AutoSpeakers   []int             `json:"auto_speakers" nullable:"false" doc:"哪几位说话人（编号）的名字是按声音认出、自动起的（声纹）。有人改过名、或者照原样保存过以后就不在里面了"`
	LastChunkAt    *time.Time        `json:"last_chunk_at,omitempty" doc:"最后一次收到录音分片的时间，还没收到过时没有这一项。网页录音进行中时每 10 秒左右更新一次"`
	UploadIdleSec  *int              `json:"upload_idle_sec,omitempty" doc:"等录音传完时（status 为 uploading）多久没收到新的录音分片了，单位秒：从最后一次收到分片算起，还没收到过的从创建会议算起。服务器按自己的时钟算好，客户端不要拿自己的时钟去减 last_chunk_at（两边的时钟可能差好几分钟）。是取会议的那一刻的值，要现在的就再取一次；进度推送（events）不会只因为它变了就推一条。不在等录音时没有这一项"`

	OwnerID   string    `json:"owner_id" format:"uuid"`
	OwnerName string    `json:"owner_name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// toMeeting 把会议转成接口的样子。can_edit、can_manage 是对取出这场会议的人而言的（见 service.Access）。
func toMeeting(m service.Meeting) Meeting {
	speakers := map[string]string{}
	names := transcript.ParseNames(m.Speakers)
	for idx, name := range names {
		speakers[strconv.Itoa(idx)] = name
	}
	auto := []int{}
	for _, idx := range m.AutoSpeakers {
		if names[int(idx)] != "" {
			auto = append(auto, int(idx))
		}
	}
	marks := make([]float64, len(m.Marks))
	for i, t := range m.Marks {
		marks[i] = float64(t)
	}
	var idleSec *int
	if idle, ok := m.UploadIdle(); ok {
		idleSec = new(int(idle / time.Second))
	}
	return Meeting{
		ID:             m.ID.String(),
		Title:          m.Title,
		Source:         m.Source,
		Status:         m.Status,
		Stage:          m.Stage,
		Progress:       float64(m.Progress),
		Error:          m.Error,
		Template:       m.Template,
		Hotwords:       m.Hotwords,
		DurationSec:    int(m.DurationSec),
		HasAudio:       m.AudioKey != "" && m.AudioDeletedAt == nil,
		AudioDeletedAt: m.AudioDeletedAt,
		MinutesVersion: int(m.CurrentVersion),
		Visibility:     m.Visibility,
		CanEdit:        m.Access.Edit,
		CanManage:      m.Access.Manage,
		Marks:          marks,
		Speakers:       speakers,
		AutoSpeakers:   auto,
		LastChunkAt:    m.LastChunkAt,
		UploadIdleSec:  idleSec,
		OwnerID:        m.OwnerID.String(),
		OwnerName:      m.OwnerName,
		CreatedAt:      m.CreatedAt,
		UpdatedAt:      m.UpdatedAt,
	}
}

type Segment struct {
	Start   float64 `json:"start" doc:"开始时间，秒"`
	End     float64 `json:"end" doc:"结束时间，秒"`
	Speaker int     `json:"speaker" doc:"说话人编号，从 0 开始；-1 表示没分出来"`
	Text    string  `json:"text"`
}

func toSegments(segs []transcript.Segment) []Segment {
	out := make([]Segment, len(segs))
	for i, s := range segs {
		out[i] = Segment(s)
	}
	return out
}

type MinutesVersion struct {
	Version   int       `json:"version"`
	Source    string    `json:"source" enum:"auto,edit,regenerate" doc:"自动生成、手动编辑、重新生成"`
	Template  string    `json:"template"`
	Author    string    `json:"author" doc:"编辑或发起重新生成的人；自动生成的为空"`
	CreatedAt time.Time `json:"created_at"`
}

type Minutes struct {
	Version  int              `json:"version" doc:"这份内容的版本号，0 表示还没有纪要"`
	Content  string           `json:"content" doc:"Markdown"`
	Versions []MinutesVersion `json:"versions" doc:"全部版本，新的在前"`
}

func toMinutes(m service.Minutes) Minutes {
	out := Minutes{Version: m.Version, Content: m.Content, Versions: make([]MinutesVersion, len(m.Versions))}
	for i, v := range m.Versions {
		out.Versions[i] = MinutesVersion{
			Version: int(v.Version), Source: v.Source, Template: v.Template, Author: v.Author, CreatedAt: v.CreatedAt,
		}
	}
	return out
}

// MeetingID 是路径里的会议 ID，各个接口的输入结构体嵌入它。
type MeetingID struct {
	ID string `path:"id" format:"uuid"`
}

type MeetingOutput struct {
	Body Meeting
}

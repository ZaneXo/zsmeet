package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// BotName 返回应用机器人的名字。测试连接时用：凭证对、开了机器人能力才拿得到。
func (c *Client) BotName(ctx context.Context) (string, error) {
	var bot struct {
		AppName string `json:"app_name"`
	}
	err := c.call(ctx, request{method: http.MethodGet, path: "/open-apis/bot/v3/info"}, &bot)
	return bot.AppName, err
}

// Chat 是机器人所在的一个群。
type Chat struct {
	ID   string `json:"chat_id"`
	Name string `json:"name"`
}

// 最多列出这么多个群
const maxChats = 1000

// Chats 列出机器人所在的群（最多 1000 个）。没有名字的群用 chat_id 代替。
func (c *Client) Chats(ctx context.Context) ([]Chat, error) {
	out := []Chat{}
	page := ""
	for len(out) < maxChats {
		q := url.Values{"page_size": {"100"}}
		if page != "" {
			q.Set("page_token", page)
		}
		var data struct {
			Items     []Chat `json:"items"`
			HasMore   bool   `json:"has_more"`
			PageToken string `json:"page_token"`
		}
		if err := c.call(ctx, request{method: http.MethodGet, path: "/open-apis/im/v1/chats", query: q}, &data); err != nil {
			return nil, err
		}
		for _, chat := range data.Items {
			if chat.Name == "" {
				chat.Name = chat.ID
			}
			out = append(out, chat)
		}
		if !data.HasMore || data.PageToken == "" {
			break
		}
		page = data.PageToken
	}
	return out, nil
}

// SendCard 以机器人的身份往群里发一张卡片，返回消息 ID。card 是卡片的 JSON（见 Card.JSON）。
// dedup 是去重用的键：一小时内同一个键只会发出一条，任务重试时不会重复发。
func (c *Client) SendCard(ctx context.Context, chatID, card, dedup string) (string, error) {
	body := map[string]string{"receive_id": chatID, "msg_type": "interactive", "content": card}
	if dedup != "" {
		body["uuid"] = dedup
	}
	var data struct {
		MessageID string `json:"message_id"`
	}
	err := c.call(ctx, request{
		method: http.MethodPost, path: "/open-apis/im/v1/messages",
		query: url.Values{"receive_id_type": {"chat_id"}}, body: body,
	}, &data)
	return data.MessageID, err
}

// Doc 是一篇飞书文档。
type Doc struct {
	Token string
	URL   string
}

// 导入文档时一次最多等多久、多久查一次结果
var (
	importWait = 2 * time.Minute
	importPoll = 1500 * time.Millisecond
)

// SetImportTiming 改导入时一次最多等多久、多久查一次，返回改回去的函数。测试用。
func SetImportTiming(wait, poll time.Duration) (restore func()) {
	oldWait, oldPoll := importWait, importPoll
	importWait, importPoll = wait, poll
	return func() { importWait, importPoll = oldWait, oldPoll }
}

// ErrImportPending 表示等了 importWait 飞书还没导入完。导入任务还在，过一会儿用同一个任务号接着查就行，不要重新导入。
var ErrImportPending = errors.New("飞书还在导入文档")

// ImportMarkdown 把 Markdown 导入成一篇飞书文档，放进 folder 文件夹（空字符串是应用自己的云空间），等它导入完。
// 文档归应用所有：调用方还要用 ShareWithChat、ShareInTenant 给人看的权限，不然谁都打不开。
// 中途出错要重试的话，用 StartImport + ImportResult，记下任务号，免得导入两遍。
func (c *Client) ImportMarkdown(ctx context.Context, folder, title, markdown string) (Doc, error) {
	ticket, err := c.StartImport(ctx, folder, title, markdown)
	if err != nil {
		return Doc{}, err
	}
	return c.ImportResult(ctx, ticket)
}

// StartImport 上传 Markdown 并建一个导入任务，返回任务号。导入在飞书那边进行，用 ImportResult 等结果。
func (c *Client) StartImport(ctx context.Context, folder, title, markdown string) (string, error) {
	name := docName(title)
	content := []byte(markdown)
	extra, _ := json.Marshal(map[string]string{"obj_type": "docx", "file_extension": "md"})
	var up struct {
		FileToken string `json:"file_token"`
	}
	err := c.call(ctx, request{
		method: http.MethodPost, path: "/open-apis/drive/v1/medias/upload_all",
		form: &formBody{
			fields: [][2]string{
				{"file_name", name + ".md"},
				{"parent_type", "ccm_import_open"},
				{"size", strconv.Itoa(len(content))},
				{"extra", string(extra)},
			},
			fileName: name + ".md",
			file:     content,
		},
	}, &up)
	if err != nil {
		return "", err
	}
	// 上传的文件 5 分钟内要建导入任务
	var task struct {
		Ticket string `json:"ticket"`
	}
	err = c.call(ctx, request{
		method: http.MethodPost, path: "/open-apis/drive/v1/import_tasks",
		body: map[string]any{
			"file_extension": "md",
			"file_token":     up.FileToken,
			"type":           "docx",
			"file_name":      name,
			"point":          map[string]any{"mount_type": 1, "mount_key": folder},
		},
	}, &task)
	return task.Ticket, err
}

// ImportResult 等导入任务完成，返回导入好的文档。等了 importWait 还没好时返回 ErrImportPending；
// 中途查询遇到网络问题、飞书繁忙时接着查，不当成失败（导入任务本身还在）。
func (c *Client) ImportResult(ctx context.Context, ticket string) (Doc, error) {
	deadline := time.Now().Add(importWait)
	for {
		var res struct {
			Result struct {
				JobStatus   int    `json:"job_status"`
				JobErrorMsg string `json:"job_error_msg"`
				Token       string `json:"token"`
				URL         string `json:"url"`
			} `json:"result"`
		}
		err := c.call(ctx, request{method: http.MethodGet, path: "/open-apis/drive/v1/import_tasks/" + url.PathEscape(ticket)}, &res)
		r := res.Result
		switch {
		case err != nil && !Retryable(err):
			return Doc{}, err
		case err != nil:
			// 查询这一下没成功，导入可能已经好了：接着查
		case r.JobStatus == 0 && r.Token != "":
			doc := Doc{Token: r.Token, URL: r.URL}
			// 链接会显示在网页上、放进卡片里：只要 http(s) 的
			if u, err := url.Parse(r.URL); err != nil || (u.Scheme != "https" && u.Scheme != "http") {
				doc.URL = ""
			}
			return doc, nil
		case r.JobStatus == 0 || r.JobStatus == 1 || r.JobStatus == 2:
			// 还在处理（状态是 0 但还没有 token 时也是）
		default:
			return Doc{}, importError(r.JobStatus, r.JobErrorMsg)
		}
		if time.Now().After(deadline) {
			return Doc{}, ErrImportPending
		}
		select {
		case <-ctx.Done():
			return Doc{}, ctx.Err()
		case <-time.After(importPoll):
		}
	}
}

// importError 把导入任务的失败状态变成说明。
func importError(status int, msg string) error {
	e := &Error{API: "drive/v1/import_tasks", Code: status, Msg: msg}
	switch status {
	case 110, 116:
		e.Msg = "应用没有权限往这个文件夹里放文档：请把文件夹共享给一个有应用机器人的群，并给编辑权限。" + msg
	case 117, 119:
		e.Msg = "文档要放的文件夹被删了或者不存在。" + msg
	case 104:
		e.Msg = "飞书云空间的容量满了。" + msg
	case 105:
		e.Msg = "文件夹里的文件太多了。" + msg
	}
	return e
}

// docName 用标题起文档名：去掉文件名里不能用的字符，太长的截短。
func docName(title string) string {
	clean := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(`\/:*?"<>|`, r) {
			return '_'
		}
		return r
	}, strings.TrimSpace(title))
	if utf8.RuneCountInString(clean) > 100 {
		clean = string([]rune(clean)[:100])
	}
	if clean == "" {
		clean = "会议纪要"
	}
	return clean
}

// ShareWithChat 让群里的人能查看这篇文档。机器人必须在这个群里。
func (c *Client) ShareWithChat(ctx context.Context, docToken, chatID string) error {
	return c.call(ctx, request{
		method: http.MethodPost, path: "/open-apis/drive/v1/permissions/" + url.PathEscape(docToken) + "/members",
		query: url.Values{"type": {"docx"}, "need_notification": {"false"}},
		body:  map[string]string{"member_type": "openchat", "member_id": chatID, "perm": "view", "type": "chat"},
	}, nil)
}

// ShareWithUser 让飞书里邮箱是 email 的人能管理这篇文档（查看、编辑、再分享给别人）。
func (c *Client) ShareWithUser(ctx context.Context, docToken, email string) error {
	return c.call(ctx, request{
		method: http.MethodPost, path: "/open-apis/drive/v1/permissions/" + url.PathEscape(docToken) + "/members",
		query: url.Values{"type": {"docx"}, "need_notification": {"false"}},
		body:  map[string]string{"member_type": "email", "member_id": email, "perm": "full_access", "type": "user"},
	}, nil)
}

// ShareInTenant 让公司里拿到链接的人都能查看这篇文档（公司外的人看不了）。
func (c *Client) ShareInTenant(ctx context.Context, docToken string) error {
	return c.call(ctx, request{
		method: http.MethodPatch, path: "/open-apis/drive/v2/permissions/" + url.PathEscape(docToken) + "/public",
		query: url.Values{"type": {"docx"}},
		body:  map[string]string{"link_share_entity": "tenant_readable"},
	}, nil)
}

// RootFolder 返回应用自己的云空间根目录。
func (c *Client) RootFolder(ctx context.Context) (string, error) {
	var data struct {
		Token string `json:"token"`
	}
	err := c.call(ctx, request{method: http.MethodGet, path: "/open-apis/drive/explorer/v2/root_folder/meta"}, &data)
	return data.Token, err
}

// Folder 返回 parent 下面叫 name 的文件夹，没有就建一个。
func (c *Client) Folder(ctx context.Context, parent, name string) (string, error) {
	page := ""
	for range 50 {
		q := url.Values{"folder_token": {parent}, "page_size": {"200"}}
		if page != "" {
			q.Set("page_token", page)
		}
		var data struct {
			Files []struct {
				Token string `json:"token"`
				Name  string `json:"name"`
				Type  string `json:"type"`
			} `json:"files"`
			HasMore       bool   `json:"has_more"`
			NextPageToken string `json:"next_page_token"`
		}
		if err := c.call(ctx, request{method: http.MethodGet, path: "/open-apis/drive/v1/files", query: q}, &data); err != nil {
			return "", err
		}
		for _, f := range data.Files {
			if f.Type == "folder" && f.Name == name {
				return f.Token, nil
			}
		}
		if !data.HasMore || data.NextPageToken == "" {
			break
		}
		page = data.NextPageToken
	}
	var created struct {
		Token string `json:"token"`
	}
	err := c.call(ctx, request{
		method: http.MethodPost, path: "/open-apis/drive/v1/files/create_folder",
		body: map[string]string{"name": name, "folder_token": parent},
	}, &created)
	return created.Token, err
}

// 飞书会议的录制还在生成、妙记还在转写时返回的错误码，过一会儿再查
const (
	CodeRecordingProcessing = 124002
	CodeNoRecording         = 121004
	CodeMinuteNotReady      = 2091003
)

// Recording 返回飞书会议云录制的妙记链接。meetingID 是会议 ID（不是 9 位会议号）。
// 录制还在生成时返回错误码 CodeRecordingProcessing，没有录制时是 CodeNoRecording。
func (c *Client) Recording(ctx context.Context, meetingID string) (string, error) {
	var data struct {
		Recording struct {
			URL string `json:"url"`
		} `json:"recording"`
	}
	err := c.call(ctx, request{method: http.MethodGet, path: "/open-apis/vc/v1/meetings/" + url.PathEscape(meetingID) + "/recording"}, &data)
	if err == nil && data.Recording.URL == "" {
		err = &Error{API: "vc/v1/meetings/recording", Code: CodeNoRecording, Msg: "没有录制"}
	}
	return data.Recording.URL, err
}

// MinuteToken 从妙记链接（https://xxx.feishu.cn/minutes/obcn...）里取出妙记的 token。
func MinuteToken(link string) (string, error) {
	u, err := url.Parse(link)
	if err != nil {
		return "", err
	}
	_, token, ok := strings.Cut(u.Path, "/minutes/")
	token = strings.Trim(token, "/")
	if !ok || token == "" || strings.Contains(token, "/") {
		return "", fmt.Errorf("看不懂飞书返回的妙记链接：%s", link)
	}
	return token, nil
}

// MediaURL 返回妙记音视频文件的下载地址（一天内有效）。妙记还没生成好时返回错误码 CodeMinuteNotReady。
func (c *Client) MediaURL(ctx context.Context, minuteToken string) (string, error) {
	var data struct {
		DownloadURL string `json:"download_url"`
	}
	err := c.call(ctx, request{method: http.MethodGet, path: "/open-apis/minutes/v1/minutes/" + url.PathEscape(minuteToken) + "/media"}, &data)
	if err == nil && data.DownloadURL == "" {
		err = &Error{API: "minutes/v1/minutes/media", Code: CodeMinuteNotReady, Msg: "还没有下载地址"}
	}
	return data.DownloadURL, err
}

// User 是通讯录里的一个人。
type User struct {
	Name            string `json:"name"`
	Email           string `json:"email"`
	EnterpriseEmail string `json:"enterprise_email"`
}

// User 按 open_id 查一个人的名字和邮箱。
func (c *Client) User(ctx context.Context, openID string) (User, error) {
	var data struct {
		User User `json:"user"`
	}
	err := c.call(ctx, request{
		method: http.MethodGet, path: "/open-apis/contact/v3/users/" + url.PathEscape(openID),
		query: url.Values{"user_id_type": {"open_id"}},
	}, &data)
	return data.User, err
}

// ErrTooLarge 表示要下载的文件超过了上限。
var ErrTooLarge = errors.New("文件太大")

// Download 把下载地址的内容写进 w，返回写了多少字节；超过 maxBytes 时返回 ErrTooLarge。
// 地址是飞书给的带签名的链接，不需要凭证，拿到它就能下载，所以报错里不带它的签名部分。
// 文件可能很大，不设整体超时，由 ctx 控制。最多跟 5 次跳转，不允许跳到 http 地址
// （开放平台的地址本身是 http 时除外，那是测试或者内网的私有部署）。
func (c *Client) Download(ctx context.Context, link string, w io.Writer, maxBytes int64) (int64, error) {
	plainOK := strings.HasPrefix(c.base, "http://")
	u, err := url.Parse(link)
	if err != nil {
		return 0, errors.New("看不懂飞书给的下载地址")
	}
	if u.Scheme != "https" && !(plainOK && u.Scheme == "http") {
		return 0, errors.New("飞书给的下载地址不是 https 的")
	}
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("跳转太多次")
		}
		if req.URL.Scheme != "https" && !plainOK {
			return errors.New("不允许跳转到 http 地址")
		}
		return nil
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		// 报错里的地址只留到域名，签名不能进日志、数据库和设置页面
		var ue *url.Error
		if errors.As(err, &ue) {
			host := ""
			if du, perr := url.Parse(ue.URL); perr == nil && du.Host != "" {
				host = du.Scheme + "://" + du.Host
			}
			ue.URL = host
		}
		return 0, fmt.Errorf("下载飞书会议的录制失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("下载飞书会议的录制失败：HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxBytes {
		return 0, ErrTooLarge
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return n, fmt.Errorf("下载飞书会议的录制失败：%w", err)
	}
	if n > maxBytes {
		return n, ErrTooLarge
	}
	return n, nil
}

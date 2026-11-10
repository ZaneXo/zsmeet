package feishu

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ZaneXo/zsmeet/internal/feishu/feishutest"
)

func newClient(t *testing.T) (*Client, *feishutest.Server) {
	t.Helper()
	fs := feishutest.New(t)
	return New(fs.URL, feishutest.AppID, feishutest.AppSecret), fs
}

func TestBotNameChatsAndToken(t *testing.T) {
	c, fs := newClient(t)
	fs.Chats = [][2]string{{"oc_1", "产品组"}, {"oc_2", "研发群"}, {"oc_3", ""}, {"oc_4", "周会"}, {"oc_5", "全员"}}

	name, err := c.BotName(t.Context())
	if err != nil || name != "会议助手" {
		t.Fatalf("机器人名字：%q %v", name, err)
	}
	chats, err := c.Chats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(chats) != 5 || chats[2] != (Chat{ID: "oc_3", Name: "oc_3"}) || chats[4].Name != "全员" {
		t.Errorf("要翻页列出全部的群，没有名字的用 chat_id：%+v", chats)
	}
	if n := fs.Count("POST /open-apis/auth/v3/tenant_access_token/internal"); n != 1 {
		t.Errorf("访问凭证要缓存起来，只换一次，实际换了 %d 次", n)
	}

	wrong := New(fs.URL, feishutest.AppID, "wrong-secret")
	if _, err := wrong.BotName(t.Context()); err == nil || !strings.Contains(err.Error(), "App ID 或 App Secret 不对") {
		t.Errorf("凭证不对时要说清楚：%v", err)
	}
}

func TestSendCard(t *testing.T) {
	c, fs := newClient(t)
	card := `{"schema":"2.0","body":{"elements":[]}}`
	id, err := c.SendCard(t.Context(), "oc_1", card, "share-1")
	if err != nil || id == "" {
		t.Fatalf("发卡片：%q %v", id, err)
	}
	// 重试时同一个去重键不会再发一条
	if _, err := c.SendCard(t.Context(), "oc_1", card, "share-1"); err != nil {
		t.Fatal(err)
	}
	if sent := fs.Sent(); len(sent) != 1 || sent[0].ChatID != "oc_1" || sent[0].Content != card {
		t.Errorf("卡片的 JSON 要原样放在 content 里，同一个去重键只发一条：%+v", sent)
	}

	fs.FailChats["oc_out"] = 230002
	_, err = c.SendCard(t.Context(), "oc_out", card, "")
	if err == nil || !strings.Contains(err.Error(), "机器人不在这个群里") || Retryable(err) {
		t.Errorf("机器人不在群里：要说清楚，而且不用重试：%v", err)
	}
	fs.FailChats["oc_busy"] = 230020
	if _, err := c.SendCard(t.Context(), "oc_busy", card, ""); !Retryable(err) {
		t.Errorf("发得太频繁要重试：%v", err)
	}
}

func TestImportMarkdownAndShare(t *testing.T) {
	importPoll = 10 * time.Millisecond
	c, fs := newClient(t)
	fs.ImportPolls = 1
	doc, err := c.ImportMarkdown(t.Context(), "fldX", "产品周会/11月", "## 摘要\n好。\n")
	if err != nil {
		t.Fatal(err)
	}
	docs := fs.Docs()
	if len(docs) != 1 || doc.Token != docs[0].Token || !strings.HasSuffix(doc.URL, "/docx/"+doc.Token) {
		t.Fatalf("导入的文档：%+v %+v", doc, docs)
	}
	if d := docs[0]; d.Folder != "fldX" || d.Name != "产品周会_11月" || d.Markdown != "## 摘要\n好。\n" {
		t.Errorf("放进指定的文件夹，名字里不能有斜杠，内容原样：%+v", d)
	}
	if n := fs.Count("GET /open-apis/drive/v1/import_tasks/*"); n != 2 {
		t.Errorf("导入还在处理时要接着查，查了 %d 次", n)
	}

	// 等太久：返回「还在导入」，之后用同一个任务号接着查，不会再导入一篇
	importWait = 0
	ticket, err := c.StartImport(t.Context(), "", "周会", "## 摘要\n好。\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ImportResult(t.Context(), ticket); !errors.Is(err, ErrImportPending) {
		t.Fatalf("还没导入完：%v", err)
	}
	importWait = time.Minute
	resumed, err := c.ImportResult(t.Context(), ticket)
	if err != nil || resumed.Token == "" || len(fs.Docs()) != 2 || fs.Count("POST /open-apis/drive/v1/import_tasks") != 2 {
		t.Errorf("用同一个任务号接着等：%+v %v，导入了 %d 次", resumed, err, fs.Count("POST /open-apis/drive/v1/import_tasks"))
	}

	if err := c.ShareWithChat(t.Context(), doc.Token, "oc_1"); err != nil {
		t.Fatal(err)
	}
	if err := c.ShareInTenant(t.Context(), doc.Token); err != nil {
		t.Fatal(err)
	}
	if len(fs.Grants) != 1 || fs.Grants[0] != (feishutest.Grant{Doc: doc.Token, MemberType: "openchat", MemberID: "oc_1", Perm: "view"}) || len(fs.PublicDocs) != 1 {
		t.Errorf("群里的人能看、公司里拿到链接的人能看：%+v %v", fs.Grants, fs.PublicDocs)
	}

	root, err := c.RootFolder(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	first, err := c.Folder(t.Context(), root, "2026-11")
	if err != nil {
		t.Fatal(err)
	}
	again, err := c.Folder(t.Context(), root, "2026-11")
	if err != nil || again != first || fs.Count("POST /open-apis/drive/v1/files/create_folder") != 1 {
		t.Errorf("同名的文件夹只建一次：%q %q %v", first, again, err)
	}
}

func TestRecordingMediaAndUser(t *testing.T) {
	c, fs := newClient(t)
	ctx := t.Context()
	if _, err := c.Recording(ctx, "m-none"); !IsCode(err, CodeNoRecording) {
		t.Errorf("没有录制：%v", err)
	}
	fs.Recordings["m1"] = &feishutest.Recording{MinuteToken: "obcnminute000000000000001", Processing: 1}
	if _, err := c.Recording(ctx, "m1"); !IsCode(err, CodeRecordingProcessing) {
		t.Errorf("录制还在生成：%v", err)
	}
	link, err := c.Recording(ctx, "m1")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := MinuteToken(link)
	if err != nil || tok != "obcnminute000000000000001" {
		t.Fatalf("从妙记链接取 token：%q %v", tok, err)
	}

	fs.Media[tok] = &feishutest.Media{Data: []byte("RIFF-audio"), NotReady: 1}
	if _, err := c.MediaURL(ctx, tok); !IsCode(err, CodeMinuteNotReady) {
		t.Errorf("妙记还没生成好：%v", err)
	}
	media, err := c.MediaURL(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if n, err := c.Download(ctx, media, &buf, 100); err != nil || n != 10 || buf.String() != "RIFF-audio" {
		t.Errorf("下载：%d %v %q", n, err, buf.String())
	}
	if _, err := c.Download(ctx, media, io.Discard, 5); !errors.Is(err, ErrTooLarge) {
		t.Errorf("超过上限要停下：%v", err)
	}
	// 下载出错时报错里不能有带签名的地址（拿到它就能下载录制）
	_, err = c.Download(ctx, "http://127.0.0.1:1/media.mp4?Expires=1&Signature=SECRETSIG", io.Discard, 100)
	if err == nil || strings.Contains(err.Error(), "SECRETSIG") || !Retryable(err) {
		t.Errorf("报错里不能带签名，连不上要能重试：%v", err)
	}
	fs.Media["denied"] = &feishutest.Media{Deny: true}
	if _, err := c.MediaURL(ctx, "denied"); err == nil || !strings.Contains(err.Error(), "妙记的数据范围设为全员") || Retryable(err) {
		t.Errorf("没有妙记权限：%v", err)
	}
	// 线上的开放平台是 https，下载地址不能是 http
	online := New("https://open.feishu.cn", "x", "y")
	if _, err := online.Download(ctx, "http://example.com/a.mp4", &buf, 100); err == nil {
		t.Error("下载地址是 http 时不该下载")
	}

	fs.Users["ou_1"] = feishutest.User{Name: "张敏", Email: "zhangmin@example.com"}
	u, err := c.User(ctx, "ou_1")
	if err != nil || u.Email != "zhangmin@example.com" || u.Name != "张敏" {
		t.Errorf("通讯录：%+v %v", u, err)
	}
	if _, err := c.User(ctx, "ou_unknown"); err == nil || !strings.Contains(err.Error(), "通讯录") {
		t.Errorf("读不到通讯录时要说清楚：%v", err)
	}
}

func TestParseMeetingEnded(t *testing.T) {
	body := []byte(`{"schema":"2.0","header":{"event_id":"ev1","event_type":"vc.meeting.all_meeting_ended_v1"},
		"event":{"meeting":{"id":"6911188411934433028","topic":"产品周会","meeting_no":"235812466",
		"start_time":"1762740000","end_time":"1762742700",
		"host_user":{"id":{"open_id":"ou_host"}},"owner":{"id":{"open_id":"ou_owner"}}}}}`)
	ev, err := ParseMeetingEnded(body)
	if err != nil {
		t.Fatal(err)
	}
	want := MeetingEnded{EventID: "ev1", MeetingID: "6911188411934433028", Topic: "产品周会", Start: 1762740000, End: 1762742700, OwnerOpenID: "ou_owner"}
	if ev != want {
		t.Errorf("得到 %+v\n想要 %+v", ev, want)
	}
	if _, err := ParseMeetingEnded([]byte(`{"event":{}}`)); err == nil {
		t.Error("没有会议 ID 的事件要报错")
	}
}

const sampleMinutes = `## 摘要
本次会议<at id=all></at>讨论了**飞书**集成，[00:15] 定了下一个版本做。

## 结论
- 飞书集成放到下一个版本

## 待办
- **王悦**：调研飞书集成 [00:15]
- 李航：周三前确认权限
`

// cardText 把卡片里的 Markdown 和按钮链接拼成一个字符串，方便检查。
func cardText(t *testing.T, card string) (markdown, header string, buttons []string) {
	t.Helper()
	var c struct {
		Schema string `json:"schema"`
		Header struct {
			Title    struct{ Content string } `json:"title"`
			Subtitle struct{ Content string } `json:"subtitle"`
			Template string                   `json:"template"`
		} `json:"header"`
		Body struct {
			Elements []struct {
				Tag     string `json:"tag"`
				Content string `json:"content"`
				Columns []struct {
					Elements []struct {
						Text      struct{ Content string } `json:"text"`
						Behaviors []struct {
							DefaultURL string `json:"default_url"`
						} `json:"behaviors"`
					} `json:"elements"`
				} `json:"columns"`
			} `json:"elements"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(card), &c); err != nil || c.Schema != "2.0" {
		t.Fatalf("卡片不是合法的 JSON 2.0：%v\n%s", err, card)
	}
	for _, e := range c.Body.Elements {
		markdown += e.Content
		for _, col := range e.Columns {
			for _, b := range col.Elements {
				buttons = append(buttons, b.Text.Content+" "+b.Behaviors[0].DefaultURL)
			}
		}
	}
	return markdown, c.Header.Title.Content + "|" + c.Header.Subtitle.Content + "|" + c.Header.Template, buttons
}

func TestCard(t *testing.T) {
	card := Card{
		Title: "产品周会", When: time.Date(2026, 11, 10, 14, 0, 0, 0, time.Local), Duration: 2700, Owner: "张敏",
		Minutes: sampleMinutes, MeetingURL: "https://zsmeet.example.com/meetings/1", DocURL: "https://example.feishu.cn/docx/doxcn1",
	}
	out, err := card.JSON()
	if err != nil {
		t.Fatal(err)
	}
	md, header, buttons := cardText(t, out)
	if header != "会议纪要｜产品周会|11月10日 14:00 · 45 分钟 · 张敏|blue" {
		t.Errorf("标题：%s", header)
	}
	for _, want := range []string{"**摘要**", "&#60;at id=all&#62;&#60;/at&#62;", "飞书集成，&#91;00:15&#93;", "**待办**\n- 王悦：调研飞书集成 &#91;00:15&#93;\n- 李航：周三前确认权限"} {
		if !strings.Contains(md, want) {
			t.Errorf("卡片内容里缺少 %q：\n%s", want, md)
		}
	}
	if strings.Contains(md, "<at") || strings.Contains(md, "**飞书**") {
		t.Errorf("纪要里的文字要转义，不能变成 @ 所有人或者格式：\n%s", md)
	}
	if len(buttons) != 2 || buttons[0] != "查看纪要和录音 https://zsmeet.example.com/meetings/1" || buttons[1] != "飞书文档 https://example.feishu.cn/docx/doxcn1" {
		t.Errorf("按钮：%v", buttons)
	}

	card.Updated, card.DocURL = true, ""
	out, _ = card.JSON()
	if _, header, buttons := cardText(t, out); !strings.Contains(header, "纪要已更新") || !strings.HasSuffix(header, "|orange") || len(buttons) != 1 {
		t.Errorf("更新的卡片：%s %v", header, buttons)
	}
}

// 纪要很长时卡片也不能超过飞书的大小限制：少列待办、缩短摘要。
func TestCardStaysSmall(t *testing.T) {
	var b strings.Builder
	b.WriteString("## 摘要\n" + strings.Repeat("这是一段很长的摘要。", 800) + "\n\n## 待办\n")
	for range 50 {
		b.WriteString("- " + strings.Repeat("待办事项", 60) + "\n")
	}
	out, err := Card{Title: "长会", When: time.Now(), Minutes: b.String(), MeetingURL: "https://x/1"}.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > cardMaxBytes {
		t.Errorf("卡片有 %d 字节，超过了 %d", len(out), cardMaxBytes)
	}
	md, _, _ := cardText(t, out)
	if !strings.Contains(md, "还有 42 条，见完整纪要") || strings.Count(md, "\n- ") != 9 {
		t.Errorf("最多列 8 条待办，再说一声还有几条：\n%s", md[len(md)-200:])
	}
}

func TestSummaryAndTodos(t *testing.T) {
	if got := Summary(sampleMinutes); got != "本次会议<at id=all></at>讨论了飞书集成，[00:15] 定了下一个版本做。" {
		t.Errorf("摘要：%q", got)
	}
	// 没有摘要一节时取第一段
	if got := Summary("## 各人进展\n- 张敏：完成了录音\n\n## 风险\n无"); got != "张敏：完成了录音" {
		t.Errorf("没有摘要时取第一段：%q", got)
	}
	// 待办写成表格
	table := "## 我方承诺的跟进事项\n| 事项 | 负责人 | 截止 |\n|---|---|---|\n| 发报价 | **王悦** | 周五 |\n| 约下次会议 | 李航 | 未定 |\n\n## 其他\n- 无"
	if got := Todos(table); len(got) != 2 || got[0] != "发报价 · 王悦 · 周五" || got[1] != "约下次会议 · 李航 · 未定" {
		t.Errorf("表格写的待办：%q", got)
	}
	if got := Todos("## 待办\n- 无\n"); len(got) != 0 {
		t.Errorf("「无」不算待办：%q", got)
	}
}

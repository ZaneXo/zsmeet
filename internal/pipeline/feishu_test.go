package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/feishu"
	"github.com/ZaneXo/zsmeet/internal/feishu/feishutest"
	"github.com/ZaneXo/zsmeet/internal/queue"
	"github.com/ZaneXo/zsmeet/internal/settings"
)

// feishu 起一个假的飞书开放平台并保存飞书配置。
func (e *env) feishu(cfg settings.Feishu) *feishutest.Server {
	e.t.Helper()
	fs := feishutest.New(e.t)
	fs.Chats = [][2]string{{"oc_team", "产品组"}}
	e.w.feishuBase = fs.URL
	cfg.Domain, cfg.AppID, cfg.AppSecret = "feishu", feishutest.AppID, feishutest.AppSecret
	if err := e.w.settings.SaveFeishu(e.t.Context(), cfg); err != nil {
		e.t.Fatal(err)
	}
	return fs
}

// jobs 返回队列里某种任务的参数（JSON），按加入的先后。
func (e *env) jobs(kind string) []string {
	e.t.Helper()
	rows, err := e.pool.Query(e.t.Context(), "SELECT args::text FROM river_job WHERE kind = $1 ORDER BY id", kind)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var args string
		if err := rows.Scan(&args); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, args)
	}
	return out
}

func jobOf[T river.JobArgs](args T, attempt int) *river.Job[T] {
	return &river.Job[T]{JobRow: &rivertype.JobRow{Attempt: attempt, MaxAttempts: 6, CreatedAt: time.Now()}, Args: args}
}

func (e *env) shares(meeting uuid.UUID) []sqlc.ListSharesRow {
	e.t.Helper()
	rows, err := e.q.ListShares(e.t.Context(), meeting)
	if err != nil {
		e.t.Fatal(err)
	}
	return rows
}

// 纪要第一次生成后自动发到默认的群：先导入成飞书文档、给群和公司里的人看的权限，再发卡片。重新生成的不再自动发。
func TestAutoPushAfterFirstMinutes(t *testing.T) {
	e := newEnv(t)
	e.llm(nil)
	fs := e.feishu(settings.Feishu{
		SiteURL: "https://meet.example.com", ChatID: "oc_team", ChatName: "产品组",
		AutoPush: true, WithDoc: true, LinkShare: true,
	})
	args := e.upload(wav())
	if err := e.work(args); err != nil {
		t.Fatal(err)
	}
	shares := e.shares(args.MeetingID)
	if len(shares) != 1 || shares[0].FeishuShare.Status != "pending" || shares[0].FeishuShare.ChatID != "oc_team" || !shares[0].FeishuShare.WithDoc {
		t.Fatalf("生成纪要后要记下一次自动发送：%+v", shares)
	}
	if jobs := e.jobs("feishu_share"); len(jobs) != 1 || !strings.Contains(jobs[0], shares[0].FeishuShare.ID.String()) {
		t.Fatalf("要加一个发送的后台任务：%v", jobs)
	}

	sw := &shareWorker{w: e.w}
	if err := sw.Work(t.Context(), jobOf(queue.FeishuShareArgs{ShareID: shares[0].FeishuShare.ID}, 1)); err != nil {
		t.Fatal(err)
	}
	share := e.shares(args.MeetingID)[0].FeishuShare
	if share.Status != "done" || share.MessageID == "" || share.DocToken == "" || share.Error != "" {
		t.Fatalf("发送完成后记下消息和文档：%+v", share)
	}
	docs := fs.Docs()
	if len(docs) != 1 || docs[0].Name != "产品周会" || !strings.Contains(docs[0].Markdown, "一期只做网页端") ||
		!strings.Contains(docs[0].Markdown, "https://meet.example.com/meetings/"+args.MeetingID.String()) {
		t.Fatalf("纪要导入成文档，文档名是会议标题，带上会议的链接：%+v", docs)
	}
	month := e.meeting(args.MeetingID).CreatedAt.Local().Format("2006-01")
	if docs[0].Folder != fs.Folders["fldroot"][month] {
		t.Errorf("文档放在应用云空间里按月份建的文件夹里：%q %v", docs[0].Folder, fs.Folders)
	}
	if len(fs.Grants) != 1 || fs.Grants[0].MemberID != "oc_team" || fs.Grants[0].Perm != "view" || len(fs.PublicDocs) != 1 {
		t.Errorf("群里的人能看、公司里拿到链接的人能看：%+v %v", fs.Grants, fs.PublicDocs)
	}
	sent := fs.Sent()
	if len(sent) != 1 || sent[0].ChatID != "oc_team" || sent[0].UUID != share.ID.String() ||
		!strings.Contains(sent[0].Content, "https://meet.example.com/meetings/"+args.MeetingID.String()) ||
		!strings.Contains(sent[0].Content, share.DocUrl) || !strings.Contains(sent[0].Content, "会议纪要｜产品周会") {
		t.Fatalf("卡片发到默认的群，带着会议和文档的链接：%+v", sent)
	}

	// 重新生成纪要不自动发
	if err := e.work(e.enqueue(args.MeetingID, "done")); err != nil {
		t.Fatal(err)
	}
	if n := len(e.shares(args.MeetingID)); n != 1 {
		t.Errorf("重新生成的纪要不该自动发，现在有 %d 条发送记录", n)
	}
}

// 发送失败：重试也没用的（机器人不在群里）记下原因不再重试；飞书繁忙的交给 River 重试。
func TestShareFailures(t *testing.T) {
	e := newEnv(t)
	e.llm(nil)
	fs := e.feishu(settings.Feishu{SiteURL: "https://meet.example.com"})
	args := e.upload(wav())
	if err := e.work(args); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	newShare := func(chat string) sqlc.FeishuShare {
		t.Helper()
		s, err := e.q.CreateShare(ctx, sqlc.CreateShareParams{MeetingID: args.MeetingID, ChatID: chat, ChatName: "群", MinutesVersion: 1})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	sw := &shareWorker{w: e.w}

	fs.FailChats["oc_out"] = 230002
	out := newShare("oc_out")
	err := sw.Work(ctx, jobOf(queue.FeishuShareArgs{ShareID: out.ID}, 1))
	if !errors.Is(err, &rivertype.JobCancelError{}) {
		t.Errorf("机器人不在群里，重试也没用：%v", err)
	}
	if s, _ := e.q.GetShare(ctx, out.ID); s.Status != "failed" || !strings.Contains(s.Error, "机器人不在这个群里") {
		t.Errorf("要记下失败的原因：%+v", s)
	}

	fs.FailChats["oc_busy"] = 230020
	busy := newShare("oc_busy")
	err = sw.Work(ctx, jobOf(queue.FeishuShareArgs{ShareID: busy.ID}, 1))
	if err == nil || errors.Is(err, &rivertype.JobCancelError{}) {
		t.Errorf("飞书繁忙时交给 River 稍后重试：%v", err)
	}
	if s, _ := e.q.GetShare(ctx, busy.ID); s.Status != "pending" || !strings.Contains(s.Error, "第 1 次没发出去，10 秒后自动重试") {
		t.Errorf("重试之前还是发送中，写着为什么、多久以后重试：%+v", s)
	}
	// 最后一次也失败：记下原因
	if err := sw.Work(ctx, jobOf(queue.FeishuShareArgs{ShareID: busy.ID}, 6)); !errors.Is(err, &rivertype.JobCancelError{}) {
		t.Errorf("重试次数用完就不再重试：%v", err)
	}
	if s, _ := e.q.GetShare(ctx, busy.ID); s.Status != "failed" {
		t.Errorf("重试次数用完要记成失败：%+v", s)
	}

	// 发送中超过半小时（网页上已经允许重新发了）：这一条不再发，免得群里收到两张
	stale := newShare("oc_team")
	if _, err := e.pool.Exec(ctx, "UPDATE feishu_shares SET created_at = now() - interval '1 hour' WHERE id = $1", stale.ID); err != nil {
		t.Fatal(err)
	}
	sentBefore := len(fs.Sent())
	if err := sw.Work(ctx, jobOf(queue.FeishuShareArgs{ShareID: stale.ID}, 1)); !errors.Is(err, &rivertype.JobCancelError{}) {
		t.Errorf("太久的不再发：%v", err)
	}
	if s, _ := e.q.GetShare(ctx, stale.ID); s.Status != "failed" || len(fs.Sent()) != sentBefore {
		t.Errorf("太久的记成失败、不发卡片：%+v", s)
	}

	// 管理员把飞书的配置清掉了
	if err := e.w.settings.SaveFeishu(ctx, settings.Feishu{Domain: "feishu"}); err != nil {
		t.Fatal(err)
	}
	gone := newShare("oc_team")
	if err := sw.Work(ctx, jobOf(queue.FeishuShareArgs{ShareID: gone.ID}, 1)); !errors.Is(err, &rivertype.JobCancelError{}) {
		t.Errorf("配置清掉了，不再重试：%v", err)
	}
}

// 导入飞书会议的录制：组织者是成员时等录制生成好、下载、建成他名下的会议并排队转写；不是成员的不导入、不记主题。
func TestImportFeishuRecording(t *testing.T) {
	e := newEnv(t)
	fs := e.feishu(settings.Feishu{ImportRecordings: true})
	ctx := t.Context()
	fs.Users["ou_owner"] = feishutest.User{Name: "张敏", Email: strings.ToUpper(e.user.Email)}
	fs.Users["ou_stranger"] = feishutest.User{Name: "外人", Email: "someone@else.example"}
	fs.Recordings["m1"] = &feishutest.Recording{MinuteToken: "obcnminutetoken0000000001", Processing: 1}
	fs.Media["obcnminutetoken0000000001"] = &feishutest.Media{Data: wav(), NotReady: 1}

	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	iw := &importWorker{w: e.w}
	args := queue.FeishuRecordingArgs{MeetingID: "m1", Topic: "产品评审", Start: start.Unix(), End: start.Add(40 * time.Minute).Unix(), OwnerOpenID: "ou_owner"}
	var snooze *rivertype.JobSnoozeError
	for i, what := range []string{"录制还在生成", "妙记还没生成好"} {
		if err := iw.Work(ctx, jobOf(args, 1)); !errors.As(err, &snooze) {
			t.Fatalf("第 %d 次：%s时过一会儿再来：%v", i+1, what, err)
		}
	}
	if err := iw.Work(ctx, jobOf(args, 1)); err != nil {
		t.Fatal(err)
	}
	imp, err := e.q.GetFeishuImport(ctx, "m1")
	if err != nil || imp.Status != "imported" || imp.MeetingID == nil || imp.OwnerID != e.user.ID {
		t.Fatalf("要记下已经导入：%+v %v", imp, err)
	}
	m := e.meeting(*imp.MeetingID)
	if m.Source != "feishu" || m.Title != "产品评审" || m.OwnerID != e.user.ID || m.Status != "queued" || !m.CreatedAt.Equal(start) {
		t.Fatalf("导入的会议：%+v", m)
	}
	if jobs := e.jobs("process_meeting"); len(jobs) != 1 || !strings.Contains(jobs[0], m.ID.String()) {
		t.Fatalf("要排队处理：%v", jobs)
	}
	// 下载的录制照常转写
	if err := e.work(queue.ProcessArgs{MeetingID: m.ID, RunID: *m.RunID}); err != nil {
		t.Fatal(err)
	}
	if m := e.meeting(m.ID); m.Status != "done" || m.DurationSec != 2 {
		t.Errorf("导入的录制要转写完：%+v", m)
	}
	if entries, _ := os.ReadDir(e.w.store.Path("tmp")); len(entries) != 0 {
		t.Errorf("下载用的临时文件要清掉：%v", entries)
	}

	// 同一场会议的事件又推了一次：不再导入
	if err := iw.Work(ctx, jobOf(args, 1)); err != nil {
		t.Fatal(err)
	}
	if n := len(e.jobs("process_meeting")); n != 1 {
		t.Errorf("同一场会议只导入一次，现在排了 %d 个处理任务", n)
	}
	// 彻底删除导入的会议：导入记录留着（不会再导入一次），但主题清掉
	if _, err := e.pool.Exec(ctx, "UPDATE meetings SET deleted_at = now() WHERE id = $1", m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.q.PurgeMeeting(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	if imp, err := e.q.GetFeishuImport(ctx, "m1"); err != nil || imp.Topic != "" || imp.Status != "imported" {
		t.Errorf("彻底删除后导入记录里不留主题：%+v %v", imp, err)
	}
	if link, err := e.q.GetFeishuLink(ctx); err != nil || strings.Contains(link.LastEvent, "产品评审") {
		t.Errorf("设置页面上「最近一次」也不能再有主题：%+v %v", link, err)
	}
	if err := iw.Work(ctx, jobOf(args, 1)); err != nil || len(e.jobs("process_meeting")) != 1 {
		t.Errorf("删掉的会议也不会再导入一次：%v", err)
	}

	// 组织者不是成员：不导入，也不记主题
	other := queue.FeishuRecordingArgs{MeetingID: "m2", Topic: "别人的机密会议", OwnerOpenID: "ou_stranger", End: time.Now().Unix()}
	if err := iw.Work(ctx, jobOf(other, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.q.GetFeishuImport(ctx, "m2"); err == nil {
		t.Error("组织者不是成员的会议不该记下来")
	}
	link, err := e.q.GetFeishuLink(ctx)
	if err != nil || strings.Contains(link.LastEvent, "机密") || !strings.Contains(link.LastEvent, "不是 zsmeet 的成员") {
		t.Errorf("最近的情况要说明没导入的原因，但不能带别人会议的主题：%+v %v", link, err)
	}

	// 早就结束、一直查不到录制：算没有录制
	old := queue.FeishuRecordingArgs{MeetingID: "m3", Topic: "没录的会", OwnerOpenID: "ou_owner", End: time.Now().Add(-2 * time.Hour).Unix()}
	if err := iw.Work(ctx, jobOf(old, 1)); err != nil {
		t.Fatal(err)
	}
	if imp, err := e.q.GetFeishuImport(ctx, "m3"); err != nil || imp.Status != "skipped" {
		t.Errorf("没有云录制的会议记成跳过：%+v %v", imp, err)
	}

	// 没有妙记权限：记下原因，不再重试
	fs.Recordings["m4"] = &feishutest.Recording{MinuteToken: "denied"}
	fs.Media["denied"] = &feishutest.Media{Deny: true}
	denied := queue.FeishuRecordingArgs{MeetingID: "m4", Topic: "没权限", OwnerOpenID: "ou_owner", End: time.Now().Unix()}
	if err := iw.Work(ctx, jobOf(denied, 1)); !errors.Is(err, &rivertype.JobCancelError{}) {
		t.Errorf("没有妙记权限，不再重试：%v", err)
	}
	if imp, err := e.q.GetFeishuImport(ctx, "m4"); err != nil || imp.Status != "failed" || !strings.Contains(imp.Detail, "妙记") {
		t.Errorf("要记下没导入的原因：%+v %v", imp, err)
	}

	// 管理员关掉了自动导入：什么也不做
	if err := e.w.settings.SaveFeishu(ctx, settings.Feishu{Domain: "feishu", AppID: feishutest.AppID, AppSecret: feishutest.AppSecret}); err != nil {
		t.Fatal(err)
	}
	if err := iw.Work(ctx, jobOf(queue.FeishuRecordingArgs{MeetingID: "m5", OwnerOpenID: "ou_owner"}, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.q.GetFeishuImport(ctx, "m5"); err == nil {
		t.Error("关掉自动导入以后不该再导入")
	}
}

// 收到会议结束：组织者不是成员的，什么都不存（任务队列里也没有它的主题）；是成员的才加导入任务。
func TestListenerStoresOnlyMembersMeetings(t *testing.T) {
	e := newEnv(t)
	fs := e.feishu(settings.Feishu{ImportRecordings: true})
	fs.Users["ou_owner"] = feishutest.User{Name: "张敏", Email: e.user.Email}
	fs.Users["ou_stranger"] = feishutest.User{Name: "外人", Email: "someone@else.example"}
	l := &listener{w: e.w}
	ctx := t.Context()
	for _, ev := range []feishu.MeetingEnded{
		{MeetingID: "m-hr", Topic: "裁员沟通会", OwnerOpenID: "ou_stranger"},
		{MeetingID: "m-legal", Topic: "并购尽调", OwnerOpenID: "ou_outside_scope"}, // 通讯录读不到
	} {
		if err := l.handle(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	var leaked int
	if err := e.pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE args::text LIKE '%裁员%' OR args::text LIKE '%并购%'").Scan(&leaked); err != nil || leaked != 0 {
		t.Errorf("别人的会议不能进任务队列：%d %v", leaked, err)
	}
	if err := l.handle(ctx, feishu.MeetingEnded{MeetingID: "m1", Topic: "产品评审", OwnerOpenID: "ou_owner"}); err != nil {
		t.Fatal(err)
	}
	if jobs := e.jobs("feishu_recording"); len(jobs) != 1 || !strings.Contains(jobs[0], "产品评审") {
		t.Errorf("成员的会议要加导入任务：%v", jobs)
	}
}

// 飞书导入文档要等很久：记下任务号，过一会儿接着等，不会导入两篇。
func TestShareResumesSlowImport(t *testing.T) {
	e := newEnv(t)
	e.llm(nil)
	fs := e.feishu(settings.Feishu{SiteURL: "https://meet.example.com"})
	args := e.upload(wav())
	if err := e.work(args); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	fs.ImportPolls = 1
	restore := feishu.SetImportTiming(0, time.Millisecond)
	defer restore()
	share, err := e.q.CreateShare(ctx, sqlc.CreateShareParams{MeetingID: args.MeetingID, ChatID: "oc_team", ChatName: "产品组", WithDoc: true, MinutesVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	sw := &shareWorker{w: e.w}
	var snooze *rivertype.JobSnoozeError
	if err := sw.Work(ctx, jobOf(queue.FeishuShareArgs{ShareID: share.ID}, 1)); !errors.As(err, &snooze) {
		t.Fatalf("飞书还在导入时过一会儿接着等：%v", err)
	}
	if s, _ := e.q.GetShare(ctx, share.ID); s.ImportTicket == "" || s.Status != "pending" {
		t.Fatalf("要记下任务号：%+v", s)
	}
	feishu.SetImportTiming(time.Minute, time.Millisecond)
	if err := sw.Work(ctx, jobOf(queue.FeishuShareArgs{ShareID: share.ID}, 1)); err != nil {
		t.Fatal(err)
	}
	if s, _ := e.q.GetShare(ctx, share.ID); s.Status != "done" || s.DocUrl == "" || fs.Count("POST /open-apis/drive/v1/import_tasks") != 1 || len(fs.Sent()) != 1 {
		t.Errorf("接着等同一个任务，只导入一篇：%+v，导入 %d 次", s, fs.Count("POST /open-apis/drive/v1/import_tasks"))
	}
}

// 只导入成文档、不发群：发送的人按邮箱拿到文档的管理权限，不然谁都打不开。同一版纪要再发时直接用导入过的文档。
func TestShareDocOnlyGrantsRequester(t *testing.T) {
	e := newEnv(t)
	e.llm(nil)
	fs := e.feishu(settings.Feishu{SiteURL: "https://meet.example.com"})
	args := e.upload(wav())
	if err := e.work(args); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	sw := &shareWorker{w: e.w}
	docOnly, err := e.q.CreateShare(ctx, sqlc.CreateShareParams{MeetingID: args.MeetingID, WithDoc: true, MinutesVersion: 1, RequestedBy: &e.user.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := sw.Work(ctx, jobOf(queue.FeishuShareArgs{ShareID: docOnly.ID}, 1)); err != nil {
		t.Fatal(err)
	}
	if len(fs.Grants) != 1 || fs.Grants[0] != (feishutest.Grant{Doc: fs.Docs()[0].Token, MemberType: "email", MemberID: e.user.Email, Perm: "full_access"}) || len(fs.Sent()) != 0 {
		t.Errorf("发送的人按邮箱拿到文档的权限，不发卡片：%+v", fs.Grants)
	}

	// 再发到群里（不勾导入文档）：卡片带上已经导入的文档，群也拿到查看权限，不再导入一遍
	toChat, err := e.q.CreateShare(ctx, sqlc.CreateShareParams{MeetingID: args.MeetingID, ChatID: "oc_team", ChatName: "产品组", MinutesVersion: 1, RequestedBy: &e.user.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := sw.Work(ctx, jobOf(queue.FeishuShareArgs{ShareID: toChat.ID}, 1)); err != nil {
		t.Fatal(err)
	}
	sent := fs.Sent()
	if len(fs.Docs()) != 1 || len(sent) != 1 || !strings.Contains(sent[0].Content, fs.Docs()[0].Token) {
		t.Errorf("同一版纪要只导入一次，卡片上带着它：%d 篇文档，卡片 %+v", len(fs.Docs()), sent)
	}
	if len(fs.Grants) != 3 || fs.Grants[1].MemberID != "oc_team" {
		t.Errorf("群里的人也能看：%+v", fs.Grants)
	}
	if s, _ := e.q.GetShare(ctx, toChat.ID); s.DocUrl == "" || !strings.Contains(sent[0].Content, `"template":"blue"`) {
		t.Errorf("用以前导入的文档，也记在这一条上；第一次发到这个群不算「已更新」：%+v", s)
	}
	// 同一版再发一次：还是不算「已更新」；纪要改了一版以后再发：算
	again, _ := e.q.CreateShare(ctx, sqlc.CreateShareParams{MeetingID: args.MeetingID, ChatID: "oc_team", ChatName: "产品组", MinutesVersion: 1})
	if err := sw.Work(ctx, jobOf(queue.FeishuShareArgs{ShareID: again.ID}, 1)); err != nil {
		t.Fatal(err)
	}
	if err := e.work(e.enqueue(args.MeetingID, "done")); err != nil {
		t.Fatal(err)
	}
	newer, _ := e.q.CreateShare(ctx, sqlc.CreateShareParams{MeetingID: args.MeetingID, ChatID: "oc_team", ChatName: "产品组", MinutesVersion: 2})
	if err := sw.Work(ctx, jobOf(queue.FeishuShareArgs{ShareID: newer.ID}, 1)); err != nil {
		t.Fatal(err)
	}
	if sent := fs.Sent(); len(sent) != 3 || !strings.Contains(sent[1].Content, `"template":"blue"`) || !strings.Contains(sent[2].Content, `"template":"orange"`) {
		t.Errorf("只有纪要换了一版再发才标成「已更新」：%+v", sent)
	}
}

// 用以前导入的文档发到一个新的群：给这个群开文档权限失败时不能照样发卡片（群里的人打不开卡片上的文档），
// 和新导入的文档一样，飞书繁忙的交给 River 重试，重试也没用的记下原因。以前发到过的群，再开一次失败不影响发卡片。
func TestShareReusedDocToNewChatNeedsGrant(t *testing.T) {
	e := newEnv(t)
	e.llm(nil)
	fs := e.feishu(settings.Feishu{SiteURL: "https://meet.example.com"})
	args := e.upload(wav())
	if err := e.work(args); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	sw := &shareWorker{w: e.w}
	newShare := func(chat string) uuid.UUID {
		t.Helper()
		s, err := e.q.CreateShare(ctx, sqlc.CreateShareParams{MeetingID: args.MeetingID, ChatID: chat, ChatName: "群", WithDoc: true, MinutesVersion: 1})
		if err != nil {
			t.Fatal(err)
		}
		return s.ID
	}
	sentTo := func(chat string) int {
		n := 0
		for _, m := range fs.Sent() {
			if m.ChatID == chat {
				n++
			}
		}
		return n
	}
	// 第一次发到产品组：导入文档，产品组能看
	if err := sw.Work(ctx, jobOf(queue.FeishuShareArgs{ShareID: newShare("oc_team")}, 1)); err != nil {
		t.Fatal(err)
	}

	// 用这篇文档发到研发群，给研发群开权限时飞书限流：先不发卡片，交给 River 重试；重试时开好了权限再发
	fs.FailGrants["oc_dev"] = 99991400
	dev := newShare("oc_dev")
	err := sw.Work(ctx, jobOf(queue.FeishuShareArgs{ShareID: dev}, 1))
	if err == nil || errors.Is(err, &rivertype.JobCancelError{}) || sentTo("oc_dev") != 0 {
		t.Fatalf("研发群还没拿到文档的权限，不能发卡片，稍后重试：%v，卡片 %+v", err, fs.Sent())
	}
	delete(fs.FailGrants, "oc_dev")
	if err := sw.Work(ctx, jobOf(queue.FeishuShareArgs{ShareID: dev}, 2)); err != nil {
		t.Fatal(err)
	}
	if s, _ := e.q.GetShare(ctx, dev); s.Status != "done" || sentTo("oc_dev") != 1 || len(fs.Docs()) != 1 {
		t.Errorf("开好权限再发，用的还是那一篇文档：%+v，%d 篇文档", s, len(fs.Docs()))
	}

	// 发到另一个新的群，开权限被拦下了（重试也没用）：记下原因，不发卡片
	fs.FailGrants["oc_ops"] = 1063003
	ops := newShare("oc_ops")
	if err := sw.Work(ctx, jobOf(queue.FeishuShareArgs{ShareID: ops}, 1)); !errors.Is(err, &rivertype.JobCancelError{}) {
		t.Errorf("开不了权限，不再重试：%v", err)
	}
	if s, _ := e.q.GetShare(ctx, ops); s.Status != "failed" || !strings.Contains(s.Error, "设置文档权限被拦下了") || sentTo("oc_ops") != 0 {
		t.Errorf("记成没发出去，写着原因，不发卡片：%+v %+v", s, fs.Sent())
	}

	// 再发到产品组：以前已经给过它这篇文档的权限，这次再开失败也照样发
	fs.FailGrants["oc_team"] = 1063003
	again := newShare("oc_team")
	if err := sw.Work(ctx, jobOf(queue.FeishuShareArgs{ShareID: again}, 1)); err != nil {
		t.Fatal(err)
	}
	if s, _ := e.q.GetShare(ctx, again); s.Status != "done" || sentTo("oc_team") != 2 {
		t.Errorf("以前发到过的群照样发：%+v", s)
	}
}

// 只有自己能看、指定了人的会议不自动发到群里：群里的人不一定能看。
// 排上队以后（重试、等文档导入期间）才改成不是团队可见的，发送时也不再发。
func TestAutoPushSkipsNonTeamMeetings(t *testing.T) {
	e := newEnv(t)
	e.llm(nil)
	fs := e.feishu(settings.Feishu{SiteURL: "https://meet.example.com", ChatID: "oc_team", ChatName: "产品组", AutoPush: true})

	queued := e.upload(wav())
	if err := e.work(queued); err != nil {
		t.Fatal(err)
	}
	shares := e.shares(queued.MeetingID)
	if len(shares) != 1 {
		t.Fatalf("团队可见的会议排上了自动发送：%+v", shares)
	}
	if _, err := e.pool.Exec(t.Context(), "UPDATE meetings SET visibility = 'private' WHERE id = $1", queued.MeetingID); err != nil {
		t.Fatal(err)
	}
	sw := &shareWorker{w: e.w}
	if err := sw.Work(t.Context(), jobOf(queue.FeishuShareArgs{ShareID: shares[0].FeishuShare.ID}, 2)); !errors.Is(err, &rivertype.JobCancelError{}) {
		t.Fatalf("改成只有自己能看以后不再发，也不再重试：%v", err)
	}
	if s := e.shares(queued.MeetingID)[0].FeishuShare; s.Status != "failed" || !strings.Contains(s.Error, "不再自动发到群里") || len(fs.Sent()) != 0 {
		t.Errorf("没有发出去，记下原因：%+v %v", s, fs.Sent())
	}

	for _, visibility := range []string{"private", "members"} {
		args := e.upload(wav())
		if _, err := e.pool.Exec(t.Context(), "UPDATE meetings SET visibility = $2 WHERE id = $1", args.MeetingID, visibility); err != nil {
			t.Fatal(err)
		}
		if err := e.work(args); err != nil {
			t.Fatal(err)
		}
		if m := e.meeting(args.MeetingID); m.Status != "done" || m.CurrentVersion != 1 {
			t.Fatalf("纪要照常生成：%s %d %s", m.Status, m.CurrentVersion, m.Error)
		}
		if shares := e.shares(args.MeetingID); len(shares) != 0 {
			t.Errorf("%s 的会议不自动发到群里：%+v", visibility, shares)
		}
	}
}

// whileMeetingLocked 相当于 service.SetMeetingAccess：锁住会议（FOR UPDATE）的时候让 run 开始做，
// 等它在锁上排队以后执行 change、提交，返回 run 的结果。run 不等锁就做完了，测试失败。
func (e *env) whileMeetingLocked(meeting uuid.UUID, what string, run func() error, change string) error {
	e.t.Helper()
	ctx := e.t.Context()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := e.q.WithTx(tx).LockMeeting(ctx, meeting); err != nil {
		e.t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- run() }()
	e.waitLockWait(done, what)
	if _, err := tx.Exec(ctx, change, meeting); err != nil {
		e.t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		e.t.Fatal(err)
	}
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		e.t.Fatalf("%s：锁放开以后应该做完", what)
	}
	return nil
}

// 自动发到飞书：把纪要传给飞书导入、开文档权限和发卡片之前，都锁住会议再看一遍可见范围。
// 正在改可见范围（已经锁住了会议、还没提交）时，发送等它改完再看，改成只有自己能看的就不发。
// 等飞书导入的时候不拿锁（snooze 以后接着等），开权限、发卡片之前重新锁、重新看。
func TestAutoShareWaitsForVisibilityChange(t *testing.T) {
	e := newEnv(t)
	e.llm(nil)
	fs := e.feishu(settings.Feishu{
		SiteURL: "https://meet.example.com", ChatID: "oc_team", ChatName: "产品组",
		AutoPush: true, WithDoc: true, LinkShare: true,
	})
	sw := &shareWorker{w: e.w}
	queued := func() (uuid.UUID, uuid.UUID) {
		t.Helper()
		args := e.upload(wav())
		if err := e.work(args); err != nil {
			t.Fatal(err)
		}
		shares := e.shares(args.MeetingID)
		if len(shares) != 1 {
			t.Fatalf("排上了自动发送：%+v", shares)
		}
		return args.MeetingID, shares[0].FeishuShare.ID
	}
	const private = "UPDATE meetings SET visibility = 'private' WHERE id = $1"

	// 还没开始导入：等改完可见范围，一看是只有自己能看的，纪要不传给飞书，也不发卡片
	meeting, share := queued()
	err := e.whileMeetingLocked(meeting, "导入文档之前改可见范围", func() error {
		return sw.Work(t.Context(), jobOf(queue.FeishuShareArgs{ShareID: share}, 1))
	}, private)
	if !errors.Is(err, &rivertype.JobCancelError{}) {
		t.Fatalf("改成只有自己能看以后不再发，也不再重试：%v", err)
	}
	if s, _ := e.q.GetShare(t.Context(), share); s.Status != "failed" || !strings.Contains(s.Error, "不再自动发到群里") {
		t.Errorf("记下没发的原因：%+v", s)
	}
	if n := fs.Count("POST /open-apis/drive/v1/import_tasks"); n != 0 || len(fs.Sent()) != 0 {
		t.Fatalf("纪要不能传给飞书，也不能发卡片：导入 %d 次，卡片 %+v", n, fs.Sent())
	}

	// 已经开始导入、snooze 着等结果：这期间不拿锁；导入好了以后，开权限、发卡片之前等改完可见范围，不发
	fs.ImportPolls = 1
	restore := feishu.SetImportTiming(0, time.Millisecond)
	defer restore()
	meeting, share = queued()
	var snooze *rivertype.JobSnoozeError
	if err := sw.Work(t.Context(), jobOf(queue.FeishuShareArgs{ShareID: share}, 1)); !errors.As(err, &snooze) {
		t.Fatalf("飞书还在导入时过一会儿接着等：%v", err)
	}
	feishu.SetImportTiming(time.Minute, time.Millisecond)
	err = e.whileMeetingLocked(meeting, "开权限、发卡片之前改可见范围", func() error {
		return sw.Work(t.Context(), jobOf(queue.FeishuShareArgs{ShareID: share}, 1))
	}, private)
	if !errors.Is(err, &rivertype.JobCancelError{}) {
		t.Fatalf("改成只有自己能看以后不再发：%v", err)
	}
	if len(fs.Docs()) != 1 || len(fs.Grants) != 0 || len(fs.PublicDocs) != 0 || len(fs.Sent()) != 0 {
		t.Errorf("文档已经导入了，但群里、公司里的人都不能拿到权限，也不发卡片：%d 篇，权限 %+v %v，卡片 %+v", len(fs.Docs()), fs.Grants, fs.PublicDocs, fs.Sent())
	}
}

// 卡片已经发到群里、记成已发送时数据库出错：多试几次，还是不行也不能让 River 重试（重试时会议可能已经改成不能发、
// 已经超过半小时，会被记成「没发出去」）。
func TestShareNoRepostWhenFinishFails(t *testing.T) {
	e := newEnv(t)
	e.llm(nil)
	fs := e.feishu(settings.Feishu{SiteURL: "https://meet.example.com"})
	args := e.upload(wav())
	if err := e.work(args); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	share, err := e.q.CreateShare(ctx, sqlc.CreateShareParams{MeetingID: args.MeetingID, ChatID: "oc_team", ChatName: "产品组", WithDoc: true, MinutesVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	e.failShareUpdate("NEW.status = 'done'")
	finishPause = 10 * time.Millisecond
	t.Cleanup(func() { finishPause = time.Second })

	sw := &shareWorker{w: e.w}
	if err := sw.Work(ctx, jobOf(queue.FeishuShareArgs{ShareID: share.ID}, 1)); err != nil {
		t.Errorf("已经发出去了，不能交给 River 重试：%v", err)
	}
	if n := fs.Count("POST /open-apis/im/v1/messages"); n != 1 || len(fs.Docs()) != 1 {
		t.Errorf("卡片只发一次、文档只导入一篇：卡片 %d 次，文档 %d 篇", n, len(fs.Docs()))
	}
	if s, _ := e.q.GetShare(ctx, share.ID); s.Status != "pending" || s.Error != "" {
		t.Errorf("记不进去就还是发送中，不能写成没发出去：%+v", s)
	}
}

// 飞书已经开始导入文档、记下任务号时数据库出错：不能让 River 重试（重试会再导入一篇），记成失败、说清楚。
func TestShareNoReimportWhenTicketNotSaved(t *testing.T) {
	e := newEnv(t)
	e.llm(nil)
	fs := e.feishu(settings.Feishu{SiteURL: "https://meet.example.com"})
	args := e.upload(wav())
	if err := e.work(args); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	share, err := e.q.CreateShare(ctx, sqlc.CreateShareParams{MeetingID: args.MeetingID, ChatID: "oc_team", ChatName: "产品组", WithDoc: true, MinutesVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	e.failShareUpdate("NEW.import_ticket <> OLD.import_ticket")
	finishPause = 10 * time.Millisecond
	t.Cleanup(func() { finishPause = time.Second })

	sw := &shareWorker{w: e.w}
	if err := sw.Work(ctx, jobOf(queue.FeishuShareArgs{ShareID: share.ID}, 1)); !errors.Is(err, &rivertype.JobCancelError{}) {
		t.Errorf("任务号没记下来，不能交给 River 重试：%v", err)
	}
	if s, _ := e.q.GetShare(ctx, share.ID); s.Status != "failed" || !strings.Contains(s.Error, "不再自动重试") {
		t.Errorf("记成失败，说清楚为什么：%+v", s)
	}
	// 就算任务又被做了一次，也不会再导入
	if err := sw.Work(ctx, jobOf(queue.FeishuShareArgs{ShareID: share.ID}, 2)); err != nil {
		t.Fatal(err)
	}
	if n := fs.Count("POST /open-apis/drive/v1/import_tasks"); n != 1 || len(fs.Sent()) != 0 {
		t.Errorf("只导入一次、不发卡片：导入 %d 次，卡片 %+v", n, fs.Sent())
	}
}

// failShareUpdate 模拟数据库写不进去：改 feishu_shares 时 when 成立就报错。测试结束时去掉。
func (e *env) failShareUpdate(when string) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.t.Context(), `
CREATE FUNCTION test_fail_share() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
	IF `+when+` THEN RAISE EXCEPTION 'database is down'; END IF;
	RETURN NEW;
END $$;
CREATE TRIGGER test_fail_share BEFORE UPDATE ON feishu_shares FOR EACH ROW EXECUTE FUNCTION test_fail_share();`); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(), "DROP TRIGGER test_fail_share ON feishu_shares; DROP FUNCTION test_fail_share()")
	})
}

// editor 加一个普通成员，指定他能编辑会议 meeting（团队可见的会议上，普通成员只有被指定了才能编辑）。
func (e *env) editor(meeting uuid.UUID, email string) sqlc.User {
	e.t.Helper()
	u, err := e.q.CreateUser(e.t.Context(), sqlc.CreateUserParams{Email: email, Name: "李雷", PasswordHash: "x", Role: "member"})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.pool.Exec(e.t.Context(), "INSERT INTO meeting_members (meeting_id, user_id, can_edit) VALUES ($1, $2, true)", meeting, u.ID); err != nil {
		e.t.Fatal(err)
	}
	return u
}

// 发起人能编辑时才会把纪要发出去。手动发到飞书以后，等文档导入期间发起人被取消了编辑权限、被停用、
// 或者会议改成了他看不到的，就不再给群开文档权限、也不发卡片，记成没发出去。
func TestShareStopsWhenRequesterLosesAccess(t *testing.T) {
	e := newEnv(t)
	e.llm(nil)
	fs := e.feishu(settings.Feishu{SiteURL: "https://meet.example.com"})
	ctx := t.Context()
	sw := &shareWorker{w: e.w}
	restore := feishu.SetImportTiming(0, time.Millisecond)
	defer restore()

	cases := []struct {
		name, change, reason string
	}{
		{"还能编辑", "", ""},
		{"取消了编辑权限", "UPDATE meeting_members SET can_edit = false WHERE user_id = $1 AND meeting_id = $2", "发起人已经不能编辑这场会议了"},
		{"不再指定", "DELETE FROM meeting_members WHERE user_id = $1 AND meeting_id = $2", "发起人已经不能编辑这场会议了"},
		{"会议改成只有创建者能看", "UPDATE meetings SET visibility = 'private' WHERE id = $2 AND owner_id <> $1", "发起人已经不能编辑这场会议了"},
		{"停用了", "UPDATE users u SET disabled = true FROM meeting_members mm WHERE u.id = $1 AND mm.user_id = u.id AND mm.meeting_id = $2", "发起人的账号已经停用了"},
	}
	for i, c := range cases {
		args := e.upload(wav())
		if err := e.work(args); err != nil {
			t.Fatal(err)
		}
		requester := e.editor(args.MeetingID, fmt.Sprintf("editor%d@example.com", i))
		share, err := e.q.CreateShare(ctx, sqlc.CreateShareParams{
			MeetingID: args.MeetingID, ChatID: "oc_team", ChatName: "产品组", WithDoc: true, MinutesVersion: 1, RequestedBy: &requester.ID,
		})
		if err != nil {
			t.Fatal(err)
		}
		// 飞书还在导入文档：过一会儿接着等
		fs.ImportPolls = 1
		feishu.SetImportTiming(0, time.Millisecond)
		var snooze *rivertype.JobSnoozeError
		if err := sw.Work(ctx, jobOf(queue.FeishuShareArgs{ShareID: share.ID}, 1)); !errors.As(err, &snooze) {
			t.Fatalf("%s：飞书还在导入时过一会儿接着等：%v", c.name, err)
		}
		if c.change != "" {
			if tag, err := e.pool.Exec(ctx, c.change, requester.ID, args.MeetingID); err != nil || tag.RowsAffected() != 1 {
				t.Fatalf("%s：%v %v", c.name, tag, err)
			}
		}
		grants, sent := len(fs.Grants), len(fs.Sent())
		feishu.SetImportTiming(time.Minute, time.Millisecond)
		err = sw.Work(ctx, jobOf(queue.FeishuShareArgs{ShareID: share.ID}, 1))
		s, _ := e.q.GetShare(ctx, share.ID)
		if c.reason == "" {
			if err != nil || s.Status != "done" || len(fs.Grants) == grants || len(fs.Sent()) != sent+1 {
				t.Errorf("%s：照常发出去：%v %+v", c.name, err, s)
			}
			continue
		}
		if !errors.Is(err, &rivertype.JobCancelError{}) {
			t.Errorf("%s：不再发，也不再重试：%v", c.name, err)
		}
		if s.Status != "failed" || !strings.Contains(s.Error, c.reason) {
			t.Errorf("%s：记成没发出去，写着原因：%+v", c.name, s)
		}
		if len(fs.Grants) != grants || len(fs.Sent()) != sent {
			t.Errorf("%s：不给群开文档权限、不发卡片：%+v %+v", c.name, fs.Grants[grants:], fs.Sent()[sent:])
		}
	}
}

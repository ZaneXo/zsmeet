package pipeline

import (
	"errors"
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

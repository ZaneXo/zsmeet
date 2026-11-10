package pipeline

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river/rivertype"

	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/queue"
	"github.com/ZaneXo/zsmeet/internal/service"
	"github.com/ZaneXo/zsmeet/internal/settings"
	"github.com/ZaneXo/zsmeet/internal/webhook"
	"github.com/ZaneXo/zsmeet/internal/webhook/webhooktest"
)

// 重试的安排：钉钉限流等 11 分钟，还能再试两次；别的错误按间隔重试；不管怎么试，最后一次都在
// WebhookRetryUntil 以内开始，加上晚开始、发送最多等的时间，也在「发送中」的期限（WebhookPendingWindow）以内结束。
func TestWebhookRetrySchedule(t *testing.T) {
	throttled := &webhook.Error{Msg: "发得太快了", Retry: true, Wait: webhook.DingTalkThrottle}
	minute := &webhook.Error{Msg: "发得太快了", Retry: true, Wait: time.Minute}
	down := &webhook.Error{Msg: "暂时出错了", Retry: true}
	config := &webhook.Error{Msg: "关键词不对"}

	created := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	// run 从发送开始一直失败下去（第 i 次失败的原因是 errs(i)），返回每一次开始的时间
	run := func(errs func(i int) error) []time.Duration {
		var starts []time.Duration
		now, attempt := created, 1
		for i := 0; ; i++ {
			starts = append(starts, now.Sub(created))
			now = now.Add(webhook.SendTimeout) // 每次都等到超时，最坏的情况
			delay, snooze, ok := webhookRetry(errs(i), attempt, 6, created, now)
			if !ok {
				return starts
			}
			if !snooze {
				attempt++
			}
			now = now.Add(delay)
			if i > 100 {
				t.Fatal("一直在重试")
			}
		}
	}
	for name, errs := range map[string]func(int) error{
		"钉钉一直限流":   func(int) error { return throttled },
		"按分钟限流":    func(int) error { return minute },
		"一直出错":     func(int) error { return down },
		"出错和限流轮着来": func(i int) error { return []error{down, throttled}[i%2] },
	} {
		starts := run(errs)
		last := starts[len(starts)-1]
		if last > service.WebhookRetryUntil {
			t.Errorf("%s：最后一次在 %v 才开始，晚于 %v：%v", name, last, service.WebhookRetryUntil, starts)
		}
		if end := last + webhookLateStart + webhook.SendTimeout; end >= service.WebhookPendingWindow {
			t.Errorf("%s：最后一次可能到 %v 才结束，网页上已经允许重新发了", name, end)
		}
		if name == "钉钉一直限流" {
			if len(starts) < 3 || starts[1] < 10*time.Minute {
				t.Errorf("钉钉限流 10 分钟：要等 10 分钟以上再试，还能再试两次：%v", starts)
			}
		}
	}

	if _, _, ok := webhookRetry(config, 1, 6, created, created); ok {
		t.Error("配置不对的不重试")
	}
	if _, _, ok := webhookRetry(permanent("会议已经删除了"), 1, 6, created, created); ok {
		t.Error("不会好的不重试")
	}
	if _, _, ok := webhookRetry(down, 6, 6, created, created); ok {
		t.Error("次数用完不再重试")
	}
	if d, snooze, ok := webhookRetry(throttled, 6, 6, created, created); !ok || !snooze || d != webhook.DingTalkThrottle {
		t.Errorf("限流不算重试次数：%v %v %v", d, snooze, ok)
	}
}

// hooks 起一个假的群机器人服务器，让 worker 认它的地址。
func (e *env) hooks() *webhooktest.Server {
	e.t.Helper()
	s := webhooktest.New(e.t)
	e.w.webhookTestHost = s.Host
	return s
}

// addHook 直接往数据库里加一个群机器人（地址加密保存，和设置页面加的一样）。
func (e *env) addHook(s *webhooktest.Server, platform, token, name string, autoPush bool) sqlc.Webhook {
	e.t.Helper()
	checked, err := webhook.CheckURL(platform, s.URL(platform, token), s.Host)
	if err != nil {
		e.t.Fatal(err)
	}
	h, err := e.q.CreateWebhook(e.t.Context(), sqlc.CreateWebhookParams{
		Platform: platform, Name: name, Url: e.w.settings.Seal(checked), UrlMasked: webhook.Mask(platform, checked), AutoPush: autoPush, CreatedBy: &e.user.ID,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return h
}

func (e *env) webhookSends(meeting uuid.UUID) []sqlc.WebhookSend {
	e.t.Helper()
	rows, err := e.pool.Query(e.t.Context(), "SELECT id FROM webhook_sends WHERE meeting_id = $1 ORDER BY created_at, webhook_name", meeting)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []sqlc.WebhookSend
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			e.t.Fatal(err)
		}
		s, err := e.q.GetWebhookSend(e.t.Context(), id)
		if err != nil {
			e.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// 纪要第一次生成后，自动发到打开了自动发送的群机器人；链接用设置里的访问地址。重新生成的不再自动发。
func TestWebhookAutoPushAfterFirstMinutes(t *testing.T) {
	e := newEnv(t)
	e.llm(nil)
	fake := e.hooks()
	e.addHook(fake, webhook.WeCom, "wecomtoken0123456789", "研发群", true)
	e.addHook(fake, webhook.Slack, "slacktoken0123456789", "#random", false)
	if err := e.w.settings.SaveSite(t.Context(), settings.Site{URL: "https://meet.example.com/"}); err != nil {
		t.Fatal(err)
	}

	args := e.upload(wav())
	if err := e.work(args); err != nil {
		t.Fatal(err)
	}
	sends := e.webhookSends(args.MeetingID)
	if len(sends) != 1 || sends[0].WebhookName != "研发群" || sends[0].Status != "pending" || sends[0].RequestedBy != nil || sends[0].MinutesVersion != 1 {
		t.Fatalf("只发到打开了自动发送的群：%+v", sends)
	}
	if jobs := e.jobs("webhook_send"); len(jobs) != 1 || !strings.Contains(jobs[0], sends[0].ID.String()) {
		t.Fatalf("要加一个发送的后台任务：%v", jobs)
	}

	ww := &webhookWorker{w: e.w}
	if err := ww.Work(t.Context(), jobOf(queue.WebhookSendArgs{SendID: sends[0].ID}, 1)); err != nil {
		t.Fatal(err)
	}
	if s := e.webhookSends(args.MeetingID)[0]; s.Status != "done" || s.Error != "" || s.FinishedAt == nil {
		t.Errorf("发完记成已发送：%+v", s)
	}
	got := fake.Received()
	if len(got) != 1 || got[0].Platform != webhook.WeCom || !strings.Contains(got[0].Body, "会议纪要｜产品周会") ||
		!strings.Contains(got[0].Body, "https://meet.example.com/meetings/"+args.MeetingID.String()) {
		t.Fatalf("纪要发到企业微信群，带着回到会议的链接：%+v", got)
	}

	// 已经发完的任务再执行一次（River 重复投递）：不再发
	if err := ww.Work(t.Context(), jobOf(queue.WebhookSendArgs{SendID: sends[0].ID}, 1)); err != nil || len(fake.Received()) != 1 {
		t.Errorf("发完的不再发：%v", err)
	}
	// 重新生成纪要不自动发
	if err := e.work(e.enqueue(args.MeetingID, "done")); err != nil {
		t.Fatal(err)
	}
	if n := len(e.webhookSends(args.MeetingID)); n != 1 {
		t.Errorf("重新生成的纪要不该自动发，现在有 %d 条发送记录", n)
	}
}

// 只有自己能看、指定了人的会议不自动发到群里；排上队以后才改成不是团队可见的，发送时也不再发。
func TestWebhookAutoPushSkipsNonTeamMeetings(t *testing.T) {
	e := newEnv(t)
	e.llm(nil)
	fake := e.hooks()
	e.addHook(fake, webhook.DingTalk, "dingtoken0123456789", "产品群", true)

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
		if sends := e.webhookSends(args.MeetingID); len(sends) != 0 {
			t.Errorf("%s 的会议不自动发到群里：%+v", visibility, sends)
		}
	}
	if n := len(e.jobs("webhook_send")); n != 0 {
		t.Errorf("不是团队可见的会议不加发送任务：%d", n)
	}

	queued := e.upload(wav())
	if err := e.work(queued); err != nil {
		t.Fatal(err)
	}
	sends := e.webhookSends(queued.MeetingID)
	if len(sends) != 1 {
		t.Fatalf("团队可见的会议排上了自动发送：%+v", sends)
	}
	if _, err := e.pool.Exec(t.Context(), "UPDATE meetings SET visibility = 'private' WHERE id = $1", queued.MeetingID); err != nil {
		t.Fatal(err)
	}
	ww := &webhookWorker{w: e.w}
	if err := ww.Work(t.Context(), jobOf(queue.WebhookSendArgs{SendID: sends[0].ID}, 2)); !errors.Is(err, &rivertype.JobCancelError{}) {
		t.Fatalf("改成只有自己能看以后不再发，也不再重试：%v", err)
	}
	if s := e.webhookSends(queued.MeetingID)[0]; s.Status != "failed" || !strings.Contains(s.Error, "不再自动发到群里") || len(fake.Received()) != 0 {
		t.Errorf("没有发出去，记下原因：%+v %v", s, fake.Received())
	}
}

// 发送失败：配置不对的（关键词、地址失效）记下原因不再重试；发得太快的交给 River 重试；
// 群机器人删了、会议删了、等太久了的不再发。没有单独填访问地址时用飞书配置里的。
func TestWebhookSendFailures(t *testing.T) {
	e := newEnv(t)
	e.llm(nil)
	fake := e.hooks()
	args := e.upload(wav())
	if err := e.work(args); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	newSend := func(h sqlc.Webhook) sqlc.WebhookSend {
		t.Helper()
		s, err := e.q.CreateWebhookSend(ctx, sqlc.CreateWebhookSendParams{
			MeetingID: args.MeetingID, WebhookID: &h.ID, WebhookName: h.Name, Platform: h.Platform, MinutesVersion: 1, RequestedBy: &e.user.ID,
		})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	get := func(s sqlc.WebhookSend) sqlc.WebhookSend {
		t.Helper()
		got, err := e.q.GetWebhookSend(ctx, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	ww := &webhookWorker{w: e.w}
	work := func(s sqlc.WebhookSend, attempt int) error {
		return ww.Work(ctx, jobOf(queue.WebhookSendArgs{SendID: s.ID}, attempt))
	}

	keyword := e.addHook(fake, webhook.DingTalk, "keywordtoken0123456", "关键词群", false)
	fake.Replies["keywordtoken0123456"] = webhooktest.Reply{Status: 200, Body: `{"errcode":310000,"errmsg":"keywords not in content"}`}
	s := newSend(keyword)
	if err := work(s, 1); !errors.Is(err, &rivertype.JobCancelError{}) {
		t.Errorf("关键词不对，重试也没用：%v", err)
	}
	if got := get(s); got.Status != "failed" || !strings.Contains(got.Error, "关键词「会议纪要」") || strings.Contains(got.Error, "keywordtoken") {
		t.Errorf("记下能照着改的原因，不带地址：%+v", got)
	}

	// 钉钉限流：超过每分钟 20 条要等 10 分钟。等 11 分钟再试（snooze，不算一次重试），不能在 10 分钟里把重试次数用完
	throttled := e.addHook(fake, webhook.DingTalk, "throttletoken012345", "钉钉限流群", false)
	fake.Replies["throttletoken012345"] = webhooktest.Reply{Status: 200, Body: `{"errcode":130101,"errmsg":"send too fast, exceed 20 times per minute"}`}
	s = newSend(throttled)
	err := work(s, 1)
	if snooze, ok := errors.AsType[*rivertype.JobSnoozeError](err); !ok || snooze.Duration < 10*time.Minute {
		t.Errorf("钉钉限流以后等 10 分钟以上再试：%v", err)
	}
	if got := get(s); got.Status != "pending" || !strings.Contains(got.Error, "被限流了，11 分钟后自动重试") || !strings.Contains(got.Error, "等 10 分钟") {
		t.Errorf("重试之前还是发送中，写着为什么、多久以后重试：%+v", got)
	}
	// 再等 11 分钟就超过了最晚的重试时间：不再重试，记成失败
	if _, err := e.pool.Exec(ctx, "UPDATE webhook_sends SET created_at = now() - interval '20 minutes' WHERE id = $1", s.ID); err != nil {
		t.Fatal(err)
	}
	if err := work(s, 1); !errors.Is(err, &rivertype.JobCancelError{}) {
		t.Errorf("等不到下一次了，不再重试：%v", err)
	}
	if got := get(s); got.Status != "failed" || !strings.Contains(got.Error, "发得太快了") {
		t.Errorf("记成失败，写着被限流：%+v", got)
	}

	// 企业微信按分钟限流：等 1 分钟
	busy := e.addHook(fake, webhook.WeCom, "busytoken0123456789", "忙的群", false)
	fake.Replies["busytoken0123456789"] = webhooktest.Reply{Status: 200, Body: `{"errcode":45009,"errmsg":"api freq out of limit"}`}
	s = newSend(busy)
	if snooze, ok := errors.AsType[*rivertype.JobSnoozeError](work(s, 1)); !ok || snooze.Duration != time.Minute {
		t.Errorf("企业微信限流以后等 1 分钟再试：%v", snooze)
	}
	if got := get(s); got.Status != "pending" || !strings.Contains(got.Error, "1 分钟后自动重试") || !strings.Contains(got.Error, "每分钟最多发 20 条") {
		t.Errorf("重试之前还是发送中：%+v", got)
	}

	// 对方暂时出错：交给 River 按间隔重试，次数用完就不再重试
	down := e.addHook(fake, webhook.Slack, "downtoken0123456789", "出错的群", false)
	fake.Replies["downtoken0123456789"] = webhooktest.Reply{Status: 502, Body: "bad gateway"}
	s = newSend(down)
	if err := work(s, 1); err == nil || errors.Is(err, &rivertype.JobCancelError{}) || errors.Is(err, &rivertype.JobSnoozeError{}) {
		t.Errorf("对方出错时交给 River 稍后重试：%v", err)
	}
	if got := get(s); got.Status != "pending" || !strings.Contains(got.Error, "第 1 次没发出去，10 秒后自动重试") || !strings.Contains(got.Error, "HTTP 502") {
		t.Errorf("重试之前还是发送中，写着为什么、多久以后重试：%+v", got)
	}
	if err := work(s, 6); !errors.Is(err, &rivertype.JobCancelError{}) {
		t.Errorf("重试次数用完就不再重试：%v", err)
	}
	if got := get(s); got.Status != "failed" {
		t.Errorf("重试次数用完要记成失败：%+v", got)
	}

	// 群机器人被删了
	gone := e.addHook(fake, webhook.Slack, "gonetoken0123456789", "删掉的群", false)
	s = newSend(gone)
	if _, err := e.q.DeleteWebhook(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	if err := work(s, 1); !errors.Is(err, &rivertype.JobCancelError{}) {
		t.Errorf("群机器人删了，不再发：%v", err)
	}
	if got := get(s); got.Status != "failed" || got.WebhookID != nil || got.WebhookName != "删掉的群" || !strings.Contains(got.Error, "删掉了") {
		t.Errorf("记录留着、名字还在，写上原因：%+v", got)
	}

	// 发送中超过半小时（网页上已经允许重新发了）：这一条不再发
	ok := e.addHook(fake, webhook.Slack, "oktoken01234567890", "#product", false)
	stale := newSend(ok)
	if _, err := e.pool.Exec(ctx, "UPDATE webhook_sends SET created_at = now() - interval '1 hour' WHERE id = $1", stale.ID); err != nil {
		t.Fatal(err)
	}
	if err := work(stale, 1); !errors.Is(err, &rivertype.JobCancelError{}) || get(stale).Status != "failed" || get(stale).Error != service.WebhookStaleMessage || len(fake.Received()) != 0 {
		t.Errorf("太久的不再发：%v", err)
	}

	// 没有在群机器人设置里填访问地址：用飞书配置里的
	if err := e.w.settings.SaveFeishu(ctx, settings.Feishu{Domain: "feishu", SiteURL: "https://feishu-site.example.com"}); err != nil {
		t.Fatal(err)
	}
	s = newSend(ok)
	if err := work(s, 1); err != nil {
		t.Fatal(err)
	}
	if got := fake.Received(); len(got) != 1 || !strings.Contains(got[0].Body, "<https://feishu-site.example.com/meetings/"+args.MeetingID.String()+"|查看完整纪要>") {
		t.Errorf("用飞书配置里的访问地址：%+v", got)
	}

	// 会议删了
	if _, err := e.pool.Exec(ctx, "UPDATE meetings SET deleted_at = now() WHERE id = $1", args.MeetingID); err != nil {
		t.Fatal(err)
	}
	s = newSend(ok)
	if err := work(s, 1); !errors.Is(err, &rivertype.JobCancelError{}) || !strings.Contains(get(s).Error, "会议已经删除了") {
		t.Errorf("会议删了不再发：%v %+v", err, get(s))
	}
	// 彻底删除：记录跟着没了，任务安静地结束
	if _, err := e.q.PurgeMeeting(ctx, args.MeetingID); err != nil {
		t.Fatal(err)
	}
	if err := work(s, 1); err != nil {
		t.Errorf("记录没了就什么都不做：%v", err)
	}
}

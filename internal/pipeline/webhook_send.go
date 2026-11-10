package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/queue"
	"github.com/ZaneXo/zsmeet/internal/service"
	"github.com/ZaneXo/zsmeet/internal/webhook"
)

// webhookWorker 把纪要发到钉钉、企业微信、Slack 的群机器人。照着 webhook_sends 表里的一条做。
type webhookWorker struct {
	river.WorkerDefaults[queue.WebhookSendArgs]
	w *Worker
}

// queueWebhookSends 在事务 tx 里给每个打开了自动发送的群机器人记下一次发送，并加上后台任务。纪要第一次生成后用。
func (w *Worker) queueWebhookSends(ctx context.Context, tx pgx.Tx, q *sqlc.Queries, meeting uuid.UUID, version int32) error {
	hooks, err := q.AutoPushWebhooks(ctx)
	if err != nil {
		return err
	}
	for _, h := range hooks {
		send, err := q.CreateWebhookSend(ctx, sqlc.CreateWebhookSendParams{
			MeetingID: meeting, WebhookID: &h.ID, WebhookName: h.Name, Platform: h.Platform, MinutesVersion: version,
		})
		if err != nil {
			return err
		}
		if _, err := w.queue.InsertTx(ctx, tx, queue.WebhookSendArgs{SendID: send.ID}, nil); err != nil {
			return err
		}
	}
	return nil
}

func (ww *webhookWorker) Work(ctx context.Context, job *river.Job[queue.WebhookSendArgs]) error {
	w := ww.w
	send, err := w.q.GetWebhookSend(ctx, job.Args.SendID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // 会议被彻底删除了，记录跟着没了
	}
	if err != nil {
		return err
	}
	if send.Status != "pending" {
		return nil
	}
	log := slog.With("meeting", send.MeetingID, "send", send.ID, "platform", send.Platform)
	// 重试最晚排在 WebhookRetryUntil，River 晚一点开始也照样发；再晚就是任务丢了很久又冒出来，
	// 离网页上允许重新发（WebhookPendingWindow）不远了：这一条不再发，免得群里收到两条一样的
	if time.Since(send.CreatedAt) > service.WebhookRetryUntil+webhookLateStart {
		if err := w.q.FailWebhookSend(ctx, sqlc.FailWebhookSendParams{ID: send.ID, Error: service.WebhookStaleMessage}); err != nil {
			return err
		}
		return river.JobCancel(errors.New("发送中太久了"))
	}

	err = ww.send(ctx, send)
	if err == nil {
		log.Info("纪要已发到群机器人", "webhook", send.WebhookName)
		return nil
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return ctx.Err() // worker 正在退出，River 会在它重新起来后再做
	}
	if delay, snooze, ok := webhookRetry(err, job.Attempt, job.MaxAttempts, send.CreatedAt, time.Now()); ok {
		log.Warn("发到群机器人失败，稍后重试", "attempt", job.Attempt, "after", delay, "err", err)
		note := fmt.Sprintf("第 %d 次没发出去，%s后自动重试：%s", job.Attempt, durationWord(delay), sendMessage(err))
		if snooze {
			note = fmt.Sprintf("被限流了，%s后自动重试：%s", durationWord(delay), sendMessage(err))
		}
		if dbErr := w.q.NoteWebhookSendRetry(ctx, sqlc.NoteWebhookSendRetryParams{ID: send.ID, Error: note}); dbErr != nil {
			log.Warn("重试的原因没能写入数据库", "err", dbErr)
		}
		if snooze {
			return river.JobSnooze(delay)
		}
		return err
	}
	log.Error("发到群机器人失败", "err", err)
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if dbErr := w.q.FailWebhookSend(bg, sqlc.FailWebhookSendParams{ID: send.ID, Error: sendMessage(err)}); dbErr != nil {
		log.Error("失败原因没能写入数据库", "err", dbErr)
	}
	return river.JobCancel(err)
}

// 排定的重试开始得晚一点（River 排队、上一条还在发）也照样发，最多晚这么久
const webhookLateStart = 2 * time.Minute

// webhookRetry 决定第 attempt 次没发出去（err）以后怎么办。created 是发送记录创建的时间，now 是现在。
//
// 平台限流（发得太快）时等够平台要求的时间再试（钉钉 11 分钟），用 River 的 snooze，不算一次重试：
// 还没到时候，试多少次都一样被拒。其他能重试的错误按 shareRetryDelays 重试，最多 maxAttempts 次。
// 不管哪种，下一次都不能排到发送开始后 service.WebhookRetryUntil 以后，排不下就不再重试（ok 为 false），
// 这样「发送中」最长也不会超过 service.WebhookPendingWindow。配置不对（重试也没用）的同样返回 ok 为 false。
func webhookRetry(err error, attempt, maxAttempts int, created, now time.Time) (delay time.Duration, snooze, ok bool) {
	var perm permanentError
	if errors.As(err, &perm) || !webhook.Retryable(err) {
		return 0, false, false
	}
	if wait := webhook.RetryWait(err); wait > 0 {
		delay, snooze = wait, true
	} else {
		if attempt >= maxAttempts {
			return 0, false, false
		}
		delay = webhookRetryDelay(attempt)
	}
	if now.Add(delay).After(created.Add(service.WebhookRetryUntil)) {
		return 0, false, false
	}
	return delay, snooze, true
}

// webhookRetryDelay 是第 attempt 次失败（不是限流）以后过多久再试：和发到飞书一样，有人在网页上等着结果，间隔短一些。
func webhookRetryDelay(attempt int) time.Duration {
	return shareRetryDelays[min(max(attempt, 1), len(shareRetryDelays))-1]
}

// sendMessage 是写给网页看的失败原因：群机器人返回的原因（已经去掉了地址和密钥）原样给，内部错误（数据库等）只说没发出去。
func sendMessage(err error) string {
	var perm permanentError
	if e, ok := errors.AsType[*webhook.Error](err); ok {
		return e.Msg
	}
	if errors.As(err, &perm) {
		return perm.Error()
	}
	return "服务器内部出错了，没发出去"
}

// send 发一条。返回 nil 表示群机器人已经收下了，之后不管怎样都不能再让 River 重试（群里会收到两条一样的）。
//
// 从检查会议到发完，一直拿着会议这一行的共享锁（ShareLockMeeting）：改可见范围、删除会议都要先锁住会议，
// 只能排在这次发送前面（这里就看得到，不发）或者后面（发的时候确实是团队可见的）。代价是这段时间里改会议的操作
// 要等这一条发完，最多等 webhook.SendTimeout（15 秒），一般一秒以内；webhook 队列一次只发一条，也只占一个数据库连接。
func (ww *webhookWorker) send(ctx context.Context, send sqlc.WebhookSend) error {
	w := ww.w
	if send.WebhookID == nil {
		return permanent("这个群机器人已经被管理员删掉了")
	}
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	q := w.q.WithTx(tx)

	_, err = q.ShareLockMeeting(ctx, send.MeetingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return permanent("会议已经删除了")
	}
	if err != nil {
		return err
	}
	hook, err := q.GetWebhook(ctx, *send.WebhookID)
	if errors.Is(err, pgx.ErrNoRows) {
		return permanent("这个群机器人已经被管理员删掉了")
	}
	if err != nil {
		return err
	}
	row, err := q.GetMeeting(ctx, send.MeetingID)
	if err != nil {
		return err
	}
	m := row.Meeting
	if m.DeletedAt != nil {
		return permanent("会议已经删除了")
	}
	// 自动发送（没有发起人）只发团队里所有人都能看的会议。排队以后（重试期间）会议可能被改成了别的可见范围
	if send.RequestedBy == nil {
		if m.Visibility != service.VisibilityTeam {
			return permanent("会议已经改成不是团队所有人可见，不再自动发到群里")
		}
		if !hook.AutoPush {
			return permanent("管理员关掉了这个群的自动发送")
		}
	}
	mv, err := q.GetMinutesVersion(ctx, sqlc.GetMinutesVersionParams{MeetingID: m.ID, Version: send.MinutesVersion})
	if err != nil {
		return err
	}
	site, err := w.settings.SiteURL(ctx)
	if err != nil {
		return err
	}
	meetingURL := ""
	if site != "" {
		meetingURL = site + "/meetings/" + m.ID.String()
	}

	u, err := w.settings.Open(hook.Url)
	if err != nil {
		return permanent("解不开保存的 Webhook 地址：ZSMEET_SECRET_KEY 可能被改过，请管理员重新填写这个群机器人的地址")
	}
	sec, err := w.settings.Open(hook.Secret)
	if err != nil {
		return permanent("解不开保存的加签密钥：ZSMEET_SECRET_KEY 可能被改过，请管理员重新填写")
	}
	payload, err := webhook.Message{
		Title: m.Title, When: m.CreatedAt, Duration: int(m.DurationSec), Owner: row.OwnerName,
		Minutes: mv.Content, MeetingURL: meetingURL,
	}.Payload(hook.Platform)
	if err != nil {
		return err
	}
	err = webhook.Send(ctx, webhook.Target{Platform: hook.Platform, URL: u, Secret: sec, TestHost: w.webhookTestHost}, payload)
	if err != nil {
		if !webhook.Retryable(err) {
			return permanentError{err}
		}
		return err
	}
	// 发出去了：先放开会议的锁（事务里只读过，回滚就行），再记成已发送
	_ = tx.Rollback(context.WithoutCancel(ctx))
	ww.finish(ctx, send)
	return nil
}

// finishTries 是群机器人收下以后，记成「已发送」最多试几次、每次隔多久
var finishTries, finishPause = 3, time.Second

// finish 把发出去了的 send 记成已发送。写不进去（数据库暂时连不上）就隔一会儿再试几次；还是不行只记日志：
// 不能让 River 重试，否则群里会再收到一条一样的。这一条在网页上会一直是发送中，过了 WebhookPendingWindow 报成没发出去。
func (ww *webhookWorker) finish(ctx context.Context, send sqlc.WebhookSend) {
	bg := context.WithoutCancel(ctx)
	var err error
	for i := range finishTries {
		if i > 0 {
			time.Sleep(finishPause)
		}
		tctx, cancel := context.WithTimeout(bg, 15*time.Second)
		err = ww.w.q.FinishWebhookSend(tctx, send.ID)
		cancel()
		if err == nil {
			return
		}
	}
	slog.Error("纪要已经发到群里了，但没能记成已发送", "meeting", send.MeetingID, "send", send.ID, "err", err)
}

// NextRetry 决定第 job.Attempt 次失败以后，过多久再试。和 webhookRetry 里算的一样（限流的不走这里，是 snooze）。
func (ww *webhookWorker) NextRetry(job *river.Job[queue.WebhookSendArgs]) time.Time {
	return time.Now().Add(webhookRetryDelay(job.Attempt))
}

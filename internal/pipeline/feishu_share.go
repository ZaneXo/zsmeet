package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/export"
	"github.com/ZaneXo/zsmeet/internal/feishu"
	"github.com/ZaneXo/zsmeet/internal/queue"
	"github.com/ZaneXo/zsmeet/internal/service"
	"github.com/ZaneXo/zsmeet/internal/settings"
	"github.com/ZaneXo/zsmeet/internal/transcript"
)

// shareWorker 把纪要发到飞书：需要的话先导入成飞书文档，再把纪要卡片推到群里。照着 feishu_shares 表里的一条做。
type shareWorker struct {
	river.WorkerDefaults[queue.FeishuShareArgs]
	w *Worker
}

// queueShare 在事务 tx 里记下一次「发到飞书默认群」，并加上后台任务。纪要第一次生成后自动发时用。
func (w *Worker) queueShare(ctx context.Context, tx pgx.Tx, q *sqlc.Queries, meeting uuid.UUID, version int32, cfg settings.Feishu) error {
	share, err := q.CreateShare(ctx, sqlc.CreateShareParams{
		MeetingID: meeting, ChatID: cfg.ChatID, ChatName: cfg.ChatName, WithDoc: cfg.WithDoc, MinutesVersion: version,
	})
	if err != nil {
		return err
	}
	_, err = w.queue.InsertTx(ctx, tx, queue.FeishuShareArgs{ShareID: share.ID}, nil)
	return err
}

// permanentError 是重试也没用的错误（配置被清掉了、会议删了……）。
type permanentError struct{ error }

func permanent(msg string) error { return permanentError{errors.New(msg)} }

func (sw *shareWorker) Work(ctx context.Context, job *river.Job[queue.FeishuShareArgs]) error {
	w := sw.w
	share, err := w.q.GetShare(ctx, job.Args.ShareID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // 会议被彻底删除了，记录跟着没了
	}
	if err != nil {
		return err
	}
	if share.Status != "pending" {
		return nil
	}
	log := slog.With("meeting", share.MeetingID, "share", share.ID)
	// 发送中超过半小时，网页上就允许重新发了（见 PendingShare）：这一条不再发，免得群里收到两张一样的卡片
	if time.Since(share.CreatedAt) > sharePendingWindow {
		if err := w.q.FailShare(ctx, sqlc.FailShareParams{ID: share.ID, Error: "等太久了，没有发出去。可以重新发一次"}); err != nil {
			return err
		}
		return river.JobCancel(errors.New("发送中太久了"))
	}

	err = sw.send(ctx, share)
	var perm permanentError
	var snooze *rivertype.JobSnoozeError
	switch {
	case err == nil:
		log.Info("纪要已发到飞书", "chat", share.ChatName, "doc", share.WithDoc)
		return nil
	case errors.As(err, &snooze):
		return err // 飞书还在导入文档，过一会儿接着等
	case errors.Is(ctx.Err(), context.Canceled):
		return ctx.Err() // worker 正在退出，River 会在它重新起来后再做
	case !errors.As(err, &perm) && feishu.Retryable(err) && job.Attempt < job.MaxAttempts:
		log.Warn("发到飞书失败，稍后重试", "attempt", job.Attempt, "err", err)
		note := fmt.Sprintf("第 %d 次没发出去，%s后自动重试：%v", job.Attempt, retryWord(job.Attempt), err)
		if dbErr := w.q.NoteShareRetry(ctx, sqlc.NoteShareRetryParams{ID: share.ID, Error: note}); dbErr != nil {
			log.Warn("重试的原因没能写入数据库", "err", dbErr)
		}
		return err
	}
	log.Error("发到飞书失败", "err", err)
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15e9)
	defer cancel()
	if dbErr := w.q.FailShare(bg, sqlc.FailShareParams{ID: share.ID, Error: err.Error()}); dbErr != nil {
		log.Error("失败原因没能写入数据库", "err", dbErr)
	}
	return river.JobCancel(err)
}

// send 发一条。返回 nil 表示已经发出去了（卡片发到了群里、文档开好了权限），之后不管怎样都不能再让 River 重试。
//
// 会把纪要发出去的几步（把纪要传给飞书导入成文档；给群、公司里的人和发送的人开文档的权限并发卡片）各自在 locked 里做：
// 拿着会议这一行的共享锁，按锁住时的会议再检查一遍（删了没有，自动发送的还是不是团队可见，手动发的发起人还能不能编辑），再调飞书。
// 改可见范围、删除会议都要先锁住会议，只能排在这一步前面（这里就看得到，不发）或者后面（发的时候确实是可以发的）。
// 等飞书导入文档可能要好几分钟、中间还要 snooze，不能一直拿着锁：等导入结果的时候不拿锁，下一步发之前重新锁、重新检查。
func (sw *shareWorker) send(ctx context.Context, share sqlc.FeishuShare) error {
	w := sw.w
	cfg, err := w.settings.Feishu(ctx)
	if err != nil {
		return err
	}
	if !cfg.Ready() {
		return permanent("管理员把飞书的配置清掉了，没法发送")
	}
	row, err := w.q.GetMeeting(ctx, share.MeetingID)
	if err != nil {
		return err
	}
	m := row.Meeting
	// 先不拿锁看一遍：明显不用发的不必再去建文件夹、读纪要。真正发之前 locked 里还会再看
	if err := shareAllowed(ctx, w.q, share, m); err != nil {
		return err
	}
	mv, err := w.q.GetMinutesVersion(ctx, sqlc.GetMinutesVersionParams{MeetingID: m.ID, Version: share.MinutesVersion})
	if err != nil {
		return err
	}
	client := w.feishu.Get(feishu.BaseURL(cfg.Domain, w.feishuBase), cfg.AppID, cfg.AppSecret)
	meetingURL := ""
	if cfg.SiteURL != "" {
		meetingURL = strings.TrimRight(cfg.SiteURL, "/") + "/meetings/" + m.ID.String()
	}

	// 飞书文档：这一版纪要导入过就直接用，没有、又要导入时才导入。
	// reused 表示文档不是这一条导入的（重试时也要记得：这一条上记着文档、却没有导入任务号）
	doc := feishu.Doc{Token: share.DocToken, URL: share.DocUrl}
	reused := share.DocToken != "" && share.ImportTicket == ""
	if doc.Token == "" {
		prev, err := w.q.DocForVersion(ctx, sqlc.DocForVersionParams{MeetingID: m.ID, MinutesVersion: share.MinutesVersion})
		switch {
		case err == nil:
			doc, reused = feishu.Doc{Token: prev.DocToken, URL: prev.DocUrl}, true
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
	}
	if reused && share.DocToken == "" {
		// 用的是以前导入的文档：也记在这一条上，网页上的这条记录才有文档的链接
		if err := w.q.SetShareDoc(ctx, sqlc.SetShareDocParams{ID: share.ID, DocToken: doc.Token, DocUrl: doc.URL}); err != nil {
			return err
		}
	}
	if share.WithDoc && doc.Token == "" {
		ticket := share.ImportTicket
		if ticket == "" {
			info := export.Info(m.CreatedAt, int(m.DurationSec), row.OwnerName, transcript.ParseNames(m.Speakers))
			if meetingURL != "" {
				info = append(info, "录音和逐句转写：[在 zsmeet 里查看]("+meetingURL+")")
			}
			parent := cfg.DocFolder
			if parent == "" {
				if parent, err = client.RootFolder(ctx); err != nil {
					return err
				}
			}
			// 按月份放进子文件夹：飞书一个文件夹里最多放 1500 个文件
			folder, err := client.Folder(ctx, parent, m.CreatedAt.Local().Format("2006-01"))
			if err != nil {
				return err
			}
			// 纪要一传上去就算发出去了（文件夹共享给了别人的话，导入好就能看到）：拿着锁传
			err = sw.locked(ctx, share, func(ctx context.Context) error {
				ticket, err = client.StartImport(ctx, folder, m.Title, export.MinutesBody(info, mv.Content))
				return err
			})
			if err != nil {
				return err
			}
			// 马上记下任务号：之后出错重试、snooze 以后接着等这个任务，不会再导入一篇。
			// 实在记不下来就不能再让 River 重试了，不然会再导入一篇
			if err := persist(ctx, func(ctx context.Context) error {
				return w.q.SetShareTicket(ctx, sqlc.SetShareTicketParams{ID: share.ID, ImportTicket: ticket})
			}); err != nil {
				slog.Error("飞书已经开始导入文档，但没能记下任务号", "meeting", m.ID, "share", share.ID, "ticket", ticket, "err", err)
				return permanent("飞书已经开始导入文档，但服务器没能记下来。为了不导入出两篇一样的文档，不再自动重试，可以过一会儿重新发一次")
			}
		}
		// 等导入结果不拿锁：最多等 2 分钟，没好就 snooze，都不能一直锁着会议
		doc, err = client.ImportResult(ctx, ticket)
		if errors.Is(err, feishu.ErrImportPending) {
			// 飞书还在导入：过一会儿接着等（不算重试次数）。太久了就不等了
			if time.Since(share.CreatedAt) > shareGiveUp {
				return permanent("飞书导入文档太久了，没有等到结果。可以过一会儿再发一次")
			}
			return river.JobSnooze(time.Minute)
		}
		if err != nil {
			return err
		}
		// 马上记下来。记不下来也没关系：重试时用记下的任务号再查一次，拿到的还是这一篇，不会再导入
		if err := w.q.SetShareDoc(ctx, sqlc.SetShareDocParams{ID: share.ID, DocToken: doc.Token, DocUrl: doc.URL}); err != nil {
			return err
		}
	}

	var card string
	if share.ChatID != "" {
		updated, err := w.q.SharedBefore(ctx, sqlc.SharedBeforeParams{MeetingID: m.ID, ChatID: share.ChatID, Version: share.MinutesVersion})
		if err != nil {
			return err
		}
		card, err = feishu.Card{
			Title: m.Title, When: m.CreatedAt, Duration: int(m.DurationSec), Owner: row.OwnerName,
			Minutes: mv.Content, MeetingURL: meetingURL, DocURL: doc.URL, Updated: updated,
		}.JSON()
		if err != nil {
			return err
		}
	}
	var requester string
	if doc.Token != "" && share.RequestedBy != nil {
		u, err := w.q.GetUser(ctx, *share.RequestedBy)
		if err != nil {
			return err
		}
		requester = u.Email
	}

	// 开权限和发卡片放在同一把锁里：会议改成不能发了的话，群里既拿不到文档的权限，也收不到卡片
	messageID := ""
	err = sw.locked(ctx, share, func(ctx context.Context) error {
		// 文档归应用所有，不给权限谁都打不开：发到的群里的人能看，管理员打开了的话公司里拿到链接的人也能看
		if doc.Token != "" && (share.WithDoc || share.ChatID != "") {
			if err := grantDoc(ctx, client, doc.Token, share.ChatID, cfg.LinkShare); err != nil {
				if !reused {
					return err
				}
				// 以前导入的文档多半已经给过权限了，这次再给失败不影响发卡片
				slog.Warn("给飞书文档加权限失败", "doc", doc.Token, "err", err)
			}
			// 发的人自己也要能打开、能再分享（按 zsmeet 账号的邮箱找飞书里的人）
			if requester != "" {
				if err := client.ShareWithUser(ctx, doc.Token, requester); err != nil {
					if share.ChatID == "" && !cfg.LinkShare {
						if feishu.Retryable(err) {
							return err // 网络问题：重试时文档已经记下了，不会再导入
						}
						return permanentError{fmt.Errorf("文档导入好了，但没法给你打开它的权限（%v）。请发到一个群，或者请管理员在「设置 → 飞书」里打开「公司里拿到链接的人都能看」", err)}
					}
					slog.Warn("没能给发送的人加飞书文档的权限", "doc", doc.Token, "err", err)
				}
			}
		}
		if share.ChatID == "" {
			return nil
		}
		// 用这条记录的 ID 去重：重试时（一小时内）飞书不会发出第二张
		var err error
		messageID, err = client.SendCard(ctx, share.ChatID, card, share.ID.String())
		return err
	})
	if err != nil {
		return err
	}
	// 发出去了：锁已经放开，记成已发送
	sw.finish(ctx, share, messageID)
	return nil
}

// shareAllowed 检查会议 m 现在还能不能照着 share 发。q 可以是事务里的（locked 里用锁住会议的那个事务）。
func shareAllowed(ctx context.Context, q *sqlc.Queries, share sqlc.FeishuShare, m sqlc.Meeting) error {
	if m.DeletedAt != nil {
		return permanent("会议已经删除了")
	}
	// 自动发送（没有发起人）只发团队里所有人都能看的会议。排队以后（重试、等文档导入期间）会议可能被改成了别的可见范围
	if share.RequestedBy == nil && m.Visibility != service.VisibilityTeam {
		return permanent("会议已经改成不是团队所有人可见，不再自动发到群里")
	}
	return requesterAllowed(ctx, q, share.RequestedBy, m)
}

// requesterAllowed 检查手动发送的发起人（requestedBy，自动发送的是 nil，不用查）现在还能不能编辑会议 m：
// 排上队以后（重试、等文档导入期间，最长半小时）他可能被停用、被取消了编辑权限，或者会议改成了他看不到的，
// 这时不能再替他把纪要发出去。和网页上「发到飞书」「发到群里」要的权限一样（service.CanEditMeeting）。
func requesterAllowed(ctx context.Context, q *sqlc.Queries, requestedBy *uuid.UUID, m sqlc.Meeting) error {
	if requestedBy == nil {
		return nil
	}
	user, err := q.GetUser(ctx, *requestedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return permanent("发起人的账号已经不在了，不再发送")
	}
	if err != nil {
		return err
	}
	if user.Disabled {
		return permanent("发起人的账号已经停用了，不再发送")
	}
	ok, err := service.CanEditMeeting(ctx, q, user, m)
	if err != nil {
		return err
	}
	if !ok {
		return permanent("发起人已经不能编辑这场会议了，不再发送")
	}
	return nil
}

// 拿着会议的锁调飞书时，最多调这么久：改可见范围、删除会议、重新生成纪要最多为这一步等这么久
const shareStepTimeout = 30 * time.Second

// locked 拿着会议这一行的共享锁（ShareLockMeeting），按锁住时的会议和发起人的权限检查还能不能发（shareAllowed），再做 step。
// step 是会把纪要发出去的一步（调飞书），限时 shareStepTimeout；做完就放开锁。
//
// 代价是这段时间里改会议的操作（包括改标题、重新生成纪要）要等它做完，最多 shareStepTimeout，一般一两秒；
// 发到飞书的队列一次只发一条，也只多占一个数据库连接。
func (sw *shareWorker) locked(ctx context.Context, share sqlc.FeishuShare, step func(ctx context.Context) error) error {
	w := sw.w
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return err
	}
	// 事务里只读过，回滚就是放开锁
	defer tx.Rollback(context.WithoutCancel(ctx))
	q := w.q.WithTx(tx)
	m, err := q.ShareLockMeeting(ctx, share.MeetingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return permanent("会议已经删除了")
	}
	if err != nil {
		return err
	}
	if err := shareAllowed(ctx, q, share, m); err != nil {
		return err
	}
	sctx, cancel := context.WithTimeout(ctx, shareStepTimeout)
	defer cancel()
	return step(sctx)
}

// finish 把发出去了的 share 记成已发送。写不进去（数据库暂时连不上）就隔一会儿再试几次；还是不行只记日志：
// 不能让 River 重试。卡片虽然有去重，重试时会议可能已经改成了不能发、已经超过了半小时，这条会被记成「没发出去」；
// 不发卡片、只导入文档的，重试会再开一遍权限。这一条在网页上会一直是发送中，过了半小时就允许重新发。
func (sw *shareWorker) finish(ctx context.Context, share sqlc.FeishuShare, messageID string) {
	err := persist(ctx, func(ctx context.Context) error {
		return sw.w.q.FinishShare(ctx, sqlc.FinishShareParams{ID: share.ID, MessageID: messageID})
	})
	if err != nil {
		slog.Error("纪要已经发到飞书了，但没能记成已发送", "meeting", share.MeetingID, "share", share.ID, "err", err)
	}
}

// 发送开始这么久还没发完就不再等了（网页上「发送中」超过半小时就不再挡着重新发，见 PendingShare）
const (
	shareGiveUp        = 25 * time.Minute
	sharePendingWindow = 30 * time.Minute
)

// 发到飞书是有人在网页上等着结果的操作：重试间隔短一些（River 默认的间隔越来越长，几次以后要等十几分钟）
var shareRetryDelays = []time.Duration{10 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute}

// NextRetry 决定第 job.Attempt 次失败以后，过多久再试。
func (sw *shareWorker) NextRetry(job *river.Job[queue.FeishuShareArgs]) time.Time {
	return time.Now().Add(shareRetryDelays[min(max(job.Attempt, 1), len(shareRetryDelays))-1])
}

// retryWord 是第 attempt 次失败以后，网页上说的「多久以后重试」。
func retryWord(attempt int) string {
	return durationWord(shareRetryDelays[min(max(attempt, 1), len(shareRetryDelays))-1])
}

// durationWord 是网页上说的「多久」：不到一分钟的说几秒，其他的说几分钟。
func durationWord(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	}
	return fmt.Sprintf("%d 分钟", int(d.Minutes()))
}

// grantDoc 让 chat 群里的人能看这篇文档；inTenant 为 true 时公司里拿到链接的人也能看。
func grantDoc(ctx context.Context, client *feishu.Client, doc, chat string, inTenant bool) error {
	if chat != "" {
		if err := client.ShareWithChat(ctx, doc, chat); err != nil {
			return err
		}
	}
	if inTenant {
		return client.ShareInTenant(ctx, doc)
	}
	return nil
}

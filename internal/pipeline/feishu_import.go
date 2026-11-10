package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/feishu"
	"github.com/ZaneXo/zsmeet/internal/queue"
	"github.com/ZaneXo/zsmeet/internal/search"
	"github.com/ZaneXo/zsmeet/internal/storage"
)

// importWorker 导入一场结束了的飞书会议的云录制：组织者是 zsmeet 的成员时，等飞书生成好录制，
// 下载下来，建成这位成员名下的会议，交给后台转写、写纪要。
type importWorker struct {
	river.WorkerDefaults[queue.FeishuRecordingArgs]
	w *Worker
}

const (
	// 从会议结束算，最多等飞书生成录制这么久
	recordingWait = 12 * time.Hour
	// 会议结束这么久以后还查不到录制，就认为这场会议没有云录制
	noRecordingWait = 30 * time.Minute
	// 录制还没好时，过多久再查
	recheckEvery = time.Minute
	// 会议标题最长多少字（和网页上一样）
	maxTitleRunes = 200
)

func (iw *importWorker) Work(ctx context.Context, job *river.Job[queue.FeishuRecordingArgs]) error {
	w, a := iw.w, job.Args
	cfg, err := w.settings.Feishu(ctx)
	if err != nil {
		return err
	}
	if !cfg.Ready() || !cfg.ImportRecordings {
		return nil // 管理员关掉了自动导入
	}
	prev, err := w.q.GetFeishuImport(ctx, a.MeetingID)
	switch {
	case err == nil && prev.Status == "imported":
		return nil // 同一场会议的事件推了两次
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	client := w.feishu.Get(feishu.BaseURL(cfg.Domain, w.feishuBase), cfg.AppID, cfg.AppSecret)

	// 只导入组织者是 zsmeet 成员的会议，别人的会议连主题都不记
	owner, err := iw.member(ctx, client, a.OwnerOpenID)
	if err != nil {
		if feishu.Retryable(err) && job.Attempt < job.MaxAttempts {
			return err
		}
		iw.note(ctx, "一场飞书会议结束了，但读不到组织者的信息，没有导入："+err.Error())
		return river.JobCancel(err)
	}
	if owner == nil {
		iw.note(ctx, "一场飞书会议结束了，组织者不是 zsmeet 的成员（按邮箱对不上），没有导入")
		return nil
	}

	topic := iw.title(a)
	ended := time.Unix(a.End, 0)
	if a.End == 0 {
		ended = job.CreatedAt
	}
	if err := iw.save(ctx, a, topic, "waiting", "等飞书生成录制", owner.ID); err != nil {
		return err
	}
	if prev.Status == "" {
		// 第一次处理这场会议（等录制时会反复回来查，只记一次）
		iw.note(ctx, fmt.Sprintf("「%s」结束了，等飞书生成录制", topic))
	}

	link, err := client.Recording(ctx, a.MeetingID)
	switch {
	case feishu.IsCode(err, feishu.CodeRecordingProcessing):
		return iw.later(ctx, a, topic, owner.ID, ended)
	case feishu.IsCode(err, feishu.CodeNoRecording):
		if time.Since(ended) < noRecordingWait {
			return river.JobSnooze(2 * recheckEvery) // 录制可能还没开始生成
		}
		iw.note(ctx, fmt.Sprintf("「%s」没有云录制，不用导入", topic))
		return iw.save(ctx, a, topic, "skipped", "这场会议没有云录制", owner.ID)
	case err != nil:
		return iw.fail(ctx, job, topic, owner.ID, err)
	}
	minute, err := feishu.MinuteToken(link)
	if err != nil {
		return iw.fail(ctx, job, topic, owner.ID, permanentError{err})
	}
	media, err := client.MediaURL(ctx, minute)
	switch {
	case feishu.IsCode(err, feishu.CodeMinuteNotReady):
		return iw.later(ctx, a, topic, owner.ID, ended)
	case err != nil:
		return iw.fail(ctx, job, topic, owner.ID, err)
	}

	file, err := iw.download(ctx, client, media)
	if err != nil {
		return iw.fail(ctx, job, topic, owner.ID, err)
	}
	id, err := iw.create(ctx, a, topic, *owner, file)
	if err != nil {
		os.Remove(file)
		return err
	}
	slog.Info("飞书会议的录制已导入", "feishu_meeting", a.MeetingID, "meeting", id, "owner", owner.Email)
	iw.note(ctx, fmt.Sprintf("「%s」的录制已经导入，正在转写", topic))
	return nil
}

// member 按会议组织者在飞书通讯录里的邮箱找 zsmeet 的成员（停用的不算）。找不到返回 nil。
func (iw *importWorker) member(ctx context.Context, client *feishu.Client, openID string) (*sqlc.User, error) {
	if openID == "" {
		return nil, nil
	}
	u, err := client.User(ctx, openID)
	if err != nil {
		return nil, err
	}
	for _, email := range []string{u.Email, u.EnterpriseEmail} {
		email = strings.ToLower(strings.TrimSpace(email))
		if email == "" {
			continue
		}
		user, err := iw.w.q.GetUserByEmail(ctx, email)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			continue
		case err != nil:
			return nil, err
		case !user.Disabled:
			return &user, nil
		}
	}
	return nil, nil
}

// title 是导入后的会议标题：飞书会议的主题，没有主题时用「飞书会议 + 开始时间」。
func (iw *importWorker) title(a queue.FeishuRecordingArgs) string {
	t := strings.TrimSpace(a.Topic)
	if t == "" {
		t = "飞书会议 " + start(a).Local().Format("1月2日 15:04")
	}
	if utf8.RuneCountInString(t) > maxTitleRunes {
		t = string([]rune(t)[:maxTitleRunes])
	}
	return t
}

// start 是飞书会议开始的时间，导入后的会议按它排序。
func start(a queue.FeishuRecordingArgs) time.Time {
	return feishu.MeetingEnded{Start: a.Start, End: a.End}.StartTime()
}

// later 录制还没生成好：过一会儿再查（不算重试次数），等太久了就放弃。
func (iw *importWorker) later(ctx context.Context, a queue.FeishuRecordingArgs, topic string, owner uuid.UUID, ended time.Time) error {
	if time.Since(ended) > recordingWait {
		iw.note(ctx, fmt.Sprintf("「%s」等了 12 小时，飞书还没生成好录制，不再等了", topic))
		return iw.save(ctx, a, topic, "failed", "等了 12 小时，飞书还没生成好录制", owner)
	}
	return river.JobSnooze(recheckEvery)
}

// fail 处理出错：网络问题、飞书繁忙时让 River 稍后重试；其他的记下原因，不再重试。
func (iw *importWorker) fail(ctx context.Context, job *river.Job[queue.FeishuRecordingArgs], topic string, owner uuid.UUID, err error) error {
	var perm permanentError
	if !errors.As(err, &perm) && feishu.Retryable(err) && job.Attempt < job.MaxAttempts && ctx.Err() == nil {
		slog.Warn("导入飞书会议的录制失败，稍后重试", "feishu_meeting", job.Args.MeetingID, "attempt", job.Attempt, "err", err)
		return err
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return ctx.Err() // worker 正在退出
	}
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	iw.note(bg, fmt.Sprintf("「%s」的录制没能导入：%v", topic, err))
	if dbErr := iw.save(bg, job.Args, topic, "failed", err.Error(), owner); dbErr != nil {
		slog.Error("导入失败的原因没能写入数据库", "err", dbErr)
	}
	return river.JobCancel(err)
}

func (iw *importWorker) save(ctx context.Context, a queue.FeishuRecordingArgs, topic, status, detail string, owner uuid.UUID) error {
	return iw.w.q.SaveFeishuImport(ctx, sqlc.SaveFeishuImportParams{
		FeishuMeetingID: a.MeetingID, Topic: topic, Status: status, Detail: detail, OwnerID: owner,
	})
}

// note 记下最近一次处理飞书会议的情况，设置页面上显示。写不进去不影响导入本身。
func (iw *importWorker) note(ctx context.Context, msg string) {
	if err := iw.w.q.NoteFeishuEvent(ctx, msg); err != nil {
		slog.Warn("飞书导入的情况没能写入数据库", "err", err)
	}
}

// download 把录制下载到数据目录里的临时文件，返回文件路径。
func (iw *importWorker) download(ctx context.Context, client *feishu.Client, media string) (string, error) {
	dir := iw.w.store.Path("tmp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "feishu-*.media")
	if err != nil {
		return "", err
	}
	n, err := client.Download(ctx, media, f, storage.MaxRecordingBytes)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	switch {
	case errors.Is(err, feishu.ErrTooLarge):
		err = permanentError{fmt.Errorf("录制文件超过 %d GB，没有导入", storage.MaxRecordingBytes>>30)}
	case errors.Is(err, syscall.ENOSPC):
		// 磁盘满了：重试只会再把磁盘写满一次
		err = permanentError{errors.New("服务器磁盘空间不够，录制没下载完")}
	case err == nil && n == 0:
		err = permanentError{errors.New("从飞书下载到的录制是空的")}
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// create 建成 zsmeet 的会议：下载好的录制当作唯一的一片，直接排队处理。和记下「已导入」在同一个事务里。
func (iw *importWorker) create(ctx context.Context, a queue.FeishuRecordingArgs, title string, owner sqlc.User, file string) (uuid.UUID, error) {
	w := iw.w
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	q := w.q.WithTx(tx)

	runID := uuid.New()
	m, err := q.CreateImportedMeeting(ctx, sqlc.CreateImportedMeetingParams{
		OwnerID: owner.ID, Title: title, RunID: &runID, CreatedAt: start(a),
	})
	if err != nil {
		return uuid.Nil, err
	}
	size, err := w.store.AdoptChunk(m.ID, 0, file)
	if err != nil {
		return uuid.Nil, err
	}
	committed := false
	defer func() {
		if !committed {
			w.store.RemoveChunks(m.ID)
		}
	}()
	if err := q.AddChunk(ctx, sqlc.AddChunkParams{MeetingID: m.ID, Seq: 0, Size: size}); err != nil {
		return uuid.Nil, err
	}
	if err := q.MarkFeishuImported(ctx, sqlc.MarkFeishuImportedParams{FeishuMeetingID: a.MeetingID, MeetingID: &m.ID}); err != nil {
		return uuid.Nil, err
	}
	if err := search.Reindex(ctx, q, m.ID); err != nil {
		return uuid.Nil, err
	}
	if _, err := w.queue.InsertTx(ctx, tx, queue.ProcessArgs{MeetingID: m.ID, RunID: runID}, nil); err != nil {
		return uuid.Nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}
	committed = true
	return m.ID, nil
}

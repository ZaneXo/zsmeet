package pipeline

import (
	"context"
	"log/slog"
	"time"

	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/feishu"
	"github.com/ZaneXo/zsmeet/internal/queue"
	"github.com/ZaneXo/zsmeet/internal/settings"
)

// listener 在后台和飞书保持长连接，接收「会议结束」事件：配置了飞书、打开了「自动导入会议录制」时连上，
// 凭证改了就用新的重连，关掉了就断开。每隔一会儿看一次配置。
type listener struct {
	w *Worker
	// 正在用的配置（地址 + 凭证），空字符串表示没连
	current string
	// 已经看过一次配置了（第一次看到「没开」时也要把状态写成 off，覆盖上次运行留下的状态）
	started bool
	stop    context.CancelFunc
	done    chan struct{}
}

// 多久看一次配置有没有变（管理员在网页上打开自动导入以后，要等这么久才连上）
const listenCheckEvery = 10 * time.Second

func (l *listener) run(ctx context.Context) {
	ticker := time.NewTicker(listenCheckEvery)
	defer ticker.Stop()
	for {
		l.check(ctx)
		select {
		case <-ctx.Done():
			l.halt()
			return
		case <-ticker.C:
		}
	}
}

func (l *listener) check(ctx context.Context) {
	cfg, err := l.w.settings.Feishu(ctx)
	if err != nil {
		slog.Warn("读不到飞书的配置", "err", err)
		return
	}
	key := ""
	if cfg.Ready() && cfg.ImportRecordings {
		key = feishu.BaseURL(cfg.Domain, l.w.feishuBase) + "\x00" + cfg.AppID + "\x00" + cfg.AppSecret
	}
	if key == l.current && l.started {
		// 配置没变：只更新一下时间，设置页面看到很久没更新，就知道后台处理（worker）没在运行
		if err := l.w.q.TouchFeishuLink(ctx); err != nil {
			slog.Warn("飞书长连接的状态没能更新", "err", err)
		}
		return
	}
	l.started = true
	l.halt()
	l.current = key
	if key == "" {
		l.state("off", "")
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	l.stop, l.done = cancel, make(chan struct{})
	go l.connect(runCtx, cfg, l.done)
}

// halt 断开现在的长连接，等它真的断了再返回。
func (l *listener) halt() {
	if l.stop != nil {
		l.stop()
		<-l.done
		l.stop, l.done = nil, nil
	}
}

func (l *listener) connect(ctx context.Context, cfg settings.Feishu, done chan struct{}) {
	defer close(done)
	base := feishu.BaseURL(cfg.Domain, l.w.feishuBase)
	for {
		l.state("connecting", "")
		err := feishu.Listen(ctx, base, cfg.AppID, cfg.AppSecret, l.handle, feishu.Hooks{
			OnReady:        func() { l.state("connected", "") },
			OnReconnected:  func() { l.state("connected", "") },
			OnError:        func(err error) { l.state("error", err.Error()) },
			OnReconnecting: func() { l.state("connecting", "连接断了，正在重连") },
		})
		if ctx.Err() != nil {
			return
		}
		msg := "和飞书的长连接断开了"
		if err != nil {
			msg = err.Error()
		}
		slog.Warn("飞书长连接断开，一分钟后重连", "err", msg)
		l.state("error", msg)
		// 凭证不对之类的错误：过一会儿再试，管理员可能正在开发者后台改配置
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
	}
}

// handle 收到一场会议结束。企业里每一场会议都会推过来，所以先查组织者是不是 zsmeet 的成员：
// 不是的话什么都不存（连会议主题都不进任务队列），是的话加一个导入任务。飞书要求 3 秒内回复，
// 查不出来（网络问题、超时）就返回错误，飞书过一会儿会重新推送。
func (l *listener) handle(ctx context.Context, ev feishu.MeetingEnded) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cfg, err := l.w.settings.Feishu(ctx)
	if err != nil {
		return err
	}
	if !cfg.Ready() || !cfg.ImportRecordings {
		return nil
	}
	client := l.w.feishu.Get(feishu.BaseURL(cfg.Domain, l.w.feishuBase), cfg.AppID, cfg.AppSecret)
	owner, err := (&importWorker{w: l.w}).member(ctx, client, ev.OwnerOpenID)
	switch {
	case err != nil && feishu.Retryable(err):
		return err
	case err != nil:
		l.note("一场飞书会议结束了，但读不到组织者的信息，没有导入：" + err.Error())
		return nil
	case owner == nil:
		l.note("一场飞书会议结束了，组织者不是 zsmeet 的成员（按邮箱对不上），没有导入")
		return nil
	}
	_, err = l.w.queue.Insert(ctx, queue.FeishuRecordingArgs{
		MeetingID: ev.MeetingID, Topic: ev.Topic, Start: ev.Start, End: ev.End, OwnerOpenID: ev.OwnerOpenID,
	}, nil)
	return err
}

// note 记下最近一次收到的会议结束的情况，设置页面上显示。
func (l *listener) note(msg string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := l.w.q.NoteFeishuEvent(ctx, msg); err != nil {
		slog.Warn("飞书事件没能记下来", "err", err)
	}
}

// state 记下长连接现在的状态，设置页面上显示。
func (l *listener) state(state, detail string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := l.w.q.SetFeishuLink(ctx, sqlc.SetFeishuLinkParams{State: state, Detail: detail}); err != nil {
		slog.Warn("飞书长连接的状态没能写入数据库", "err", err)
	}
}

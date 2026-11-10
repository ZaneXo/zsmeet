package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

// EventMeetingEnded 是企业里任意一场飞书会议结束时的事件。只要应用有 vc:meeting.all_meeting:readonly 权限、
// 在开发者后台订阅了这个事件就会收到，不需要每个人授权，也不需要机器人进会。
const EventMeetingEnded = "vc.meeting.all_meeting_ended_v1"

// MeetingEnded 是「一场飞书会议结束了」。事件里没有录制的链接，录制要等它生成好以后再去查。
type MeetingEnded struct {
	// 事件 ID，飞书可能重复推送同一个事件
	EventID   string `json:"event_id"`
	MeetingID string `json:"meeting_id"`
	Topic     string `json:"topic"`
	Start     int64  `json:"start"` // unix 秒
	End       int64  `json:"end"`
	// 会议拥有者（组织者）的 open_id；没有时是主持人的
	OwnerOpenID string `json:"owner_open_id"`
}

// ParseMeetingEnded 解析「会议结束」事件的原始 JSON。
func ParseMeetingEnded(body []byte) (MeetingEnded, error) {
	type user struct {
		ID struct {
			OpenID string `json:"open_id"`
		} `json:"id"`
	}
	var raw struct {
		Header struct {
			EventID   string `json:"event_id"`
			EventType string `json:"event_type"`
		} `json:"header"`
		Event struct {
			Meeting struct {
				ID        string `json:"id"`
				Topic     string `json:"topic"`
				StartTime string `json:"start_time"`
				EndTime   string `json:"end_time"`
				Owner     *user  `json:"owner"`
				HostUser  *user  `json:"host_user"`
			} `json:"meeting"`
		} `json:"event"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return MeetingEnded{}, err
	}
	m := raw.Event.Meeting
	if m.ID == "" {
		return MeetingEnded{}, errors.New("事件里没有会议 ID")
	}
	ev := MeetingEnded{EventID: raw.Header.EventID, MeetingID: m.ID, Topic: m.Topic}
	ev.Start, _ = strconv.ParseInt(m.StartTime, 10, 64)
	ev.End, _ = strconv.ParseInt(m.EndTime, 10, 64)
	switch {
	case m.Owner != nil && m.Owner.ID.OpenID != "":
		ev.OwnerOpenID = m.Owner.ID.OpenID
	case m.HostUser != nil:
		ev.OwnerOpenID = m.HostUser.ID.OpenID
	}
	return ev, nil
}

// Hooks 是长连接状态变化时的回调，用来在设置页面上显示连接状态。都可以为 nil。
type Hooks struct {
	// 第一次连上（SDK 只在第一次连接就成功时调用它，之后重新连上调的是 OnReconnected）
	OnReady        func()
	OnReconnected  func()
	OnError        func(err error)
	OnReconnecting func()
}

// Listen 用长连接（不需要公网地址，服务器能访问飞书就行）接收「会议结束」事件，直到 ctx 取消，
// 或者出现重试也没用的错误（比如凭证不对）。断线会自动重连。
// handle 要在 3 秒内返回：返回错误或者超时，飞书会过一会儿重新推送。
func Listen(ctx context.Context, base, appID, appSecret string, handle func(context.Context, MeetingEnded) error, hooks Hooks) error {
	events := dispatcher.NewEventDispatcher("", "").
		OnCustomizedEvent(EventMeetingEnded, func(ctx context.Context, req *larkevent.EventReq) error {
			ev, err := ParseMeetingEnded(req.Body)
			if err != nil {
				// 看不懂的事件重推也没用，收下就算了
				slog.Warn("飞书会议结束事件看不懂，跳过", "err", err)
				return nil
			}
			return handle(ctx, ev)
		})
	opts := []larkws.ClientOption{
		larkws.WithEventHandler(events),
		larkws.WithDomain(base),
		larkws.WithLogger(sdkLogger{}),
		larkws.WithLogLevel(larkcore.LogLevelWarn),
	}
	if hooks.OnReady != nil {
		opts = append(opts, larkws.WithOnReady(hooks.OnReady))
	}
	if hooks.OnError != nil {
		opts = append(opts, larkws.WithOnError(hooks.OnError))
	}
	if hooks.OnReconnecting != nil {
		opts = append(opts, larkws.WithOnReconnecting(hooks.OnReconnecting))
	}
	if hooks.OnReconnected != nil {
		opts = append(opts, larkws.WithOnReconnected(hooks.OnReconnected))
	}
	err := larkws.NewClient(appID, appSecret, opts...).Start(ctx)
	if ctx.Err() != nil {
		return nil
	}
	var ce *larkws.ClientError
	if errors.As(err, &ce) {
		return fmt.Errorf("飞书拒绝了长连接（App ID、App Secret 不对，或者应用没有启用）：%w", err)
	}
	return err
}

// sdkLogger 把 SDK 的日志转到 slog。
type sdkLogger struct{}

func (sdkLogger) Debug(_ context.Context, args ...any) {
	slog.Debug(fmt.Sprint(args...), "from", "飞书长连接")
}
func (sdkLogger) Info(_ context.Context, args ...any) {
	slog.Info(fmt.Sprint(args...), "from", "飞书长连接")
}
func (sdkLogger) Warn(_ context.Context, args ...any) {
	slog.Warn(fmt.Sprint(args...), "from", "飞书长连接")
}
func (sdkLogger) Error(_ context.Context, args ...any) {
	slog.Error(fmt.Sprint(args...), "from", "飞书长连接")
}

// StartTime 是会议开始的时间；事件里没有时用结束时间，再没有就用现在。
func (e MeetingEnded) StartTime() time.Time {
	switch {
	case e.Start > 0:
		return time.Unix(e.Start, 0)
	case e.End > 0:
		return time.Unix(e.End, 0)
	}
	return time.Now()
}

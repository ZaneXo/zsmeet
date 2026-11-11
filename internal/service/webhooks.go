package service

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/queue"
	"github.com/ZaneXo/zsmeet/internal/secret"
	"github.com/ZaneXo/zsmeet/internal/settings"
	"github.com/ZaneXo/zsmeet/internal/webhook"
)

// ---- zsmeet 的访问地址 ----

// SiteSettings 是 zsmeet 的访问地址：Site 是「设置 → 群机器人」里填的；FeishuURL 是飞书配置里填的，前者为空时用它。
type SiteSettings struct {
	Site      settings.Site
	FeishuURL string
}

func (s *Service) SiteSettings(ctx context.Context) (SiteSettings, error) {
	site, err := s.settings.Site(ctx)
	if err != nil {
		return SiteSettings{}, err
	}
	f, err := s.settings.Feishu(ctx)
	if err != nil && !errors.Is(err, secret.ErrDecrypt) {
		return SiteSettings{}, err
	}
	return SiteSettings{Site: site, FeishuURL: f.SiteURL}, nil
}

// cleanSiteURL 检查 zsmeet 的访问地址（飞书设置和群机器人设置里填的是同一个），去掉结尾的斜杠。空字符串原样返回。
func cleanSiteURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	// 地址要放进各平台 Markdown 的链接里：带空格、括号、尖括号、竖线的会把链接弄断
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		strings.ContainsAny(raw, " \t()[]<>|\"'`") {
		return "", invalid("zsmeet 的访问地址要以 http:// 或 https:// 开头，例如 https://meet.example.com")
	}
	return raw, nil
}

// SaveSite 保存 zsmeet 的访问地址。为空表示不填（有飞书配置里的旧地址时用那个）。
func (s *Service) SaveSite(ctx context.Context, raw string) (SiteSettings, error) {
	raw, err := cleanSiteURL(raw)
	if err != nil {
		return SiteSettings{}, err
	}
	if err := s.settings.SaveSite(ctx, settings.Site{URL: raw}); err != nil {
		return SiteSettings{}, err
	}
	return s.SiteSettings(ctx)
}

// ---- 群机器人：管理员的配置 ----

// 最多加多少个群机器人
const maxWebhooks = 50

var errNoWebhook = notFound("群机器人不存在，可能已被删除")

// Webhooks 列出所有群机器人。Webhook 地址和加签的密钥是加密存的，不会传到接口层之外。
func (s *Service) Webhooks(ctx context.Context) ([]sqlc.Webhook, error) {
	return s.q.ListWebhooks(ctx)
}

// WebhookInput 是网页提交的群机器人。URL、Secret 为空表示沿用已保存的；ClearSecret 表示钉钉不再加签。
type WebhookInput struct {
	Platform    string
	Name        string
	URL         string
	Secret      string
	ClearSecret bool
	AutoPush    bool
}

// resolveWebhook 检查网页提交的群机器人，返回要存进数据库的值（地址和密钥已经加密）。prev 是改之前的，新建时为 nil。
func (s *Service) resolveWebhook(in WebhookInput, prev *sqlc.Webhook) (sqlc.Webhook, error) {
	w := sqlc.Webhook{Platform: in.Platform, Name: strings.TrimSpace(in.Name), AutoPush: in.AutoPush}
	if !slices.Contains(webhook.Platforms, w.Platform) {
		return sqlc.Webhook{}, invalid("不认识的平台：%s（只能是钉钉、企业微信或 Slack）", w.Platform)
	}
	switch n := utf8.RuneCountInString(w.Name); {
	case n == 0:
		return sqlc.Webhook{}, invalid("请给群机器人起个名字，比如它所在的群的名字")
	case n > 50:
		return sqlc.Webhook{}, invalid("名字最多 50 个字")
	}

	if raw := strings.TrimSpace(in.URL); raw != "" {
		checked, err := webhook.CheckURL(w.Platform, raw, s.WebhookTestHost)
		if err != nil {
			return sqlc.Webhook{}, invalid("%s", err.Error())
		}
		w.Url, w.UrlMasked = s.settings.Seal(checked), webhook.Mask(w.Platform, checked)
	} else {
		switch {
		case prev == nil:
			return sqlc.Webhook{}, invalid("请填写 Webhook 地址")
		case prev.Platform != w.Platform:
			return sqlc.Webhook{}, invalid("换了平台，请重新填写 Webhook 地址")
		}
		w.Url, w.UrlMasked = prev.Url, prev.UrlMasked
	}

	// 只有钉钉有「加签」
	if w.Platform == webhook.DingTalk {
		switch sec := strings.TrimSpace(in.Secret); {
		case sec != "":
			if err := webhook.CheckSecret(sec); err != nil {
				return sqlc.Webhook{}, invalid("%s", err.Error())
			}
			w.Secret = s.settings.Seal(sec)
		case in.ClearSecret:
		case prev != nil && prev.Platform == webhook.DingTalk:
			w.Secret = prev.Secret
		}
	}
	return w, nil
}

// 群机器人的锁（事务级的 advisory lock）：加群机器人的事务一个一个来，数出来的个数才不会过期
const webhooksLock = 7_368_825

// CreateWebhook 加一个群机器人。
func (s *Service) CreateWebhook(ctx context.Context, user sqlc.User, in WebhookInput) (sqlc.Webhook, error) {
	w, err := s.resolveWebhook(in, nil)
	if err != nil {
		return sqlc.Webhook{}, err
	}
	var created sqlc.Webhook
	err = s.tx(ctx, func(tx pgx.Tx, q *sqlc.Queries) error {
		// 两个管理员同时加第 50 个：先拿锁再数，后一个数的时候能看到前一个加的
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", webhooksLock); err != nil {
			return err
		}
		if n, err := q.CountWebhooks(ctx); err != nil {
			return err
		} else if n >= maxWebhooks {
			return invalid("最多加 %d 个群机器人，请先删掉不用的再加", maxWebhooks)
		}
		created, err = q.CreateWebhook(ctx, sqlc.CreateWebhookParams{
			Platform: w.Platform, Name: w.Name, Url: w.Url, UrlMasked: w.UrlMasked, Secret: w.Secret, AutoPush: w.AutoPush, CreatedBy: &user.ID,
		})
		return err
	})
	return created, err
}

func (s *Service) getWebhook(ctx context.Context, q *sqlc.Queries, id uuid.UUID) (sqlc.Webhook, error) {
	w, err := q.GetWebhook(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlc.Webhook{}, errNoWebhook
	}
	return w, err
}

// UpdateWebhook 改一个群机器人。
func (s *Service) UpdateWebhook(ctx context.Context, id uuid.UUID, in WebhookInput) (sqlc.Webhook, error) {
	prev, err := s.getWebhook(ctx, s.q, id)
	if err != nil {
		return sqlc.Webhook{}, err
	}
	w, err := s.resolveWebhook(in, &prev)
	if err != nil {
		return sqlc.Webhook{}, err
	}
	w, err = s.q.UpdateWebhook(ctx, sqlc.UpdateWebhookParams{
		ID: id, Platform: w.Platform, Name: w.Name, Url: w.Url, UrlMasked: w.UrlMasked, Secret: w.Secret, AutoPush: w.AutoPush,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlc.Webhook{}, errNoWebhook
	}
	return w, err
}

// DeleteWebhook 删掉一个群机器人。发送记录留着，还在排队的那几条发送时会失败（说明机器人已经删了）。
func (s *Service) DeleteWebhook(ctx context.Context, id uuid.UUID) error {
	n, err := s.q.DeleteWebhook(ctx, id)
	if err == nil && n == 0 {
		err = errNoWebhook
	}
	return err
}

// testMinutes 是测试消息的内容。标题里有「会议纪要」四个字：钉钉机器人设了关键词「会议纪要」的话，测试消息也能发出去。
const testMinutes = "## 摘要\n这是 zsmeet 发来的一条测试消息。看到它，说明群机器人配好了：以后纪要会以这样的格式发到这个群里，带着摘要、待办和查看完整纪要的链接。"

// TestWebhook 往群机器人发一条测试消息，等它发完。地址失效、关键词、加签不对这些都算业务错误，把原因告诉管理员。
func (s *Service) TestWebhook(ctx context.Context, user sqlc.User, id uuid.UUID) error {
	w, err := s.getWebhook(ctx, s.q, id)
	if err != nil {
		return err
	}
	target, err := s.webhookTarget(w)
	if err != nil {
		return err
	}
	payload, err := webhook.Message{Title: "测试消息", When: time.Now(), Owner: user.Name, Minutes: testMinutes}.Payload(w.Platform)
	if err != nil {
		return err
	}
	if err := webhook.Send(ctx, target, payload); err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return ctx.Err()
		}
		return &Error{Invalid, "测试消息没发出去：" + err.Error()}
	}
	return nil
}

// webhookTarget 解开保存的地址和密钥。
func (s *Service) webhookTarget(w sqlc.Webhook) (webhook.Target, error) {
	u, err := s.settings.Open(w.Url)
	if err != nil {
		return webhook.Target{}, &Error{Conflict, "解不开保存的 Webhook 地址：ZSMEET_SECRET_KEY 可能被改过，请重新填写这个群机器人的地址"}
	}
	sec, err := s.settings.Open(w.Secret)
	if err != nil {
		return webhook.Target{}, &Error{Conflict, "解不开保存的加签密钥：ZSMEET_SECRET_KEY 可能被改过，请重新填写"}
	}
	return webhook.Target{Platform: w.Platform, URL: u, Secret: sec, TestHost: s.WebhookTestHost}, nil
}

// ---- 把纪要发到群机器人 ----

// 发到群机器人的时间限制，后台、接口和网页用同一套：
//   - 后台最晚在发送开始后 WebhookRetryUntil 再试一次。钉钉限流以后要等 11 分钟，这样被限流的还能再试两次；
//   - 发送中超过 WebhookPendingWindow 就当作后台任务丢了：接口把它报成失败、允许重新发，后台也不再发这一条。
//     多出来的几分钟留给最后一次发送（最多等 15 秒）和后台任务排队。
const (
	WebhookRetryUntil    = 25 * time.Minute
	WebhookPendingWindow = 30 * time.Minute
)

// WebhookStaleMessage 是发送中太久、当作没发出去时给人看的原因。
const WebhookStaleMessage = "等太久了，没有发出去。可以重新发一次"

// WebhookSendStale 表示 s 还记着发送中，但已经超过 WebhookPendingWindow：后台任务多半丢了，当作失败。
func WebhookSendStale(s sqlc.WebhookSend, now time.Time) bool {
	return s.Status == "pending" && now.Sub(s.CreatedAt) > WebhookPendingWindow
}

// MeetingWebhook 是会议页上「发到群里」能选的一个群机器人，和这场会议最近一次发给它的记录（没发过时为 nil）。
type MeetingWebhook struct {
	Webhook sqlc.Webhook
	Latest  *sqlc.LatestWebhookSendsRow
}

// MeetingWebhooks 列出群机器人和这场会议最近一次发到每一个的情况。能看这场会议的人都能看。
func (s *Service) MeetingWebhooks(ctx context.Context, user sqlc.User, id uuid.UUID) ([]MeetingWebhook, error) {
	if _, err := s.GetMeeting(ctx, user, id); err != nil {
		return nil, err
	}
	hooks, err := s.q.ListWebhooks(ctx)
	if err != nil {
		return nil, err
	}
	latest, err := s.q.LatestWebhookSends(ctx, id)
	if err != nil {
		return nil, err
	}
	byHook := map[uuid.UUID]*sqlc.LatestWebhookSendsRow{}
	for i := range latest {
		byHook[*latest[i].WebhookSend.WebhookID] = &latest[i]
	}
	out := make([]MeetingWebhook, len(hooks))
	for i, h := range hooks {
		out[i] = MeetingWebhook{Webhook: h, Latest: byHook[h.ID]}
	}
	return out, nil
}

// SendToWebhooks 把会议当前版本的纪要发到选的几个群机器人。真正的发送在后台做，这里记下来、加任务就返回。
// 能编辑这场会议的人可以发。version 是网页上看到的、要发的那一版：纪要已经变成别的版本时不发，免得发出去的不是看到的那一版；为 0 时不检查。
func (s *Service) SendToWebhooks(ctx context.Context, user sqlc.User, id uuid.UUID, ids []uuid.UUID, version int32) ([]sqlc.WebhookSend, error) {
	if _, err := s.editable(ctx, user, id); err != nil {
		return nil, err
	}
	// 去掉重复的，保持选的顺序
	seen := map[uuid.UUID]bool{}
	ids = slices.DeleteFunc(slices.Clone(ids), func(hid uuid.UUID) bool {
		dup := seen[hid]
		seen[hid] = true
		return dup
	})
	if len(ids) == 0 {
		return nil, invalid("请至少选一个群")
	}

	var sends []sqlc.WebhookSend
	err := s.tx(ctx, func(tx pgx.Tx, q *sqlc.Queries) error {
		// 同一个群一次只发一条，免得连点几下发出好几条。先锁住会议这一行，
		// 同时到的两个请求排队检查，后一个能看到前一个刚记下的「发送中」
		m, err := q.LockMeeting(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && m.DeletedAt != nil) {
			return errNoMeeting
		}
		if err != nil {
			return err
		}
		// 上面的检查是在锁外面做的：等锁的时候权限可能被收回了、纪要可能重新生成了，锁住以后按现在的再看一遍
		access, err := accessIn(ctx, q, user, m)
		if err != nil {
			return err
		}
		if !access.View {
			return errNoMeeting
		}
		if !access.Edit {
			return errNotAllowed
		}
		if m.CurrentVersion == 0 {
			return conflict("这场会议还没有纪要，等生成以后再发")
		}
		if version != 0 && version != m.CurrentVersion {
			return conflict("纪要刚刚更新成了第 %d 版，和你看到的第 %d 版不一样。请看一下新的纪要，再重新发送", m.CurrentVersion, version)
		}
		pending, err := q.PendingWebhookSends(ctx, sqlc.PendingWebhookSendsParams{MeetingID: id, WindowSecs: WebhookPendingWindow.Seconds()})
		if err != nil {
			return err
		}
		for _, hid := range ids {
			w, err := s.getWebhook(ctx, q, hid)
			if errors.Is(err, errNoWebhook) {
				return invalid("选的群里有已经被管理员删掉的，请关掉窗口重新选")
			}
			if err != nil {
				return err
			}
			if slices.ContainsFunc(pending, func(p *uuid.UUID) bool { return p != nil && *p == hid }) {
				return conflict("发到「%s」的上一条还在发送中，等发完再发", w.Name)
			}
			send, err := q.CreateWebhookSend(ctx, sqlc.CreateWebhookSendParams{
				MeetingID: id, WebhookID: &w.ID, WebhookName: w.Name, Platform: w.Platform,
				MinutesVersion: m.CurrentVersion, RequestedBy: &user.ID,
			})
			if err != nil {
				return err
			}
			if _, err := s.queue.InsertTx(ctx, tx, queue.WebhookSendArgs{SendID: send.ID}, nil); err != nil {
				return err
			}
			sends = append(sends, send)
		}
		return nil
	})
	return sends, err
}

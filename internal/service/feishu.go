package service

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/feishu"
	"github.com/ZaneXo/zsmeet/internal/queue"
	"github.com/ZaneXo/zsmeet/internal/settings"
)

// FeishuSettings 返回飞书配置。App Secret 不会传到接口层之外，那里只告诉网页「填没填」。
func (s *Service) FeishuSettings(ctx context.Context) (settings.Feishu, error) {
	return s.settings.Feishu(ctx)
}

// FeishuInput 是网页提交的飞书配置。AppSecret 为空表示沿用已保存的。
type FeishuInput struct {
	Domain           string
	AppID            string
	AppSecret        string
	SiteURL          string
	ChatID           string
	ChatName         string
	AutoPush         bool
	WithDoc          bool
	DocFolder        string
	LinkShare        bool
	ImportRecordings bool
}

// feishuCreds 检查凭证：App Secret 留空、App ID 没变时用已保存的。
func (s *Service) feishuCreds(ctx context.Context, in FeishuInput) (settings.Feishu, error) {
	cfg := settings.Feishu{
		Domain:    strings.TrimSpace(in.Domain),
		AppID:     strings.TrimSpace(in.AppID),
		AppSecret: strings.TrimSpace(in.AppSecret),
	}
	switch cfg.Domain {
	case "":
		cfg.Domain = "feishu"
	case "feishu", "lark":
	default:
		return settings.Feishu{}, invalid("不认识的平台：%s（只能是飞书或 Lark）", cfg.Domain)
	}
	if cfg.AppID == "" {
		return cfg, nil
	}
	if !strings.HasPrefix(cfg.AppID, "cli_") {
		return settings.Feishu{}, invalid("App ID 是开发者后台「凭证与基础信息」里以 cli_ 开头的那一串")
	}
	if cfg.AppSecret == "" {
		saved, err := s.settings.Feishu(ctx)
		if err != nil {
			return settings.Feishu{}, err
		}
		if saved.AppID == cfg.AppID {
			cfg.AppSecret = saved.AppSecret
		}
	}
	if cfg.AppSecret == "" {
		return settings.Feishu{}, invalid("请填写 App Secret")
	}
	return cfg, nil
}

// 云空间文件夹的链接：https://xxx.feishu.cn/drive/folder/<token>
var folderLink = regexp.MustCompile(`/drive/folder/([A-Za-z0-9]+)`)
var folderToken = regexp.MustCompile(`^[A-Za-z0-9]{10,64}$`)

// parseFolder 从文件夹的链接（或者直接填的 token）里取出文件夹的 token。空字符串表示放在应用自己的云空间。
func parseFolder(s string) (string, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return "", nil
	case strings.Contains(s, "/wiki/"):
		return "", invalid("这是知识库的链接。请填云空间里一个文件夹的链接，形如 https://xxx.feishu.cn/drive/folder/…")
	case folderToken.MatchString(s):
		return s, nil
	}
	if m := folderLink.FindStringSubmatch(s); m != nil {
		return m[1], nil
	}
	return "", invalid("看不懂文件夹的链接。请打开飞书云空间里那个文件夹，复制浏览器地址栏里的链接，形如 https://xxx.feishu.cn/drive/folder/…")
}

func (s *Service) resolveFeishu(ctx context.Context, in FeishuInput) (settings.Feishu, error) {
	cfg, err := s.feishuCreds(ctx, in)
	if err != nil || cfg.AppID == "" {
		// App ID 清空表示不用飞书了，其他配置一起清掉
		return settings.Feishu{Domain: cfg.Domain}, err
	}
	// 卡片和文档里的链接都用它拼，不填的话从飞书点不回 zsmeet
	cfg.SiteURL = strings.TrimRight(strings.TrimSpace(in.SiteURL), "/")
	if cfg.SiteURL == "" {
		return settings.Feishu{}, invalid("请填写 zsmeet 的访问地址：群里的卡片和飞书文档要用它链接回会议")
	}
	if u, err := url.Parse(cfg.SiteURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return settings.Feishu{}, invalid("zsmeet 的访问地址要以 http:// 或 https:// 开头，例如 https://meet.example.com")
	}
	cfg.ChatID, cfg.ChatName = strings.TrimSpace(in.ChatID), strings.TrimSpace(in.ChatName)
	cfg.AutoPush, cfg.WithDoc, cfg.LinkShare = in.AutoPush, in.WithDoc, in.LinkShare
	if cfg.AutoPush && cfg.ChatID == "" {
		return settings.Feishu{}, invalid("要在纪要生成后自动发到群里，先选一个默认的群")
	}
	if cfg.DocFolder, err = parseFolder(in.DocFolder); err != nil {
		return settings.Feishu{}, err
	}
	cfg.ImportRecordings = in.ImportRecordings
	return cfg, nil
}

// SaveFeishu 保存飞书配置。
func (s *Service) SaveFeishu(ctx context.Context, in FeishuInput) (settings.Feishu, error) {
	cfg, err := s.resolveFeishu(ctx, in)
	if err != nil {
		return settings.Feishu{}, err
	}
	return cfg, s.settings.SaveFeishu(ctx, cfg)
}

// FeishuCheck 是测试连接的结果：机器人的名字和它所在的群。
type FeishuCheck struct {
	BotName string
	Chats   []feishu.Chat
}

// TestFeishu 用网页上填的凭证（还没保存也可以）换访问凭证、读机器人信息和它所在的群。
// 凭证不对、没开机器人能力、缺权限这些都算业务错误，把原因原样告诉管理员。
func (s *Service) TestFeishu(ctx context.Context, in FeishuInput) (FeishuCheck, error) {
	cfg, err := s.feishuCreds(ctx, in)
	if err != nil {
		return FeishuCheck{}, err
	}
	if cfg.AppID == "" {
		return FeishuCheck{}, invalid("请填写 App ID 和 App Secret")
	}
	client := s.feishuClient(cfg)
	name, err := client.BotName(ctx)
	if err != nil {
		return FeishuCheck{}, &Error{Invalid, err.Error()}
	}
	chats, err := client.Chats(ctx)
	if err != nil {
		return FeishuCheck{}, &Error{Invalid, "机器人信息读到了，但列不出它所在的群：" + err.Error()}
	}
	return FeishuCheck{BotName: name, Chats: chats}, nil
}

func (s *Service) feishuClient(cfg settings.Feishu) *feishu.Client {
	return s.feishu.Get(feishu.BaseURL(cfg.Domain, s.FeishuBaseURL), cfg.AppID, cfg.AppSecret)
}

var errNoFeishu = &Error{Conflict, "管理员还没有配置飞书，请到「设置 → 飞书」里填写"}

// FeishuChats 列出机器人所在的群（用保存的配置），发纪要时选群用。还返回默认的群和是否默认导入文档。
func (s *Service) FeishuChats(ctx context.Context) ([]feishu.Chat, settings.Feishu, error) {
	cfg, err := s.settings.Feishu(ctx)
	if err != nil {
		return nil, settings.Feishu{}, err
	}
	if !cfg.Ready() {
		return nil, settings.Feishu{}, errNoFeishu
	}
	chats, err := s.feishuClient(cfg).Chats(ctx)
	if err != nil {
		return nil, settings.Feishu{}, &Error{Invalid, err.Error()}
	}
	return chats, cfg, nil
}

// ShareToFeishu 把会议当前版本的纪要发到飞书：chatID 不为空时推到这个群，withDoc 为 true 时同时导入成飞书文档。
// 真正的发送在后台做，这里记下来、加任务就返回。能编辑这场会议的人可以发。
func (s *Service) ShareToFeishu(ctx context.Context, user sqlc.User, id uuid.UUID, chatID string, withDoc bool) (sqlc.FeishuShare, error) {
	m, err := s.editable(ctx, user, id)
	if err != nil {
		return sqlc.FeishuShare{}, err
	}
	if m.CurrentVersion == 0 {
		return sqlc.FeishuShare{}, conflict("这场会议还没有纪要，等生成以后再发")
	}
	chatID = strings.TrimSpace(chatID)
	if chatID == "" && !withDoc {
		return sqlc.FeishuShare{}, invalid("请选一个群，或者勾上「导入成飞书文档」")
	}
	cfg, err := s.settings.Feishu(ctx)
	if err != nil {
		return sqlc.FeishuShare{}, err
	}
	if !cfg.Ready() {
		return sqlc.FeishuShare{}, errNoFeishu
	}
	chatName := ""
	if chatID != "" {
		chats, err := s.feishuClient(cfg).Chats(ctx)
		if err != nil {
			return sqlc.FeishuShare{}, &Error{Invalid, err.Error()}
		}
		for _, c := range chats {
			if c.ID == chatID {
				chatName = c.Name
			}
		}
		if chatName == "" {
			return sqlc.FeishuShare{}, invalid("应用的机器人不在这个群里，请先在群设置里把它加进群")
		}
	}

	var share sqlc.FeishuShare
	err = s.tx(ctx, func(tx pgx.Tx, q *sqlc.Queries) error {
		// 同一场会议一次只发一个，免得连点几下发出好几张卡片。先锁住会议这一行，
		// 同时到的两个请求排队检查，后一个能看到前一个刚记下的「发送中」
		if _, err := q.LockMeeting(ctx, id); err != nil {
			return err
		}
		if n, err := q.PendingShare(ctx, id); err != nil {
			return err
		} else if n > 0 {
			return conflict("上一次还在发送中，等发完再发")
		}
		share, err = q.CreateShare(ctx, sqlc.CreateShareParams{
			MeetingID: id, ChatID: chatID, ChatName: chatName, WithDoc: withDoc,
			MinutesVersion: m.CurrentVersion, RequestedBy: &user.ID,
		})
		if err != nil {
			return err
		}
		_, err = s.queue.InsertTx(ctx, tx, queue.FeishuShareArgs{ShareID: share.ID}, nil)
		return err
	})
	return share, err
}

// FeishuShares 列出一场会议最近发到飞书的记录，最新的在前。
func (s *Service) FeishuShares(ctx context.Context, user sqlc.User, id uuid.UUID) ([]sqlc.ListSharesRow, error) {
	if _, err := s.GetMeeting(ctx, user, id); err != nil {
		return nil, err
	}
	return s.q.ListShares(ctx, id)
}

// FeishuStatus 是设置页面上看的飞书运行情况：长连接的状态、最近一次收到的会议、最近导入的会议。
type FeishuStatus struct {
	Link    sqlc.FeishuLink
	Imports []sqlc.RecentFeishuImportsRow
}

// 后台处理（worker）每 10 秒重写一次长连接的状态，超过这么久没更新就是 worker 没在运行
const feishuLinkStale = time.Minute

func (s *Service) FeishuStatus(ctx context.Context) (FeishuStatus, error) {
	cfg, err := s.settings.Feishu(ctx)
	if err != nil {
		return FeishuStatus{}, err
	}
	link, err := s.q.GetFeishuLink(ctx)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		link = sqlc.FeishuLink{State: "off"}
	case err != nil:
		return FeishuStatus{}, err
	}
	switch {
	case !cfg.Ready() || !cfg.ImportRecordings:
		link.State, link.Detail = "off", ""
	case link.UpdatedAt.IsZero() || time.Since(link.UpdatedAt) > feishuLinkStale:
		// worker 每 10 秒报一次平安；好久没报（或者从来没报过）就是没在运行
		link.State, link.Detail = "error", "后台处理（zsmeet worker）没有在运行，收不到飞书会议结束的消息。请检查 worker 容器"
	}
	imports, err := s.q.RecentFeishuImports(ctx)
	if err != nil {
		return FeishuStatus{}, err
	}
	return FeishuStatus{Link: link, Imports: imports}, nil
}

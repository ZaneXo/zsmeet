// Package i18n 解析请求里的语言偏好（Accept-Language），并把给人看的说明、报错、模板翻译成对应的语言。
//
// 默认语言是简体中文（ZH），和已有数据、测试保持一致；请求头里优先要英文时返回英文（EN）。
package i18n

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Lang 是界面和接口支持的语言。
type Lang string

const (
	ZH Lang = "zh"
	EN Lang = "en"
)

type ctxKey struct{}

// WithLang 把语言偏好放进 context。
func WithLang(ctx context.Context, lang Lang) context.Context {
	return context.WithValue(ctx, ctxKey{}, lang)
}

// FromContext 取出请求里的语言偏好；没有时返回中文（ZH）。
func FromContext(ctx context.Context) Lang {
	if ctx == nil {
		return ZH
	}
	if lang, ok := ctx.Value(ctxKey{}).(Lang); ok && (lang == ZH || lang == EN) {
		return lang
	}
	return ZH
}

// ParseAcceptLanguage 解析 RFC 9110 Accept-Language（例如 "en-US,en;q=0.9,zh-CN;q=0.8"）。
// 没有传、全是 * 或权重相同时默认返回中文（ZH）。
func ParseAcceptLanguage(header string) Lang {
	header = strings.TrimSpace(header)
	if header == "" {
		return ZH
	}
	bestLang := Lang("")
	bestQ := -1.0
	for part := range strings.SplitSeq(header, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		tag, params, hasParams := strings.Cut(part, ";")
		tag = strings.ToLower(strings.TrimSpace(tag))
		q := 1.0
		if hasParams {
			for p := range strings.SplitSeq(params, ";") {
				k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
				if ok && strings.EqualFold(strings.TrimSpace(k), "q") {
					parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
					if err != nil || parsed < 0 || parsed > 1 {
						q = -1
					} else {
						q = parsed
					}
				}
			}
		}
		if q <= 0 {
			continue
		}
		var cand Lang
		switch {
		case tag == "zh" || strings.HasPrefix(tag, "zh-"):
			cand = ZH
		case tag == "en" || strings.HasPrefix(tag, "en-"):
			cand = EN
		default:
			continue
		}
		if q > bestQ {
			bestQ = q
			bestLang = cand
		}
	}
	if bestLang == "" {
		return ZH
	}
	return bestLang
}

type templateCopy struct {
	name  string
	guide string
}

var builtinTemplatesEN = map[string]templateCopy{
	"general": {
		name: "General Meeting",
		guide: `1. Summary: 3–5 sentences explaining what was discussed and decided
2. Decisions: Bullet points of everything agreed upon
3. Discussion Points: Organized by topic, each annotated with timestamps
4. Action Items: Who, what, and by when for each item; write "TBD" if not mentioned`,
	},
	"weekly": {
		name: "Weekly Sync",
		guide: `1. Summary: 2–3 sentences summarizing overall progress this week
2. Updates by Person: Grouped by speaker — what was done last week, planned for this week, and help needed
3. Risks & Decisions Needed
4. Action Items: Who, what, and by when for each item`,
	},
	"tech": {
		name: "Tech Review",
		guide: `1. Summary: Which design/proposal was reviewed, and whether it passed, passed with conditions, or needs revision
2. Key Design Points: Core architecture and design of the proposal
3. Questions & Conclusions: Each question raised during the review and its outcome, with timestamps
4. Open Issues & Follow-up Materials Needed
5. Action Items: Who, what, and by when for each item`,
	},
	"client": {
		name: "Customer Interview",
		guide: `1. Summary: Who the customer is, the topic of this conversation, and their overall sentiment
2. Customer Feedback: Preserve the customer's own phrasing where possible, with timestamps
3. Pain Points & Needs: Problems encountered and explicit feature/service requests
4. Competitors or Alternatives Mentioned
5. Promised Follow-ups: Who on our side, what, and by when`,
	},
}

// LocalizeTemplate 把内置纪要模板的名字和说明翻译成目标语言；管理员自己建的模板保持原样。
func LocalizeTemplate(lang Lang, id, name, guide string, custom bool) (string, string) {
	if lang != EN || custom {
		return name, guide
	}
	if t, ok := builtinTemplatesEN[id]; ok {
		return t.name, t.guide
	}
	return name, guide
}

// TranslateMessage 把服务端产生（或已存进数据库）的中文报错、警告、状态说明翻成英文。
// lang 不是 EN 时原样返回；已经是英文或没匹配上的文字也原样返回。
func TranslateMessage(lang Lang, msg string) string {
	if lang != EN || msg == "" {
		return msg
	}
	if s, ok := exactEN[msg]; ok {
		return s
	}
	if s, ok := translateWrapped(msg); ok {
		return s
	}
	for _, p := range patternsEN {
		if m := p.re.FindStringSubmatch(msg); m != nil {
			return p.fn(m)
		}
	}
	return msg
}

func translateWrapped(msg string) (string, bool) {
	if after, ok := strings.CutPrefix(msg, "在线识别没用上："); ok {
		if inner, ok2 := strings.CutSuffix(after, "；本场已改用本机识别"); ok2 {
			return "Online ASR skipped: " + TranslateMessage(EN, inner) + "; fell back to local recognition for this meeting", true
		}
		if inner, ok2 := strings.CutSuffix(after, "，本场已改用本机识别"); ok2 {
			return "Online ASR skipped: " + TranslateMessage(EN, inner) + "; fell back to local recognition for this meeting", true
		}
	}
	for _, w := range prefixWrappers {
		if inner, ok := strings.CutPrefix(msg, w.zh); ok {
			return w.en + TranslateMessage(EN, inner), true
		}
	}
	return "", false
}

var prefixWrappers = []struct {
	zh string
	en string
}{
	{"测试消息没发出去：", "Failed to send test message: "},
	{"机器人信息读到了，但列不出它所在的群：", "Bot info loaded, but could not list its group chats: "},
	{"大模型没有答出来：", "LLM failed to answer: "},
	{"一场飞书会议结束了，但读不到组织者的信息，没有导入：", "A Feishu meeting ended, but the organizer's info could not be read (not imported): "},
	{"下载飞书会议的录制失败：", "Failed to download Feishu meeting recording: "},
	{"飞书拒绝了长连接（App ID、App Secret 不对，或者应用没有启用）：", "Feishu rejected the long connection (invalid App ID/App Secret, or app not enabled): "},
	{"连不上飞书：", "Cannot reach Feishu: "},
	{"调用大模型失败：", "LLM request failed: "},
	{"连不上大模型接口，检查接口地址和网络：", "Cannot reach the LLM endpoint; check the base URL and network: "},
	{"大模型的回答读不出来：", "Could not read the LLM response: "},
	{"大模型的回答中途断了：", "LLM response stream was interrupted: "},
	{"返回的内容看不懂：", "Unrecognized response payload: "},
	{"应用没有权限往这个文件夹里放文档：请把文件夹共享给一个有应用机器人的群，并给编辑权限。", "The app does not have permission to create documents in this folder: share the folder with a group containing the app bot and grant edit access. "},
	{"文档要放的文件夹被删了或者不存在。", "The target folder was deleted or does not exist. "},
	{"飞书云空间的容量满了。", "Feishu Drive storage is full. "},
	{"文件夹里的文件太多了。", "Too many files in the target folder. "},
}

func translateDurationWord(s string) string {
	s = strings.TrimSpace(s)
	if v, ok := strings.CutSuffix(s, " 秒"); ok {
		return v + "s"
	}
	if v, ok := strings.CutSuffix(s, " 分钟"); ok {
		return v + "m"
	}
	if v, ok := strings.CutSuffix(s, " 小时"); ok {
		return v + "h"
	}
	return s
}

func translatePlatformLabel(s string) string {
	switch s {
	case "钉钉":
		return "DingTalk"
	case "企业微信":
		return "WeCom"
	case "飞书":
		return "Feishu"
	default:
		return s
	}
}

func translateASRModelWhat(s string) string {
	switch s {
	case "区分说话人的模型":
		return "Speaker diarization model"
	case "识别模型":
		return "Speech recognition model"
	case "语音检测模型":
		return "Voice activity detection model"
	case "声纹模型":
		return "Voiceprint model"
	default:
		return s
	}
}

func translateLLMHint(s string) string {
	switch s {
	case "（检查 API Key，以及这个 Key 能不能用这个模型）":
		return " (check your API key and whether it has access to this model)"
	case "（检查接口地址和模型名；接口地址一般以 /v1 结尾）":
		return " (check the base URL and model name; base URLs usually end with /v1)"
	case "（请求太频繁或额度用完了）":
		return " (rate limited or quota exceeded)"
	default:
		return s
	}
}

func translateWebhookExample(s string) string {
	switch s {
	case "钉钉的 Webhook 地址形如 https://oapi.dingtalk.com/robot/send?access_token=…，在钉钉群的「设置 → 机器人」里添加自定义机器人后复制":
		return "DingTalk Webhook URLs look like https://oapi.dingtalk.com/robot/send?access_token=… — copy it after adding a custom bot in DingTalk group Settings -> Bots."
	case "企业微信的 Webhook 地址形如 https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=…，在企业微信群里添加机器人后，在机器人的资料页复制":
		return "WeCom Webhook URLs look like https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=… — copy it from the bot profile after adding a bot to the WeCom group."
	case "Slack 的 Webhook 地址形如 https://hooks.slack.com/services/T…/B…/…，在 Slack 应用的「Incoming Webhooks」页面复制":
		return "Slack Webhook URLs look like https://hooks.slack.com/services/T…/B…/… — copy it from Incoming Webhooks in your Slack app."
	default:
		return s
	}
}

func translateFeishuHint(s string) string {
	if en, ok := feishuHintsEN[s]; ok {
		return en
	}
	return TranslateMessage(EN, s)
}

var feishuHintsEN = map[string]string{
	"应用缺少需要的权限，请到飞书开发者后台「权限管理」开通，并发布新版本":          "The app is missing required permissions; enable them under Permissions & Scopes in the Feishu Developer Console and publish a new version",
	"飞书的访问凭证失效了，请稍后再试":                            "Feishu access token expired; please try again shortly",
	"请求太频繁，被飞书限流了，稍后会自动重试":                        "Rate limited by Feishu; will retry automatically",
	"应用的机器人不在这个群里，请先在群设置里把它加进群":                   "The app's bot is not in this group chat; add it in the group settings first",
	"应用还没有开启机器人能力，请在飞书开发者后台「添加应用能力」里添加机器人，并发布新版本": "Bot capability is not enabled for this app; add Bot under Add Features in the Feishu Developer Console and publish a new version",
	"群设置不允许机器人发言":                                 "Group chat settings do not allow bots to post messages",
	"机器人在这个群里不能发言（被禁言，或者公司限制了）":                   "The bot cannot post in this group (muted or restricted by organization policy)",
	"没有权限往这个群发消息（外部群需要应用开通对外共享能力）":                "No permission to post to this group (external groups require external sharing capability enabled on the app)",
	"这个群已经解散了":                       "This group chat has been disbanded",
	"往这个群发消息太频繁了，稍后会自动重试":            "Posting too frequently to this group; will retry automatically",
	"内容被飞书判定为敏感内容，没有发出去":             "Message blocked by Feishu sensitive-content filter",
	"内容里有手机号、邮箱之类的信息，被公司的数据防泄漏策略拦下了": "Message blocked by organization DLP policy (contains phone numbers, emails, or similar sensitive info)",
	"卡片的格式飞书不认，没有发出去":                "Card payload rejected by Feishu",
	"文档要放的文件夹不存在，或者应用没有权限往里放：请把这个文件夹共享给一个有应用机器人的群，并给编辑权限": "Target folder does not exist or the app lacks permission: share the folder with a group containing the app bot and grant edit permission",
	"文档要放的文件夹不存在":                                     "Target folder does not exist",
	"应用没有权限修改这个文档的分享设置":                               "The app does not have permission to update sharing settings on this document",
	"设置文档权限被拦下了（公司的安全策略不允许，或者对方看不到这个应用）":              "Updating document permissions was blocked by organization security policy or app visibility",
	"应用没有读取妙记的权限：请在飞书开发者后台「权限管理」里把妙记的数据范围设为全员，并发布新版本": "The app lacks Feishu Minutes permission: set Minutes data scope to All Employees in the Feishu Developer Console and publish a new version",
	"应用读不到这个人的通讯录信息：请在飞书开发者后台把通讯录的数据范围设为全员":           "The app cannot read this user's directory profile: set Contacts data scope to All Employees in the Feishu Developer Console",
}

var exactEN = map[string]string{
	// internal/api
	"服务器内部出错了，请稍后再试":            "Internal server error, please try again later",
	"会话读写失败，请稍后再试":              "Session error, please try again later",
	"这个接口只能在网页上用，请求里不要带 App 令牌": "This endpoint is for the web app only; do not send an app token",
	"这个操作只能在网页上登录后做，App 里做不了":   "This action must be performed while signed in on the web app, not from the desktop app",
	"这个接口只给 App 用，请求里要带 App 令牌": "This endpoint requires a desktop app token",
	"只有管理员能做这个操作":               "Only administrators can perform this action",
	"请先登录":                      "Please sign in first",
	"需要登录":                      "Authentication required",
	"会议不存在，可能已被删除":              "Meeting not found or has been deleted",
	"会议不存在":                     "Meeting not found",
	"没有这个接口":                    "Endpoint not found",
	"选的人不对，请刷新页面重新选":            "Invalid member selection; please refresh the page and select again",
	"没有这台设备，可能已经退出登录了":          "Device not found or already signed out",
	"没有这个自定义模板":                 "Custom template not found",
	"没有这个成员或邀请":                 "Member or invitation not found",
	"没有这个声纹，可能已经删掉了":            "Voiceprint not found or already deleted",
	"群机器人不存在，可能已被删除":            "Group bot not found or has been deleted",
	"选的群里有已经被管理员删掉的，请关掉窗口重新选":   "One or more selected bots were deleted by an admin; please close the dialog and select again",

	// internal/service/access.go
	"你不能修改这场会议：只有创建者、管理员和被指定能编辑的人能改": "You don't have permission to edit this meeting: only the creator, admins, and designated editors can edit",
	"只有这场会议的创建者和管理员能做这件事":            "Only the meeting creator and admins can perform this action",
	"选的人里有不在团队里、或者已经停用了的，请刷新页面重新选":   "Some selected users are not in the team or have been deactivated; please refresh and select again",

	// internal/service/apps.go
	"登录没有完成：授权码无效或者已经过期。请回到 zsmeet 桌面版，重新点「在浏览器里登录」":                             "Sign-in incomplete: authorization code is invalid or expired. Return to zsmeet Desktop and click \"Sign in via browser\" again",
	"这台设备的登录已失效，请在 App 里重新登录":                                                    "This device's session has expired; please sign in again in the app",
	"登录已失效（密码改过了，或者账号被停用了）。请重新登录后，回到 zsmeet 桌面版重新点「在浏览器里登录」":                     "Session expired (password changed or account deactivated). Sign in again, then click \"Sign in via browser\" in zsmeet Desktop",
	"登录链接不完整（code_challenge 不对）。请回到 zsmeet 桌面版，重新点「在浏览器里登录」":                     "Incomplete sign-in link (invalid code_challenge). Return to zsmeet Desktop and click \"Sign in via browser\" again",
	"设备名里有不能显示的字符。请在电脑的「系统设置 → 通用 → 关于本机」里把名称改成普通的文字，再回到 zsmeet 桌面版重新点「在浏览器里登录」": "Device name contains invalid characters. Rename your computer in System Settings -> General -> About, then sign in again from zsmeet Desktop",

	// internal/service/ask.go
	"在你能看到的会议里没有找到和这个问题相关的内容。可以换个说法，或者用会上可能说过的词（人名、产品名、项目名）再问一次。": "Nothing related to your question was found in the meetings you have access to. Try rephrasing or using specific terms mentioned in the meeting (names, products, or projects).",
	"请输入要问的问题": "Please enter a question",
	"还没有配置大模型，请管理员先到「设置」里填写":                       "No LLM configured yet; ask an admin to configure one in Settings",
	"大模型太久没有回答，请稍后再试，或者把问题问得具体一些":                  "The LLM took too long to respond; please try again later or make your question more specific",
	"大模型暂时没有答出来，请稍后再试。一直不行的话，请管理员到「设置」里测试一下大模型的连接": "The LLM could not answer right now; please try again later. If this persists, ask an admin to test the LLM connection in Settings",
	"这场会议还没有转写和纪要，处理完以后再问":                         "This meeting hasn't finished transcribing yet; please ask after processing completes",

	// internal/service/auth.go & members.go
	"系统已经初始化过了，请直接登录": "System is already initialized; please sign in",
	"姓名不能为空，最长 50 个字": "Name is required and must be at most 50 characters",
	"邮箱或密码不对":         "Incorrect email or password",
	"登录已失效，请重新登录":     "Session expired; please sign in again",
	"邮箱格式不对":          "Invalid email address",
	"角色只能是管理员或成员":     "Role must be admin or member",
	"这个邮箱的账号已被停用。在成员列表里重新启用就行，不需要再邀请": "An account with this email is deactivated. Re-enable it in the member list instead of sending a new invite",
	"这个邮箱已经是成员了":           "This email is already a team member",
	"这个邀请不存在，可能已经被接受或者撤销了": "Invitation not found; it may have already been accepted or revoked",
	"没有这个成员":               "Member not found",
	"这个账号已被停用，请联系管理员":      "This account has been deactivated; please contact an administrator",
	"不能修改自己的角色，也不能停用自己。需要的话请让另一位管理员操作":      "You cannot change your own role or deactivate yourself; ask another admin if needed",
	"你已经不是管理员了，刷新页面看看":                      "You are no longer an administrator; please refresh the page",
	"要改自己的密码，请到「设置 → 账号」里修改":                "To change your own password, go to Settings -> Account",
	"这个账号已被停用，先启用再重置密码":                     "This account is deactivated; re-enable it before resetting the password",
	"这个成员没有还没用过的重置密码链接，可能已经用掉或者撤销了":         "No active password reset link for this member; it may have been used or revoked",
	"链接无效。请确认链接复制完整了；如果管理员重新生成过链接，旧的就不能用了":  "Invalid link. Make sure you copied the full URL; generating a new link invalidates previous ones",
	"这个链接已经用过了。如果是你自己用的，直接登录就行":             "This link has already been used. If you used it yourself, please sign in directly",
	"这个链接已经过期了，请让管理员重新发一个":                  "This link has expired; please ask an admin to send a new one",
	"发出这个链接的人已经不是管理员了，链接随之失效。请让现在的管理员重新发一个": "The admin who created this link is no longer an admin, so the link is invalid. Ask a current admin for a new one",
	"这个邮箱已经注册过了，请直接登录":                      "This email is already registered; please sign in",
	"当前密码不对":    "Current password is incorrect",
	"没有这个邮箱的账号": "No account found with this email",

	// internal/service/export.go
	"这场会议还没有纪要，等生成以后再导出":  "Minutes haven't been generated yet; please wait before exporting",
	"这场会议还没有转写，等识别完以后再导出": "Transcript isn't ready yet; please wait for recognition to finish before exporting",

	// internal/service/feishu.go & feishu_login.go
	"App ID 是开发者后台「凭证与基础信息」里以 cli_ 开头的那一串": "App ID must start with cli_ (found under Credentials & Basic Info in the Developer Console)",
	"请填写 App Secret": "Please enter the App Secret",
	"这是知识库的链接。请填云空间里一个文件夹的链接，形如 https://xxx.feishu.cn/drive/folder/…":               "That is a Wiki link. Please enter a Feishu Drive folder URL like https://xxx.feishu.cn/drive/folder/…",
	"看不懂文件夹的链接。请打开飞书云空间里那个文件夹，复制浏览器地址栏里的链接，形如 https://xxx.feishu.cn/drive/folder/…": "Invalid folder URL. Open the folder in Feishu Drive and copy the URL from your browser's address bar (e.g. https://xxx.feishu.cn/drive/folder/…)",
	"请填写 zsmeet 的访问地址：群里的卡片和飞书文档要用它链接回会议":                                           "Please enter zsmeet's site URL so cards and Feishu docs can link back to meetings",
	"要在纪要生成后自动发到群里，先选一个默认的群":                                                        "Select a default group chat before enabling automatic sharing",
	"请填写 App ID 和 App Secret":                             "Please enter both App ID and App Secret",
	"管理员还没有配置飞书，请到「设置 → 飞书」里填写":                           "Feishu is not configured yet; ask an admin to configure it in Settings -> Feishu",
	"这场会议还没有纪要，等生成以后再发":                                   "Minutes haven't been generated yet; please wait before sending",
	"请选一个群，或者勾上「导入成飞书文档」":                                 "Please select a group chat or check \"Import as Feishu Doc\"",
	"应用的机器人不在这个群里，请先在群设置里把它加进群":                           "The app's bot is not in this group chat; add it in the group settings first",
	"上一次还在发送中，等发完再发":                                      "Previous send is still in progress; please wait for it to finish",
	"后台处理（zsmeet worker）没有在运行，收不到飞书会议结束的消息。请检查 worker 容器": "Background worker (zsmeet worker) is not running, so Feishu meeting-ended events cannot be received. Please check the worker container",
	"密码不对。用飞书自动创建的账号没有密码：请管理员在「设置 → 成员」里给你生成重置密码的链接，设好密码再关联": "Incorrect password. Accounts auto-created via Feishu do not have a password yet: ask an admin to generate a password reset link in Settings -> Members first",
	"这个账号没有关联飞书账号，可能已经解除了": "No Feishu account is linked to this account, or it was already unlinked",

	// internal/service/meetings.go & templates.go & trash.go
	"热词太多了，最长 500 个字": "Hotwords are too long (at most 500 characters)",
	"新建会议时可见范围只能是团队所有人或者只有自己，指定具体的人请建好以后在会议页面上设置": "Initial visibility must be \"Team\" or \"Only me\"; specific members can be configured on the meeting page after creation",
	"标题不能为空":   "Title cannot be empty",
	"分片序号超出范围": "Chunk sequence number out of range",
	"这场会议已经结束录音、开始处理了，剩下的录音传不上去了": "Recording has already ended and processing has started; remaining audio chunks cannot be uploaded",
	"分片是空的":  "Audio chunk is empty",
	"分片数量不对": "Chunk count mismatch",
	"服务器又收到了一些录音，请刷新页面再试":               "New audio chunks just arrived on the server; please refresh the page and try again",
	"这场会议已经在处理了":                        "This meeting is already being processed",
	"这场会议已经结束录音、开始处理了，不能再改标记":           "Recording has already ended and processing has started; bookmarks can no longer be modified",
	"这场会议的录音已经过了保留期被删除，又还没有转写出来，没法重新处理": "Audio has expired and been deleted before transcription completed, so this meeting cannot be reprocessed",
	"只有处理失败的会议才能重试":                     "Only failed meetings can be retried",
	"这场会议的录音已经不在了（可能过了保留期），没法重新转写":      "Audio file is no longer available (it may have passed the retention period), so it cannot be re-transcribed",
	"会议正在处理中，等处理完再重新转写":                 "Meeting is currently being processed; wait for it to finish before re-transcribing",
	"这场会议还没有转写，不能生成纪要":                  "This meeting has no transcript yet, so minutes cannot be generated",
	"会议正在处理中，等处理完再重新生成":                 "Meeting is currently being processed; wait for it to finish before regenerating minutes",
	"没有这个版本的纪要":                         "Minutes version not found",
	"纪要不能为空":                            "Minutes content cannot be empty",
	"纪要太长了":                             "Minutes content is too long",
	"会议正在处理中，等处理完再编辑纪要":                 "Meeting is currently being processed; wait for it to finish before editing minutes",
	"这场会议没有录音，或者录音已过保留期":                "No audio recording available, or it has passed the retention period",
	"没有这个纪要模板，可能已经被管理员删掉了":              "Minutes template not found or was deleted by an admin",
	"给模板起个名字":                           "Please enter a template name",
	"写一下纪要要有哪几部分、每部分写什么":                "Please describe what sections the minutes should include",
	"没有这个自定义模板，可能已经被删掉了（内置的模板不能改）":      "Custom template not found or already deleted (built-in templates cannot be modified)",
	"回收站里没有这场会议，可能已经被恢复或者彻底删除了":         "Meeting not found in Trash; it may have already been restored or permanently deleted",

	// internal/service/settings.go & webhooks.go
	"模型名不能为空": "Model name cannot be empty",
	"上下文长度只能从列表里选（8K 到 200K），不知道的话选 32K":                               "Context length must be one of the listed options (8K to 200K); choose 32K if unsure",
	"接口地址要以 http:// 或 https:// 开头，例如 https://api.openai.com/v1":        "Base URL must start with http:// or https://, e.g. https://api.openai.com/v1",
	"识别方式只能选「本机识别」或「DeepSeek 在线识别」":                                    "ASR provider must be \"Local\" or \"DeepSeek Online\"",
	"打开 DeepSeek 在线识别需要填写 API Key":                                     "An API key is required to enable DeepSeek Online ASR",
	"请先填写 DeepSeek 的 API Key":                                          "Please enter a DeepSeek API key first",
	"zsmeet 的访问地址要以 http:// 或 https:// 开头，例如 https://meet.example.com": "zsmeet site URL must start with http:// or https://, e.g. https://meet.example.com",
	"请给群机器人起个名字，比如它所在的群的名字":                                            "Please enter a name for the bot (e.g. the group chat name)",
	"名字最多 50 个字":            "Name must be at most 50 characters",
	"请填写 Webhook 地址":        "Please enter the Webhook URL",
	"换了平台，请重新填写 Webhook 地址": "Platform changed; please re-enter the Webhook URL",
	"解不开保存的 Webhook 地址：ZSMEET_SECRET_KEY 可能被改过，请重新填写这个群机器人的地址": "Cannot decrypt saved Webhook URL: ZSMEET_SECRET_KEY may have changed; please re-enter this bot's URL",
	"解不开保存的加签密钥：ZSMEET_SECRET_KEY 可能被改过，请重新填写":                 "Cannot decrypt saved signing secret: ZSMEET_SECRET_KEY may have changed; please re-enter it",
	"等太久了，没有发出去。可以重新发一次":                                       "Timed out before sending. You can try sending again",
	"请至少选一个群": "Please select at least one group",

	// internal/secret
	"解不开已保存的凭证：ZSMEET_SECRET_KEY 可能被改过，请在设置里重新填写": "Cannot decrypt saved credentials: ZSMEET_SECRET_KEY may have changed; please re-enter them in Settings",
	"内容被改过，或者不是这个用途的":                             "Content was modified or is invalid for this purpose",

	// internal/pipeline
	"处理超时": "Processing timed out",
	"处理程序多次异常退出，已停止自动重试。常见原因是服务器内存不足，可以点「重试」再试一次": "Worker exited unexpectedly multiple times and stopped retrying automatically (often caused by insufficient server memory). Click \"Retry\" to try again",
	"录音太短，没有可以识别的内容": "Recording is too short to transcribe",
	"在线识别没用上：解不开已保存的 API Key（ZSMEET_SECRET_KEY 可能被改过），本场已改用本机识别": "Online ASR skipped: saved API key could not be decrypted (ZSMEET_SECRET_KEY may have changed); fell back to local recognition for this meeting",
	"在线识别没用上：尚未配置 DeepSeek API Key，本场已改用本机识别":                    "Online ASR skipped: DeepSeek API key is not configured; fell back to local recognition for this meeting",
	"没有识别到说话内容，请确认录音里有人声":                                        "No speech detected; please check that the recording contains audible voices",
	"请求全部失败": "all requests failed",
	"在线识别没用上：识别结果与本机时间轴对不上，本场已改用本机识别":         "Online ASR skipped: cloud transcript could not be aligned with local timestamps; fell back to local recognition for this meeting",
	"找不到上传的录音，请重新上传":                          "Uploaded audio file not found; please upload again",
	"录音不完整：第 1 片读不出来，请重新上传":                   "Incomplete recording: chunk 1 could not be read; please upload again",
	"连接断了，正在重连":                               "Connection lost, reconnecting…",
	"和飞书的长连接断开了":                              "Disconnected from Feishu long connection",
	"一场飞书会议结束了，组织者不是 zsmeet 的成员（按邮箱对不上），没有导入": "A Feishu meeting ended, but the organizer is not a zsmeet member (no email match), so it was skipped",
	"等飞书生成录制":                                 "Waiting for Feishu to generate cloud recording",
	"这场会议没有云录制":                               "No cloud recording for this meeting",
	"等了 12 小时，飞书还没生成好录制":                      "Timed out after waiting 12 hours for Feishu to generate the recording",
	"服务器磁盘空间不够，录制没下载完":                        "Insufficient server disk space to finish downloading the recording",
	"从飞书下载到的录制是空的":                            "Downloaded Feishu recording file was empty",
	"一场飞书会议的录制导入以后被彻底删除了":                     "An imported Feishu meeting recording was permanently deleted",
	"管理员把飞书的配置清掉了，没法发送":                       "Feishu configuration was cleared by an admin; cannot send",
	"飞书已经开始导入文档，但服务器没能记下来。为了不导入出两篇一样的文档，不再自动重试，可以过一会儿重新发一次": "Feishu started importing the document, but the server could not record the task. Automatic retry was skipped to avoid duplicate documents; please try again later",
	"飞书导入文档太久了，没有等到结果。可以过一会儿再发一次":                           "Feishu document import took too long to finish. Please try sending again later",
	"会议已经删除了": "Meeting was deleted",
	"会议已经改成不是团队所有人可见，不再自动发到群里":                                    "Meeting visibility is no longer set to Team; automatic group sharing was skipped",
	"发起人的账号已经不在了，不再发送":                                            "Sender's account no longer exists; cancelled",
	"发起人的账号已经停用了，不再发送":                                            "Sender's account has been deactivated; cancelled",
	"发起人已经不能编辑这场会议了，不再发送":                                         "Sender no longer has edit access to this meeting; cancelled",
	"服务器内部出错了，没发出去":                                               "Internal server error; message was not sent",
	"这个群机器人已经被管理员删掉了":                                             "This group bot was deleted by an admin",
	"管理员关掉了这个群的自动发送":                                              "Auto-push for this group bot was turned off by an admin",
	"解不开保存的 Webhook 地址：ZSMEET_SECRET_KEY 可能被改过，请管理员重新填写这个群机器人的地址": "Cannot decrypt saved Webhook URL: ZSMEET_SECRET_KEY may have changed; ask an admin to re-enter this bot's URL",
	"解不开保存的加签密钥：ZSMEET_SECRET_KEY 可能被改过，请管理员重新填写":                 "Cannot decrypt saved signing secret: ZSMEET_SECRET_KEY may have changed; ask an admin to re-enter it",

	// internal/llm
	"还没有配置大模型":            "LLM is not configured yet",
	"大模型返回了空内容":           "The LLM returned an empty response",
	"大模型没有返回内容":           "The LLM returned no choices",
	"接口没有列出任何模型，请直接填写模型名": "The endpoint returned an empty model list; please enter the model name manually",
	"大模型超时没有响应":           "The LLM timed out without responding",

	// internal/feishu
	"返回的内容看不懂":           "Unrecognized response payload",
	"没有返回用户的 ID":         "Feishu did not return a user ID",
	"看不懂飞书给的下载地址":        "Invalid download URL returned by Feishu",
	"飞书给的下载地址不是 https 的": "Feishu download URL does not use HTTPS",
	"跳转太多次":              "Too many redirects",
	"不允许跳转到 http 地址":     "Redirect to unencrypted HTTP is not allowed",
	"飞书应用的 App ID 或 App Secret 不对，请管理员到「设置 → 飞书」里检查":            "Invalid Feishu App ID or App Secret; ask an admin to check Settings -> Feishu",
	"这次飞书登录已经用过或者失效了，请重新点「用飞书登录」":                               "This Feishu sign-in session has expired or already been used; click \"Sign in with Feishu\" again",
	"这次飞书登录已经超时，请重新点「用飞书登录」":                                    "This Feishu sign-in session timed out; click \"Sign in with Feishu\" again",
	"你不在这个飞书应用的可用范围里，请管理员在飞书管理后台把你加进应用的可用范围":                    "You are outside this Feishu app's availability scope; ask an admin to add you in the Feishu Admin Console",
	"这次飞书登录校验没有通过，请重新点「用飞书登录」":                                  "Feishu sign-in verification failed; click \"Sign in with Feishu\" again",
	"飞书应用里填的重定向 URL 和 zsmeet 的访问地址对不上，请管理员按「设置 → 飞书」里显示的地址重新填写": "Feishu redirect URI mismatch; ask an admin to configure the exact redirect URL shown in Settings -> Feishu",

	// internal/webhook
	"加签的密钥不对：是钉钉机器人安全设置里「加签」下面以 SEC 开头的那一串": "Invalid signing secret: enter the string starting with SEC under Security Settings -> Additional Signature in DingTalk",
	"Webhook 地址不对": "Invalid Webhook URL",
	"钉钉没有收下这条消息：机器人设置了自定义关键词，消息里没有。请在钉钉机器人的安全设置里加一个关键词「会议纪要」（zsmeet 发的消息都带着这四个字），或者改用加签": "DingTalk rejected the message: custom keyword did not match. Add the keyword \"会议纪要\" in the DingTalk bot's security settings, or switch to signature verification",
	"钉钉没有收下这条消息：加签没通过。请确认填的加签密钥和钉钉机器人安全设置里的一样，并且服务器的时间是准的":                               "DingTalk rejected the message: signature verification failed. Check that the SEC signing secret matches DingTalk and that your server clock is accurate",
	"钉钉没有收下这条消息：zsmeet 服务器的 IP 不在机器人设置的 IP 地址（段）里。请把服务器的出口 IP 加进去，或者改用加签":                "DingTalk rejected the message: server egress IP is not in the bot's IP allowlist. Add your server's IP or switch to signature verification",
	"钉钉找不到这个机器人：Webhook 地址不对，或者机器人已经从群里移除了。请重新复制机器人的 Webhook 地址":                         "DingTalk bot not found: the Webhook URL is invalid or the bot was removed from the group. Copy the Webhook URL again",
	"发得太快了：钉钉限制每个机器人每分钟最多发 20 条，超过以后要等 10 分钟才能再发":                                        "Rate limited: DingTalk allows at most 20 messages per minute per bot and pauses for 10 minutes once exceeded",
	"钉钉系统繁忙": "DingTalk service is busy",
	"企业微信找不到这个机器人：Webhook 地址不对，或者机器人已经从群里移除了。请重新复制机器人的 Webhook 地址": "WeCom bot not found: the Webhook URL is invalid or the bot was removed from the group. Copy the Webhook URL again",
	"发得太快了：企业微信限制每个机器人每分钟最多发 20 条":                                 "Rate limited: WeCom allows at most 20 messages per minute per bot",
	"企业微信系统繁忙":            "WeCom service is busy",
	"发得太快了，Slack 限制了发送频率": "Rate limited by Slack",
	"Slack 找不到这个 Webhook：地址不对，或者已经被停用、删除了。请在 Slack 里重新生成一个 Webhook 地址": "Slack Webhook not found: the URL is invalid or has been disabled/deleted. Generate a new Webhook URL in Slack",
	"Slack 的频道已经归档或者不存在了。请换一个频道，重新生成 Webhook 地址":                       "The Slack channel is archived or no longer exists. Choose another channel and generate a new Webhook URL",
	"Slack 的管理员限制了往这个频道发消息。请联系 Slack 管理员，或者换一个频道":                      "Posting to this Slack channel is restricted by a workspace admin. Contact your Slack admin or choose another channel",

	// internal/cloudasr
	"在线识别的接口地址格式不对，请在「设置 → 语音识别」里检查":                     "Invalid Online ASR endpoint URL; please check Settings -> Speech Recognition",
	"连不上 DeepSeek 在线识别接口，请检查服务器网络和接口地址":                  "Cannot reach DeepSeek Online ASR endpoint; please check server network and base URL",
	"读取 DeepSeek 在线识别响应中断，请稍后再试":                         "Connection interrupted while reading DeepSeek Online ASR response; please try again later",
	"在线识别接口返回的不是预期的 JSON，请检查接口地址是否正确":                    "Online ASR endpoint returned unexpected non-JSON content; please check the base URL",
	"DeepSeek 的 API Key 不对或已失效，请在「设置 → 语音识别」里检查":         "DeepSeek API key is invalid or expired; please check Settings -> Speech Recognition",
	"DeepSeek 账户余额不足，请充值后再试":                             "Insufficient DeepSeek account balance; please top up and try again",
	"DeepSeek 拒绝了请求（API Key 没有权限或账户受限），请在「设置 → 语音识别」里检查": "DeepSeek rejected the request (API key lacks permission or account is restricted); please check Settings -> Speech Recognition",
	"DeepSeek 在线识别请求过于频繁（HTTP 429），请稍后再试":                "DeepSeek Online ASR rate limit exceeded (HTTP 429); please try again later",
	"发送给在线识别的音频片段超过大小限制（HTTP 413）":                       "Audio segment sent to Online ASR exceeded size limit (HTTP 413)",

	// internal/audio & internal/models
	"音频无法解码：文件可能已损坏，或者不是音视频文件": "Cannot decode audio: the file may be corrupted or is not a valid audio/video file",
	"录音超过 6 小时，暂时处理不了，请拆分后再上传": "Recordings longer than 6 hours are not supported; please split the file before uploading",
	"服务器上没有安装 ffmpeg":          "ffmpeg is not installed on the server",
	"下载卡住了":                    "Download stalled",
}

type patternRule struct {
	re *regexp.Regexp
	fn func(m []string) string
}

var patternsEN []patternRule

func init() {
	patternsEN = []patternRule{
		// Retry wrappers in pipeline
		{
			re: regexp.MustCompile(`^第 (\d+) 次没发出去，(.+?)后自动重试：(.+)$`),
			fn: func(m []string) string {
				return fmt.Sprintf("Attempt %s failed, retrying in %s: %s", m[1], translateDurationWord(m[2]), TranslateMessage(EN, m[3]))
			},
		},
		{
			re: regexp.MustCompile(`^被限流了，(.+?)后自动重试：(.+)$`),
			fn: func(m []string) string {
				return fmt.Sprintf("Rate limited, retrying in %s: %s", translateDurationWord(m[1]), TranslateMessage(EN, m[2]))
			},
		},
		{
			re: regexp.MustCompile(`^文档导入好了，但没法给你打开它的权限（(.+)）。请发到一个群，或者请管理员在「设置 → 飞书」里打开「公司里拿到链接的人都能看」$`),
			fn: func(m []string) string {
				return fmt.Sprintf("Document imported, but could not grant you view access (%s). Share to a group chat instead, or ask an admin to enable \"Anyone in organization with link can view\" in Settings -> Feishu", TranslateMessage(EN, m[1]))
			},
		},

		// internal/api/meetings.go
		{
			re: regexp.MustCompile(`^说话人编号要是数字：(.+)$`),
			fn: func(m []string) string { return "Speaker index must be an integer: " + m[1] },
		},

		// internal/service dynamic errors
		{
			re: regexp.MustCompile(`^不认识的可见范围：(.+)$`),
			fn: func(m []string) string { return "Unknown visibility: " + m[1] },
		},
		{
			re: regexp.MustCompile(`^一场会议最多单独指定 (\d+) 个人$`),
			fn: func(m []string) string {
				return fmt.Sprintf("At most %s specific members can be selected per meeting", m[1])
			},
		},
		{
			re: regexp.MustCompile(`^不认识的 App 类型：(.+)。请确认用的是 zsmeet 桌面版$`),
			fn: func(m []string) string {
				return fmt.Sprintf("Unrecognized app client \"%s\". Make sure you are using zsmeet Desktop", m[1])
			},
		},
		{
			re: regexp.MustCompile(`^设备名不能为空，最长 (\d+) 个字$`),
			fn: func(m []string) string {
				return fmt.Sprintf("Device name is required and must be at most %s characters", m[1])
			},
		},
		{
			re: regexp.MustCompile(`^问题太长了，最多 (\d+) 个字$`),
			fn: func(m []string) string { return fmt.Sprintf("Question is too long (at most %s characters)", m[1]) },
		},
		{
			re: regexp.MustCompile(`^提问太频繁了，每分钟最多问 (\d+) 次，请 (\d+) 秒后再试$`),
			fn: func(m []string) string {
				return fmt.Sprintf("Too many questions (limit %s per minute); please try again in %ss", m[1], m[2])
			},
		},
		{
			re: regexp.MustCompile(`^密码输错的次数太多，为了账号安全暂时不能登录，请 (\d+) 分钟后再试$`),
			fn: func(m []string) string {
				return fmt.Sprintf("Too many failed sign-in attempts; account temporarily locked, please try again in %s minutes", m[1])
			},
		},
		{
			re: regexp.MustCompile(`^不认识的导出内容：(.+)$`),
			fn: func(m []string) string { return "Unknown export content type: " + m[1] },
		},
		{
			re: regexp.MustCompile(`^不认识的导出格式：(.+)$`),
			fn: func(m []string) string { return "Unknown export format: " + m[1] },
		},
		{
			re: regexp.MustCompile(`^不认识的平台：(.+)（只能是飞书或 Lark）$`),
			fn: func(m []string) string { return fmt.Sprintf("Unknown platform \"%s\" (must be feishu or lark)", m[1]) },
		},
		{
			re: regexp.MustCompile(`^不认识的平台：(.+)（只能是钉钉、企业微信或 Slack）$`),
			fn: func(m []string) string {
				return fmt.Sprintf("Unknown platform \"%s\" (must be dingtalk, wecom, or slack)", m[1])
			},
		},
		{
			re: regexp.MustCompile(`^不认识的平台：(.+)$`),
			fn: func(m []string) string { return "Unknown platform: " + m[1] },
		},
		{
			re: regexp.MustCompile(`^纪要刚刚更新成了第 (\d+) 版，和你看到的第 (\d+) 版不一样。请看一下新的纪要，再重新发送$`),
			fn: func(m []string) string {
				return fmt.Sprintf("Minutes were just updated to v%s (you were viewing v%s). Please review the latest version before sending", m[1], m[2])
			},
		},
		{
			re: regexp.MustCompile(`^说话人数要在 1 到 (\d+) 之间；不确定有几个人时选「自动区分」$`),
			fn: func(m []string) string {
				return fmt.Sprintf("Speaker count must be between 1 and %s, or choose \"Auto-detect\" if unsure", m[1])
			},
		},
		{
			re: regexp.MustCompile(`^不认识的会议来源：(.+)$`),
			fn: func(m []string) string { return "Unknown meeting source: " + m[1] },
		},
		{
			re: regexp.MustCompile(`^标题最长 (\d+) 个字$`),
			fn: func(m []string) string { return fmt.Sprintf("Title must be at most %s characters", m[1]) },
		},
		{
			re: regexp.MustCompile(`^第 (\d+) 片还没有传上来，请补传后再试$`),
			fn: func(m []string) string {
				return fmt.Sprintf("Chunk %s has not been uploaded yet; please upload it and retry", m[1])
			},
		},
		{
			re: regexp.MustCompile(`^文件超过 (\d+) GB，请压缩或拆分后再上传$`),
			fn: func(m []string) string {
				return fmt.Sprintf("File exceeds %s GB; please compress or split it before uploading", m[1])
			},
		},
		{
			re: regexp.MustCompile(`^标记的重点太多了，最多 (\d+) 个$`),
			fn: func(m []string) string { return fmt.Sprintf("Too many bookmarks (at most %s)", m[1]) },
		},
		{
			re: regexp.MustCompile(`^标记重点的时间不对：(.+)$`),
			fn: func(m []string) string { return "Invalid bookmark timestamp: " + m[1] },
		},
		{
			re: regexp.MustCompile(`^密码至少 (\d+) 位$`),
			fn: func(m []string) string { return fmt.Sprintf("Password must be at least %s characters", m[1]) },
		},
		{
			re: regexp.MustCompile(`^密码最长 (\d+) 位，现在是 (\d+) 位$`),
			fn: func(m []string) string {
				return fmt.Sprintf("Password must be at most %s bytes (currently %s)", m[1], m[2])
			},
		},
		{
			re: regexp.MustCompile(`^录音保留期要在 (\d+) 到 (\d+) 天之间，或者选「一直保留」$`),
			fn: func(m []string) string {
				return fmt.Sprintf("Audio retention must be between %s and %s days, or choose \"Keep forever\"", m[1], m[2])
			},
		},
		{
			re: regexp.MustCompile(`^接口地址要以 http:// 或 https:// 开头，例如 (.+)$`),
			fn: func(m []string) string {
				return fmt.Sprintf("Base URL must start with http:// or https://, e.g. %s", m[1])
			},
		},
		{
			re: regexp.MustCompile(`^说话人编号不对：(.+)$`),
			fn: func(m []string) string { return "Invalid speaker index: " + m[1] },
		},
		{
			re: regexp.MustCompile(`^名字最长 (\d+) 个字$`),
			fn: func(m []string) string { return fmt.Sprintf("Name must be at most %s characters", m[1]) },
		},
		{
			re: regexp.MustCompile(`^模板名字最长 (\d+) 个字$`),
			fn: func(m []string) string { return fmt.Sprintf("Template name must be at most %s characters", m[1]) },
		},
		{
			re: regexp.MustCompile(`^写法说明最长 (\d+) 个字$`),
			fn: func(m []string) string { return fmt.Sprintf("Template guide must be at most %s characters", m[1]) },
		},
		{
			re: regexp.MustCompile(`^最多加 (\d+) 个群机器人，请先删掉不用的再加$`),
			fn: func(m []string) string {
				return fmt.Sprintf("At most %s group bots allowed; please delete unused ones first", m[1])
			},
		},
		{
			re: regexp.MustCompile(`^发到「(.+)」的上一条还在发送中，等发完再发$`),
			fn: func(m []string) string {
				return fmt.Sprintf("Previous message to \"%s\" is still sending; please wait for it to finish", m[1])
			},
		},

		// internal/pipeline dynamic strings
		{
			re: regexp.MustCompile(`^录音不完整：缺少第 (\d+) 片，请重新上传$`),
			fn: func(m []string) string {
				return fmt.Sprintf("Incomplete recording: chunk %s is missing; please upload again", m[1])
			},
		},
		{
			re: regexp.MustCompile(`^录音不完整：第 (\d+) 片读不出来，请重新上传$`),
			fn: func(m []string) string {
				return fmt.Sprintf("Incomplete recording: chunk %s could not be read; please upload again", m[1])
			},
		},
		{
			re: regexp.MustCompile(`^「(.+)」结束了，等飞书生成录制$`),
			fn: func(m []string) string {
				return fmt.Sprintf("\"%s\" ended; waiting for Feishu to generate recording", m[1])
			},
		},
		{
			re: regexp.MustCompile(`^「(.+)」没有云录制，不用导入$`),
			fn: func(m []string) string { return fmt.Sprintf("\"%s\" had no cloud recording; skipped", m[1]) },
		},
		{
			re: regexp.MustCompile(`^「(.+)」的录制已经导入，正在转写$`),
			fn: func(m []string) string {
				return fmt.Sprintf("Recording for \"%s\" imported and queued for transcription", m[1])
			},
		},
		{
			re: regexp.MustCompile(`^「(.+)」等了 12 小时，飞书还没生成好录制，不再等了$`),
			fn: func(m []string) string {
				return fmt.Sprintf("Gave up waiting for \"%s\" after 12 hours without a Feishu recording", m[1])
			},
		},
		{
			re: regexp.MustCompile(`^「(.+)」的录制没能导入：(.+)$`),
			fn: func(m []string) string {
				return fmt.Sprintf("Failed to import recording for \"%s\": %s", m[1], TranslateMessage(EN, m[2]))
			},
		},
		{
			re: regexp.MustCompile(`^录制文件超过 (\d+) GB，没有导入$`),
			fn: func(m []string) string { return fmt.Sprintf("Recording exceeds %s GB; skipped import", m[1]) },
		},
		{
			re: regexp.MustCompile(`^录音超过 (\d+) 小时，暂时处理不了，请拆分后再上传$`),
			fn: func(m []string) string {
				return fmt.Sprintf("Recordings longer than %s hours are not supported; please split before uploading", m[1])
			},
		},

		// internal/llm dynamic strings
		{
			re: regexp.MustCompile(`^这个接口不支持列出模型（返回 (\d+)），请直接填写模型名。也检查一下接口地址，一般以 /v1 结尾$`),
			fn: func(m []string) string {
				return fmt.Sprintf("This endpoint does not support listing models (HTTP %s); please enter the model name manually, and check that the base URL ends with /v1", m[1])
			},
		},
		{
			re: regexp.MustCompile(`(?s)^大模型返回 (\d+)：(.*?)(（检查 API Key，以及这个 Key 能不能用这个模型）|（检查接口地址和模型名；接口地址一般以 /v1 结尾）|（请求太频繁或额度用完了）)?$`),
			fn: func(m []string) string {
				return fmt.Sprintf("LLM returned HTTP %s: %s%s", m[1], m[2], translateLLMHint(m[3]))
			},
		},

		// internal/feishu dynamic strings
		{
			re: regexp.MustCompile(`^App ID 或 App Secret 不对，换不到飞书的访问凭证（飞书返回 (\d+)：(.*)）$`),
			fn: func(m []string) string {
				return fmt.Sprintf("Invalid App ID or App Secret: could not obtain Feishu access token (Feishu code %s: %s)", m[1], TranslateMessage(EN, m[2]))
			},
		},
		{
			re: regexp.MustCompile(`^([a-z0-9_./-]+) 调用失败（飞书返回 HTTP (\d+)）$`),
			fn: func(m []string) string { return fmt.Sprintf("Feishu API %s failed (HTTP %s)", m[1], m[2]) },
		},
		{
			re: regexp.MustCompile(`^([a-z0-9_./-]+) 调用失败（飞书返回 (\d+)(?:：(.*))?）$`),
			fn: func(m []string) string {
				if m[3] != "" {
					return fmt.Sprintf("Feishu API %s failed (code %s: %s)", m[1], m[2], TranslateMessage(EN, m[3]))
				}
				return fmt.Sprintf("Feishu API %s failed (code %s)", m[1], m[2])
			},
		},
		{
			re: regexp.MustCompile(`^(.+)（飞书返回 HTTP (\d+)）$`),
			fn: func(m []string) string {
				return fmt.Sprintf("%s (Feishu HTTP %s)", translateFeishuHint(m[1]), m[2])
			},
		},
		{
			re: regexp.MustCompile(`^(.+)（飞书返回 (\d+)(?:：(.*))?）$`),
			fn: func(m []string) string {
				if m[3] != "" {
					return fmt.Sprintf("%s (Feishu code %s: %s)", translateFeishuHint(m[1]), m[2], TranslateMessage(EN, m[3]))
				}
				return fmt.Sprintf("%s (Feishu code %s)", translateFeishuHint(m[1]), m[2])
			},
		},
		{
			re: regexp.MustCompile(`^用飞书登录没有成功（飞书返回 HTTP (\d+)(?:：(.*))?）$`),
			fn: func(m []string) string {
				if m[2] != "" {
					return fmt.Sprintf("Feishu sign-in failed (HTTP %s: %s)", m[1], TranslateMessage(EN, m[2]))
				}
				return fmt.Sprintf("Feishu sign-in failed (HTTP %s)", m[1])
			},
		},
		{
			re: regexp.MustCompile(`^用飞书登录没有成功（飞书返回 (\d+)(?:：(.*))?）$`),
			fn: func(m []string) string {
				if m[2] != "" {
					return fmt.Sprintf("Feishu sign-in failed (code %s: %s)", m[1], TranslateMessage(EN, m[2]))
				}
				return fmt.Sprintf("Feishu sign-in failed (code %s)", m[1])
			},
		},
		{
			re: regexp.MustCompile(`^看不懂飞书返回的妙记链接：(.+)$`),
			fn: func(m []string) string { return "Unrecognized Feishu Minutes link: " + m[1] },
		},

		// internal/webhook dynamic strings
		{
			re: regexp.MustCompile(`^请填写 Webhook 地址。(.+)$`),
			fn: func(m []string) string { return "Please enter the Webhook URL. " + translateWebhookExample(m[1]) },
		},
		{
			re: regexp.MustCompile(`^Webhook 地址不对。(.+)$`),
			fn: func(m []string) string { return "Invalid Webhook URL. " + translateWebhookExample(m[1]) },
		},
		{
			re: regexp.MustCompile(`^连不上(钉钉|企业微信|Slack)：等了 (\d+) 秒没有响应$`),
			fn: func(m []string) string {
				return fmt.Sprintf("Cannot reach %s: timed out after %ss", translatePlatformLabel(m[1]), m[2])
			},
		},
		{
			re: regexp.MustCompile(`^连不上(钉钉|企业微信|Slack)：(.+)$`),
			fn: func(m []string) string {
				return fmt.Sprintf("Cannot reach %s: %s", translatePlatformLabel(m[1]), m[2])
			},
		},
		{
			re: regexp.MustCompile(`^(钉钉|企业微信)暂时出错了（HTTP (\d+)）$`),
			fn: func(m []string) string {
				return fmt.Sprintf("%s temporary error (HTTP %s)", translatePlatformLabel(m[1]), m[2])
			},
		},
		{
			re: regexp.MustCompile(`^Slack 暂时出错了（HTTP (\d+)）$`),
			fn: func(m []string) string { return fmt.Sprintf("Slack temporary error (HTTP %s)", m[1]) },
		},
		{
			re: regexp.MustCompile(`^(钉钉|企业微信)拒绝了这条消息（HTTP (\d+)）。请检查 Webhook 地址$`),
			fn: func(m []string) string {
				return fmt.Sprintf("%s rejected the message (HTTP %s). Please check the Webhook URL", translatePlatformLabel(m[1]), m[2])
			},
		},
		{
			re: regexp.MustCompile(`^(钉钉|企业微信)返回的内容看不懂，可能 Webhook 地址不对$`),
			fn: func(m []string) string {
				return fmt.Sprintf("Unrecognized response from %s; please check the Webhook URL", translatePlatformLabel(m[1]))
			},
		},
		{
			re: regexp.MustCompile(`^钉钉没有收下这条消息：没通过机器人的安全设置（(.*)）$`),
			fn: func(m []string) string {
				return fmt.Sprintf("DingTalk rejected the message: bot security check failed (%s)", m[1])
			},
		},
		{
			re: regexp.MustCompile(`^(钉钉|企业微信)没有收下这条消息（(-?\d+)：(.*)）$`),
			fn: func(m []string) string {
				return fmt.Sprintf("%s rejected the message (%s: %s)", translatePlatformLabel(m[1]), m[2], m[3])
			},
		},
		{
			re: regexp.MustCompile(`^Slack 没有收下这条消息（(.*)）$`),
			fn: func(m []string) string { return fmt.Sprintf("Slack rejected the message (%s)", m[1]) },
		},

		// internal/cloudasr dynamic strings
		{
			re: regexp.MustCompile(`^在线识别接口地址不对（HTTP 404），默认应该是 (.+)$`),
			fn: func(m []string) string {
				return fmt.Sprintf("Online ASR endpoint not found (HTTP 404); default URL is %s", m[1])
			},
		},
		{
			re: regexp.MustCompile(`^DeepSeek 在线识别服务暂时不可用（HTTP (\d+)），请稍后再试$`),
			fn: func(m []string) string {
				return fmt.Sprintf("DeepSeek Online ASR is temporarily unavailable (HTTP %s); please try again later", m[1])
			},
		},
		{
			re: regexp.MustCompile(`^DeepSeek 在线识别请求失败（HTTP (\d+)），请检查配置$`),
			fn: func(m []string) string {
				return fmt.Sprintf("DeepSeek Online ASR request failed (HTTP %s); please check configuration", m[1])
			},
		},

		// internal/models & internal/asr dynamic strings
		{
			re: regexp.MustCompile(`^建不了模型目录 (.+?)：(.+)$`),
			fn: func(m []string) string { return fmt.Sprintf("Cannot create model directory %s: %s", m[1], m[2]) },
		},
		{
			re: regexp.MustCompile(`^下载识别模型 (.+?) 失败：(.+)$`),
			fn: func(m []string) string {
				return fmt.Sprintf("Failed to download speech model %s: %s", m[1], TranslateMessage(EN, m[2]))
			},
		},
		{
			re: regexp.MustCompile(`^文件大小不对：收到 (\d+) 字节，应该是 (\d+)$`),
			fn: func(m []string) string {
				return fmt.Sprintf("File size mismatch: received %s bytes, expected %s", m[1], m[2])
			},
		},
		{
			re: regexp.MustCompile(`^校验和不对：(.+)$`),
			fn: func(m []string) string { return "Checksum mismatch: " + m[1] },
		},
		{
			re: regexp.MustCompile(`^(区分说话人的模型|识别模型|语音检测模型|声纹模型)加载失败，模型文件 (.+) 可能坏了。请管理员删掉模型目录（ZSMEET_MODELS_DIR，docker compose 部署的是 models 数据卷）里的这个文件，再重启后台处理（docker compose restart worker），它会自动重新下载$`),
			fn: func(m []string) string {
				return fmt.Sprintf("%s failed to load; model file %s may be corrupted. Ask an admin to delete this file from ZSMEET_MODELS_DIR (the models volume in docker compose) and restart the worker (docker compose restart worker) to re-download it", translateASRModelWhat(m[1]), m[2])
			},
		},
		{
			re: regexp.MustCompile(`^区分说话人的模型 (.+) 加载失败：请确认文件存在、可以读取，而且是 sherpa-onnx 能用的说话人声纹模型（\.onnx）$`),
			fn: func(m []string) string {
				return fmt.Sprintf("Failed to load speaker model %s: ensure the file exists, is readable, and is a valid sherpa-onnx speaker embedding model (.onnx)", m[1])
			},
		},
	}
}

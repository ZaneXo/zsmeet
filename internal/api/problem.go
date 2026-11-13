package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/validation"
)

// 这个文件把 huma 自己产生的错误换成中文：参数校验没通过、请求内容不是 JSON、内容太大……
// 它们原本是英文的，网页上会显示成「path.id：expected string to be RFC 4122 uuid」。
//
// 换成中文以后，错误响应（problem+json）是这样的：
//   - detail 是给人看的一句完整的话：哪一项不对、该怎么改，比如「标题太长了，最多 200 个字」。
//     网页和桌面版直接显示它；
//   - errors 里每一项的 message 是这一项的原因（不带是哪一项，比如「太长了，最多 200 个字」）；
//     status、location（body.title、path.id）这些给程序看的字段不变；
//   - errors 里不带 value：huma 会把提交上来的值原样放回去，整个请求出错时（比如少了必填的字段）
//     就是提交的全部内容，里面可能有密码、API Key。

// validationMessages 是 huma 校验提示（validation/messages.go 里的 Msg* 变量）的中文，顺序和原文一样。
//
// 这些话接在出错的那一项后面（「标题」+「太长了，最多 200 个字」），所以都不带主语。
// huma 用 fmt 把参数填进去，占位符要和原文一一对应。原文里有几处 %v 填的是 Go 标准库的英文报错
// （比如 invalid UUID length: 3），用户看不懂，这里写成 %.0v：参数照收，但什么也不显示。
var validationMessages = []struct {
	msg *string
	zh  string
}{
	{&validation.MsgUnexpectedProperty, "是不认识的字段，请去掉"},
	{&validation.MsgExpectedRFC3339DateTime, "不是有效的时间，请写成 2006-01-02T15:04:05+08:00 这样的格式"},
	{&validation.MsgExpectedRFC1123DateTime, "不是有效的时间，请写成 Mon, 02 Jan 2006 15:04:05 GMT 这样的格式"},
	{&validation.MsgExpectedRFC3339Date, "不是有效的日期，请写成 2006-01-02 这样的格式"},
	{&validation.MsgExpectedRFC3339Time, "不是有效的时间，请写成 15:04:05 这样的格式"},
	{&validation.MsgExpectedRFC5322Email, "格式不对，应该是 name@example.com 这样的邮箱地址%.0v"},
	{&validation.MsgExpectedRFC5322EmailBare, "只填邮箱地址本身，不要带名字，比如 name@example.com"},
	{&validation.MsgExpectedRFC5890Hostname, "不是有效的主机名，请检查有没有写错"},
	{&validation.MsgExpectedRFC2673IPv4, "不是有效的 IPv4 地址，应该像 192.168.1.10 这样"},
	{&validation.MsgExpectedRFC2373IPv6, "不是有效的 IPv6 地址，请检查有没有写错"},
	{&validation.MsgExpectedRFCIPAddr, "不是有效的 IP 地址，请检查有没有写错"},
	{&validation.MsgExpectedRFC3986URI, "不是有效的网址，请检查有没有写错%.0v"},
	{&validation.MsgExpectedRFC3986AbsoluteURI, "要写完整的网址，以 https:// 这样的协议开头"},
	{&validation.MsgExpectedRFC4122UUID, "格式不对，应该是一串 UUID，请检查是否完整%.0v"},
	{&validation.MsgExpectedRFC6570URITemplate, "不是有效的 URI 模板，请检查有没有写错"},
	{&validation.MsgExpectedRFC6901JSONPointer, "不是有效的 JSON Pointer，应该像 /a/b 这样"},
	{&validation.MsgExpectedRFC6901RelativeJSONPointer, "不是有效的相对 JSON Pointer，应该像 0/a 这样"},
	{&validation.MsgExpectedRegexp, "不是有效的正则表达式，请检查有没有写错%.0v"},
	{&validation.MsgExpectedMatchAtLeastOneSchema, "不符合任何一种允许的写法，请对照接口文档修改"},
	{&validation.MsgExpectedMatchExactlyOneSchema, "不符合任何一种允许的写法，请对照接口文档修改"},
	{&validation.MsgExpectedNotMatchSchema, "不能用这种写法，请对照接口文档修改"},
	{&validation.MsgExpectedPropertyNameInObject, "没有填，请补上"},
	{&validation.MsgExpectedBoolean, "应该是 true 或 false"},
	{&validation.MsgExpectedNull, "应该是 null"},
	{&validation.MsgExpectedDuration, "不是有效的时长，应该像 1h30m、90s 这样%.0v"},
	{&validation.MsgExpectedNumber, "应该是数字"},
	{&validation.MsgExpectedInteger, "应该是整数"},
	{&validation.MsgExpectedString, "应该是字符串"},
	{&validation.MsgExpectedBase64String, "应该是 Base64 编码的内容"},
	{&validation.MsgExpectedArray, "应该是数组"},
	{&validation.MsgExpectedObject, "应该是 JSON 对象"},
	{&validation.MsgExpectedArrayItemsUnique, "里有重复的项，请去掉重复的"},
	{&validation.MsgExpectedOneOf, "取值不对，只能是 %s 中的一个"},
	{&validation.MsgExpectedConst, "取值不对，只能是 %v"},
	{&validation.MsgExpectedMinimumNumber, "太小了，不能小于 %v"},
	{&validation.MsgExpectedExclusiveMinimumNumber, "太小了，要大于 %v"},
	{&validation.MsgExpectedMaximumNumber, "太大了，不能大于 %v"},
	{&validation.MsgExpectedExclusiveMaximumNumber, "太大了，要小于 %v"},
	{&validation.MsgExpectedNumberBeMultipleOf, "要是 %v 的整数倍"},
	{&validation.MsgExpectedMinLength, "至少要 %d 个字"},
	{&validation.MsgExpectedMaxLength, "太长了，最多 %d 个字"},
	{&validation.MsgExpectedBePattern, "格式不对，要求：%s"},
	{&validation.MsgExpectedMatchPattern, "格式不对，要符合正则表达式 %s"},
	{&validation.MsgExpectedMinItems, "至少要 %d 项"},
	{&validation.MsgExpectedMaxItems, "太多了，最多 %d 项"},
	{&validation.MsgExpectedMinProperties, "至少要 %d 项"},
	{&validation.MsgExpectedMaxProperties, "太多了，最多 %d 项"},
	{&validation.MsgExpectedRequiredProperty, "缺少「%s」，请补上"},
	{&validation.MsgExpectedDependentRequiredProperty, "缺少「%s」：有「%s」时它也要填"},
	{&validation.MsgExpectedResolvableSchemaRef, "服务器的接口定义有问题（找不到 %s），请联系管理员"},
}

// localizeValidation 把 huma 的校验提示换成中文，整个程序只换一次。huma 注册接口时就用它们拼好了一部分提示，
// 所以要在注册接口之前调用。返回换掉的英文原文，测试用它核对 huma 的提示一条不漏都有中文。
var localizeValidation = sync.OnceValue(func() []string {
	en := make([]string, len(validationMessages))
	for i, m := range validationMessages {
		en[i] = *m.msg
		*m.msg = m.zh
	}
	return en
})

// paramMessages 是 huma 写死在代码里的英文原因（不在 validation 包里，没法事先换掉），多数是解析参数时的。
var paramMessages = map[string]string{
	"invalid integer":         "应该是整数",
	"invalid float":           "应该是数字",
	"invalid floating value":  "应该是数字",
	"invalid boolean":         "应该是 true 或 false",
	"invalid url.URL value":   "不是有效的网址，请检查有没有写错",
	"unknown query parameter": "是不认识的参数，请去掉",

	"required path parameter is missing":   "没有填，请补上",
	"required query parameter is missing":  "没有填，请补上",
	"required header parameter is missing": "没有填，请补上",
	"required cookie parameter is missing": "没有填，请补上",
	"required form parameter is missing":   "没有填，请补上",

	"expected at most one value, but received multiple values":        "只能填一个值，请去掉多余的",
	"expected value to match exactly one schema but matched multiple": "同时符合好几种写法，分不清是哪一种，请对照接口文档修改",
}

// paramPrefixes 是后面跟着具体原因（Go 标准库的英文报错）的几种，只看开头。
var paramPrefixes = []struct{ en, zh string }{
	{"invalid date/time for format ", "不是有效的时间，请检查格式"},
	{"invalid value: ", "取值不对，请检查有没有写错"},
	{"invalid JSON: ", "不是合法的 JSON，请检查格式"},
	{"cannot read multipart form: ", "没能读出上传的表单，请重新上传"},
}

// reason 把 huma 写死的英文原因换成中文。已经是中文的（校验提示、jsonFormat 的报错）原样返回。
func reason(msg string) string {
	if zh, ok := paramMessages[msg]; ok {
		return zh
	}
	// 只在请求内容按 JSON 解析的接口出现（上传分片收的是原始内容，什么类型都行）
	if ct, ok := strings.CutPrefix(msg, huma.ErrUnknownContentType.Error()+": "); ok {
		return "格式不支持（Content-Type: " + ct + "），请用 JSON 提交（Content-Type: application/json）"
	}
	for _, p := range paramPrefixes {
		if strings.HasPrefix(msg, p.en) {
			return p.zh
		}
	}
	return msg
}

// jsonFormat 是收发 JSON 用的格式，和 huma 默认的一样，只是请求内容解析不了时报错换成中文。
var jsonFormat = huma.Format{
	Marshal:   huma.DefaultJSONFormat.Marshal,
	Unmarshal: unmarshalJSON,
}

func unmarshalJSON(data []byte, v any) error {
	err := json.Unmarshal(data, v)
	if syntax, ok := errors.AsType[*json.SyntaxError](err); ok {
		// Offset 是读到出错的那个字节为止的字节数，换成第几个字符（一个汉字占三个字节）
		end := min(syntax.Offset, int64(len(data)))
		return fmt.Errorf("不是合法的 JSON，第 %d 个字符附近有错，请检查格式", max(utf8.RuneCount(data[:end]), 1))
	}
	if typ, ok := errors.AsType[*json.UnmarshalTypeError](err); ok {
		// 校验已经通过了，只剩数字超出范围这类情况
		if typ.Field == "" {
			return errors.New("类型不对，请对照接口文档修改")
		}
		return fmt.Errorf("有一项类型不对或者超出了范围：「%s」，请检查", typ.Field)
	}
	return err
}

// readDetails 是 huma 读请求内容出错时写的 detail（huma.go 的 contextError），太大的另外拼（见 tooLarge）。
// 这几种的 errors 是 huma 读请求内容之前已经查出来的参数错误（地址里的 id 不是 UUID 之类），没有就是空的。
var readDetails = map[string]string{
	"request body is required":  "没有收到要提交的内容，请重新提交",
	"request body read timeout": "内容没能及时传完，请检查网络后重试",
}

// goErrorDetails 是 huma 写死的另外几种 detail（huma.go 的 WriteErr、contextError）。
// 它们的 errors 里是 Go 的英文报错（处理函数返回的错误、读请求时网络断了……），不给用户看。
var goErrorDetails = map[string]string{
	"cannot read request body":   "没能收完提交的内容，请检查网络后重试",
	"unexpected error occurred":  "服务器内部出错了，请稍后再试",
	"unable to marshal response": "没法按要求的格式返回结果，请在请求头 Accept 里接受 application/json",
}

// localizeProblem 在错误响应发出去之前把 huma 写的英文换成中文（huma 的 Transformer）。
// 接口自己返回的错误（toHTTP、huma.Error404NotFound……）本来就是中文，原样不动。
func localizeProblem(_ huma.Context, _ string, v any) (any, error) {
	p, ok := v.(*huma.ErrorModel)
	if !ok {
		return v, nil
	}
	out := *p
	if p.Detail == "validation failed" {
		out.Errors = localizeDetails(p.Errors)
		out.Detail = summarize(out.Errors)
		return &out, nil
	}
	if zh, ok := goErrorDetails[p.Detail]; ok {
		if p.Detail == "unexpected error occurred" {
			// 处理函数返回了不认识的错误：和 status 一样只进日志，不给用户看
			for _, e := range p.Errors {
				if e != nil {
					slog.Error("接口内部错误", "err", e.Message)
				}
			}
		}
		// errors 不给用户看，detail 已经说清楚了
		out.Detail, out.Errors = zh, nil
		return &out, nil
	}
	zh, ok := readDetails[p.Detail]
	if strings.HasPrefix(p.Detail, "request body is too large") {
		zh, ok = tooLarge(p.Detail), true
	}
	if !ok {
		return v, nil
	}
	// 读请求内容时出错（没有、太大、没能及时传完），地址里的参数又不对的话（PUT /api/meetings/xyz/chunks/0
	// 不带内容），huma 把查出来的参数错误一起放进 errors：和校验没通过时一样换成中文、位置不变，
	// detail 先说哪个参数不对，再说请求内容的问题
	out.Errors = localizeDetails(p.Errors)
	out.Detail = zh
	if len(out.Errors) > 0 {
		out.Detail = summarize(out.Errors) + "；" + zh
	}
	return &out, nil
}

// localizeDetails 把每一处错误的原因换成中文，位置（location）不变。
// 提交的值（value）不放回去：可能是密码、API Key，或者带着它们的整个请求（见文件开头）。
func localizeDetails(errs []*huma.ErrorDetail) []*huma.ErrorDetail {
	out := make([]*huma.ErrorDetail, 0, len(errs))
	for _, e := range errs {
		if e != nil {
			out = append(out, &huma.ErrorDetail{Message: reason(e.Message), Location: e.Location})
		}
	}
	return out
}

// summarize 把第一处错误写成一句完整的话（哪一项、怎么了、怎么改），放进 detail。
func summarize(errs []*huma.ErrorDetail) string {
	if len(errs) == 0 {
		return "提交的内容不符合要求，请检查后重试"
	}
	s := join(subject(errs[0].Location), errs[0].Message)
	if n := len(errs) - 1; n > 0 {
		s += fmt.Sprintf("（另外还有 %d 处不对）", n)
	}
	return s
}

// tooLarge 换掉 huma 的「request body is too large limit=1048576 bytes」，上限写成 MB。
func tooLarge(detail string) string {
	var limit int64
	if _, err := fmt.Sscanf(detail, "request body is too large limit=%d bytes", &limit); err != nil || limit <= 0 {
		return "提交的内容太大了，请删减一些再提交"
	}
	return fmt.Sprintf("提交的内容太大了，最多 %s，请删减一些再提交", byteSize(limit))
}

// byteSize 把字节数写成 1 MB、1.5 KB 这样。
func byteSize(n int64) string {
	size, unit := float64(n), "字节"
	switch {
	case n >= 1<<20:
		size, unit = size/(1<<20), "MB"
	case n >= 1<<10:
		size, unit = size/(1<<10), "KB"
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", size), ".0") + " " + unit
}

// fieldNames 是网页上要用户自己填的那些字段在界面上的叫法，报错时用它说是哪一项。
// 没列出来的照原样写字段名（「source」）：多半是 App 或者脚本传错了，对写程序的人来说字段名更清楚。
var fieldNames = map[string]string{
	"email":            "邮箱",
	"name":             "名字",
	"password":         "密码",
	"current_password": "当前密码",
	"new_password":     "新密码",
	"title":            "标题",
	"hotwords":         "热词",
	"speaker_count":    "说话人数",
	"content":          "纪要内容",
	"question":         "问题",
	"q":                "搜索词",
	"guide":            "写法说明",
	"base_url":         "接口地址",
	"model":            "模型",
	"api_key":          "API Key",
	"audio_days":       "录音保留天数",
	"url":              "网址",
	"site_url":         "访问地址",
	"secret":           "密钥",
	"app_id":           "App ID",
	"app_secret":       "App Secret",
	"doc_folder":       "文件夹链接",
}

// subject 把出错的位置（location）换成用户看得懂的说法，放在原因前面：body.title → 标题，
// body.members[0].user_id → 「members[0].user_id」，body → 提交的内容，path.id → 地址里的 id。
func subject(location string) string {
	where, field, _ := strings.Cut(location, ".")
	if name, ok := fieldNames[field]; ok && (where == "body" || where == "query") {
		return name
	}
	switch {
	case location == "":
		return ""
	case location == "body":
		return "提交的内容"
	case where == "body":
		return "「" + field + "」"
	case where == "path" || where == "query":
		return "地址里的 " + field
	case where == "header":
		return "请求头 " + field
	}
	return "「" + location + "」"
}

// join 把是哪一项和原因连成一句话。英文、数字和中文之间空一格（地址里的 id 格式不对）。
func join(subject, reason string) string {
	if subject == "" {
		return reason
	}
	last, _ := utf8.DecodeLastRuneInString(subject)
	first, _ := utf8.DecodeRuneInString(reason)
	if isWordChar(last) || isWordChar(first) {
		return subject + " " + reason
	}
	return subject + reason
}

func isWordChar(r rune) bool {
	return r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r))
}

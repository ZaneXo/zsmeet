package feishu

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// 用飞书登录（网页应用的 OAuth 授权码流程）：
//
//  1. 浏览器跳到飞书的授权页（AuthorizeURL）。在飞书网页版已经登录的话点一下就行，没登录的话用飞书手机端扫码；
//  2. 飞书带着授权码（code，5 分钟内有效、只能用一次）跳回 redirect_uri；
//  3. 服务器用 App Secret 和 PKCE 的 code_verifier 把授权码换成用户身份的访问凭证（LoginUser），
//     再用它读这个人的名字、邮箱和 ID。
//
// 文档：https://open.feishu.cn/document/authentication-management/access-token/obtain-oauth-code
// https://open.feishu.cn/document/authentication-management/access-token/get-user-access-token
// https://open.feishu.cn/document/server-docs/authentication-management/login-state-management/get

// LoginScopes 是用飞书登录时向用户申请的权限：读邮箱（email）和企业邮箱（enterprise_email），按邮箱找 zsmeet 的成员。
// 名字和 ID 不需要权限。这两个权限必须先在开发者后台开通，否则用户在授权页上就会看到错误（20027）。
var LoginScopes = []string{"contact:user.email:readonly", "contact:user.employee:readonly"}

// 授权页的地址
const (
	feishuAccounts = "https://accounts.feishu.cn"
	larkAccounts   = "https://accounts.larksuite.com"
)

// AuthorizeURL 返回飞书授权页的地址。override 是 ZSMEET_FEISHU_BASE_URL（私有部署、测试），
// 设了的话授权页也在那里。challenge 是 PKCE 的 code_challenge（S256，见 PKCEChallenge）。
func AuthorizeURL(domain, override, appID, redirectURI, state, challenge string) string {
	host := feishuAccounts
	switch {
	case override != "":
		host = strings.TrimRight(override, "/")
	case domain == "lark":
		host = larkAccounts
	}
	q := url.Values{
		"client_id":             {appID},
		"response_type":         {"code"},
		"redirect_uri":          {redirectURI},
		"scope":                 {strings.Join(LoginScopes, " ")},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	return host + "/open-apis/authen/v1/authorize?" + q.Encode()
}

// PKCEChallenge 按 S256 从 code_verifier 算出 code_challenge。
func PKCEChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// LoginUser 是用飞书登录的人。邮箱拿不到（没开权限、通讯录里没填）时是空的。
type LoginUser struct {
	Name    string `json:"name"`
	OpenID  string `json:"open_id"`
	UnionID string `json:"union_id"`
	// 通讯录里的邮箱：企业管理员填的联系方式，飞书没有让本人验证过
	Email string `json:"email"`
	// 企业邮箱：公司用飞书邮箱时才有
	EnterpriseEmail string `json:"enterprise_email"`
	TenantKey       string `json:"tenant_key"`
}

// OAuthError 是换取用户访问凭证失败时飞书返回的错误。Error() 是给人看的说明，不带授权码和凭证。
type OAuthError struct {
	Status int
	Code   int
	Err    string // 例如 invalid_grant
	Desc   string
}

// 换取用户访问凭证的常见错误码，告诉用户或管理员怎么处理
var oauthHints = map[int]string{
	20002: "飞书应用的 App ID 或 App Secret 不对，请管理员到「设置 → 飞书」里检查",
	20003: "这次飞书登录已经用过或者失效了，请重新点「用飞书登录」",
	20004: "这次飞书登录已经超时，请重新点「用飞书登录」",
	20010: "你不在这个飞书应用的可用范围里，请管理员在飞书管理后台把你加进应用的可用范围",
	20049: "这次飞书登录校验没有通过，请重新点「用飞书登录」",
	20071: "飞书应用里填的重定向 URL 和 zsmeet 的访问地址对不上，请管理员按「设置 → 飞书」里显示的地址重新填写",
}

func (e *OAuthError) Error() string {
	if hint := oauthHints[e.Code]; hint != "" {
		return hint
	}
	detail := fmt.Sprintf("飞书返回 %d", e.Code)
	if e.Code == 0 {
		detail = fmt.Sprintf("飞书返回 HTTP %d", e.Status)
	}
	if e.Desc != "" {
		detail += "：" + e.Desc
	}
	return "用飞书登录没有成功（" + detail + "）"
}

// LoginUser 用授权码换取用户身份的访问凭证，再读这个人的信息。redirectURI 要和打开授权页时的一样，
// verifier 是 PKCE 的 code_verifier。用户的访问凭证只在这里用一次，不保存。
func (c *Client) LoginUser(ctx context.Context, code, redirectURI, verifier string) (LoginUser, error) {
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     c.appID,
		"client_secret": c.appSecret,
		"code":          code,
		"redirect_uri":  redirectURI,
		"code_verifier": verifier,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/open-apis/authen/v2/oauth/token", bytes.NewReader(body))
	if err != nil {
		return LoginUser{}, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := c.http.Do(req)
	if err != nil {
		return LoginUser{}, fmt.Errorf("连不上飞书：%w", err)
	}
	defer resp.Body.Close()
	// 这个接口不套 data，凭证和错误都在最外层
	var tok struct {
		Code        int    `json:"code"`
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Desc        string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return LoginUser{}, &OAuthError{Status: resp.StatusCode, Desc: "返回的内容看不懂"}
	}
	if tok.Code != 0 || tok.AccessToken == "" || resp.StatusCode >= 400 {
		return LoginUser{}, &OAuthError{Status: resp.StatusCode, Code: tok.Code, Err: tok.Error, Desc: tok.Desc}
	}

	var user LoginUser
	env, err := c.once(ctx, request{method: http.MethodGet, path: "/open-apis/authen/v1/user_info"}, tok.AccessToken)
	if err != nil {
		return LoginUser{}, err
	}
	if err := json.Unmarshal(env.Data, &user); err != nil {
		return LoginUser{}, &Error{API: "authen/v1/user_info", Msg: "返回的内容看不懂：" + err.Error()}
	}
	if user.OpenID == "" && user.UnionID == "" {
		return LoginUser{}, &Error{API: "authen/v1/user_info", Msg: "没有返回用户的 ID"}
	}
	return user, nil
}

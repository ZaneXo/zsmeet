package api

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// invite 以管理员身份邀请一个邮箱，返回邀请的 ID 和链接里的 token。
func (c *client) invite(email, role string) (id, token string) {
	c.t.Helper()
	r := c.want(c.do("POST", "/api/invites", obj{"email": email, "role": role}), 201)
	return r.Body["invite"].(obj)["id"].(string), r.Body["token"].(string)
}

// join 用邀请链接注册一个新成员，返回他登录好的客户端。
func (c *client) join(token, name, password string) *client {
	c.t.Helper()
	member := c.another()
	member.want(member.do("POST", "/api/account-links/accept", obj{"token": token, "name": name, "password": password}), 200)
	return member
}

// memberID 按邮箱找出成员的 ID（以管理员身份查列表）。
func (c *client) memberID(email string) string {
	c.t.Helper()
	list := c.want(c.do("GET", "/api/members", nil), 200)
	for _, m := range list.Body["members"].([]any) {
		if m.(obj)["email"] == email {
			return m.(obj)["id"].(string)
		}
	}
	c.t.Fatalf("成员列表里没有 %s：%s", email, list.Raw)
	return ""
}

func lookup(c *client, token string) result {
	return c.do("POST", "/api/account-links/lookup", obj{"token": token})
}

const garbageToken = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func TestInviteAndJoin(t *testing.T) {
	admin := newServer(t)
	admin.setup()

	admin.want(admin.do("POST", "/api/invites", obj{"email": "不是邮箱", "role": "member"}), 422)
	admin.want(admin.do("POST", "/api/invites", obj{"email": "new@example.com", "role": "owner"}), 422)
	already := admin.want(admin.do("POST", "/api/invites", obj{"email": "Admin@Example.com", "role": "member"}), 409)
	if !strings.Contains(already.Body["detail"].(string), "已经是成员") {
		t.Errorf("邀请已有成员的邮箱时应该说明原因：%s", already.Raw)
	}

	created := admin.want(admin.do("POST", "/api/invites", obj{"email": " New@Example.com ", "role": "member"}), 201)
	token := created.Body["token"].(string)
	invite := created.Body["invite"].(obj)
	if invite["email"] != "new@example.com" || invite["role"] != "member" || invite["invited_by"] != "张敏" || invite["expired"] != false {
		t.Errorf("邀请的内容不对：%s", created.Raw)
	}
	if len(token) < 40 {
		t.Errorf("token 太短，不够随机：%q", token)
	}
	// 数据库里只有哈希，没有 token 原文
	var hashLen int
	var leaked bool
	if err := admin.pool.QueryRow(t.Context(),
		"SELECT length(token_hash), position($1::bytea in token_hash) > 0 FROM account_links", []byte(token)).Scan(&hashLen, &leaked); err != nil {
		t.Fatal(err)
	}
	if hashLen != 32 || leaked {
		t.Errorf("数据库里应该只存 32 字节的哈希：长度 %d，包含原文 %v", hashLen, leaked)
	}

	list := admin.want(admin.do("GET", "/api/members", nil), 200)
	if len(list.Body["members"].([]any)) != 1 || len(list.Body["invites"].([]any)) != 1 {
		t.Errorf("应该有 1 个成员、1 个待接受的邀请：%s", list.Raw)
	}
	if strings.Contains(list.Raw, token) {
		t.Error("列表里不应该再出现 token，它只在生成的那一刻返回")
	}

	// 被邀请的人没有账号，不登录就能看这个链接是给谁的
	visitor := admin.another()
	info := visitor.want(lookup(visitor, token), 200)
	if info.Body["kind"] != "invite" || info.Body["email"] != "new@example.com" || info.Body["invited_by"] != "张敏" {
		t.Errorf("落地页拿到的信息不对：%s", info.Raw)
	}
	visitor.want(lookup(visitor, garbageToken), 404)
	// 链接被截断得很短时，也是同一句「链接无效，请确认复制完整」，而不是参数校验的报错
	if cut := visitor.want(lookup(visitor, token[:6]), 404); !strings.Contains(cut.Body["detail"].(string), "复制完整") {
		t.Errorf("被截断的链接应该提示确认复制完整：%s", cut.Raw)
	}
	// 链接后面粘上了一大段别的文字，同样处理
	visitor.want(lookup(visitor, token+strings.Repeat("%E8%AF%B7%E5%9C%A8%E4%B8%83%E5%A4%A9%E5%86%85%E6%89%93%E5%BC%80", 10)), 404)

	visitor.want(visitor.do("POST", "/api/account-links/accept", obj{"token": token, "name": "李航", "password": "短"}), 422)
	visitor.want(visitor.do("POST", "/api/account-links/accept", obj{"token": token, "name": "  ", "password": "member-pass-123"}), 422)
	// 上面两次失败不应该把链接用掉
	visitor.want(lookup(visitor, token), 200)

	joined := visitor.want(visitor.do("POST", "/api/account-links/accept", obj{"token": token, "name": " 李航 ", "password": "member-pass-123"}), 200)
	if joined.Body["email"] != "new@example.com" || joined.Body["name"] != "李航" || joined.Body["role"] != "member" {
		t.Errorf("新成员的信息不对：%s", joined.Raw)
	}
	// 接受邀请后直接是登录状态
	visitor.want(visitor.do("GET", "/api/meetings", nil), 200)

	// 链接只能用一次
	used := visitor.want(lookup(visitor, token), 409)
	if !strings.Contains(used.Body["detail"].(string), "已经用过了") {
		t.Errorf("用过的链接应该提示已经用过了：%s", used.Raw)
	}
	admin.another().want(admin.another().do("POST", "/api/account-links/accept", obj{"token": token, "name": "冒名", "password": "another-pass-123"}), 409)

	list = admin.want(admin.do("GET", "/api/members", nil), 200)
	if len(list.Body["members"].([]any)) != 2 || len(list.Body["invites"].([]any)) != 0 {
		t.Errorf("接受后应该有 2 个成员、没有待接受的邀请：%s", list.Raw)
	}
	admin.want(admin.do("POST", "/api/invites", obj{"email": "new@example.com", "role": "member"}), 409)

	// 新成员能正常登录
	again := admin.another()
	again.want(again.do("POST", "/api/login", obj{"email": "new@example.com", "password": "member-pass-123"}), 200)
}

func TestInviteRenewReinviteRevoke(t *testing.T) {
	admin := newServer(t)
	admin.setup()
	visitor := admin.another()

	id, first := admin.invite("new@example.com", "member")

	// 重新生成：旧链接作废
	renewed := admin.want(admin.do("POST", "/api/invites/"+id+"/renew", nil), 200)
	second := renewed.Body["token"].(string)
	if second == first {
		t.Fatal("重新生成后 token 应该变")
	}
	visitor.want(lookup(visitor, first), 404)
	visitor.want(lookup(visitor, second), 200)

	// 对同一个邮箱再邀请一次（这次设为管理员）：还是只有一个邀请，之前的链接作废
	_, third := admin.invite("new@example.com", "admin")
	visitor.want(lookup(visitor, second), 404)
	visitor.want(lookup(visitor, third), 200)
	list := admin.want(admin.do("GET", "/api/members", nil), 200)
	invites := list.Body["invites"].([]any)
	if len(invites) != 1 || invites[0].(obj)["role"] != "admin" {
		t.Fatalf("同一个邮箱应该只有一个邀请，角色是最后一次选的：%s", list.Raw)
	}
	id = invites[0].(obj)["id"].(string)

	// 撤销
	admin.want(admin.do("DELETE", "/api/invites/"+id, nil), 204)
	visitor.want(lookup(visitor, third), 404)
	visitor.want(visitor.do("POST", "/api/account-links/accept", obj{"token": third, "name": "李航", "password": "member-pass-123"}), 404)
	admin.want(admin.do("DELETE", "/api/invites/"+id, nil), 404)
	admin.want(admin.do("POST", "/api/invites/"+id+"/renew", nil), 404)
}

func TestExpiredInvite(t *testing.T) {
	admin := newServer(t)
	admin.setup()
	visitor := admin.another()
	id, token := admin.invite("new@example.com", "member")

	if _, err := admin.pool.Exec(t.Context(), "UPDATE account_links SET expires_at = now() - interval '1 minute'"); err != nil {
		t.Fatal(err)
	}
	expired := visitor.want(lookup(visitor, token), 409)
	if !strings.Contains(expired.Body["detail"].(string), "过期") {
		t.Errorf("过期的链接应该提示已过期：%s", expired.Raw)
	}
	visitor.want(visitor.do("POST", "/api/account-links/accept", obj{"token": token, "name": "李航", "password": "member-pass-123"}), 409)

	list := admin.want(admin.do("GET", "/api/members", nil), 200)
	if list.Body["invites"].([]any)[0].(obj)["expired"] != true {
		t.Errorf("列表里应该标出这个邀请已过期：%s", list.Raw)
	}

	// 重新生成以后又能用了
	renewed := admin.want(admin.do("POST", "/api/invites/"+id+"/renew", nil), 200)
	admin.join(renewed.Body["token"].(string), "李航", "member-pass-123")
}

func TestRolesAndDisabling(t *testing.T) {
	admin := newServer(t)
	admin.setup()
	_, token := admin.invite("lihang@example.com", "member")
	member := admin.join(token, "李航", "member-pass-123")
	adminID, memberID := admin.memberID("admin@example.com"), admin.memberID("lihang@example.com")

	// 普通成员碰不了成员管理
	for name, r := range map[string]result{
		"看成员列表":  member.do("GET", "/api/members", nil),
		"邀请":     member.do("POST", "/api/invites", obj{"email": "x@example.com", "role": "admin"}),
		"改别人的角色": member.do("PATCH", "/api/members/"+adminID, obj{"role": "member"}),
		"提升自己":   member.do("PATCH", "/api/members/"+memberID, obj{"role": "admin"}),
		"重置别人密码": member.do("POST", "/api/members/"+adminID+"/reset-link", nil),
	} {
		if r.Status != 403 {
			t.Errorf("普通成员%s应该被拒绝（403），得到 %d：%s", name, r.Status, r.Raw)
		}
	}

	// 管理员不能改自己
	admin.want(admin.do("PATCH", "/api/members/"+adminID, obj{"role": "member"}), 422)
	admin.want(admin.do("PATCH", "/api/members/"+adminID, obj{"disabled": true}), 422)
	admin.want(admin.do("PATCH", "/api/members/00000000-0000-0000-0000-000000000001", obj{"role": "admin"}), 404)
	admin.want(admin.do("PATCH", "/api/members/"+memberID, obj{"role": "owner"}), 422)

	// 提升为管理员：立刻生效，不用重新登录
	promoted := admin.want(admin.do("PATCH", "/api/members/"+memberID, obj{"role": "admin"}), 200)
	if promoted.Body["role"] != "admin" {
		t.Errorf("角色没有改过来：%s", promoted.Raw)
	}
	member.want(member.do("GET", "/api/members", nil), 200)

	// 新管理员把原来的管理员降级：原管理员立刻失去权限
	member.want(member.do("PATCH", "/api/members/"+adminID, obj{"role": "member"}), 200)
	admin.want(admin.do("GET", "/api/members", nil), 403)
	admin.want(admin.do("GET", "/api/meetings", nil), 200)

	// 停用：正在用的登录立刻失效，也不能再登录
	member.want(member.do("PATCH", "/api/members/"+adminID, obj{"disabled": true}), 200)
	admin.want(admin.do("GET", "/api/meetings", nil), 401)
	admin.want(admin.do("POST", "/api/login", obj{"email": "admin@example.com", "password": "sample-pass-123"}), 403)
	// 停用的邮箱不能被重新邀请，提示去启用
	reinvite := member.want(member.do("POST", "/api/invites", obj{"email": "admin@example.com", "role": "member"}), 409)
	if !strings.Contains(reinvite.Body["detail"].(string), "停用") {
		t.Errorf("应该提示这个账号已被停用：%s", reinvite.Raw)
	}

	// 重新启用后可以登录；但停用前的那个登录不会跟着复活（比如丢了的那台电脑上的）
	member.want(member.do("PATCH", "/api/members/"+adminID, obj{"disabled": false}), 200)
	admin.want(admin.do("GET", "/api/meetings", nil), 401)
	admin.want(admin.do("POST", "/api/login", obj{"email": "admin@example.com", "password": "sample-pass-123"}), 200)
	admin.want(admin.do("GET", "/api/meetings", nil), 200)
}

// 两个管理员同时把对方降级：不管谁先谁后，最后一定还剩一个管理员。
func TestMutualDemotionAlwaysLeavesAnAdmin(t *testing.T) {
	a := newServer(t)
	a.setup()
	_, token := a.invite("b@example.com", "admin")
	b := a.join(token, "李航", "member-pass-123")
	aID, bID := a.memberID("admin@example.com"), a.memberID("b@example.com")

	for round := range 20 {
		if _, err := a.pool.Exec(t.Context(), "UPDATE users SET role = 'admin'"); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		var ra, rb result
		wg.Go(func() { <-start; ra = a.do("PATCH", "/api/members/"+bID, obj{"role": "member"}) })
		wg.Go(func() { <-start; rb = b.do("PATCH", "/api/members/"+aID, obj{"role": "member"}) })
		close(start)
		wg.Wait()

		var admins int
		if err := a.pool.QueryRow(t.Context(), "SELECT count(*) FROM users WHERE role = 'admin' AND NOT disabled").Scan(&admins); err != nil {
			t.Fatal(err)
		}
		if admins != 1 {
			t.Fatalf("第 %d 轮：应该正好剩 1 个管理员，实际 %d 个（两个请求分别返回 %d 和 %d）", round+1, admins, ra.Status, rb.Status)
		}
		if ok := (ra.Status == 200) != (rb.Status == 200); !ok {
			t.Fatalf("第 %d 轮：两个请求应该一个成功、一个被拒绝，实际 %d 和 %d", round+1, ra.Status, rb.Status)
		}
	}
}

func TestResetPasswordLink(t *testing.T) {
	admin := newServer(t)
	admin.setup()
	_, token := admin.invite("lihang@example.com", "member")
	device1 := admin.join(token, "李航", "old-password-123")
	memberID := admin.memberID("lihang@example.com")

	created := admin.want(admin.do("POST", "/api/members/"+memberID+"/reset-link", nil), 200)
	first := created.Body["token"].(string)
	expires, err := time.Parse(time.RFC3339Nano, created.Body["expires_at"].(string))
	if err != nil || time.Until(expires) < 23*time.Hour || time.Until(expires) > 25*time.Hour {
		t.Errorf("重置链接应该 24 小时后过期，实际 %v（%v）", created.Body["expires_at"], err)
	}
	// 再生成一次，前一个就作废
	second := admin.want(admin.do("POST", "/api/members/"+memberID+"/reset-link", nil), 200).Body["token"].(string)
	visitor := admin.another()
	visitor.want(lookup(visitor, first), 404)

	info := visitor.want(lookup(visitor, second), 200)
	if info.Body["kind"] != "reset" || info.Body["email"] != "lihang@example.com" || info.Body["name"] != "李航" {
		t.Errorf("落地页拿到的信息不对：%s", info.Raw)
	}

	visitor.want(visitor.do("POST", "/api/account-links/accept", obj{"token": second, "password": "短"}), 422)
	reset := visitor.want(visitor.do("POST", "/api/account-links/accept", obj{"token": second, "password": "new-password-456"}), 200)
	if reset.Body["email"] != "lihang@example.com" {
		t.Errorf("重置后登录的应该是这个账号：%s", reset.Raw)
	}
	// 设置新密码的这台设备直接登录；原来那台设备被退出
	visitor.want(visitor.do("GET", "/api/meetings", nil), 200)
	device1.want(device1.do("GET", "/api/meetings", nil), 401)

	visitor.want(lookup(visitor, second), 409)
	fresh := admin.another()
	fresh.want(fresh.do("POST", "/api/login", obj{"email": "lihang@example.com", "password": "old-password-123"}), 401)
	fresh.want(fresh.do("POST", "/api/login", obj{"email": "lihang@example.com", "password": "new-password-456"}), 200)
}

func TestResetLinkAndDisabledAccounts(t *testing.T) {
	admin := newServer(t)
	admin.setup()
	_, token := admin.invite("lihang@example.com", "member")
	admin.join(token, "李航", "old-password-123")
	memberID := admin.memberID("lihang@example.com")

	// 链接生成以后账号被停用了：链接随之作废，不能用来绕过停用
	link := admin.want(admin.do("POST", "/api/members/"+memberID+"/reset-link", nil), 200).Body["token"].(string)
	admin.want(admin.do("PATCH", "/api/members/"+memberID, obj{"disabled": true}), 200)
	visitor := admin.another()
	visitor.want(visitor.do("POST", "/api/account-links/accept", obj{"token": link, "password": "new-password-456"}), 404)
	visitor.want(visitor.do("GET", "/api/meetings", nil), 401)

	// 已停用的账号不能生成重置链接
	admin.want(admin.do("POST", "/api/members/"+memberID+"/reset-link", nil), 409)
	admin.want(admin.do("POST", "/api/members/00000000-0000-0000-0000-000000000001/reset-link", nil), 404)

	// 重新启用以后，停用前的那个链接也不会复活
	admin.want(admin.do("PATCH", "/api/members/"+memberID, obj{"disabled": false}), 200)
	visitor.want(lookup(visitor, link), 404)

	// 即使停用期间库里还留着一个重置链接（生成和停用几乎同时发生时会这样），启用时也会清掉
	late := admin.want(admin.do("POST", "/api/members/"+memberID+"/reset-link", nil), 200).Body["token"].(string)
	if _, err := admin.pool.Exec(t.Context(), "UPDATE users SET disabled = true WHERE id = $1", memberID); err != nil {
		t.Fatal(err)
	}
	// 停用期间这个链接用不了：落地页直接说明，硬提交也不能给停用的账号换密码
	if r := visitor.want(lookup(visitor, late), 403); !strings.Contains(r.Body["detail"].(string), "停用") {
		t.Errorf("应该说明账号已被停用：%s", r.Raw)
	}
	visitor.want(visitor.do("POST", "/api/account-links/accept", obj{"token": late, "password": "attacker-pass-789"}), 403)
	admin.want(admin.do("PATCH", "/api/members/"+memberID, obj{"disabled": false}), 200)
	visitor.want(lookup(visitor, late), 404)
	// 折腾了这么多次，密码始终没被换掉
	visitor.want(visitor.do("POST", "/api/login", obj{"email": "lihang@example.com", "password": "old-password-123"}), 200)
}

// 重置链接能直接接管一个账号，所以管理员们要看得到谁名下有一个，并且能撤销。
func TestResetLinkIsVisibleAndRevocable(t *testing.T) {
	admin := newServer(t)
	admin.setup()
	_, token := admin.invite("lihang@example.com", "member")
	member := admin.join(token, "李航", "member-pass-123")
	memberID := admin.memberID("lihang@example.com")
	visitor := admin.another()

	pending := func() any {
		t.Helper()
		list := admin.want(admin.do("GET", "/api/members", nil), 200)
		for _, m := range list.Body["members"].([]any) {
			if m.(obj)["id"] == memberID {
				return m.(obj)["reset_link"]
			}
		}
		t.Fatalf("成员列表里没有李航：%s", list.Raw)
		return nil
	}
	if pending() != nil {
		t.Fatal("还没生成过重置链接，列表里不应该显示")
	}

	link := admin.want(admin.do("POST", "/api/members/"+memberID+"/reset-link", nil), 200).Body["token"].(string)
	shown, ok := pending().(obj)
	if !ok || shown["created_by"] != "张敏" || shown["expires_at"] == nil {
		t.Fatalf("列表里应该显示谁生成了重置链接、什么时候过期：%v", pending())
	}
	if list := admin.do("GET", "/api/members", nil); strings.Contains(list.Raw, link) {
		t.Error("列表里不应该出现链接本身")
	}

	// 普通成员不能撤销；管理员撤销后链接立刻失效
	member.want(member.do("DELETE", "/api/members/"+memberID+"/reset-link", nil), 403)
	visitor.want(lookup(visitor, link), 200)
	admin.want(admin.do("DELETE", "/api/members/"+memberID+"/reset-link", nil), 204)
	visitor.want(lookup(visitor, link), 404)
	if pending() != nil {
		t.Error("撤销以后列表里不应该再显示")
	}
	admin.want(admin.do("DELETE", "/api/members/"+memberID+"/reset-link", nil), 404)

	// 用掉的链接也不再显示
	link = admin.want(admin.do("POST", "/api/members/"+memberID+"/reset-link", nil), 200).Body["token"].(string)
	visitor.want(visitor.do("POST", "/api/account-links/accept", obj{"token": link, "password": "new-password-456"}), 200)
	if pending() != nil {
		t.Error("用掉以后列表里不应该再显示")
	}
}

// 自己改密码（常常是因为担心账号被人动过）时，之前发出去的重置链接一起作废。
func TestChangingPasswordCancelsPendingResetLink(t *testing.T) {
	admin := newServer(t)
	admin.setup()
	_, token := admin.invite("lihang@example.com", "member")
	member := admin.join(token, "李航", "old-password-123")
	link := admin.want(admin.do("POST", "/api/members/"+admin.memberID("lihang@example.com")+"/reset-link", nil), 200).Body["token"].(string)

	member.want(member.do("POST", "/api/me/password", obj{"current_password": "old-password-123", "new_password": "new-password-456"}), 204)

	visitor := admin.another()
	visitor.want(lookup(visitor, link), 404)
	visitor.want(visitor.do("POST", "/api/account-links/accept", obj{"token": link, "password": "attacker-pass-789"}), 404)
}

// 邀请和重置链接的效力来自发出它的管理员。他被降级或停用以后，还没用掉的链接全部作废：
// 否则刚被撤掉权限的人，可以用手里留着的链接再拿到一个管理员账号。
func TestLinksStopWorkingWhenIssuerLosesAdmin(t *testing.T) {
	for _, how := range []struct {
		name   string
		change obj
	}{
		{"降级", obj{"role": "member"}},
		{"停用", obj{"disabled": true}},
	} {
		t.Run(how.name, func(t *testing.T) {
			boss := newServer(t)
			boss.setup()
			_, token := boss.invite("rogue@example.com", "admin")
			rogue := boss.join(token, "王悦", "rogue-pass-123")
			bossID, rogueID := boss.memberID("admin@example.com"), boss.memberID("rogue@example.com")

			// 这位管理员给自己留了两条后路：一个管理员角色的邀请，和另一位管理员账号的重置链接
			_, backdoorInvite := rogue.invite("second-mailbox@example.com", "admin")
			backdoorReset := rogue.want(rogue.do("POST", "/api/members/"+bossID+"/reset-link", nil), 200).Body["token"].(string)
			visitor := boss.another()
			visitor.want(lookup(visitor, backdoorInvite), 200)
			visitor.want(lookup(visitor, backdoorReset), 200)

			boss.want(boss.do("PATCH", "/api/members/"+rogueID, how.change), 200)

			// 他给另一位管理员生成的重置链接直接作废
			visitor.want(lookup(visitor, backdoorReset), 404)
			visitor.want(visitor.do("POST", "/api/account-links/accept", obj{"token": backdoorReset, "password": "takeover-pass-1"}), 404)
			// 他发的邀请立刻过期，用不了了
			if r := visitor.want(lookup(visitor, backdoorInvite), 409); !strings.Contains(r.Body["detail"].(string), "过期") {
				t.Errorf("%s后，他发的邀请应该提示已过期：%s", how.name, r.Raw)
			}
			visitor.want(visitor.do("POST", "/api/account-links/accept", obj{"token": backdoorInvite, "name": "小号", "password": "takeover-pass-1"}), 409)
			// 另一位管理员的账号没有被动过
			boss.want(boss.do("GET", "/api/meetings", nil), 200)

			// 邀请还留在列表里（标成已失效），别的管理员重新生成一个链接就能接着用，不用重新填邮箱
			list := boss.want(boss.do("GET", "/api/members", nil), 200)
			invites := list.Body["invites"].([]any)
			if len(invites) != 1 || invites[0].(obj)["expired"] != true || invites[0].(obj)["email"] != "second-mailbox@example.com" {
				t.Fatalf("他发的邀请应该留在列表里并标成已失效：%s", list.Raw)
			}
			renewed := boss.want(boss.do("POST", "/api/invites/"+invites[0].(obj)["id"].(string)+"/renew", nil), 200)
			if renewed.Body["invite"].(obj)["expired"] != false || renewed.Body["invite"].(obj)["invited_by"] != "张敏" {
				t.Errorf("重新生成后，邀请应该记在现在这位管理员名下并且有效：%s", renewed.Raw)
			}
			visitor.want(lookup(visitor, renewed.Body["token"].(string)), 200)
			visitor.want(lookup(visitor, backdoorInvite), 404)
		})
	}
}

// 上面那条规则不依赖「改角色的代码记得去删链接」：使用链接时会再确认一次发出者的身份。
func TestLinkChecksIssuerWhenUsed(t *testing.T) {
	boss := newServer(t)
	boss.setup()
	_, token := boss.invite("other@example.com", "admin")
	other := boss.join(token, "王悦", "other-pass-123")
	_, invite := other.invite("new@example.com", "member")
	reset := other.want(other.do("POST", "/api/members/"+boss.memberID("admin@example.com")+"/reset-link", nil), 200).Body["token"].(string)

	// 绕过成员管理的接口，直接在数据库里把发出者改成普通成员，链接还留在库里
	if _, err := boss.pool.Exec(t.Context(), "UPDATE users SET role = 'member' WHERE email = 'other@example.com'"); err != nil {
		t.Fatal(err)
	}
	visitor := boss.another()
	stale := visitor.want(lookup(visitor, invite), 409)
	if !strings.Contains(stale.Body["detail"].(string), "不是管理员") {
		t.Errorf("应该说明发出链接的人已经不是管理员：%s", stale.Raw)
	}
	visitor.want(visitor.do("POST", "/api/account-links/accept", obj{"token": invite, "name": "新人", "password": "member-pass-123"}), 409)
	// 重置链接也一样：它能直接接管另一位管理员的账号，更不能漏
	visitor.want(lookup(visitor, reset), 409)
	visitor.want(visitor.do("POST", "/api/account-links/accept", obj{"token": reset, "password": "takeover-pass-1"}), 409)
	visitor.want(visitor.do("POST", "/api/login", obj{"email": "admin@example.com", "password": "sample-pass-123"}), 200)
	// 列表里也要标成已失效，不能让管理员以为它还能用
	list := boss.want(boss.do("GET", "/api/members", nil), 200)
	if invites := list.Body["invites"].([]any); len(invites) != 1 || invites[0].(obj)["expired"] != true {
		t.Errorf("发出者不是管理员的邀请在列表里应该标成已失效：%s", list.Raw)
	}
}

// 管理员被降级、停用的那一瞬间，他正在发的链接不能成为漏网之鱼：要么赶在前面写完、被一起清掉，
// 要么排在后面、因为已经不是管理员而被拒绝。否则这条链接会一直留在库里，等他哪天恢复身份就又能用了。
func TestLinksIssuedWhileLosingAdminDoNotSurvive(t *testing.T) {
	boss := newServer(t)
	boss.setup()
	_, token := boss.invite("other@example.com", "admin")
	boss.join(token, "王悦", "other-pass-123")
	bossID, otherID := boss.memberID("admin@example.com"), boss.memberID("other@example.com")
	ctx := t.Context()

	for round := range 12 {
		// 每一轮开始时王悦都是在任的管理员。重新登录：上一轮被停用的话，原来的登录已经作废了
		if _, err := boss.pool.Exec(ctx, "UPDATE users SET role = 'admin', disabled = false WHERE id = $1", otherID); err != nil {
			t.Fatal(err)
		}
		other := boss.another()
		other.want(other.do("POST", "/api/login", obj{"email": "other@example.com", "password": "other-pass-123"}), 200)

		// 准备一个张敏发的邀请，让王悦反复「重新生成链接」：重新生成以后，这个邀请就记在她名下了
		renewID, _ := boss.invite(fmt.Sprintf("renew-%d@example.com", round), "member")

		// 王悦同时用三种方式不停地发链接，直到被拒绝为止
		sends := []func() result{
			func() result { return other.do("POST", "/api/members/"+bossID+"/reset-link", nil) },
			func() result {
				return other.do("POST", "/api/invites", obj{"email": "backdoor@example.com", "role": "admin"})
			},
			func() result { return other.do("POST", "/api/invites/"+renewID+"/renew", nil) },
		}
		last := make([]result, len(sends))
		// 每个循环连着成功几次以后报一声 true；还没成功几次就被拒绝了，报 false。每个循环只报一次
		ready := make(chan bool, len(sends))
		var wg sync.WaitGroup
		for k, send := range sends {
			wg.Go(func() {
				for i := range 5000 {
					if last[k] = send(); last[k].Status >= 400 {
						if i <= 5 {
							ready <- false
						}
						return
					}
					if i == 5 {
						ready <- true
					}
				}
			})
		}
		warmedUp := true
		for range sends {
			warmedUp = <-ready && warmedUp
		}

		how, change := "降级", obj{"role": "member"}
		if round%2 == 1 {
			how, change = "停用", obj{"disabled": true}
		}
		boss.want(boss.do("PATCH", "/api/members/"+otherID, change), 200)
		wg.Wait()
		for k, r := range last {
			if !warmedUp || (r.Status != 401 && r.Status != 403) {
				t.Fatalf("第 %d 轮：王悦是管理员时发链接应该成功，%s以后应该被拒绝（401 或 403）。第 %d 种请求最后得到 %d：%s",
					round+1, how, k+1, r.Status, r.Raw)
			}
		}

		var left int
		if err := boss.pool.QueryRow(ctx,
			"SELECT count(*) FROM account_links WHERE created_by = $1 AND used_at IS NULL AND expires_at > now()", otherID,
		).Scan(&left); err != nil {
			t.Fatal(err)
		}
		if left != 0 {
			t.Fatalf("第 %d 轮：王悦被%s以后，库里还留着 %d 条她发的、没过期的链接", round+1, how, left)
		}
	}
}

// 就算真有一条链接躲过了卸任时的清理（这里直接改数据库来模拟），发出者恢复管理员身份时它也不会跟着复活。
func TestLinksDoNotReviveWhenIssuerIsRestored(t *testing.T) {
	for _, how := range []struct {
		name    string
		restore func(boss *client, otherID string)
	}{
		{"网页上重新设为管理员", func(boss *client, otherID string) {
			boss.want(boss.do("PATCH", "/api/members/"+otherID, obj{"role": "admin"}), 200)
		}},
		{"服务器上执行 make-admin", func(boss *client, _ string) {
			if _, err := boss.svc.RecoverAdmin(boss.t.Context(), "other@example.com"); err != nil {
				boss.t.Fatal(err)
			}
		}},
	} {
		t.Run(how.name, func(t *testing.T) {
			boss := newServer(t)
			boss.setup()
			_, token := boss.invite("other@example.com", "admin")
			other := boss.join(token, "王悦", "other-pass-123")
			bossID, otherID := boss.memberID("admin@example.com"), boss.memberID("other@example.com")
			_, invite := other.invite("second-mailbox@example.com", "admin")
			reset := other.want(other.do("POST", "/api/members/"+bossID+"/reset-link", nil), 200).Body["token"].(string)

			// 绕过成员管理的接口把她降级：链接还留在库里，只是暂时用不了
			if _, err := boss.pool.Exec(t.Context(), "UPDATE users SET role = 'member' WHERE id = $1", otherID); err != nil {
				t.Fatal(err)
			}
			visitor := boss.another()
			visitor.want(lookup(visitor, invite), 409)
			visitor.want(lookup(visitor, reset), 409)

			how.restore(boss, otherID)

			// 她又是管理员了，但上一任留下的链接仍然用不了
			visitor.want(lookup(visitor, invite), 409)
			visitor.want(visitor.do("POST", "/api/account-links/accept", obj{"token": invite, "name": "小号", "password": "takeover-pass-1"}), 409)
			visitor.want(lookup(visitor, reset), 404)
			visitor.want(visitor.do("POST", "/api/account-links/accept", obj{"token": reset, "password": "takeover-pass-1"}), 404)
			boss.want(boss.do("GET", "/api/meetings", nil), 200)
		})
	}
}

// 在服务器上用 zsmeet reset-link 生成的链接，效力来自能登录服务器，和账号的角色无关。
func TestServerIssuedResetLink(t *testing.T) {
	boss := newServer(t)
	boss.setup()
	_, token := boss.invite("other@example.com", "admin")
	other := boss.join(token, "王悦", "other-pass-123")
	otherID := boss.memberID("other@example.com")
	visitor := boss.another()

	link, _, err := boss.svc.RecoverResetLink(t.Context(), "other@example.com")
	if err != nil {
		t.Fatal(err)
	}
	// 成员列表里说明它是在服务器上生成的，而不是写成「王悦 生成」
	list := boss.want(boss.do("GET", "/api/members", nil), 200)
	for _, m := range list.Body["members"].([]any) {
		if m.(obj)["id"] != otherID {
			continue
		}
		reset, _ := m.(obj)["reset_link"].(obj)
		if reset == nil || reset["from_server"] != true || reset["created_by"] != "" {
			t.Errorf("服务器上生成的重置链接应该标出来源：%s", list.Raw)
		}
	}

	// 账号被降级（没有停用）以后，这个链接照样能用
	boss.want(boss.do("PATCH", "/api/members/"+otherID, obj{"role": "member"}), 200)
	visitor.want(lookup(visitor, link), 200)
	visitor.want(visitor.do("POST", "/api/account-links/accept", obj{"token": link, "password": "recovered-pass-1"}), 200)

	// 网页上不能给自己生成重置链接：自己的密码在设置里改
	selfReset := boss.want(boss.do("POST", "/api/members/"+boss.memberID("admin@example.com")+"/reset-link", nil), 422)
	if !strings.Contains(selfReset.Body["detail"].(string), "设置") {
		t.Errorf("应该提示到设置里改自己的密码：%s", selfReset.Raw)
	}
	_ = other
}

// 有人在用重置链接的同时，账号主人正好在改密码：两边不能互相卡死。
// 谁先谁后都可以，另一个得到一个说得清的结果，但不能是服务器内部错误。
func TestResetAndPasswordChangeDoNotDeadlock(t *testing.T) {
	admin := newServer(t)
	admin.setup()
	_, token := admin.invite("lihang@example.com", "member")
	admin.join(token, "李航", "password-round-0")
	memberID := admin.memberID("lihang@example.com")
	current := "password-round-0"

	for round := 1; round <= 8; round++ {
		member := admin.another()
		member.want(member.do("POST", "/api/login", obj{"email": "lihang@example.com", "password": current}), 200)
		link := admin.want(admin.do("POST", "/api/members/"+memberID+"/reset-link", nil), 200).Body["token"].(string)
		changed, reset := fmt.Sprintf("changed-in-round-%d", round), fmt.Sprintf("reset-in-round-%d", round)

		start := make(chan struct{})
		var wg sync.WaitGroup
		var change, accept result
		visitor := admin.another()
		wg.Go(func() {
			<-start
			change = member.do("POST", "/api/me/password", obj{"current_password": current, "new_password": changed})
		})
		wg.Go(func() {
			<-start
			accept = visitor.do("POST", "/api/account-links/accept", obj{"token": link, "password": reset})
		})
		close(start)
		wg.Wait()

		if change.Status >= 500 || accept.Status >= 500 {
			t.Fatalf("第 %d 轮：不应该出现服务器内部错误。改密码 %d：%s；用重置链接 %d：%s", round, change.Status, change.Raw, accept.Status, accept.Raw)
		}
		if change.Status != 204 && accept.Status != 200 {
			t.Fatalf("第 %d 轮：两个操作至少应该有一个成功。改密码 %d：%s；用重置链接 %d：%s", round, change.Status, change.Raw, accept.Status, accept.Raw)
		}
		// 最后生效的是哪个密码，取决于谁后完成；找出来给下一轮用
		current = ""
		for _, candidate := range []string{changed, reset} {
			if r := admin.another().do("POST", "/api/login", obj{"email": "lihang@example.com", "password": candidate}); r.Status == 200 {
				current = candidate
			}
		}
		if current == "" {
			t.Fatalf("第 %d 轮：两个新密码都登录不了。改密码 %d，用重置链接 %d", round, change.Status, accept.Status)
		}
	}
}

// 上传录音的同时在另一个标签页改密码：正在进行的那个上传请求结束时，
// 不能把会话换回改密码之前的那个，否则改完密码当前设备也被退出了。
func TestSlowRequestDoesNotUndoPasswordChange(t *testing.T) {
	admin := newServer(t)
	admin.setup()
	_, token := admin.invite("lihang@example.com", "member")
	here := admin.join(token, "李航", "old-password-123")
	id := here.want(here.do("POST", "/api/meetings", obj{"title": "上传中的会", "source": "upload"}), 201).Body["id"].(string)

	// 一个传得很慢的分片：请求头已经发出（服务器这时已经读了会话），内容还没传完
	body, feed := io.Pipe()
	req, err := http.NewRequest("PUT", here.base+"/api/meetings/"+id+"/chunks/0", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	uploaded := make(chan int, 1)
	go func() {
		resp, err := here.http.Do(req)
		if err != nil {
			uploaded <- 0
			return
		}
		resp.Body.Close()
		uploaded <- resp.StatusCode
	}()
	if _, err := feed.Write([]byte("first half ")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	// 这时在「另一个标签页」改密码
	here.want(here.do("POST", "/api/me/password", obj{"current_password": "old-password-123", "new_password": "new-password-456"}), 204)

	// 上传的请求这才结束
	feed.Write([]byte("second half"))
	feed.Close()
	if status := <-uploaded; status != 204 {
		t.Fatalf("分片应该上传成功，得到 %d", status)
	}

	// 当前设备应该还是登录状态
	here.want(here.do("GET", "/api/meetings", nil), 200)
}

func TestChangeOwnPassword(t *testing.T) {
	admin := newServer(t)
	admin.setup()
	_, token := admin.invite("lihang@example.com", "member")
	here := admin.join(token, "李航", "old-password-123")
	elsewhere := admin.another()
	elsewhere.want(elsewhere.do("POST", "/api/login", obj{"email": "lihang@example.com", "password": "old-password-123"}), 200)

	admin.another().want(admin.another().do("POST", "/api/me/password", obj{"current_password": "x", "new_password": "new-password-456"}), 401)
	wrong := here.want(here.do("POST", "/api/me/password", obj{"current_password": "not-my-password", "new_password": "new-password-456"}), 422)
	if !strings.Contains(wrong.Body["detail"].(string), "当前密码不对") {
		t.Errorf("当前密码填错时应该说明：%s", wrong.Raw)
	}
	here.want(here.do("POST", "/api/me/password", obj{"current_password": "old-password-123", "new_password": "短"}), 422)
	tooLong := here.want(here.do("POST", "/api/me/password", obj{"current_password": "old-password-123", "new_password": strings.Repeat("长", 201)}), 422)
	if !strings.Contains(tooLong.Body["detail"].(string), "密码最长 200 位") {
		t.Errorf("密码太长时应该用中文说明：%s", tooLong.Raw)
	}
	// 填错不应该影响现有的登录
	elsewhere.want(elsewhere.do("GET", "/api/meetings", nil), 200)

	here.want(here.do("POST", "/api/me/password", obj{"current_password": "old-password-123", "new_password": "new-password-456"}), 204)

	// 改密码的这台设备保持登录，别的设备被退出
	here.want(here.do("GET", "/api/meetings", nil), 200)
	elsewhere.want(elsewhere.do("GET", "/api/meetings", nil), 401)
	elsewhere.want(elsewhere.do("POST", "/api/login", obj{"email": "lihang@example.com", "password": "old-password-123"}), 401)
	elsewhere.want(elsewhere.do("POST", "/api/login", obj{"email": "lihang@example.com", "password": "new-password-456"}), 200)
}

// 服务器上的救急命令：唯一的管理员忘了密码，或者没有管理员了。
func TestRecoveryCommands(t *testing.T) {
	admin := newServer(t)
	admin.setup()
	_, token := admin.invite("lihang@example.com", "member")
	member := admin.join(token, "李航", "member-pass-123")
	ctx := t.Context()

	// zsmeet reset-link：不需要任何人登录
	link, _, err := admin.svc.RecoverResetLink(ctx, " Admin@Example.com ")
	if err != nil {
		t.Fatal(err)
	}
	recovered := admin.another()
	recovered.want(recovered.do("POST", "/api/account-links/accept", obj{"token": link, "password": "recovered-pass-789"}), 200)
	recovered.want(recovered.do("GET", "/api/members", nil), 200)
	if _, _, err := admin.svc.RecoverResetLink(ctx, "nobody@example.com"); err == nil {
		t.Error("不存在的邮箱应该报错")
	}

	// zsmeet make-admin：把一个被停用的普通成员变成能用的管理员。
	// 他名下要是还留着重置密码的链接（正常情况下停用时就删了，这里直接改数据库来模拟），启用时一起作废
	stale := recovered.want(recovered.do("POST", "/api/members/"+recovered.memberID("lihang@example.com")+"/reset-link", nil), 200).Body["token"].(string)
	if _, err := admin.pool.Exec(ctx, "UPDATE users SET disabled = true WHERE email = 'lihang@example.com'"); err != nil {
		t.Fatal(err)
	}
	user, err := admin.svc.RecoverAdmin(ctx, "lihang@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if user.Role != "admin" || user.Disabled {
		t.Errorf("应该变成启用状态的管理员：%+v", user)
	}
	visitor := admin.another()
	visitor.want(lookup(visitor, stale), 404)
	// 停用前的登录不会跟着复活；重新登录以后就是管理员了
	member.want(member.do("GET", "/api/members", nil), 401)
	member.want(member.do("POST", "/api/login", obj{"email": "lihang@example.com", "password": "member-pass-123"}), 200)
	member.want(member.do("GET", "/api/members", nil), 200)
	if _, err := admin.svc.RecoverAdmin(ctx, "nobody@example.com"); err == nil {
		t.Error("不存在的邮箱应该报错")
	}
}

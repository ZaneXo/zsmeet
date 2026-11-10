package api

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZaneXo/zsmeet/internal/transcript"
)

// voiced 造一场处理完的会议，两位说话人都有存好的声音特征（相当于声纹打开时转写的）。
func (c *client) voiced(title string) string {
	c.t.Helper()
	id := c.processed(title, []transcript.Segment{
		{Start: 0, End: 2, Speaker: 0, Text: "一期只做网页端。"},
		{Start: 2, End: 4, Speaker: 1, Text: "同意。"},
	}, "## 摘要\n- 说话人 1 提出一期只做网页端，说话人 2 同意。", `[]`)
	for speaker, v := range [][]float32{{1, 0, 0}, {0, 1, 0}} {
		if _, err := c.pool.Exec(c.t.Context(),
			"INSERT INTO speaker_embeddings (meeting_id, speaker, embedding, seconds) VALUES ($1, $2, $3, 12.5)", id, speaker, v); err != nil {
			c.t.Fatal(err)
		}
	}
	return id
}

func (c *client) count(sql string, args ...any) int {
	c.t.Helper()
	var n int
	if err := c.pool.QueryRow(c.t.Context(), sql, args...).Scan(&n); err != nil {
		c.t.Fatal(err)
	}
	return n
}

// voiceprints 列出声纹库，写成「名字:几场会议」，方便比较。
func (c *client) voiceprints() string {
	c.t.Helper()
	r := c.want(c.do("GET", "/api/voiceprints", nil), 200)
	out := ""
	for _, item := range r.Body["items"].([]any) {
		v := item.(map[string]any)
		out += fmt.Sprintf("%v:%v ", v["name"], v["meetings"])
	}
	return out
}

// 声纹的设置、列表、删除都只有管理员能用；默认关着。
func TestVoiceprintSettingsAreAdminOnly(t *testing.T) {
	c := newServer(t)
	c.setup()
	member := c.addMember("member@example.com", "李航")

	member.want(member.do("GET", "/api/settings/voiceprints", nil), 403)
	member.want(member.do("PUT", "/api/settings/voiceprints", obj{"enabled": true}), 403)
	member.want(member.do("GET", "/api/voiceprints", nil), 403)
	member.want(member.do("DELETE", "/api/voiceprints", nil), 403)
	member.want(member.do("DELETE", "/api/voiceprints/00000000-0000-0000-0000-000000000000", nil), 403)

	if r := c.want(c.do("GET", "/api/settings/voiceprints", nil), 200); r.Body["enabled"] != false {
		t.Errorf("声纹默认关着：%s", r.Raw)
	}
	if r := member.want(member.do("GET", "/api/status", nil), 200); r.Body["voiceprints"] != false {
		t.Errorf("状态接口显示关着：%s", r.Raw)
	}
	if r := c.want(c.do("PUT", "/api/settings/voiceprints", obj{"enabled": true}), 200); r.Body["enabled"] != true {
		t.Errorf("打开以后：%s", r.Raw)
	}
	if r := member.want(member.do("GET", "/api/status", nil), 200); r.Body["voiceprints"] != true {
		t.Errorf("成员也能知道声纹打开了（改名的地方据此提示）：%s", r.Raw)
	}
	if r := c.want(c.do("GET", "/api/voiceprints", nil), 200); len(r.Body["items"].([]any)) != 0 {
		t.Errorf("还没有声纹：%s", r.Raw)
	}
}

// 声纹关着时改名不记任何东西；打开以后，改名把声音特征记到这个名字下面，改成别的名字挪过去，去掉名字删掉；
// 名字不分大小写；没有编辑权限的人改不了，也什么都不记。
func TestRenameLearnsVoiceprints(t *testing.T) {
	c := newServer(t)
	c.setup()
	member := c.addMember("member@example.com", "李航")
	first, second := c.voiced("产品周会"), c.voiced("技术评审")
	rename := func(id string, names obj) result {
		t.Helper()
		return c.want(c.do("PUT", "/api/meetings/"+id+"/speakers", obj{"names": names}), 200)
	}
	samples := func() int { return c.count("SELECT count(*) FROM voiceprint_samples") }

	// 关着：照常改名，但不记声纹
	rename(first, obj{"0": "张敏"})
	if n := samples(); n != 0 || c.voiceprints() != "" {
		t.Fatalf("声纹关着时改名不该记声纹：%d 个样本，%s", n, c.voiceprints())
	}

	c.want(c.do("PUT", "/api/settings/voiceprints", obj{"enabled": true}), 200)
	// 没有编辑权限：403，什么都不记
	member.want(member.do("PUT", "/api/meetings/"+first+"/speakers", obj{"names": obj{"1": "王悦"}}), 403)
	if n := samples(); n != 0 {
		t.Fatalf("改不了名的人不能记声纹：%d", n)
	}

	// 打开以后：改名记一个样本；名字没变不重复记
	rename(first, obj{"0": "张敏敏"})
	rename(first, obj{"0": "张敏敏"})
	if got := c.voiceprints(); got != "张敏敏:1 " || samples() != 1 {
		t.Fatalf("改名应该记下一个样本：%s，%d 个样本", got, samples())
	}
	var seconds float64
	if err := c.pool.QueryRow(t.Context(), "SELECT seconds FROM voiceprint_samples").Scan(&seconds); err != nil || seconds != 12.5 {
		t.Errorf("样本记下用了多少秒的说话：%v %v", seconds, err)
	}

	// 改成别的名字：样本挪过去，没有样本的旧名字从声纹库里去掉
	rename(first, obj{"0": "Zhang Min"})
	if got := c.voiceprints(); got != "Zhang Min:1 " || samples() != 1 {
		t.Fatalf("改名后样本应该挪到新名字下面：%s", got)
	}
	// 另一场会议里的同一个人，名字大小写和空格不一样也算同一个人
	rename(second, obj{"1": "  zhang   min "})
	if got := c.voiceprints(); got != "Zhang Min:2 " {
		t.Fatalf("名字不分大小写：%s", got)
	}

	// 去掉名字：删掉样本
	rename(first, obj{"0": ""})
	if got := c.voiceprints(); got != "Zhang Min:1 " || samples() != 1 {
		t.Fatalf("去掉名字应该删掉样本：%s", got)
	}

	// 没有声音特征的说话人（声纹打开之前处理的会议）改名：学不了，照常改名
	old := c.processed("以前的会", []transcript.Segment{{Start: 0, End: 2, Speaker: 0, Text: "好。"}}, "## 摘要\n好。", `[]`)
	if m := rename(old, obj{"0": "王悦"}); fmt.Sprint(m.Body["speakers"]) != "map[0:王悦]" || samples() != 1 {
		t.Errorf("没有声音特征时照常改名、不记样本：%s，%d 个样本", m.Raw, samples())
	}

	// 声纹关掉以后再改名：不记新的，这位说话人以前的样本删掉（名字改了，旧样本记的人就不对了）
	c.want(c.do("PUT", "/api/settings/voiceprints", obj{"enabled": false}), 200)
	rename(second, obj{"1": "李航"})
	if got := c.voiceprints(); got != "" || samples() != 0 {
		t.Errorf("关着时改名只删不记：%s，%d 个样本", got, samples())
	}
}

// 按声音认出的名字：会议接口标出来；有人改了名、或者照原样保存一次（确认）以后，就算人起的名字，也记进声纹库。
func TestAutoNamedSpeakers(t *testing.T) {
	c := newServer(t)
	c.setup()
	c.want(c.do("PUT", "/api/settings/voiceprints", obj{"enabled": true}), 200)
	id := c.voiced("产品周会")
	// 相当于转写完按声音认出了两位说话人（自动认出的不记样本）；编号 5 已经没有名字了，不算
	if _, err := c.pool.Exec(t.Context(), `UPDATE meetings SET speakers = '{"0": "张敏", "1": "王悦"}', auto_speakers = '{0,1,5}' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	m := c.want(c.do("GET", "/api/meetings/"+id, nil), 200)
	if fmt.Sprint(m.Body["auto_speakers"]) != "[0 1]" {
		t.Fatalf("会议接口要标出按声音认出的说话人：%s", m.Raw)
	}

	// 照原样保存张敏：确认了，不再是自动的；记进声纹库；纪要不变
	m = c.want(c.do("PUT", "/api/meetings/"+id+"/speakers", obj{"names": obj{"0": "张敏"}}), 200)
	if fmt.Sprint(m.Body["auto_speakers"]) != "[1]" || m.Body["minutes_version"] != float64(1) {
		t.Errorf("确认以后不再是自动的、纪要不变：%s", m.Raw)
	}
	// 把认错的王悦改成李航：记在李航名下
	m = c.want(c.do("PUT", "/api/meetings/"+id+"/speakers", obj{"names": obj{"1": "李航"}}), 200)
	if fmt.Sprint(m.Body["auto_speakers"]) != "[]" {
		t.Errorf("改过名以后不再是自动的：%s", m.Raw)
	}
	if got := c.voiceprints(); got != "李航:1 张敏:1 " {
		t.Errorf("确认和改正的名字都要记进声纹库：%s", got)
	}
}

// 删除一个人的声纹、删除全部声纹（连同每场会议里存的声音特征）。会议里的名字不变。
func TestDeleteVoiceprints(t *testing.T) {
	c := newServer(t)
	c.setup()
	c.want(c.do("PUT", "/api/settings/voiceprints", obj{"enabled": true}), 200)
	id := c.voiced("产品周会")
	c.want(c.do("PUT", "/api/meetings/"+id+"/speakers", obj{"names": obj{"0": "张敏", "1": "王悦"}}), 200)

	list := c.want(c.do("GET", "/api/voiceprints", nil), 200).Body["items"].([]any)
	if len(list) != 2 {
		t.Fatalf("应该有两个人：%v", list)
	}
	first := list[0].(map[string]any)
	if first["meetings"] != float64(1) || first["seconds"] != 12.5 || first["updated_at"] == nil {
		t.Errorf("列表里有几场会议、多少秒、什么时候更新的：%v", first)
	}
	c.want(c.do("DELETE", "/api/voiceprints/"+first["id"].(string), nil), 204)
	c.want(c.do("DELETE", "/api/voiceprints/"+first["id"].(string), nil), 404)
	c.want(c.do("DELETE", "/api/voiceprints/不是编号", nil), 422)
	if got := c.voiceprints(); got != fmt.Sprintf("%v:1 ", list[1].(map[string]any)["name"]) {
		t.Errorf("删掉一个还剩一个：%s", got)
	}

	c.want(c.do("DELETE", "/api/voiceprints", nil), 204)
	if got := c.voiceprints(); got != "" {
		t.Errorf("全部删掉：%s", got)
	}
	if n := c.count("SELECT count(*) FROM speaker_embeddings") + c.count("SELECT count(*) FROM voiceprint_samples"); n != 0 {
		t.Errorf("服务器上不能再留声音特征，还有 %d 条", n)
	}
	m := c.want(c.do("GET", "/api/meetings/"+id, nil), 200)
	if fmt.Sprint(m.Body["speakers"]) != "map[0:张敏 1:王悦]" {
		t.Errorf("会议里的名字不变：%s", m.Raw)
	}
	// 删了以后改名学不了（没有声音特征了），照常改名
	c.want(c.do("PUT", "/api/meetings/"+id+"/speakers", obj{"names": obj{"0": "李航"}}), 200)
	if got := c.voiceprints(); got != "" {
		t.Errorf("没有声音特征了就学不了：%s", got)
	}
}

// 改名后只清理这次动过的声纹：样本挪走、变空的旧名字删掉；别的没有样本的声纹（比如另一个事务刚建好、
// 还没来得及加样本的）不是这次的清理对象，留着。
func TestVoiceprintCleanupOnlyTouchesItsOwn(t *testing.T) {
	c := newServer(t)
	c.setup()
	c.want(c.do("PUT", "/api/settings/voiceprints", obj{"enabled": true}), 200)
	id := c.voiced("产品周会")
	if _, err := c.pool.Exec(t.Context(), "INSERT INTO voiceprints (name) VALUES ('赵磊')"); err != nil {
		t.Fatal(err)
	}
	names := func() string {
		t.Helper()
		rows, err := c.pool.Query(t.Context(), "SELECT name FROM voiceprints ORDER BY name")
		if err != nil {
			t.Fatal(err)
		}
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(got, ",")
	}

	c.want(c.do("PUT", "/api/meetings/"+id+"/speakers", obj{"names": obj{"0": "张敏"}}), 200)
	if got := names(); got != "张敏,赵磊" {
		t.Fatalf("记下张敏，赵磊不动：%s", got)
	}
	// 样本从张敏挪到王悦：张敏空了删掉；赵磊这次没动过，不删
	c.want(c.do("PUT", "/api/meetings/"+id+"/speakers", obj{"names": obj{"0": "王悦"}}), 200)
	if got := names(); got != "王悦,赵磊" {
		t.Errorf("只删这次动过、变空的声纹：%s", got)
	}
	// 去掉名字：王悦的样本删掉，王悦空了删掉
	c.want(c.do("PUT", "/api/meetings/"+id+"/speakers", obj{"names": obj{"0": ""}}), 200)
	if got := names(); got != "赵磊" {
		t.Errorf("去掉名字后只删王悦：%s", got)
	}
}

// 改声纹库的操作（改名记样本、删一个、全部删掉）都要先拿声纹库的锁：别的事务拿着锁时它们等着，放开以后才做。
func TestVoiceprintMutationsWaitForLibraryLock(t *testing.T) {
	c := newServer(t)
	c.setup()
	c.want(c.do("PUT", "/api/settings/voiceprints", obj{"enabled": true}), 200)
	id := c.voiced("产品周会")
	c.want(c.do("PUT", "/api/meetings/"+id+"/speakers", obj{"names": obj{"0": "张敏", "1": "王悦"}}), 200)
	var zhang string
	if err := c.pool.QueryRow(t.Context(), "SELECT id FROM voiceprints WHERE name = '张敏'").Scan(&zhang); err != nil {
		t.Fatal(err)
	}

	for _, op := range []struct {
		name, method, path string
		body               any
		status             int
	}{
		{"改名记样本", "PUT", "/api/meetings/" + id + "/speakers", obj{"names": obj{"1": "李航"}}, 200},
		{"删一个", "DELETE", "/api/voiceprints/" + zhang, nil, 204},
		{"全部删掉", "DELETE", "/api/voiceprints", nil, 204},
	} {
		t.Run(op.name, func(t *testing.T) {
			tx, err := c.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			// 声纹库的锁（service.voiceprintsLock）
			if _, err := tx.Exec(t.Context(), "SELECT pg_advisory_xact_lock(7368824)"); err != nil {
				t.Fatal(err)
			}
			done := make(chan result, 1)
			go func() { done <- c.do(op.method, op.path, op.body) }()

			// 等到这个请求在锁上排队
			deadline := time.Now().Add(5 * time.Second)
			for {
				var waiting int
				if err := c.pool.QueryRow(t.Context(),
					"SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND objid = 7368824 AND NOT granted").Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting > 0 {
					break
				}
				select {
				case r := <-done:
					t.Fatalf("锁被别人拿着时不该做完：%d %s", r.Status, r.Raw)
				default:
				}
				if time.Now().After(deadline) {
					t.Fatal("等了 5 秒也没在声纹库的锁上排队")
				}
				time.Sleep(20 * time.Millisecond)
			}
			if err := tx.Rollback(t.Context()); err != nil {
				t.Fatal(err)
			}
			select {
			case r := <-done:
				c.want(r, op.status)
			case <-time.After(5 * time.Second):
				t.Fatal("锁放开以后应该做完")
			}
		})
	}
}

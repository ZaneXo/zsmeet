package api

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ZaneXo/zsmeet/internal/audio/audiotest"
)

// 桌面版录音的上传，请求和 App 发的一样：带 App 令牌，分片是录音程序按帧切好的 AAC（ADTS），
// Content-Type 是 audio/aac。边录边传，每标记一次重点就整份传一次，结束后带上全部标记「上传完成」。
// App 靠 409 的说明区分「已经在处理了」（当成传完）和「缺片」（要补传），这两句话也要守住。
func TestDesktopRecordingUpload(t *testing.T) {
	c := newServer(t)
	c.setup()
	app := c.addMember("lihang@example.com", "李航").appLogin("李航的 MacBook")

	stream := audiotest.Tones(t, 8, 440, 660, 880)
	chunks := audiotest.Split(t, stream, 10)
	if len(chunks) != 3 {
		t.Fatalf("24 秒的录音应该切成 3 片，得到 %d 片", len(chunks))
	}
	aac := func(seq int) typed { return typed{"audio/aac", chunks[seq]} }

	m := app.want(app.do("POST", "/api/meetings", obj{"title": "客户电话", "source": "desktop", "template": "general", "visibility": "private"}), 201)
	if m.Body["source"] != "desktop" || m.Body["status"] != "uploading" || m.Body["visibility"] != "private" {
		t.Fatalf("要能建只有自己能看的桌面版录音：%s", m.Raw)
	}
	id := m.Body["id"].(string)
	base := "/api/meetings/" + id
	put := func(seq int) {
		t.Helper()
		app.want(app.do("PUT", fmt.Sprintf("%s/chunks/%d", base, seq), aac(seq)), 204)
	}

	put(0)
	if got := app.want(app.do("GET", base, nil), 200); got.Body["last_chunk_at"] == nil {
		t.Errorf("收到分片要记下时间，网页据此显示「电脑录音中」：%s", got.Raw)
	}
	app.want(app.do("PUT", base+"/marks", obj{"marks": []float64{4.5}}), 204)
	put(1)
	put(1) // 断网后重传同一片
	app.want(app.do("PUT", base+"/marks", obj{"marks": []float64{4.5, 12.5}}), 204)
	put(2)
	if got := app.want(app.do("GET", base, nil), 200); fmt.Sprint(got.Body["marks"]) != "[4.5 12.5]" {
		t.Errorf("录音过程中的标记要存下来：%s", got.Raw)
	}
	if got := app.want(app.do("GET", base+"/chunks", nil), 200); fmt.Sprint(got.Body["received"]) != "[0 1 2]" {
		t.Errorf("应该收到第 0、1、2 片：%s", got.Raw)
	}

	// 存下的分片和传上去的一字不差，按序号接起来就是录音程序写的那一整段
	var stored []byte
	for seq := range chunks {
		data, err := os.ReadFile(c.store.ChunkPath(uuid.MustParse(id), seq))
		if err != nil {
			t.Fatal(err)
		}
		stored = append(stored, data...)
	}
	if !bytes.Equal(stored, stream) {
		t.Error("分片按序号接起来应该就是原来的录音")
	}

	done := app.want(app.do("POST", base+"/complete", obj{"chunks": len(chunks), "marks": []float64{4.5, 12.5, 20}}), 200)
	if done.Body["status"] != "queued" || fmt.Sprint(done.Body["marks"]) != "[4.5 12.5 20]" {
		t.Fatalf("传完应该带着全部标记进入排队：%s", done.Raw)
	}
	var jobs int
	if err := c.pool.QueryRow(t.Context(),
		"SELECT count(*) FROM river_job WHERE kind = 'process_meeting' AND args->>'meeting_id' = $1", id).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 {
		t.Errorf("应该正好有一个后台任务，实际 %d 个", jobs)
	}

	// App 不知道上次「上传完成」成没成功（比如发出去以后断网、App 被关掉）会再调一次：409 说「已经在处理了」，App 当成传完了
	again := app.want(app.do("POST", base+"/complete", obj{"chunks": len(chunks), "marks": []float64{4.5, 12.5, 20}}), 409)
	if detail, _ := again.Body["detail"].(string); !strings.Contains(detail, "已经在处理了") {
		t.Errorf("重复「上传完成」要说已经在处理了：%s", again.Raw)
	}
	// 交给后台以后不能再传分片、改标记
	app.want(app.do("PUT", base+"/chunks/3", aac(2)), 409)
	app.want(app.do("PUT", base+"/marks", obj{"marks": []float64{1}}), 409)

	// 缺片也是 409，但说的是缺哪一片，App 不能当成传完
	other := app.want(app.do("POST", "/api/meetings", obj{"title": "周会", "source": "desktop"}), 201)
	otherBase := "/api/meetings/" + other.Body["id"].(string)
	app.want(app.do("PUT", otherBase+"/chunks/0", aac(0)), 204)
	app.want(app.do("PUT", otherBase+"/chunks/2", aac(2)), 204)
	missing := app.want(app.do("POST", otherBase+"/complete", obj{"chunks": 3}), 409)
	if detail, _ := missing.Body["detail"].(string); !strings.Contains(detail, "第 2 片") || strings.Contains(detail, "已经在处理了") {
		t.Errorf("缺片时要说缺哪一片：%s", missing.Raw)
	}
	// 录着录着会议在网页上被删了：分片传不上去，返回 404，App 不再重试
	app.want(app.do("DELETE", otherBase, nil), 204)
	app.want(app.do("PUT", otherBase+"/chunks/1", aac(1)), 404)
}

// 录音的电脑找不回来了（丢了、坏了）：桌面版录的会议也能在会议页面上用服务器已经收到的分片生成纪要。
// 会议页面和网页录音一样收尾：先看收到了哪几片，再带着从第 0 片起连续的片数「上传完成」，不带标记
// （录音时存下的标记照样用）。只有创建者和管理员能收尾，别人只能看收到了哪几片。
// 收尾以后那台电脑上的 App 再传分片得到 409，App 把这段录音当成传不上去；已经传完的 App 补交「上传完成」
// 也是 409，App 看一眼会议已经不在等上传，就当它传完了。
func TestFinishDesktopRecordingFromReceivedChunks(t *testing.T) {
	c := newServer(t)
	c.setup()
	owner := c.addMember("lihang@example.com", "李航")
	colleague := c.addMember("wangyue@example.com", "王悦")
	app := owner.appLogin("李航的 MacBook")
	// 这里只看收尾的规则，分片的内容不要紧
	chunk := func(seq int) typed { return typed{"audio/aac", fmt.Appendf(nil, "第 %d 片", seq)} }
	jobs := func(id string) int {
		t.Helper()
		var n int
		if err := c.pool.QueryRow(t.Context(),
			"SELECT count(*) FROM river_job WHERE kind = 'process_meeting' AND args->>'meeting_id' = $1", id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// 李航用桌面版录：边录边传了两片、标记了一个重点，之后那台电脑就再没传上来
	id := app.want(app.do("POST", "/api/meetings", obj{"title": "客户电话", "source": "desktop"}), 201).Body["id"].(string)
	base := "/api/meetings/" + id
	app.want(app.do("PUT", base+"/chunks/0", chunk(0)), 204)
	app.want(app.do("PUT", base+"/chunks/1", chunk(1)), 204)
	app.want(app.do("PUT", base+"/marks", obj{"marks": []float64{4.5}}), 204)

	// 同事看得到收到了哪几片，但不能替他收尾
	if got := colleague.want(colleague.do("GET", base, nil), 200); got.Body["can_manage"] != false {
		t.Errorf("同事不能收尾，会议页面上不给他这个按钮：%s", got.Raw)
	}
	if got := colleague.want(colleague.do("GET", base+"/chunks", nil), 200); fmt.Sprint(got.Body["received"]) != "[0 1]" {
		t.Errorf("同事也能看收到了哪几片：%s", got.Raw)
	}
	colleague.want(colleague.do("POST", base+"/complete", obj{"chunks": 2}), 403)

	// 李航在网页上（用网页的登录，不是 App 的令牌）：收到了两片，用这两片生成纪要
	if got := owner.want(owner.do("GET", base, nil), 200); got.Body["can_manage"] != true || got.Body["status"] != "uploading" {
		t.Fatalf("创建者在会议页面上能收尾：%s", got.Raw)
	}
	if got := owner.want(owner.do("GET", base+"/chunks", nil), 200); fmt.Sprint(got.Body["received"]) != "[0 1]" {
		t.Fatalf("应该收到第 0、1 片：%s", got.Raw)
	}
	// 页面上的片数是之前看到的：那以后录音的电脑又传上来了的话（这里只看到了第 0 片），不按旧的片数处理，提示刷新
	stale := owner.want(owner.do("POST", base+"/complete", obj{"chunks": 1}), 409)
	if detail, _ := stale.Body["detail"].(string); !strings.Contains(detail, "又收到了") || !strings.Contains(detail, "刷新页面") {
		t.Errorf("要说服务器又收到了录音、刷新页面再试：%s", stale.Raw)
	}
	done := owner.want(owner.do("POST", base+"/complete", obj{"chunks": 2}), 200)
	if done.Body["status"] != "queued" || fmt.Sprint(done.Body["marks"]) != "[4.5]" {
		t.Fatalf("应该开始处理，录音时存下的标记也要留着：%s", done.Raw)
	}
	if n := jobs(id); n != 1 {
		t.Errorf("应该正好有一个后台任务，实际 %d 个", n)
	}

	// 那台电脑上的 App 又连上了：剩下的分片、标记都传不上去（409，App 不再重试，显示这段录音传不上去）。
	// App 把服务器说的原因显示在这段录音上，要说清楚是会议已经开始处理了，不是 App 出了错
	late := app.want(app.do("PUT", base+"/chunks/2", chunk(2)), 409)
	if detail, _ := late.Body["detail"].(string); !strings.Contains(detail, "开始处理了") || !strings.Contains(detail, "传不上去") {
		t.Errorf("要说会议已经开始处理了、剩下的录音传不上去：%s", late.Raw)
	}
	app.want(app.do("PUT", base+"/marks", obj{"marks": []float64{4.5, 25}}), 409)
	// 已经全传上来了的 App 补交「上传完成」也是 409，什么也不改；App 看一眼会议，已经不在等上传，就当它传完了
	again := app.want(app.do("POST", base+"/complete", obj{"chunks": 2, "marks": []float64{4.5, 25}}), 409)
	if detail, _ := again.Body["detail"].(string); !strings.Contains(detail, "已经在处理了") {
		t.Errorf("要说已经在处理了：%s", again.Raw)
	}
	if got := app.want(app.do("GET", base, nil), 200); got.Body["status"] == "uploading" || fmt.Sprint(got.Body["marks"]) != "[4.5]" {
		t.Errorf("会议已经不在等上传，标记也不该被改：%s", got.Raw)
	}
	// 后来传的那一片没有收下，服务器上还是收尾时的那两片（这里没起后台；后台拼好整段录音以后会删掉分片）
	if got := app.want(app.do("GET", base+"/chunks", nil), 200); fmt.Sprint(got.Body["received"]) != "[0 1]" {
		t.Errorf("收尾以后不该再收下分片：%s", got.Raw)
	}
	if n := jobs(id); n != 1 {
		t.Errorf("不该再排一次后台任务，实际 %d 个", n)
	}

	// 管理员也能替创建者收尾，「只有自己能看」的会议也一样
	private := app.want(app.do("POST", "/api/meetings", obj{"title": "一对一", "source": "desktop", "visibility": "private"}), 201).Body["id"].(string)
	privateBase := "/api/meetings/" + private
	for seq := range 3 {
		app.want(app.do("PUT", fmt.Sprintf("%s/chunks/%d", privateBase, seq), chunk(seq)), 204)
	}
	colleague.want(colleague.do("GET", privateBase+"/chunks", nil), 404)
	if got := c.want(c.do("GET", privateBase, nil), 200); got.Body["can_manage"] != true {
		t.Fatalf("管理员能收尾别人的会议：%s", got.Raw)
	}
	if got := c.want(c.do("GET", privateBase+"/chunks", nil), 200); fmt.Sprint(got.Body["received"]) != "[0 1 2]" {
		t.Fatalf("应该收到第 0、1、2 片：%s", got.Raw)
	}
	if got := c.want(c.do("POST", privateBase+"/complete", obj{"chunks": 3}), 200); got.Body["status"] != "queued" {
		t.Fatalf("管理员收尾以后应该开始处理：%s", got.Raw)
	}
	if n := jobs(private); n != 1 {
		t.Errorf("应该正好有一个后台任务，实际 %d 个", n)
	}
}

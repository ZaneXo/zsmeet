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

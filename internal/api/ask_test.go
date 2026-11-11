package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ZaneXo/zsmeet/internal/service"
	"github.com/ZaneXo/zsmeet/internal/transcript"
)

// fakeLLM 是假的 OpenAI 兼容接口：第一次调用（提取搜索词）回 plan，第二次调用（回答）回 answer。
// 记下每次请求的全部内容，用来检查看不到的会议有没有漏进提示词。
type fakeLLM struct {
	mu      sync.Mutex
	plan    string
	answer  string
	fail    bool
	prompts []string // 每次请求的 system + user
	planned int      // 提取搜索词的调用次数
	asked   int      // 回答的调用次数
}

func (f *fakeLLM) set(plan, answer string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plan, f.answer = plan, answer
}

// last 返回最后一次请求的内容。
func (f *fakeLLM) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.prompts) == 0 {
		return ""
	}
	return f.prompts[len(f.prompts)-1]
}

func (f *fakeLLM) counts() (planned, asked int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.planned, f.asked
}

// useFakeLLM 起一个假的大模型接口并保存为团队配置。
func (c *client) useFakeLLM() *fakeLLM {
	c.t.Helper()
	f := &fakeLLM{plan: `{"keywords": ["报价"], "from": "", "to": ""}`, answer: "没有找到。"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var parts []string
		for _, m := range req.Messages {
			parts = append(parts, m.Content)
		}
		prompt := strings.Join(parts, "\n")
		f.mu.Lock()
		f.prompts = append(f.prompts, prompt)
		reply := f.answer
		if strings.Contains(prompt, "搜索用的关键词") {
			reply = f.plan
			f.planned++
		} else {
			f.asked++
		}
		fail := f.fail
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if fail {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"message":"Incorrect API key provided","type":"invalid_request_error"}}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"id": "x", "object": "chat.completion", "model": "test",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": reply},
			}},
		})
	}))
	c.t.Cleanup(server.Close)
	if _, err := c.svc.SaveLLM(c.t.Context(), service.LLMInput{BaseURL: server.URL + "/v1", Model: "test", APIKey: "sk-test"}); err != nil {
		c.t.Fatal(err)
	}
	return f
}

func (c *client) ask(body obj) result {
	c.t.Helper()
	return c.do("POST", "/api/ask", body)
}

// 在所有会议里问：只用提问的人能看的会议，别人的私密会议不进提示词、不进出处；编造的出处去掉；
// 时间范围按会议时间筛。
func TestAskAcrossMeetings(t *testing.T) {
	c := newServer(t)
	c.setup()
	owner := c.addMember("owner@example.com", "李航")
	outsider := c.addMember("outsider@example.com", "赵雷")
	llm := c.useFakeLLM()

	team := owner.processed("客户报价评审", []transcript.Segment{
		{Start: 60, End: 64, Speaker: 0, Text: "先过一下排期。"},
		{Start: 70.5, End: 74, Speaker: 1, Text: "报价定为每年十二万。"},
		{Start: 75, End: 78, Speaker: 0, Text: "好，就这么定。"},
		{Start: 300, End: 303, Speaker: 0, Text: "散会。"},
	}, "## 结论\n- [01:10] 报价定为每年十二万，李航负责发给客户。", `[]`)
	if _, err := c.pool.Exec(t.Context(), `UPDATE meetings SET speakers = '{"1": "王悦"}' WHERE id = $1`, team); err != nil {
		t.Fatal(err)
	}
	private := owner.processed("私下沟通", []transcript.Segment{
		{Start: 5, End: 8, Speaker: 0, Text: "报价的底线是八万，别告诉客户。"},
	}, "## 摘要\n- [00:05] 报价底线八万。", `[]`)
	owner.want(owner.do("PUT", "/api/meetings/"+private+"/access", obj{"visibility": "private"}), 200)
	// 和问题无关的会议不进材料
	c.processed("团建安排", []transcript.Segment{{Start: 1, End: 2, Text: "周五去爬山。"}}, "## 摘要\n去爬山。", `[]`)

	// 赵雷只能看到团队的那场：材料里只有它，[M1-…] 是它，编造的 [M2-1] 去掉
	llm.set(`好的 {"keywords": ["报价", "定价"], "from": "", "to": ""}`, "报价定为每年十二万 [M1-3][M1-2]，由李航发给客户 [M1-1]。底线是八万 [M2-1]。")
	r := outsider.want(outsider.ask(obj{"question": "  关于报价定了什么？ "}), 200)
	prompt := llm.last()
	if strings.Contains(prompt, "八万") || strings.Contains(prompt, "私下沟通") || strings.Contains(prompt, "爬山") {
		t.Errorf("看不到的会议、不相关的会议不能进提示词：\n%s", prompt)
	}
	for _, want := range []string{"问题：关于报价定了什么？", "## 会议 M1：客户报价评审", "[M1-1] 纪要（01:10）：报价定为每年十二万，李航负责发给客户。", "[M1-3] 01:10 王悦：报价定为每年十二万。", "[M1-4] 01:15 说话人 1：好，就这么定。"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("提示词里应该有 %q：\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "散会") {
		t.Errorf("只带命中句子前后各一句：\n%s", prompt)
	}
	if r.Body["answer"] != "报价定为每年十二万[1][2]，由李航发给客户[3]。底线是八万。" {
		t.Errorf("编号换成序号、编造的去掉：%s", r.Raw)
	}
	cites := r.Body["citations"].([]any)
	if len(cites) != 3 {
		t.Fatalf("三处出处：%s", r.Raw)
	}
	first := cites[0].(map[string]any)
	if first["n"] != float64(1) || first["meeting_id"] != team || first["title"] != "客户报价评审" || first["at"] != 70.5 ||
		first["source"] != "transcript" || first["quote"] != "报价定为每年十二万。" || first["date"] == "" {
		t.Errorf("第一处出处是 70.5 秒那句：%v", first)
	}
	if third := cites[2].(map[string]any); third["source"] != "minutes" || third["at"] != float64(70) {
		t.Errorf("第三处出处是纪要那一行（时间点 01:10）：%v", third)
	}
	if r.Body["meetings_searched"] != float64(1) || fmt.Sprint(r.Body["keywords"]) != "[报价 定价]" || r.Body["from"] != nil {
		t.Errorf("材料来自一场会议，搜了两个词，没有时间范围：%s", r.Raw)
	}

	// 管理员什么都能看：两场都进材料
	llm.set(`{"keywords": ["报价"]}`, "两场会议都谈了报价 [M1-1][M2-1]。")
	r = c.want(c.ask(obj{"question": "报价是多少？"}), 200)
	if prompt := llm.last(); !strings.Contains(prompt, "私下沟通") || !strings.Contains(prompt, "客户报价评审") {
		t.Errorf("管理员能看的会议都要进材料：\n%s", prompt)
	}
	if r.Body["meetings_searched"] != float64(2) || len(r.Body["citations"].([]any)) != 2 {
		t.Errorf("材料来自两场会议：%s", r.Raw)
	}

	// 限定了时间：会议时间不在范围里的不要；什么都没找到时不用再问大模型
	if _, err := c.pool.Exec(t.Context(), "UPDATE meetings SET created_at = now() - interval '60 days' WHERE id = $1", team); err != nil {
		t.Fatal(err)
	}
	_, asked := llm.counts()
	llm.set(`{"keywords": ["报价"], "from": "2000-01-01", "to": "2000-01-07"}`, "不该被调用")
	r = outsider.want(outsider.ask(obj{"question": "上周关于报价定了什么？"}), 200)
	if _, now := llm.counts(); now != asked || !strings.Contains(r.Body["answer"].(string), "没有找到") || len(r.Body["citations"].([]any)) != 0 || r.Body["meetings_searched"] != float64(0) {
		t.Errorf("范围里没有会议时直接说没找到：%s", r.Raw)
	}
	if r.Body["from"] != "2000-01-01" || r.Body["to"] != "2000-01-07" {
		t.Errorf("返回问题限定的日期范围：%s", r.Raw)
	}

	// 只限定了时间、没有搜索词：列出这段时间里能看的会议
	llm.set(fmt.Sprintf(`{"keywords": [], "from": "%s", "to": ""}`, time.Now().AddDate(0, -6, 0).Format(time.DateOnly)), "开了一场 [M1-1]。")
	r = outsider.want(outsider.ask(obj{"question": "最近开了哪些会？"}), 200)
	if prompt := llm.last(); strings.Contains(prompt, "私下沟通") || !strings.Contains(prompt, "客户报价评审") || !strings.Contains(prompt, "团建安排") {
		t.Errorf("按时间列出能看的会议：\n%s", prompt)
	}

	// 大模型没按格式给搜索词：从问题里拆词去搜（问题就是一个词时就搜它）
	llm.set("我不知道", "报价是十二万 [M1-2]。")
	r = outsider.want(outsider.ask(obj{"question": "报价"}), 200)
	if fmt.Sprint(r.Body["keywords"]) != "[报价]" || len(r.Body["citations"].([]any)) != 1 {
		t.Errorf("搜索词解析不出来时从问题里拆词：%s", r.Raw)
	}

	// 长问题、搜索词解析不出来：从问题里拆出几个短词去搜，不拿整句话（模糊搜索几乎匹配不上）
	question := "上周关于客户报价定了什么？" + strings.Repeat("麻烦把每个人的意见都列出来，", 20)
	if n := len([]rune(question)); n < 250 || n > 300 {
		t.Fatalf("问题要接近 300 个字：%d", n)
	}
	llm.set("抱歉，我没法给出 {关键词}。", "报价是十二万 [M1-1]。")
	r = outsider.want(outsider.ask(obj{"question": question}), 200)
	keywords := r.Body["keywords"].([]any)
	if len(keywords) == 0 || len(keywords) > 5 || keywords[0] != "客户报价" {
		t.Errorf("从问题里拆出不超过 5 个短词：%s", r.Raw)
	}
	for _, k := range keywords {
		if n := len([]rune(k.(string))); n > 100 {
			t.Errorf("搜索词不能超过搜索框的长度限制：%d 个字", n)
		}
	}
	if r.Body["meetings_searched"] != float64(1) || len(r.Body["citations"].([]any)) != 1 || !strings.Contains(llm.last(), "## 会议 M1：客户报价评审") {
		t.Errorf("按问题里的词找到客户报价评审：%s", r.Raw)
	}
}

// 只问一场会议：看不到的当作不存在；整场放得下时直接交给大模型，不用先提取搜索词。
func TestAskOneMeeting(t *testing.T) {
	c := newServer(t)
	c.setup()
	owner := c.addMember("owner@example.com", "李航")
	outsider := c.addMember("outsider@example.com", "赵雷")
	llm := c.useFakeLLM()

	m := owner.processed("产品周会", []transcript.Segment{
		{Start: 1, End: 3, Speaker: 0, Text: "今天说一下排期。"},
		{Start: 12.5, End: 15, Speaker: 1, Text: "网页录音下周上线。"},
	}, "## 结论\n- [00:12] 网页录音下周上线。", `[]`)
	private := owner.processed("一对一", []transcript.Segment{{Start: 1, End: 2, Text: "绩效的事。"}}, "## 摘要\n绩效。", `[]`)
	owner.want(owner.do("PUT", "/api/meetings/"+private+"/access", obj{"visibility": "private"}), 200)

	planned, asked := llm.counts()
	for _, id := range []string{private, uuid.NewString(), "不是 ID"} {
		outsider.want(outsider.ask(obj{"question": "说了什么？", "meeting_id": id}), 404)
	}
	if p, a := llm.counts(); p != planned || a != asked {
		t.Error("看不到的会议不能调用大模型")
	}

	llm.set("不该被调用", "网页录音下周上线 [M1-2]，[M1-9] 是编的。")
	r := outsider.want(outsider.ask(obj{"question": "网页录音什么时候上线？", "meeting_id": m}), 200)
	if p, _ := llm.counts(); p != planned {
		t.Error("整场放得下时不用先提取搜索词")
	}
	prompt := llm.last()
	if !strings.Contains(prompt, "[M1-1] 纪要·结论（00:12）：网页录音下周上线。") || !strings.Contains(prompt, "[M1-2] 00:01 说话人 1：今天说一下排期。") || strings.Contains(prompt, "绩效") {
		t.Errorf("材料是这场会议的整篇纪要和整场转写：\n%s", prompt)
	}
	cites := r.Body["citations"].([]any)
	if r.Body["answer"] != "网页录音下周上线[1]，是编的。" || len(cites) != 1 || cites[0].(map[string]any)["at"] != float64(1) || cites[0].(map[string]any)["meeting_id"] != m {
		t.Errorf("出处是这场会议 1 秒那句：%s", r.Raw)
	}
	if len(r.Body["keywords"].([]any)) != 0 || r.Body["meetings_searched"] != float64(1) {
		t.Errorf("整场交给大模型时没有搜索词：%s", r.Raw)
	}

	// 整场放不下：先提取搜索词，只放纪要和命中的句子前后几句
	segs := make([]transcript.Segment, 0, 600)
	for i := range 600 {
		segs = append(segs, transcript.Segment{Start: float64(i * 10), End: float64(i*10 + 5), Text: fmt.Sprintf("第 %d 句，闲聊一些别的事情，凑够字数。", i)})
	}
	segs[300].Text = "合同的报价定为十二万。"
	long := owner.processed("长会", segs, "## 摘要\n聊了很多。", `[]`)
	llm.set(`{"keywords": ["报价"]}`, "报价十二万 [M1-4]。")
	r = outsider.want(outsider.ask(obj{"question": "报价多少？", "meeting_id": long}), 200)
	prompt = llm.last()
	if !strings.Contains(prompt, "合同的报价定为十二万") || !strings.Contains(prompt, "第 298 句") || strings.Contains(prompt, "第 290 句") || len([]rune(prompt)) > 14_000 {
		t.Errorf("放不下时只带命中的句子前后两句：\n%s", prompt)
	}
	if cites := r.Body["citations"].([]any); len(cites) != 1 || cites[0].(map[string]any)["at"] != float64(3000) {
		t.Errorf("出处是 3000 秒那句：%s", r.Raw)
	}

	// 还没有转写和纪要的会议
	empty := owner.want(owner.do("POST", "/api/meetings", obj{"title": "空的", "source": "upload"}), 201).Body["id"].(string)
	owner.want(owner.ask(obj{"question": "说了什么？", "meeting_id": empty}), 409)
}

// 参数检查、没配置大模型、限流、大模型出错。
func TestAskValidationAndLimits(t *testing.T) {
	c := newServer(t)
	c.setup()
	member := c.addMember("member@example.com", "李航")
	c.another().want(c.another().ask(obj{"question": "报价？"}), 401)

	r := member.want(member.ask(obj{"question": "报价？"}), 409)
	if !strings.Contains(r.Raw, "还没有配置大模型") {
		t.Errorf("没配置大模型要说清楚：%s", r.Raw)
	}

	llm := c.useFakeLLM()
	member.processed("报价评审", []transcript.Segment{{Start: 1, End: 2, Text: "报价十二万。"}}, "## 摘要\n报价。", `[]`)
	if r := member.want(member.ask(obj{"question": "   "}), 422); !strings.Contains(r.Raw, "请输入要问的问题") {
		t.Errorf("空的问题：%s", r.Raw)
	}
	if r := member.want(member.ask(obj{"question": strings.Repeat("问", 301)}), 422); !strings.Contains(r.Raw, "最多 300 个字") {
		t.Errorf("太长的问题：%s", r.Raw)
	}
	member.want(member.ask(obj{"question": strings.Repeat("问", 300)}), 200)

	// 大模型出错：成员看到让他稍后再试、找管理员，管理员看到原因
	llm.mu.Lock()
	llm.fail = true
	llm.mu.Unlock()
	r = member.want(member.ask(obj{"question": "报价？"}), 502)
	if !strings.Contains(r.Raw, "请管理员到「设置」里测试") || strings.Contains(r.Raw, "Incorrect API key") {
		t.Errorf("成员看到的是能照着做的话：%s", r.Raw)
	}
	if r := c.want(c.ask(obj{"question": "报价？"}), 502); !strings.Contains(r.Raw, "Incorrect API key") {
		t.Errorf("管理员看到大模型返回的原因：%s", r.Raw)
	}
	llm.mu.Lock()
	llm.fail = false
	llm.mu.Unlock()

	// 每人每分钟最多 6 次（上面成员已经问了 2 次，参数不对的不算），别人不受影响
	for range 4 {
		member.want(member.ask(obj{"question": "报价？"}), 200)
	}
	r = member.want(member.ask(obj{"question": "报价？"}), 429)
	if !strings.Contains(r.Raw, "提问太频繁了") || !strings.Contains(r.Raw, "秒后再试") {
		t.Errorf("限流要说清楚等多久：%s", r.Raw)
	}
	c.want(c.ask(obj{"question": "报价？"}), 200)
}

// 问会议的材料有多少字跟着上下文长度走：同一场会，32K 时整场交给大模型，8K 时放不下，先提取搜索词、只带命中的片段。
func TestAskBudgetFollowsContextLength(t *testing.T) {
	c := newServer(t)
	c.setup()
	llm := c.useFakeLLM()

	// 200 句，约 7000 字：比 32K 时的 1.2 万字少，比 8K 时的 2000 字多
	segs := make([]transcript.Segment, 0, 200)
	for i := range 200 {
		segs = append(segs, transcript.Segment{Start: float64(i * 10), End: float64(i*10 + 5), Text: fmt.Sprintf("第 %d 句，闲聊一些别的事情。", i)})
	}
	segs[100].Text = "合同的报价定为十二万。"
	m := c.processed("长会", segs, "## 摘要\n聊了很多。", `[]`)

	llm.set(`{"keywords": ["报价"]}`, "报价十二万 [M1-2]。")
	planned, _ := llm.counts()
	c.want(c.ask(obj{"question": "报价多少？", "meeting_id": m}), 200)
	if p, _ := llm.counts(); p != planned {
		t.Error("32K 时整场放得下，不用先提取搜索词")
	}
	if prompt := llm.last(); !strings.Contains(prompt, "第 0 句") || !strings.Contains(prompt, "第 199 句") {
		t.Error("32K 时整场转写都交给大模型")
	}

	cfg, err := c.svc.LLMSettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.svc.SaveLLM(t.Context(), service.LLMInput{BaseURL: cfg.BaseURL, Model: cfg.Model, ContextTokens: 8_192}); err != nil {
		t.Fatal(err)
	}
	c.want(c.ask(obj{"question": "报价多少？", "meeting_id": m}), 200)
	if p, _ := llm.counts(); p != planned+1 {
		t.Error("8K 时整场放不下，要先提取搜索词")
	}
	prompt := llm.last()
	if !strings.Contains(prompt, "合同的报价定为十二万") || strings.Contains(prompt, "第 0 句") || strings.Contains(prompt, "第 199 句") {
		t.Errorf("8K 时只带命中的片段：\n%s", prompt)
	}
	if n := len([]rune(prompt)); n > 4_000 {
		t.Errorf("8K 时材料不超过 2000 字，整个提示词也不该有 %d 字", n)
	}
}

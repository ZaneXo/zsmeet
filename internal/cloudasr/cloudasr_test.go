package cloudasr

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZaneXo/zsmeet/internal/audio"
	"github.com/ZaneXo/zsmeet/internal/transcript"
)

func TestSpokenNumbers(t *testing.T) {
	for in, want := range map[string]string{
		"十八到35岁":      "十八到三十五岁",
		"下午3点":        "下午三点",
		"3:30开会":      "三点三十开会",
		"9:00开会":      "九点开会",
		"增长了12.5%":    "增长了百分之十二点五",
		"2025年的预算":    "二零二五年的预算",
		"一共105个":      "一共一百零五个",
		"30000块":      "三万块",
		"1200人":       "一千二百人",
		"10086":       "一万零八十六",
		"13800138000": "一三八零零一三八零零零",
		"编号007":       "编号零零七",
		"1001":        "一千零一",
		"版本3.0":       "版本三点零",
		"没有数字":        "没有数字",
		"第10个和第20个":   "第十个和第二十个",
		"120万":        "一百二十万",
	} {
		if got := SpokenNumbers(in); got != want {
			t.Errorf("SpokenNumbers(%q) = %q, want %q", in, got, want)
		}
	}
}

// makeSpeech 用「字@相对秒」造一个 Speech，方便写对齐测试。
func makeSpeech(start, end float64, spec string) transcript.Speech {
	var toks []transcript.Token
	for _, f := range strings.Fields(spec) {
		text, at, _ := strings.Cut(f, "@")
		sec, err := strconv.ParseFloat(at, 64)
		if err != nil {
			panic(err)
		}
		toks = append(toks, transcript.Token{Text: text, Start: sec})
	}
	return transcript.Speech{
		Start:    start,
		End:      end,
		Tokens:   toks,
		Segments: transcript.Sentences(toks, start, end),
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 0.01 }

func TestAlignSubstitutionInsertDelete(t *testing.T) {
	// SenseVoice 听错了一个同音字（非→飞），漏了中间两个字（集、成），多听了一个杂音字（嗯）
	sp := makeSpeech(10.0, 16.0, "非@0.2 书@0.5 放@1.4 到@1.8 嗯@2.1 下@2.4 周@2.8 。@3.0")
	segs, ok := Align([]transcript.Speech{sp}, "飞书集成放到下周。")
	if !ok {
		t.Fatal("应该对齐成功，不应退回")
	}
	if len(segs) != 1 || segs[0].Text != "飞书集成放到下周。" {
		t.Fatalf("句子不对：%+v", segs)
	}
	// 第一个字「飞」替换了「非@0.2」，开始时间就是 10.2；结尾是 16.0
	if !near(segs[0].Start, 10.2) || !near(segs[0].End, 16.0) {
		t.Errorf("时间不对：%+v", segs[0])
	}

	// 开头和结尾各多出两个字：开头摊到 sp.Start（10.0），结尾摊到 sp.End（16.0）
	sp2 := makeSpeech(10.0, 16.0, "开@2.0 会@3.0 讨@4.0 论@5.0")
	segs, ok = Align([]transcript.Speech{sp2}, "今天开会讨论。好吧。")
	if !ok || len(segs) != 2 {
		t.Fatalf("开头结尾多字对齐不对：ok=%v segs=%+v", ok, segs)
	}
	if !near(segs[0].Start, 10.0) {
		t.Errorf("开头多出的字应该从段开头 10.0 开始：%+v", segs[0])
	}
	if !(segs[1].Start > 15.0 && segs[1].Start < 16.0 && near(segs[1].End, 16.0)) {
		t.Errorf("结尾多出的字应该摊在 (15.0, 16.0]：%+v", segs[1])
	}
}

func TestAlignNumbersAndEnglishAcrossSilence(t *testing.T) {
	// 两段话中间有 3.5 秒静音（4.5 到 8.0）：第一句的 End 要收回到 4.5，不能拉到 8.1
	sp1 := makeSpeech(1.0, 4.5, "▁V@0.1 ers@0.2 ion@0.3 ▁@0.4 3@0.5 .@0.6 5@0.7 发@1.0 布@1.3 了@1.6 。@1.9")
	sp2 := makeSpeech(8.0, 12.0, "2@0.1 0@0.2 2@0.3 5@0.4 年@0.6 有@1.0 1@1.3 8@1.5 人@2.0 。@2.3")

	segs, ok := Align([]transcript.Speech{sp1, sp2}, "Version 3.5 发布了。2025年有十八人。")
	if !ok || len(segs) != 2 {
		t.Fatalf("数字和中英文混合应该对齐成两句：ok=%v segs=%+v", ok, segs)
	}
	if segs[0].Text != "Version 3.5 发布了。" || !near(segs[0].Start, 1.1) || !near(segs[0].End, 4.5) {
		t.Errorf("第一句（含静音收紧）不对：%+v", segs[0])
	}
	if segs[1].Text != "2025年有十八人。" || !near(segs[1].Start, 8.1) || !near(segs[1].End, 12.0) {
		t.Errorf("第二句（年份 + 阿拉伯数字对汉字数字）不对：%+v", segs[1])
	}
}

func TestAlignFallbackWhenMismatch(t *testing.T) {
	sp := makeSpeech(0, 5.0, "今@0.2 天@0.5 天@0.8 气@1.1 不@1.4 错@1.7 。@2.0")
	// 对上的字不到一半：退回 SenseVoice 的句子
	segs, ok := Align([]transcript.Speech{sp}, "完全不相干的另外一段长文字内容。")
	if ok {
		t.Fatal("两边差别太大应该退回")
	}
	if len(segs) != 1 || segs[0].Text != "今天天气不错。" {
		t.Errorf("退回后应该保留 SenseVoice 的原文：%+v", segs)
	}

	// 在线识别返回空串、而本机有字时也退回
	segs, ok = Align([]transcript.Speech{sp}, "   ")
	if ok || len(segs) != 1 || segs[0].Text != "今天天气不错。" {
		t.Errorf("空串应该退回 SenseVoice：ok=%v segs=%+v", ok, segs)
	}

	// 远端小声发言恢复：本机只听清前后两句（9 个字），中间一大段小声讨论只有在线识别认出来了，
	// 只要本机听清的部分绝大部分按顺序对得上，就保留在线识别补回来的句子并插值时间。
	spHead := makeSpeech(0, 2.0, "今@0.2 天@0.5 开@0.9 会@1.3 。@1.6")
	spTail := makeSpeech(12.0, 15.0, "大@0.2 家@0.6 同@1.0 意@1.4 吗@1.8 ？@2.1")
	recovered, ok := Align([]transcript.Speech{spHead, spTail}, "今天开会。中间这段离麦克风远的话本机没听见，在线识别完整认出来了。大家同意吗？")
	if !ok || len(recovered) != 3 {
		t.Fatalf("小声发言恢复不应退回：ok=%v segs=%+v", ok, recovered)
	}
	if recovered[1].Start < 1.3 || recovered[1].End > 12.2 {
		t.Errorf("补回来的中间句时间应插值在前后两句之间：%+v", recovered[1])
	}
}

func TestChunkSpans(t *testing.T) {
	spans := []transcript.Speech{
		{Start: 0, End: 8},
		{Start: 10, End: 55},
		{Start: 60, End: 98},   // 从 0 到 98 不超过 100 秒，合进第一组
		{Start: 102, End: 110}, // 再加就 110 秒了，另起第二组
	}
	chunks := ChunkSpans(spans, 100)
	if len(chunks) != 2 || len(chunks[0]) != 3 || len(chunks[1]) != 1 {
		t.Fatalf("切段不对：%+v", chunks)
	}
}

func TestDeepSeekClientRetryAndAuth(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer secret-ds-key" {
			t.Errorf("Authorization = %q", got)
		}
		if err := r.ParseMultipartForm(4 << 20); err != nil {
			t.Fatalf("解析 multipart 失败：%v", err)
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("缺少 file 字段：%v", err)
		}
		wav, _ := io.ReadAll(f)
		f.Close()
		if len(wav) < 44 || string(wav[:4]) != "RIFF" || binary.LittleEndian.Uint32(wav[24:28]) != audio.SampleRate {
			t.Errorf("不是 16k WAV：len=%d", len(wav))
		}
		if n <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limit"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"重试后成功了。"}`))
	}))
	defer srv.Close()

	ds := &DeepSeek{
		URL:     srv.URL,
		APIKey:  "secret-ds-key",
		Backoff: func(int) time.Duration { return time.Millisecond },
	}
	got, err := ds.Recognize(t.Context(), make([]float32, audio.SampleRate))
	if err != nil || got != "重试后成功了。" || calls.Load() != 3 {
		t.Fatalf("got=%q err=%v calls=%d", got, err, calls.Load())
	}

	// 401 不重试，且错误信息不带出密钥
	var authCalls atomic.Int32
	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		authCalls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`invalid key secret-ds-key`))
	}))
	defer authSrv.Close()

	dsAuth := &DeepSeek{
		URL:     authSrv.URL,
		APIKey:  "secret-ds-key",
		Backoff: func(int) time.Duration { return time.Millisecond },
	}
	_, err = dsAuth.Recognize(t.Context(), make([]float32, 1600))
	if !IsAuthError(err) || authCalls.Load() != 1 {
		t.Fatalf("401 应该立即返回 AuthError：err=%v calls=%d", err, authCalls.Load())
	}
	if strings.Contains(err.Error(), "secret-ds-key") {
		t.Fatalf("报错里不能带出 API Key：%v", err)
	}

	if err := Test(t.Context(), srv.URL, "secret-ds-key"); err != nil {
		t.Fatalf("Test 应该成功：%v", err)
	}
}

type fakeProvider func(ctx context.Context, samples []float32) (string, error)

func (f fakeProvider) Recognize(ctx context.Context, samples []float32) (string, error) {
	return f(ctx, samples)
}

func TestTranscribePartialAndAuthFallback(t *testing.T) {
	sp1 := makeSpeech(0, 5, "第@0.2 一@0.5 段@0.8 原@1.1 文@1.4 。@1.7")
	sp2 := makeSpeech(105, 110, "第@0.2 二@0.5 段@0.8 原@1.1 文@1.4 。@1.7")
	samples := make([]float32, 112*audio.SampleRate)
	// 用第一个采样区分是哪一段
	samples[0] = 0.25
	samples[105*audio.SampleRate] = 0.75

	// 第一段成功，第二段 500 失败：第一段用在线识别，第二段退回 SenseVoice
	res := Transcribe(t.Context(), fakeProvider(func(_ context.Context, s []float32) (string, error) {
		if s[0] < 0.5 {
			return "第一段新文。", nil
		}
		return "", errors.New("HTTP 502")
	}), samples, []transcript.Speech{sp1, sp2}, 2, nil)

	if res.Chunks != 2 || res.Succeeded != 1 || res.Aligned != 1 || res.AuthErr != nil {
		t.Fatalf("统计不对：%+v", res)
	}
	if len(res.Segments) != 2 || res.Segments[0].Text != "第一段新文。" || res.Segments[1].Text != "第二段原文。" {
		t.Fatalf("分段退回不对：%+v", res.Segments)
	}

	// 遇到 401：整场退回 SenseVoice，AuthErr 非空
	res = Transcribe(t.Context(), fakeProvider(func(_ context.Context, _ []float32) (string, error) {
		return "", &AuthError{Code: 401, Msg: "Key 不对"}
	}), samples, []transcript.Speech{sp1, sp2}, 2, nil)
	if res.AuthErr == nil || res.Aligned != 0 || len(res.Segments) != 2 || res.Segments[0].Text != "第一段原文。" {
		t.Fatalf("401 整场退回不对：%+v", res)
	}
}

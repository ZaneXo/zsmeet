package asr

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/ZaneXo/zsmeet/internal/audio"
	"github.com/ZaneXo/zsmeet/internal/audio/audiotest"
	"github.com/ZaneXo/zsmeet/internal/models"
	"github.com/ZaneXo/zsmeet/internal/transcript"
	"github.com/ZaneXo/zsmeet/internal/voiceprint"
)

// 这个测试要用到约 300 MB 的模型和一段真实录音，默认跳过。运行方法：
//
//	ZSMEET_TEST_MODELS=模型目录 ZSMEET_TEST_AUDIO=录音文件 go test ./internal/asr/ -v
func TestEngineOnRealAudio(t *testing.T) {
	dir, file := os.Getenv("ZSMEET_TEST_MODELS"), os.Getenv("ZSMEET_TEST_AUDIO")
	if dir == "" || file == "" {
		t.Skip("没有设置 ZSMEET_TEST_MODELS 和 ZSMEET_TEST_AUDIO")
	}
	pcm := filepath.Join(t.TempDir(), "audio.f32")
	if err := audio.Decode(t.Context(), file, pcm); err != nil {
		t.Fatal(err)
	}
	samples, err := audio.ReadPCM(pcm)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := Load(models.Paths{Dir: dir}, 4, "zh")
	if err != nil {
		t.Fatal(err)
	}

	segs, err := engine.Transcribe(t.Context(), samples, func(float64) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) == 0 {
		t.Fatal("一句话都没识别出来")
	}
	for _, s := range segs {
		if s.End <= s.Start || strings.TrimSpace(s.Text) == "" {
			t.Errorf("片段不对：%+v", s)
		}
	}

	// 按声音估计人数。声纹：每位说话人一个，不同的人之间的相似度应该明显低于 voiceprint.Threshold，调阈值时看
	turns, err := engine.Diarize(t.Context(), samples, 0, func(float64) {})
	if err != nil {
		t.Fatal(err)
	}
	copied := append([]transcript.Segment(nil), segs...)
	ids := transcript.AssignSpeakers(copied, turns)
	t.Logf("按声音估计：%d 位说话人，各句依次是%s", len(ids), speakerSequence(copied))
	voices, err := engine.Embed(samples, transcript.RenumberTurns(turns, ids))
	if err != nil {
		t.Fatal(err)
	}
	for a, va := range voices {
		for b, vb := range voices {
			if a < b {
				t.Logf("说话人 %d（%.0f 秒）和说话人 %d（%.0f 秒）的声纹相似度 %.2f", a+1, va.Seconds, b+1, vb.Seconds, voiceprint.Cosine(va.Embedding, vb.Embedding))
			}
		}
	}

	// 知道有几个人说话时（ZSMEET_TEST_SPEAKERS）按人数分：分出来正好这么多人
	if n, err := strconv.Atoi(os.Getenv("ZSMEET_TEST_SPEAKERS")); err == nil && n > 0 {
		turns, err := engine.Diarize(t.Context(), samples, n, func(float64) {})
		if err != nil {
			t.Fatal(err)
		}
		copied := append([]transcript.Segment(nil), segs...)
		transcript.AssignSpeakers(copied, turns)
		clusters := map[int]bool{}
		for _, turn := range turns {
			clusters[turn.Speaker] = true
		}
		t.Logf("按 %d 个人分：分出 %d 个，各句依次是%s", n, len(clusters), speakerSequence(copied))
		if len(clusters) != n {
			t.Errorf("要求按 %d 个人分，实际分出 %d 个", n, len(clusters))
		}
	}
}

// speakerSequence 是每一句的说话人编号（从 1 开始）连在一起，没分到说话人的写成「?」。
func speakerSequence(segs []transcript.Segment) string {
	var b strings.Builder
	for _, s := range segs {
		if s.Speaker < 0 {
			b.WriteString("?")
			continue
		}
		b.WriteString(strconv.Itoa(s.Speaker + 1))
	}
	return b.String()
}

// 很轻的说话声（没开自动增益、离麦克风远，比正常小 20 dB）经过和后台处理一样的转码，识别出来的字和正常音量的一样；
// 正常音量的也不因为音量处理认错。要用到约 280 MB 的模型和 macOS 的 say，默认跳过。运行方法：
//
//	ZSMEET_TEST_MODELS=模型目录 go test ./internal/asr/ -run TestQuietSpeech -v
func TestQuietSpeech(t *testing.T) {
	dir := os.Getenv("ZSMEET_TEST_MODELS")
	if dir == "" {
		t.Skip("没有设置 ZSMEET_TEST_MODELS")
	}
	if !audiotest.CanSpeak() {
		t.Skip("这台机器上没有 say（婷婷），合成不了中文")
	}
	engine, err := Load(models.Paths{Dir: dir}, 4, "zh")
	if err != nil {
		t.Fatal(err)
	}
	// recognize 返回识别出的字（去掉标点：句子切在哪里可能不一样）
	recognize := func(gain float64) string {
		tmp := t.TempDir()
		in, pcm := filepath.Join(tmp, "speech.aac"), filepath.Join(tmp, "speech.f32")
		if err := os.WriteFile(in, audiotest.Speech(t, gain), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := audio.Decode(t.Context(), in, pcm); err != nil {
			t.Fatal(err)
		}
		samples, err := audio.ReadPCM(pcm)
		if err != nil {
			t.Fatal(err)
		}
		segs, err := engine.Transcribe(t.Context(), samples, func(float64) {})
		if err != nil {
			t.Fatal(err)
		}
		var text strings.Builder
		for _, s := range segs {
			text.WriteString(strings.Map(func(r rune) rune {
				if strings.ContainsRune("，。？！、,.?! ", r) {
					return -1
				}
				return r
			}, s.Text))
		}
		return text.String()
	}
	normal, quiet := recognize(0), recognize(-20)
	t.Logf("正常音量：%s", normal)
	t.Logf("小 20 dB：%s", quiet)
	for _, want := range []string{"上传接口", "转写太慢", "搜索框的样式", "发布的时间"} {
		if !strings.Contains(normal, want) || !strings.Contains(quiet, want) {
			t.Errorf("两种音量都应该认出「%s」", want)
		}
	}
	if quiet != normal {
		t.Errorf("小 20 dB 的录音认出来的字和正常音量的不一样")
	}
}

// 算声纹用的片段：每人从最长的一段挑起，最多 30 秒；说话不到 2 秒的人不要；超出录音的部分截掉。
func TestPickTurns(t *testing.T) {
	const rate = audio.SampleRate
	turns := []transcript.Turn{
		{Start: 0, End: 5, Speaker: 0},
		{Start: 5, End: 25, Speaker: 0},
		{Start: 25, End: 26.5, Speaker: 1}, // 说话人 1 一共 1.5 秒，不够
		{Start: 30, End: 45, Speaker: 0},
		{Start: 50, End: 51, Speaker: 2},
		{Start: 59, End: 70, Speaker: 2}, // 录音只有 60 秒，这段只剩 1 秒；加上前面一共 2 秒，刚好够
		{Start: 10, End: 20, Speaker: -1},
	}
	got := pickTurns(turns, 60*rate)
	want := map[int][][2]int{
		0: {{5 * rate, 25 * rate}, {30 * rate, 40 * rate}},
		2: {{50 * rate, 51 * rate}, {59 * rate, 60 * rate}},
	}
	if len(got) != len(want) {
		t.Fatalf("应该只有说话人 0 和 2：%v", got)
	}
	for speaker, spans := range want {
		if !slices.Equal(got[speaker], spans) {
			t.Errorf("说话人 %d 挑的片段是 %v，应该是 %v", speaker, got[speaker], spans)
		}
	}
}

// 模型加载失败时告诉管理员是哪个文件、怎么恢复（删掉以后重启后台处理会重新下载），不只是「加载失败」。
func TestModelError(t *testing.T) {
	msg := modelError("区分说话人的模型", "/models/speaker-diarization.onnx").Error()
	for _, want := range []string{"区分说话人的模型加载失败", "speaker-diarization.onnx", "删掉", "docker compose restart worker", "重新下载"} {
		if !strings.Contains(msg, want) {
			t.Errorf("说明里应该有「%s」：%s", want, msg)
		}
	}
}

// SenseVoice auto 模式的误判过滤：在中文会议里把短语气词幻觉（日文假名/纯汉字 ja、韩文谚文/混杂汉字、超短孤立英文词）挑出来重算，
// 同时保留真正的整句外语、中英夹杂和纯日/韩/粤/英会议。
func TestPlanAutoRedecode(t *testing.T) {
	// 1. 中文主语言会议（含中英混说 + 单独一句英文），夹杂日/韩/超短英文幻觉
	zhMeeting := []autoSegment{
		{dur: 35.0, lang: "zh", text: "我们今天看一下 API latency 和三季度的发布计划。"},
		{dur: 0.6, lang: "ja", text: "うん。"},                                    // 1: 日文假名幻觉 → 重算
		{dur: 0.7, lang: "ja", text: "四？"},                                     // 2: 无假名的 ja 汉字幻觉 → 重算
		{dur: 0.8, lang: "ko", text: "분."},                                     // 3: 韩文幻觉 → 重算
		{dur: 2.3, lang: "ko", text: "하늘튀 왜种这种。"},                              // 4: 较长的韩汉混杂底噪幻觉 → 重算
		{dur: 0.5, lang: "en", text: "O."},                                     // 5: 超短英文语气词误判 → 重算
		{dur: 2.8, lang: "en", text: "Let's ship version 2.1 this afternoon."}, // 超过 1.5 秒的真实英文整句 → 保留
		{dur: 30.0, lang: "zh", text: "好的，那下午四点准时上线。"},
	}
	fb, got := planAutoRedecode(zhMeeting)
	if fb != "zh" || !slices.Equal(got, []int{1, 2, 3, 4, 5}) {
		t.Errorf("中文会议：fallback=%q, indices=%v, want zh & [1 2 3 4 5]", fb, got)
	}

	// 2. 中英双语会议（英文长段充足）：连短英文回应（"Yeah."）也保留，只重算日/韩误判
	bilingual := []autoSegment{
		{dur: 5.0, lang: "zh", text: "我先同步一下国内这边的研发进度。"},
		{dur: 5.2, lang: "en", text: "Sounds good, our team finished the SDK integration yesterday."},
		{dur: 0.8, lang: "en", text: "Yeah."},
		{dur: 0.6, lang: "ja", text: "も."},
	}
	fb, got = planAutoRedecode(bilingual)
	if fb != "en" && fb != "zh" {
		t.Errorf("双语会议 fallback=%q", fb)
	}
	if !slices.Equal(got, []int{3}) {
		t.Errorf("双语会议只应该重算日文幻觉：got %v, want [3]", got)
	}

	// 3. 纯日文会议（哪怕只有短短几秒）：完全保留，不误当成幻觉重算
	jaClip := []autoSegment{
		{dur: 3.2, lang: "ja", text: "こんにちは、本日の会議を始めます。"},
	}
	if fb, got := planAutoRedecode(jaClip); fb != "ja" || len(got) != 0 {
		t.Errorf("纯日文会议不应重算：fb=%q, got=%v", fb, got)
	}

	// 4. 纯韩文会议：完全保留
	koClip := []autoSegment{
		{dur: 3.4, lang: "ko", text: "안녕하세요 오늘 회의를 시작하겠습니다."},
	}
	if fb, got := planAutoRedecode(koClip); fb != "ko" || len(got) != 0 {
		t.Errorf("纯韩文会议不应重算：fb=%q, got=%v", fb, got)
	}

	// 5. 粤语会议（带英文混说）夹杂短语气词误判：按 yue 重算幻觉，保留粤语口语用字
	yueMeeting := []autoSegment{
		{dur: 4.2, lang: "yue", text: "大家早晨，今日我哋一齐倾下 Q3 嘅发布计划。"},
		{dur: 3.6, lang: "yue", text: "呢个改动唔大，下星期可以上线。"},
		{dur: 0.6, lang: "ja", text: "うん。"},
	}
	if fb, got := planAutoRedecode(yueMeeting); fb != "yue" || !slices.Equal(got, []int{2}) {
		t.Errorf("粤语会议：fb=%q, got=%v, want yue & [2]", fb, got)
	}

	// 6. 极短孤立中文语气词（整段录音只有一句 0.7 秒的「嗯」被判成 ja）
	shortInterjection := []autoSegment{
		{dur: 0.7, lang: "ja", text: "四？"},
	}
	if fb, got := planAutoRedecode(shortInterjection); fb != "zh" || !slices.Equal(got, []int{0}) {
		t.Errorf("孤立短语气词：fb=%q, got=%v, want zh & [0]", fb, got)
	}
}

// 在真实 SenseVoice 模型上验证 auto 多语种识别、短语气词防幻觉，以及 SetLanguage 原地切换语言。
// 需要 ZSMEET_TEST_MODELS 和 macOS 的 say，默认跳过。
func TestMultilingualAndSetLanguage(t *testing.T) {
	dir := os.Getenv("ZSMEET_TEST_MODELS")
	if dir == "" {
		t.Skip("没有设置 ZSMEET_TEST_MODELS")
	}
	if _, err := exec.LookPath("say"); err != nil {
		t.Skip("这台机器上没有 say")
	}
	p := models.Paths{Dir: dir}
	engine, err := NewTranscriber(SenseVoice(p, 4, "auto"), p.VAD())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	synth := func(voice, text string) []float32 {
		tmp := t.TempDir()
		aiff := filepath.Join(tmp, "in.aiff")
		pcm := filepath.Join(tmp, "out.f32")
		if out, err := exec.CommandContext(t.Context(), "say", "-v", voice, "-o", aiff, text).CombinedOutput(); err != nil {
			t.Skipf("本机没有语音 %s：%v (%s)", voice, err, out)
		}
		if err := audio.Decode(t.Context(), aiff, pcm); err != nil {
			t.Fatal(err)
		}
		samples, err := audio.ReadPCM(pcm)
		if err != nil {
			t.Fatal(err)
		}
		return samples
	}

	// 1. 中文会议里夹着孤立的「嗯。」（静音隔开）和英文句子：auto 模式下不应出现日韩字符
	silence := make([]float32, audio.SampleRate) // 1 秒静音让 VAD 切成独立短段
	part1 := synth("Tingting", "我们今天讨论一下接口的性能优化和发布计划。")
	hmm := synth("Tingting", "嗯。")
	part2 := synth("Samantha", "Let us review the release checklist this afternoon.")
	mixed := slices.Concat(silence, part1, silence, hmm, silence, part2, silence)

	segs, err := engine.Transcribe(t.Context(), mixed, func(float64) {})
	if err != nil {
		t.Fatal(err)
	}
	joined := joinSegmentText(segs)
	t.Logf("中英混合+短语气词（auto）：%s", joined)
	for _, r := range joined {
		if unicode.In(r, unicode.Hiragana, unicode.Katakana, unicode.Hangul) {
			t.Errorf("中英会议不应出现日韩假名或谚文：%s", joined)
			break
		}
	}
	if !strings.Contains(strings.ToLower(joined), "release") {
		t.Errorf("英文句子应当被识别出来：%s", joined)
	}

	// 2. 日文识别：词组间不应残留多余空格，且 SetLanguage 来回切换正常
	engine.SetLanguage("ja")
	jaSegs, err := engine.Transcribe(t.Context(), slices.Concat(silence, synth("Kyoko", "こんにちは、本日の会議を始めます。"), silence), func(float64) {})
	if err != nil {
		t.Fatal(err)
	}
	jaText := joinSegmentText(jaSegs)
	t.Logf("日文（ja）：%s", jaText)
	if strings.Contains(jaText, " ") || !strings.Contains(jaText, "会議") {
		t.Errorf("日文不应带分词空格且应认出「会議」：%q", jaText)
	}
	engine.SetLanguage("auto")
}

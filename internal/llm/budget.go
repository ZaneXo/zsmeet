package llm

import "github.com/ZaneXo/zsmeet/internal/settings"

// 按模型的上下文长度（token 数）算一次请求里能放多少字的会议内容。
//
// 中文一个字大约多少 token 要看分词器：用一段会议转写（「[00:12] 说话人 2：……」这样一行一段）量过，
// Qwen2.5、DeepSeek-V3、GLM-4.5、Llama 3 每个 token 平均 1.36～1.57 个字（Llama 3 最费，约 0.73 token 一个字）；
// DeepSeek 的文档也说一个汉字约 0.6 token。这里按每个 token 0.9 个字算，比实测最费的还多留三成多，
// 换成老一些的分词器、夹着英文和数字也放得下。
//
// 上下文里还要留出提示词和模型的输出：系统提示、要求、纪要模板、标题等留 promptReserve；
// 纪要可能写几千个 token，输出留上下文的四分之一，最少 2K、最多 8K。
// 32K 上下文正好是以前写死的每段 2 万字、问会议 1.2 万字，以前装好的不用改设置，行为不变。
const (
	promptReserve = 2_048
	minOutput     = 2_048
	maxOutput     = 8_192
	// 每 10 个 token 按 9 个字算
	charsPer10Tokens = 9
	// 问会议的材料占写纪要那份的五分之三：还要放问题、编号和出处的要求，回答也要快
	askShareNum, askShareDen = 3, 5

	// 写纪要时每段至少这么多字，太小的设置也能分段写完
	minTranscriptChars = 3_000
	// 每段最多这么多字（约 4 小时的会）。上下文再长也不一次塞进去：
	// 一次几万 token 的请求在本地模型上要算很久，还容易超时、漏掉中间的内容，不如分段写
	maxTranscriptChars = 60_000
	minAskChars        = 2_000
	// 问会议要在几分钟内答完（两次调用），材料比写纪要的少
	maxAskChars = 30_000
)

// usableChars 是上下文里除去提示词和输出以后，还能放多少字。0 按 settings.DefaultContextTokens 算。
func usableChars(contextTokens int) int {
	if contextTokens <= 0 {
		contextTokens = settings.DefaultContextTokens
	}
	output := min(max(contextTokens/4, minOutput), maxOutput)
	return max(contextTokens-promptReserve-output, 0) * charsPer10Tokens / 10
}

// TranscriptChars 是写纪要时一次请求里最多放多少字的转写，超过就分段（见 minutes.Input.MaxChars）。
// 按千字取整，限制在 3000 到 6 万字之间。
func TranscriptChars(contextTokens int) int {
	return clampThousands(usableChars(contextTokens), minTranscriptChars, maxTranscriptChars)
}

// AskChars 是问会议时交给大模型的材料最多多少字。按千字取整，限制在 2000 到 3 万字之间。
func AskChars(contextTokens int) int {
	return clampThousands(usableChars(contextTokens)*askShareNum/askShareDen, minAskChars, maxAskChars)
}

func clampThousands(n, lo, hi int) int {
	return min(max(n/1000*1000, lo), hi)
}

# 识别方案测评结果

2026 年 11 月用 `tools/asreval` 在公开数据集上量过的结果：识别模型选哪个、送去识别之前的音量处理、按热词读音纠错、大模型校对，以及 DeepSeek 的在线识别。怎么量见[开发 → 比较识别方案](development.md#比较识别方案)，换了模型或改了处理步骤，按最后一节的命令再量一遍、更新这里。

## 结论

1. **送去识别之前把音量调匀是这几项改动里收益最大的**：会议录音的字错误率降了 13～20 个点（AliMeeting 45～49% → 29～30%，AISHELL-4 34% → 21%），三种本地识别模型都一样。线上用的是 `dynaudnorm`（`audio.SpeechFilter`），和 `loudnorm` 效果差不多。
2. **本地识别模型继续用 SenseVoice**：调匀音量以后，会议录音上 SenseVoice 比 Fun-ASR-Nano、Qwen3-ASR 0.6B 都好，CPU 耗时不到它们的 1/8。Fun-ASR-Nano 只在专有名词多的录音上带热词时更好（5.3% 对 6.5%），但不如 SenseVoice 加纠错（2.7%）。
3. **按热词读音纠错和大模型校对对专有名词很有用，对普通会议几乎没用**：ContextASR 上 8.2% → 6.5% → 2.7%，热词认对的从 361 次到 940 次（共 966 次）；会议录音上大模型校对只降了 0.2 个点。会议录音的错大多是漏识别（识别出的字比标准文字少两成多），校对补不回来。校对时附上拼音没有用，线上保持关闭。
4. **DeepSeek 在线识别比 SenseVoice 好，整段送比按句送好得多**：把相邻的句子合到 100 秒一个请求，会议录音上比 SenseVoice 低 4～5 个点（AISHELL-4 20.6% → 15.7%）；专有名词多的录音上不传热词就到 3.1%。代价是录音要发给 DeepSeek，而且接口不返回每句的时间，线上还没有接入。
5. **GPU 不改变准确率**，SenseVoice 快 3 倍左右。CPU 上 SenseVoice 已经只要录音时长的 0.02～0.03 倍，部署不需要显卡。

## 数据集

| 数据集 | 内容 | 用了哪些 | 时长 |
|---|---|---|---|
| [AliMeeting](https://openslr.org/119/) Eval / Test | 真实的中文会议，2～4 人，会议室里的 8 路麦克风阵列 | Eval 全部 8 场、Test 全部 20 场，远场阵列的第 1 路 | 4.2 / 10.8 小时 |
| [AISHELL-4](https://www.openslr.org/111) Test | 真实的中文会议，4～8 人，8 路麦克风阵列，分小（S）、中（M）、大（L）三种会议室 | Test 全部 20 场，第 1 路 | 12.7 小时 |
| [ContextASR-Bench](https://huggingface.co/datasets/MrSupW/ContextASR-Bench) | 中文对话，每条带一份专有名词列表（当热词用）；录音是语音合成的 | 中文对话的前 60 条 | 2.6 小时 |

- 会议录音取阵列的一路，和用一个麦克风录会议一样；标准文字是所有人的话按时间排好，说话重叠的部分也算在内，所以字错误率比近讲录音高得多。
- 数据集的标准文字按读音写数字，评分前两边的阿拉伯数字都换成汉字读法再比。
- ContextASR 的录音是语音合成的，干净、没有重叠，只用来看专有名词。

## 结果

字错误率按「一个汉字算一个、一个英文单词算一个」算编辑距离。耗时是识别用的时间和录音时长之比，CPU 是 Xeon Platinum 8582C 上每份模型 4 个线程。

### 音量处理

离麦克风远的人声音很小，不处理的话 VAD 把小声的话当成静音扔掉，根本到不了识别模型。比较了两种处理：

- `loudnorm=I=-20`：按整段录音的响度（EBU R128）调到 -20 LUFS。
- `dynaudnorm=f=250:g=15:p=0.9:m=20`：线上现在用的（`audio.SpeechFilter`），每 0.25 秒看一次峰值、按前后几秒平滑地调增益，最多放大 20 倍。它是在一场真实站会的录音上选出来的，那场会上比 `loudnorm` 好（见 `internal/audio`）。

| 数据集 | 模型 | 不处理 | loudnorm | dynaudnorm（线上） |
|---|---|---|---|---|
| AliMeeting Eval | SenseVoice | 49.25% | 30.02% | **29.12%** |
| | Fun-ASR-Nano | 53.51% | 36.72% | |
| | Qwen3-ASR | 51.21% | 32.64% | |
| AliMeeting Test | SenseVoice | 45.18% | 29.63% | **29.59%** |
| | Fun-ASR-Nano | 50.19% | 34.14% | |
| | Qwen3-ASR | 47.37% | 31.39% | |
| AISHELL-4 Test | SenseVoice | 33.99% | **20.64%** | 21.19% |
| | Fun-ASR-Nano | 37.91% | 21.25% | |
| | Qwen3-ASR | 36.20% | 20.84% | |
| ContextASR | SenseVoice | 8.24% | 8.20% | 8.27% |

两种处理在公开数据集上差不到 1 个点，所以下面各节在 `loudnorm` 下量的结论（选哪个模型、纠错和 DeepSeek 有没有用）在线上的处理下照样成立，具体的数会差零点几个点。AISHELL-4 用 `loudnorm` 时：小会议室 8～19%，中、大会议室 18～36%。

### 本地识别模型（loudnorm）

| 方案 | AliMeeting Eval | AliMeeting Test | AISHELL-4 Test | ContextASR | 热词认对（ContextASR） | CPU 耗时 |
|---|---|---|---|---|---|---|
| SenseVoice（线上） | **30.02%** | **29.63%** | **20.64%** | 8.20% | 361/966 | 0.025 |
| Fun-ASR-Nano | 36.72% | 34.14% | 21.25% | 5.88% | 545/966 | 0.19～0.23 |
| Fun-ASR-Nano 带热词 | | | | 5.27% | 659/966 | 0.29 |
| Qwen3-ASR 0.6B | 32.64% | 31.39% | 20.84% | 6.32% | 490/966 | 0.26～0.43 |
| Qwen3-ASR 带热词 | | | | 9.32% | 832/966 | 0.47 |

- Fun-ASR-Nano 是 sherpa-onnx-funasr-nano-int8-2025-12-30，Qwen3-ASR 是 sherpa-onnx-qwen3-asr-0.6B-int8-2026-03-25。
- Qwen3-ASR 带热词时，遇到很短或开头不清楚的片段，会把热词列表原样当成识别结果吐出来，所以热词认对的多、字错误率反而升了。

### GPU（RTX 5090，不处理音量）

| 方案 | AliMeeting Eval | AISHELL-4 Test | ContextASR | 耗时（CPU → GPU） |
|---|---|---|---|---|
| SenseVoice | 49.16% | 33.90% | 8.15% | 0.02 → 0.007～0.011 |
| Fun-ASR-Nano | 53.56% | 38.43% | 5.82% | 0.11～0.25 → 0.05～0.07 |
| Fun-ASR-Nano 带热词 | | | 4.71%（788/966） | 0.31 → 0.08 |

GPU 上用的是没有量化的模型（fp32），一份模型、一路录音。和 CPU 上的 int8 模型（不处理音量时 SenseVoice 49.25%、33.99%、8.24%，Fun-ASR-Nano 53.51%、37.91%、5.96%）基本一样，只有 Fun-ASR-Nano 带热词好一些（CPU 5.34%）。Qwen3-ASR 只有 int8 版，没在 GPU 上测。

### 纠错（在 SenseVoice 的结果上，loudnorm）

| 方案 | ContextASR | 热词认对 | AliMeeting Eval |
|---|---|---|---|
| SenseVoice | 8.20% | 361/966 | 30.02% |
| + 按热词读音纠错 | 6.47% | 802/966 | （没有热词） |
| + 大模型校对 | **2.67%** | 940/966 | 29.81% |
| + 大模型校对，附拼音 | 2.99% | 942/966 | 29.89% |

- 大模型是 deepseek-v4-pro，经一个 OpenAI 兼容的中转服务调用，同时最多 3 个请求（和线上一样）。中转服务不稳定：ContextASR 60 批里失败 2 批，AliMeeting 53 批里失败 16 批（连接被重置、超过账号并发上限、请求太密触发保护），失败的批保持原样，所以 AliMeeting 上的数偏保守；但会议录音识别出的字比标准文字少 22.6%，光漏识别就占了至少四分之三的错，全部成功也只能再降零点几个点。
- 在两种都成功的 51 段 ContextASR 上：不附拼音 2.41%，附拼音 2.45%；附拼音好的 21 段、差的 20 段。大模型校对没有让任何一段变差。
- 附拼音的请求长一倍，不值得。

### DeepSeek 在线识别（loudnorm）

[接口](https://api-docs.deepseek.com/zh-cn/guides/asr)一次最多收 120 秒、不返回时间、没有热词参数。按 SenseVoice 的 VAD 切出的句子切录音，比较两种送法：一句一个请求（和线上的切法一样），相邻的句子连同中间的停顿合到最长 100 秒一个请求。同时最多 16 个请求。

| 方案 | AliMeeting Eval | AliMeeting Test | AISHELL-4 Test | ContextASR | 热词认对（ContextASR） |
|---|---|---|---|---|---|
| SenseVoice（线上） | 30.02% | 29.63% | 20.64% | 8.20% | 361/966 |
| DeepSeek 一句一个请求 | 28.08% | 27.94% | 19.73% | 5.25% | 529/966 |
| DeepSeek 合到 100 秒 | **25.78%** | **25.11%** | **15.72%** | 3.08% | 652/966 |

- 合到 100 秒时，AISHELL-4 的 20 场里 20 场都比 SenseVoice 好（小会议室 8.2～19.1% → 5.8～16.8%，中会议室 29～36% → 20～27%）；AliMeeting 的 8 场里 R8007_M8010 只从 50.2% 降到 47.4%，其余降 1～9 个点。
- 一共约 2.5 万个请求，没有一个失败（遇到 429、5xx 会等一会儿重试，这次没用上）。合到 100 秒时，16 个请求同时发，整个数据集的实际用时约为录音时长的 0.003 倍；单个请求 100 秒的录音约 5 秒返回。
- 价格按录音时长算，刊例价每小时 0.78 元（测的时候五折，0.39 元）。

## 复现

数据集用 `tools/asrdata` 整理成 `asreval` 要的样子：

```bash
# AliMeeting（Eval_Ali.tar.gz、Test_Ali.tar.gz 解压后）、AISHELL-4（test.tar.gz 解压后）
go run ./tools/asrdata textgrid -audio Eval_Ali/Eval_Ali_far/audio_dir -textgrid Eval_Ali/Eval_Ali_far/textgrid_dir -out data/alimeeting-eval
go run ./tools/asrdata textgrid -audio Test_Ali/Test_Ali_far/audio_dir -textgrid Test_Ali/Test_Ali_far/textgrid_dir -out data/alimeeting-test
go run ./tools/asrdata textgrid -audio aishell4/test/wav -textgrid aishell4/test/TextGrid -out data/aishell4-test
# ContextASR-Bench 中文对话的前 60 条
go run ./tools/asrdata contextasr -jsonl ContextASR-Dialogue_Mandarin.jsonl -n 60 -out data/contextasr
```

每个数据集单独跑一遍：

```bash
go run ./tools/asreval -data data/alimeeting-eval -workers 8 \
  -funasr-nano 模型目录 -qwen3-asr 模型目录 \            # 比较本地识别模型
  -llm-url https://.../v1 -llm-model 模型名 \            # 比较大模型校对，Key 放在 ZSMEET_EVAL_LLM_KEY
  -deepseek-asr https://api.deepseek.com/v1/tools/asr \  # 比较 DeepSeek 在线识别，Key 放在 ZSMEET_EVAL_DEEPSEEK_KEY
  -out 结果目录
# 不给 -filter 是线上的音量处理；-filter none：不处理，-filter loudnorm=I=-20：换成别的 ffmpeg 滤镜；-provider cuda：在 GPU 上跑（要 CUDA 版的 sherpa-onnx 库）
```

# 开发

## 它是怎么工作的

```
浏览器 ──分片上传──▶  api（zsmeet serve）  ──任务入队──▶  Postgres
   ▲                    网页 · 接口 · 登录                    │
   │                                                          ▼
   └──── 进度推送 ◀──── 读数据库里的进度 ◀──── worker（zsmeet worker）
                                              拼接分片 → ffmpeg 转码 → 识别文字
                                              → 区分说话人 → 大模型写纪要
```

- `api` 和 `worker` 是同一个程序的两种启动方式，镜像也是同一个。
- 任务队列用 [River](https://riverqueue.com)，任务就存在 Postgres 里，不需要 Redis。
  「把会议改成排队中」和「任务入队」在同一个事务里，不会出现改了状态却没有任务的情况。
- worker 处理期间定时写心跳。worker 被杀掉或者机器重启时，心跳一停，巡检任务会在两三分钟内把会议重新入队，不用人工处理。
- 语音识别用 [sherpa-onnx](https://github.com/k2-fsa/sherpa-onnx)：Silero VAD 切出有人说话的片段，SenseVoice 识别成带标点的文字（数字写成阿拉伯数字，每个字都有时间），按标点切成句子。区分说话人照着 [3D-Speaker](https://github.com/modelscope/3D-Speaker) 的做法（`internal/diarize`）：有人说话的地方切成 1.5 秒长、每隔 0.75 秒一段的小段，用 3D-Speaker 的 CAM++ 模型给每段算声纹，谱聚类分成几个人（没选说话人数时按拉普拉斯矩阵特征值的间隔估计人数），很小的类归到最像的人，相邻的小段接成每个人说话的时间段。打开了声纹的话，再用 CAM++ 给每位说话人算一个声音特征，和改过名的人比对。
- 识别之前先用 ffmpeg 把音量动态地调匀（`internal/audio`）：离麦克风远、没有自动增益的录音（比如以前的桌面版直接录的麦克风）声音很小，不调的话小声的话会被当成静音丢掉。一场 -39 LUFS 的真实站会，调之前只认出 75 句、26% 的时间有字，调了以后 118 句、75% 的时间有字。正常音量的录音只调大两三 dB，认出来的字不变。回放用的录音也调响一些，调得更慢更轻，听起来不会忽大忽小。
- 桌面版录音时自己先把麦克风调到正常的说话音量（WebRTC 的 AGC2，在混音之前，见[桌面版的设计](desktop.md)的「麦克风自动增益」）：服务器拿到的是麦克风和电脑声音混好的一路，线上会议时你和对方一来一回地说，按几秒平滑的音量处理跟不上，只能在混音之前把小了 20 dB 的麦克风单独调上来。
- 转写按句存，每句都有开始和结束时间：搜索命中、点击跳转、播放高亮都精确到句。

## 开发环境

需要 Go 1.26、Node 24 + pnpm、Docker、ffmpeg。

```bash
make dev-api      # 终端 1：接口服务（会顺带起一个开发用的 Postgres）
make dev-worker   # 终端 2：后台处理
make dev-web      # 终端 3：前端，打开 http://127.0.0.1:5173
make test         # 跑全部测试（Go 的测试，加上前端的类型检查和 web/src 下的 *.test.ts）
make help         # 看所有命令
```

## 调识别参数

调识别参数（音量处理、语音检测的阈值、区分说话人用的模型）时，用 `zsmeet transcribe 录音文件` 把一个文件按后台处理完全一样的流程转写一遍，打印每一句和统计（几句、有字的时长、超过 10 秒没有字的空档、分出几位说话人），不连数据库。选项可以换各项参数来比较（`-speakers 8` 是按 8 个人分，和会议上选了说话人数一样），`-h` 看说明；用 docker compose 部署的服务器上，在 `deploy` 目录下执行 `docker compose run --rm --no-deps worker transcribe /data/audio/<会议 ID>.m4a`。服务器上存的录音是回放用的那一份：加了音量处理以后才处理的会议，它已经按回放的处理调过音量，拿它比较「不处理」（`-filter none`）时送去识别的也不是原来的音量。

## 目录

```
cmd/app/            程序入口：serve、worker，以及 models、openapi、reset-link、transcribe 等辅助命令
internal/
  api/              HTTP 接口（huma）。只做取参数、调 service、转成响应
  service/          业务逻辑：账号、成员、会议、纪要版本、团队设置。不认识 HTTP
  pipeline/         后台处理一场会议的全过程，以及卡住时的自动恢复
  asr/              语音识别（sherpa-onnx，cgo）
  minutes/          纪要模板和提示词
  ask/              问会议：提问的提示词、交给大模型的材料、回答里的出处
  llm/              调用 OpenAI 兼容接口
  db/               表结构迁移（migrations/）、SQL（queries/）、sqlc 生成的代码（sqlc/）
  …                 其余是各管一件小事的包：audio、models、search、storage、secret、settings
web/                前端：React + TypeScript + Tailwind（网页版和桌面版共用，不同的地方在 src/platform/）
desktop/            桌面版（macOS，Tauri 2）：本地代理、离线缓存、登录、菜单栏、录音（recorder/ 是 Swift 写的录音小程序）
deploy/             Dockerfile、docker-compose.yml、.env.example
tools/newapi/       make api 用的接口骨架生成器
```

## 加一个接口

```bash
make api NAME=DeleteMeeting METHOD=DELETE ROUTE=/api/meetings/{id}
```

会生成 `internal/api/delete_meeting.go`（输入输出结构体、接口说明、返回 501 的处理函数）和对应的测试，并注册好路由。接下来：

1. 在生成的文件里填请求和返回的字段。这些结构体就是前后端的约定，参数校验和接口文档都从它来。
2. 在 `internal/service` 里写业务逻辑，处理函数里调用它。
3. 补测试，然后 `make gen`：接口文档（`web/openapi.json`）和前端的 TypeScript 类型（`web/src/api/schema.d.ts`）会一起更新。前端用到了改动的字段，编译时就会报错。

改数据库：在 `internal/db/migrations` 里加一个迁移文件，在 `internal/db/queries` 里写 SQL，然后 `make gen`。

## 几条约定

- 接口框架 huma 只出现在 `internal/api` 里，业务逻辑写在 `internal/service`。有测试守着这条规则（`internal/arch`），以后要换框架只需要重写接口层。
- 给用户看的文字（报错、提示）都写成能照着处理的中文，不把内部报错直接抛给用户。
- 参数校验没通过、请求内容不是 JSON 这类进不到处理函数、被 huma 直接挡下的请求，报错也换成了中文（`internal/api/problem.go`）：`detail` 是一句说清楚哪一项不对、怎么改的话（「标题太长了，最多 200 个字」），网页和桌面版直接显示它。加接口时，用户在网页上自己填的字段，要把它在界面上的叫法加进 `fieldNames`，没加的报错里写字段名。升级 huma 以后跑一遍测试：它新加的校验提示还没有中文时，测试会失败。
- 生成的文件（sqlc 的代码、`openapi.json`、`schema.d.ts`）都提交进仓库，`make test` 和 `make check` 会检查它们是不是最新的。
- 前端不依赖 React 的小函数可以在旁边写 `*.test.ts`，用 Node 自带的测试运行（`pnpm -C web test`，`make test` 会跑）；测试里 import 要带 `.ts` 后缀，被测的文件引用别的文件里的值（不只是类型）时也要带。
- 网页里的站内链接用 `AppLink`（`web/src/components/AppLink.tsx`），不直接用 react-router 的 `Link`：用户按住 Option 点 `Link` 时，浏览器会下载一个没用的网页空壳。`make check` 会检查。

## 桌面版

需要 Rust（[rustup](https://rustup.rs)，版本由 `desktop/src-tauri/rust-toolchain.toml` 固定）和上面的 Node + pnpm。

```bash
make desktop-dev        # 打开桌面版窗口，界面（web/）改了自动刷新。先用 make dev-api 起好服务器
make desktop-test       # 录音小程序的测试，cargo fmt、clippy 和 Rust 的测试（不碰钥匙串，用假的服务器），再和真的录音小程序一起录一遍（用测试音频，不要权限），还有发布脚本的测试
make desktop-build      # 构建本机架构的 .app 和 DMG，在 desktop/src-tauri/target/release/bundle/ 下
make desktop-universal  # 构建 Apple 芯片和 Intel 通用的 DMG，和发布时一样（只是不打自动更新用的更新包，那要发布用的密钥）
```

开发模式下（`make desktop-dev`）macOS 不会把 `zsmeet://` 的链接交给它，浏览器登录完跳不回来：要试登录，用 `make desktop-build` 打出来的 App。界面里网页版和桌面版不一样的地方都在 `web/src/platform/`；网页版的构建（`pnpm -C web build`）里没有桌面版的代码。

录音小程序 `zsmeet-recorder` 在 `desktop/recorder/`（Swift，只装 Xcode 的 Command Line Tools 就能编），`desktop/scripts/build-recorder.sh` 构建它（`make desktop-dev`、`make desktop-build` 会自动构建），`desktop/recorder/test.sh` 跑它自己的测试（要 ffmpeg）。开发模式下录音的权限算在终端头上（系统设置里要打开的是终端），要试真的麦克风和电脑声音，用 `make desktop-build` 打出来的 App。

## 发布

每次 push 到 main、每个 PR，GitHub Actions 都会跑一遍 `make check` 和 `make test`（`.github/workflows/ci.yml`）。

发新版本就推一个 v 开头的标签：

```bash
git tag v0.1.0 && git push origin v0.1.0
```

Actions（`.github/workflows/release.yml`）会先跑一遍检查和测试，再分别在 amd64 和 arm64 的机器上构建镜像（识别库通过 cgo 调用，不交叉编译），合成一个多架构镜像推到 `ghcr.io/zanexo/zsmeet`，打上 `0.1.0`、`0.1`、`latest` 三个标签（`v0.2.0-rc.1` 这样带「-」的预发布版本只打它自己的版本号），最后发一个 GitHub Release。在 Actions 页面手动运行 Release 时只试着构建两种架构，不推镜像、不发 Release。

桌面版单独发布，推一个 desktop-v 开头的标签（版本号要和 `desktop/src-tauri/tauri.conf.json`、`Cargo.toml` 里的一致，用 `make desktop-version` 改）：

```bash
make desktop-version VERSION=0.2.0    # 改版本号（Cargo.lock 一起改），提交并合并到 main
git tag -a desktop-v0.2.0 -m "这一版改了什么"    # 标签的说明就是 App 里看到的更新说明
git push origin desktop-v0.2.0
```

Actions（`.github/workflows/desktop.yml`）先核对标签和 App 的版本号，跑完检查和测试，在 macOS 上打一个 Apple 芯片和 Intel 通用的 DMG（ad-hoc 签名，没有苹果的开发者签名，也不做公证），再把 App 打成自动更新用的更新包，用仓库 Secrets 里的更新密钥（`TAURI_SIGNING_PRIVATE_KEY`、`TAURI_SIGNING_PRIVATE_KEY_PASSWORD`）签名，用 App 里的公钥核对过以后生成更新清单 `latest.json`。DMG、更新包、签名和 `latest.json` 发到这个标签的 Release 上（不会顶掉服务器版本的「Latest」），`latest.json` 同时换到固定的 Release「desktop-latest」上：装着旧版本的 App 从那里知道有新版本。`desktop-v0.3.0-rc.1` 这样带「-」的预发布版本只发它自己的 Release，App 不会提示更新到它。

第一次发布之前，先在 Actions 页面选 Desktop → Run workflow 试运行一次（`dry_run` 默认勾着）：照样打包、签名、核对，只把打出来的文件放在这次运行的 Artifacts 里，什么都不发布，用来确认两个 Secrets 配对了。细节（更新清单的格式、密钥丢了怎么办）见[桌面版的设计](desktop.md#发布和自动更新)。平时改了 `desktop/` 或 `web/` 的 PR 也会跑一遍桌面版的检查和测试。

第一次发布后如果别人拉镜像提示没有权限，说明镜像还是私有的：在仓库主页右边的 Packages 里打开 zsmeet，到 Package settings 把 Visibility 改成 Public（只用改一次）。

## 代码审查

PR 由 [CodeAnt AI](https://www.codeant.ai) 自动做 AI 代码审查，审查意见写成 PR 评论。审查要求放在 `.codeant/` 目录里：

- `instructions.json`：审查用中文、意见怎么写。
- `review.json`：按目录检查的项目规矩，比如会议的权限规则、迁移只加不改、后台任务的 run_id。
- `configuration.json`：不审查的文件，也就是生成的代码和锁文件。CodeAnt 的两份文档写法不一样，所以 `review_configuration`（PR 审查用）和 `file_filters`（代码扫描用）都写了。

它是一个 GitHub App，不跑在 Actions 里，只审查 PR，直接 push 到 main 的提交不审查。fork 的仓库要用，得自己在 codeant.ai 装。

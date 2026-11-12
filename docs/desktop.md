# zsmeet 桌面版（macOS）设计

桌面版是 zsmeet 的 macOS 客户端：连上团队自己部署的 zsmeet 服务器，看会议、搜索、问会议，录电脑里会议软件的声音和麦克风。界面打包在 App 里，通过接口取数据；服务器连不上时，打开过的会议照样能看，录音照样能录，恢复后自动上传。

## 决定

- **只做 macOS**，最低 **macOS 14.4**（录系统声音用 Core Audio 的进程音频捕获，权限是「录制系统音频」，不用申请录屏权限）。Apple 芯片和 Intel 都支持（universal）。
- **Tauri 2**：Rust 写本地部分，界面复用网页版（`web/`）的同一套 React 代码，按「桌面版」模式构建打包进 App。网页版加的功能，桌面版重新构建就有。
- **先不签名**：CI 打出未签名（ad-hoc 签名）的 DMG。第一次打开要在 Finder 里右键 →「打开」。签名、公证的步骤在 CI 里预留，配了 Apple 证书的 Secrets 才执行。
- 安卓以后再说。

## 结构

```
desktop/
  src-tauri/        Rust：本地代理、登录、钥匙串、菜单栏、录音管理（src/recorder/）
  recorder/         Swift：录电脑声音和麦克风的小程序 zsmeet-recorder
  scripts/          build-recorder.sh：构建录音小程序，放到 src-tauri/binaries/ 给 Tauri 打包
web/src/platform/   网页版和桌面版不一样的地方（见下）
```

桌面版的界面就是 `web/` 用 `pnpm -C web build --mode desktop` 构建出来的，`VITE_PLATFORM=desktop`。

### 本地代理

Rust 在 App 进程里起一个只监听 `127.0.0.1` 随机端口的 HTTP 代理，界面里所有接口请求都发给它：

- 转发到配置好的服务器，加上 `Authorization: Bearer <App 令牌>`。令牌只在 Rust 这边（存在钥匙串里），界面拿不到。
- 请求要带每次启动随机生成的 `X-Zsmeet-App: <secret>`，没有的拒绝（别的程序、浏览器不能借它冒充你）。播放录音的 `<audio>` 加不了请求头，录音接口允许把 secret 放在查询参数 `_s` 里。
- 只接受来自 App 界面的跨域请求（`tauri://localhost`；开发版另外接受 `tauri dev` 用的 Vite 地址）。别的来源连跨域预检都拒绝。
- 原样转发流式响应和 `Range` 请求（录音能拖动、处理进度的推送能用）。
- 服务器返回 401：这个令牌不能用了（被吊销、账号停用），App 删掉令牌和这个用户的离线缓存，回到登录页并提示重新登录。
- **离线**：会议列表（含搜索）、会议详情、纪要、转写、模板、状态这些 GET 请求的最近一次成功结果存在本地；连不上服务器时返回存下的结果，并加上响应头 `X-Zsmeet-Offline: 1`，界面据此显示「离线，显示的是上次同步的内容」。录音文件不缓存。
  - 只有连不上（连接失败、超时）才算离线，服务器返回的错误照原样交给界面。没存过的请求返回 502，同样带 `X-Zsmeet-Offline: 1`。
  - 存在 App 的缓存目录（`~/Library/Caches/io.github.zanexo.zsmeet/offline/`），每台服务器、每个用户一个子目录，单个响应最多 16 MB，每个用户最多约 256 MB，超了先删最久没更新的。退出登录、切换服务器、登录失效时删掉。

### 登录（App 令牌）

不在 App 里输密码，统一在浏览器里登录（密码、飞书扫码都行），登录完把令牌交回 App：

1. 第一次打开时填服务器地址，App 用 `GET {服务器}/api/status` 检查：要已经初始化，`api` 至少是 1。没写协议时先试 https 再试 http；地址去掉末尾的「/」、查询参数后保存。
   - 检查时跟着同一台主机上的跳转走（换路径，或者 http 换成 https，这时端口可以跟着换），最多 10 次，保存跳转以后的地址；从 https 跳到 http 不接受；跳到别的主机、协议不变却换了端口的不跟，告诉用户新地址，让他确认以后自己填。
   - App 别的请求（换令牌、退出登录、导出、传录音）都带着令牌或者 `code_verifier`，一概不跟随跳转，收到跳转按服务器出错处理。
2. 点「在浏览器里登录」：App 生成 PKCE 的 `code_verifier` 和 `state`，打开 `{服务器}/app/authorize?client=desktop&device=<电脑名>&challenge=<S256(code_verifier)>&state=<state>`。
3. 网页上没登录先登录；登录后显示「允许 zsmeet 桌面版（电脑名）登录你的账号吗？」，点「允许」：网页调用 `POST /api/apps/authorize`，拿到一次性的 `code`，跳到 `zsmeet://auth?code=<code>&state=<state>`；点「取消」跳到 `zsmeet://auth?error=denied&state=<state>`。只允许跳到这一个地址。
4. App 收到这个链接（deep link），核对 `state`，用 `POST /api/apps/token {code, code_verifier}` 换出令牌，存进钥匙串（服务是 App 的 identifier，账户是服务器地址）。`state` 对不上、等了超过 15 分钟、换令牌失败，都提示重新点「在浏览器里登录」。
   - `state` 对上了、网页上点的是「取消」：不再等这次登录，提示是在浏览器里取消的。`state` 对不上的（旧的标签页）和别的旧链接一样不认，正在等的登录照样等着。
   - 换令牌的时候点了取消、重新点了「在浏览器里登录」、退出或者换了服务器：这次登录不算数（每次登录有个编号，换令牌回来时对不上就不用），换到的令牌用 `POST /api/apps/logout` 作废，不存。
   - 设置文件（记着服务器地址和登录的是谁）存不下来：不算登录，删掉钥匙串里的令牌、通知服务器作废，提示原因。不然看起来登录了，重启以后却找不到是谁登录的。填服务器地址存不下来就不换；退出登录照样退出，在登录页上说明。
5. 退出登录：`POST /api/apps/logout`（连不上也照样在本机退出），删掉钥匙串里的令牌和离线缓存。切换服务器 = 退出登录再清掉服务器地址。两个一次只做一个：正在做的时候再点（导航栏、账号页上都能点）不会再做一遍，账号页上这两个按钮也变灰。

### 服务端接口（第一阶段）

- `POST /api/apps/authorize`（要登录）：`{client: "desktop", device_name, code_challenge}` → `{code}`。`code` 只能用一次，2 分钟内有效，和 `code_challenge`、用户绑定。
- `POST /api/apps/token`（不用登录）：`{code, code_verifier}` → `{token, user}`。令牌形如 `zsm_` 加 32 字节随机数（base64url），数据库里只存它的 SHA-256。
- `Authorization: Bearer zsm_…` 在所有要登录的接口上都能用，和网页的登录状态一样有效（包括 `/api/meetings/{id}/audio`）。停用的账号、被吊销的令牌返回 401。
- `GET /api/me/devices`、`DELETE /api/me/devices/{id}`：账号页上列出已登录的 App（设备名、上次使用时间），可以退出某一台。`POST /api/apps/logout`（带令牌）：退出当前这台。
- 改密码、用重置链接设新密码、停用账号时，这个人的 App 令牌全部作废（和网页上其他设备的登录一起退出）。
- 会议来源多一种 `desktop`，界面上显示「桌面版录音」。
- `/api/status` 多一个 `api` 版本号（整数，从 1 开始），App 发现服务器太旧时提示升级服务器。

### 平台差异（`web/src/platform/`）

| | 网页版 | 桌面版 |
|---|---|---|
| 接口地址 | 同源 `/api` | 本地代理，带 `X-Zsmeet-App` |
| 路由 | BrowserRouter | HashRouter |
| 登录 | 登录页、Cookie | 填服务器地址 →「在浏览器里登录」；账号页可以退出、换服务器 |
| 录音的地址 | `/api/meetings/{id}/audio` | 代理地址加 `_s` |
| 进度推送 | `EventSource` | 用 `fetch` 读流（`EventSource` 加不了 `X-Zsmeet-App`） |
| 下载（导出） | 浏览器下载 | 保存对话框（Rust 取回文件，最大 64 MB；先写临时文件再换掉选的文件，写坏了原来的还在。界面不需要文件读写权限） |
| 外部链接 | 新标签页 | 系统浏览器（打不开时弹提示框说明原因） |
| 录音 | 浏览器录音（MediaRecorder） | 原生录音：电脑里的声音和麦克风（见下面的「录音」） |
| 管理员设置 | 有 | 先只放「在网页里打开」的入口 |
| 账号 | 改密码、关联飞书 | 服务器地址、退出登录、切换服务器；改密码、关联飞书「在网页里打开」 |
| 离线 | 无 | 顶上显示「离线」提示条，连上以后自动重新取数据。系统说断网了时界面照常发请求、照常能点（TanStack Query 的 `networkMode: "always"`）：都由 App 来答，录音的暂停、标记、结束和退出登录断网时也能用 |

用哪一边由构建时的 `VITE_PLATFORM` 决定（`--mode desktop` 时是 desktop），是常量，网页版的构建里没有桌面版的代码。

### 录音（第三阶段，已完成）

```
界面（录音页）──invoke──▶ Rust（src-tauri/src/recorder/）──stdin 命令──▶ zsmeet-recorder（Swift）
     ▲                          │    ▲                                           │
     └──── recorder://state ────┘    └──────── stdout 事件、<序号>.aac 分片 ◀──────┘
                                │
                                └── PUT …/chunks/{seq}、PUT …/marks、POST …/complete ──▶ 服务器
```

录音、上传都在 App 里，界面只发命令、显示状态：关掉窗口、切到别的页面录音都不停，菜单栏上也能暂停、标记、结束。

#### 录音小程序 zsmeet-recorder（`desktop/recorder/`）

- 打包在 App 里（`zsmeet.app/Contents/MacOS/zsmeet-recorder`，Tauri 的 `externalBin`），由 App 启动，权限算在 App 头上：弹窗、系统设置里都是 zsmeet。`desktop/scripts/build-recorder.sh` 用 swiftc 把 Apple 芯片和 Intel 各编一份、用 lipo 合成通用的（只装 Command Line Tools 也能编），`tauri dev`、`tauri build` 之前自动执行。打包时 Tauri 用 App 的身份重新签名，带上 hardened runtime 和 `entitlements.plist` 里的 `com.apple.security.device.audio-input`（没有它，麦克风录到的全是静音）。App 的 Info.plist 里有麦克风（`NSMicrophoneUsageDescription`）和录制系统音频（`NSAudioCaptureUsageDescription`）的说明。
- `zsmeet-recorder devices`：标准输出一行 `{"microphones":[{"uid":"…","name":"…","default":true}]}`。只读设备信息，不弹权限窗口。
- `zsmeet-recorder record --out <目录> [--start-seq N] [--chunk-seconds 10] [--mic <uid> | --mic default | --no-mic] [--no-system] [--input-file <音频文件> [--realtime]]`
  - `--input-file` 是测试用的：拿音频文件代替麦克风和电脑声音（不要权限、不弹窗），尽快读完，读完自动结束（和收到 stop 一样）；加 `--realtime` 按文件的实际时长读，用来测暂停、继续。读到一半出错（比如文件被截短）以 error（`input_file`）结束、以 1 退出，不当成读完了。
  - `--start-seq N`：接着录一场中断了的录音时，第一片写成 N。N 只能是 0 到 3999（服务器一场会议最多收 4000 片），别的数按参数不对处理（`bad_args`，以 2 退出）。目录里已经有序号不小于 N 的分片时拒绝开始（`chunk_exists`），不会覆盖；目录列不出里面有什么（比如只能写、不能读）时也不开始（`out_dir`），免得看不到已有的分片、把它们覆盖掉。
- 标准输入一行一个命令：`{"cmd":"pause"}`、`{"cmd":"resume"}`、`{"cmd":"stop"}`，标准输入关掉等于 stop；SIGINT、SIGTERM、SIGHUP 也按 stop 处理。标准输出一行一个事件（日志写在标准错误）：

| 事件 | 说明 |
|---|---|
| `{"event":"started","sample_rate":48000,"channels":1,"mic":"<麦克风名>"或null,"system":true}` | 开始录了。第一次录音时要等用户点完权限弹窗，可能要等很久。`mic`、`system` 只算真的在录的：开始时去掉了的那一路（没有权限、打不开、采样率处理不了）是 null、false |
| `{"event":"level","seconds":12.3,"mic":0.5,"system":0.2}` | 约每 100 毫秒一个：录了多少秒（不算暂停），两路的音量（0 到 1）。暂停时没有 |
| `{"event":"chunk","seq":3,"path":"<绝对路径>","bytes":80000,"seconds":10.005}` | 一片写好了：先写 `<序号>.aac.part`、落盘，改名成 `<序号>.aac` 以后才报告 |
| `{"event":"paused"}`、`{"event":"resumed"}` | 暂停时先把正在写的那一片写完（`paused` 前面紧跟着一个 `chunk`） |
| `{"event":"warning","code":"mic_denied","message":"…"}` | 有一路录不了或者出了问题，另一路照常录。code 是 `mic_denied`、`mic_unavailable`、`system_denied`、`system_unavailable`，message 是给用户看的中文，说了怎么处理。同一个 code 可能先后有几条，说的不是一回事（比如先是没找到选的麦克风、改用了别的，后来这个麦克风又断开了） |
| `{"event":"error","code":"no_source","message":"…"}` | 录不下去了，接着以 1 退出（参数不对是 `bad_args`，以 2 退出） |
| `{"event":"stopped","next_seq":7,"seconds":65.2}` | 停好了，最后一片也写完了，接着以 0 退出 |

- 分片 `<目录>/<序号>.aac`：AAC-LC、48 kHz、单声道、约 64 kbps、ADTS，每片约 10 秒（按录到的声音算，不算暂停的时间），不会有空的分片。分片（包括写着的 `.part`）只有自己能读写（0600）；录音目录要是录音小程序自己建的（包括中间的目录），只有自己能进（0700），已经有的目录不动。
  - 整场录音只建一个编码器，暂停、切分片都不重建，切分片只是把 ADTS 帧分到不同的文件里：所有分片按序号接起来就是一路连续的 ADTS，时长和录到的一样。每片重开编码器的话，每片开头多出约 44 毫秒的预热，一小时差十几秒。编码器开头的预热里够一整帧的部分丢掉，只剩 1.3 毫秒，标记的时间点和转写的时间都对得上。
  - 服务器把分片按序号原样接起来交给 ffmpeg，和网页录音的分片走同一条路。
- 混音：麦克风和电脑声音各自转成 48 kHz 单声道，按时间混成一路。两个设备的时钟不一样快、交数据时快时慢，所以按经过的时间取：每一路先攒 0.15 秒，取的时候不够就补静音，积压超过 0.5 秒丢掉最早的，两路一直对得上。处理线程卡住一阵以后分几次补上，卡住期间录下、等着补上的声音不算积压。点暂停之前采到的声音（包括各路攒着的那 0.15 秒）都编进去，暂停时采到的声音直接丢掉，什么都不编码；继续以后各路重新攒，只编点了继续以后采到的声音，还用同一个编码器。结束时把编码器里剩下的编进最后一片（可能不满 10 秒）。
- 采样率：每一路用一个转换器转成 48 kHz，转换器只支持 8 kHz 到 384 kHz。开始时哪一路的采样率处理不了，就不录这一路，发 warning（`mic_unavailable`、`system_unavailable`，说去「音频 MIDI 设置」里改成 48000 Hz），只录另一路；两路都不行时发 error（`no_source`）。录音中设备的采样率变了跟着换（先挂上监听再读一次、最后才开始录，开始前后变的都跟得上）。转换出错时那一小段丢掉（混音时补成静音），每一路提醒一次（同样是 `mic_unavailable`、`system_unavailable`，说「录音还在继续」），录音不停。
- 权限：麦克风用 AVCaptureDevice 查，还没问过就弹窗问。电脑声音用 Core Audio 的进程音频捕获（`CATapDescription` + `AudioHardwareCreateProcessTap`，放进一个只有本进程看得见的聚合设备）；苹果没有公开查这个权限的接口，用系统 TCC 框架里的 `TCCAccessPreflight`、`TCCAccessRequest` 查和问（将来找不到这两个函数就照常开始，系统第一次录的时候自己弹窗，只是拒绝了的话发现不了，录到的是静音）。一路录不了发 warning、录另一路；两路都录不了发 error（`no_source`）。
- 结束时拆掉 tap 和聚合设备再退出。结束卡住 8 秒（`stop_timeout`）或者结束时又收到一次信号（`killed`）就发 error 直接退出，系统会收回本进程私有的设备。

#### App 里的录音管理（`desktop/src-tauri/src/recorder/`）

- 找录音小程序：环境变量 `ZSMEET_RECORDER`，没设就用 App 主程序旁边的 `zsmeet-recorder`。一次只录一场；stdin 一直开着（关掉就是让它停），stderr 记进日志。
- 结束：点结束、退出 App、换账号时等录音小程序报 `stopped` 退出，最多等 10 秒（它自己最多花 8 秒写完最后一片），还不退出就强制结束。不管是怎么结束的（包括录满了自动结束），只有报了 `stopped` 才算正常结束、传完以后 complete；没报就退出了（它说结束卡住了、出错了，或者被强制结束了），最后一片可能没写完，算中断，不自动 complete，等用户选接着录、直接结束还是删掉。
- 本地数据：`<App 数据目录>/recordings/<会议 id>/`（`~/Library/Application Support/io.github.zanexo.zsmeet/recordings/`，只有自己能进，0700），放 `recording.json` 和录音小程序写的分片。`recording.json` 记着 `meeting_id`、`server`、`user_id`、`title`、`started_at`、`next_seq`（录好了几片）、`uploaded`（从第 0 片起连续传上去了几片）、`marks`、`seconds`、`status`（`recording`、`stopped`、`interrupted`、`completed`、`failed`）、`error`。先写临时文件、落盘（`F_FULLFSYNC`）再改名，目录也落盘；上一份留在 `recording.json.bak`，断电后新的那份读不出来时用它（分片的情况照着目录里的分片补上）。
- 界面能调用的命令（出错时返回给用户看的中文）：

| 命令 | 做什么 |
|---|---|
| `recorder_devices()` | `{ microphones: [{ uid, name, default }] }` |
| `recorder_start({ meetingId, mic, system })` | 会议要先在服务器上建好（`source: "desktop"`）。mic 是麦克风的 uid 或 `"default"`，null 是不录麦克风；麦克风和电脑的声音至少录一个。这台电脑上有这场会议中断了的录音时接着录（分片已经到了服务器能收的 4000 片时不录，提示直接结束）。等录音小程序报了 `started` 才返回（最多等 2 分钟，留给权限弹窗） |
| `recorder_pause()`、`recorder_resume()` | 暂停、继续 |
| `recorder_mark()` | 记下现在录到第几秒（精确到 0.1 秒，1 秒内连着点只算一次，最多 500 个），顺带把整份标记存到服务器（`PUT …/marks`，存不上也没关系，complete 时一起交） |
| `recorder_stop()` | 结束，等最后一片写完。都传完以后 `POST …/complete {chunks: next_seq, marks}` |
| `recorder_finish({ meetingId })` | 中断了的录音不接着录了：传完录好的、complete（中断前最后一片之后点的标记不要，服务器上存过的也换掉）。和接着录一样先问服务器收到了哪几片（`GET …/chunks`，最多等 5 秒）：complete 的片数、时长都算上服务器上、这台电脑上已有的分片；连不上、回复看不懂时照这台电脑上记着的来；401 退出登录，403、404 返回服务器说的原因、录音不动。传不上去的也可以用它再试一次 |
| `recorder_discard({ meetingId })` | 删掉这台电脑上一段中断了的、传不上去的录音（服务器上已经收到的不动） |
| `recorder_state()` | 现在的状态 |

- 事件 `recorder://state`：状态一变就发，录音时随音量约每 100 毫秒发一次。内容（各个命令的返回值也是它）：`{ status, meetingId, title, seconds, level: { mic, system } 或 null, mic, system, marks, warnings: [{ code, message }], error, uploads: [{ meetingId, title, recorded, uploaded, status, error }] }`。
  - `status`：`idle`、`starting`（在等录音小程序开始，可能在等权限弹窗）、`recording`、`paused`、`stopping`（在等最后一片写完）。
  - `warnings`：有一路录不了或者出了问题的提醒，同一个 code 说的事不一样时都留着（一模一样的只留一条）；没能开始录音时留着录音小程序说的原因，下次开始录音时清掉。`mic`：在录的麦克风的名字，录到一半断开了还是它的名字（`warnings` 里有提醒）。`error`：上一场不正常结束的原因（中断了、录满 6 小时自动结束了）。
  - `uploads`：当前账号在这台电脑上的录音，`status` 是 `recording`（边录边传）、`uploading`、`waiting`（暂时传不上去，过一会儿自动再试）、`completing`、`completed`（App 重启前一直留着）、`interrupted`、`failed`。录音时就传不上去了（会议被删了）也是 `failed`，录音照常进行。
- 接着录：从 `recording.json` 的 `next_seq`、目录里已有分片的下一片、服务器已经收到的分片（`GET …/chunks`）的下一片里取最大的开始写，哪一片都不会被覆盖。秒数和标记接着上次的：新标记的时间从已经写好的分片的总时长算起，和服务器接起来的录音对得上；中断前最后一片之后点的标记，对应的声音没存下来，不要了。`recording.json` 记少了、跳过了几片时（比如它退回了上一份），跳过的分片也算进时长：还在这台电脑上的照着分片算，只在服务器上的按每片 10 秒算。
  - 问不到服务器（连不上、出错、回复看不懂）时照这台电脑上记着的来，也不会占用服务器上已有的序号：一片传上去以后，要等记着它的 `recording.json` 存好了才删本地的文件，所以服务器上的每一片，要么 `next_seq` 已经算上了，要么文件还在这台电脑上。问服务器是多一道保险。
- 上传：后台一直在跑，只传当前登录的账号（同一个服务器地址、同一个用户 id）的录音，用它的令牌；别的账号的录音留在本地、界面上也不显示。按开始录音的先后，每场按序号一片一片地 `PUT /api/meetings/{id}/chunks/{seq}`（`Content-Type: audio/aac`，body 是分片本身），记下传到哪一片以后再删本地的文件。
  - 连不上、超时、5xx、408、429、回复看不懂（2xx 却不是 JSON 或者没收全，比如连的网络要先登录、中间的设备返回了一个网页；`GET …/chunks` 的回复不是 `{"received":[…]}`）：这一场等一会儿再试（1、2、5、10、20、30 秒，之后每 30 秒），不耽误别的，分片也不删；登录的账号一变就马上再试。
  - 401：App 退出登录，同一个账号重新登录以后接着传。录音不停。
  - 403、404、其他 4xx、分片返回 409（会议已经上传完成）：不再传，算失败，等用户删掉。
  - complete 返回 409：看一眼会议，已经不在 `uploading` 就算完成（上次其实成功了），否则算失败，显示服务器说的原因。完成以后删掉整个目录。
  - App 重启以后接着传；状态还是 `recording` 的（录音小程序已经不在了）算中断，中断的录音先把录好的传着，等用户选接着录、直接结束还是删掉。
  - 录音的电脑找不回来了（丢了、坏了、一直连不上网）：会议的创建者或管理员可以在会议页面上（网页上，或者另一台电脑的 zsmeet 里）先用服务器已经收到的部分生成纪要，和网页录音一样：`GET …/chunks` 看收到了哪几片，`POST …/complete {chunks: 从第 0 片起连续收到的片数}`，不带标记（录音时存下的照样用）。最近 90 秒里还收到过分片的不给这样做，只说正在录：多久没收到看会议的 `upload_idle_sec`，是服务器按数据库的时钟算好的，不拿看会议的这台电脑的时钟去减 `last_chunk_at`（服务器没有自动对时的话，两边的时钟可能差好几分钟）。会议因此不在 `uploading` 以后，那台电脑再传分片也是上面说的 409（服务器说「这场会议已经结束录音、开始处理了，剩下的录音传不上去了」），这段录音算失败，等用户删掉；那台电脑上已经都传完、只差 complete 的，看到会议已经不在 `uploading`，算完成。
- 最长 6 小时（服务器单场最多处理 6 小时）：录到 5 小时 58 分自动结束，录好的照常上传。
- 账号：退出登录、切换服务器会先结束正在录的那一场（界面上先确认），换成别的账号登录时也结束（结束之前，新登录的人也只看得到这台电脑在录音，看不到是哪场会议、录了多久）；录音留在本地，只等原来的账号登录。
- 菜单栏：录音时图标旁边显示录了多久（每秒更新，暂停时加「已暂停」），菜单里有 暂停/继续、标记重点、结束录音，还有没传完、需要处理的录音。关掉窗口录音不停；退出 App（菜单栏的「退出」、⌘Q、关机）时先让录音小程序结束，和点了结束一样最多等 10 秒写完最后一片（它自己最多花 8 秒），还不退出就强制结束，这场录音算中断；没传完的下次打开接着传。

#### 界面（`web/src/platform/`，只在桌面版里）

- 录音页 `/record`（`DesktopRecord.tsx`，不是网页录音的 MediaRecorder）：开始前填标题、模板、本场热词、谁能看（和网页录音一样），选麦克风（或者不录）、录不录电脑里的声音（说明是录飞书、腾讯会议这些会议软件里其他人的声音）。开始时先通过平常的接口客户端（本地代理）`POST /api/meetings {title, source: "desktop", template, hotwords, visibility}`，再 `recorder_start`；开始不了、或者一片都没录下来就结束了，删掉刚建的空会议。录音中：时长、两路音量、暂停/继续、标记重点（显示个数）、结束录音（要确认）、提醒（没有权限时说去哪里打开）。结束以后：上传进度，传完给会议链接。
- 别的页面（`DesktopRecordings.tsx`）：录音时顶上一条（时长、回到录音），没传完的录音一行一个（接着录、传上去生成纪要、删除）；会议页面上，这场会议正在这台电脑上录、在传时显示出来。这台电脑上没有这场会议的录音时（别的电脑录的，或者这台电脑上的已经删了），和网页版一样显示服务器收到了多少，录音的电脑找不回来时可以用已经收到的部分生成纪要（`MeetingDetail.tsx` 的 `ReceivedSoFar`）。
- 网页版的构建里没有这些代码。

#### 测试

- `desktop/recorder/test.sh`：录音小程序的单元测试（事件格式、ADTS、分片、混音、编码、暂停）和端到端测试（真的跑 zsmeet-recorder，用 `--input-file`，ffmpeg、ffprobe 检查分片能解码、接起来的时长对得上）。每次运行用自己的临时目录，几个工作区同时跑互不影响（同一个工作区里的 `build-recorder.sh`、`test.sh` 不要同时跑：它们会删掉重建 `.build/` 下的目录）。`ZSMEET_RECORDER_TEST_FILTER=暂停 desktop/recorder/test.sh` 只跑名字里有「暂停」的测试。
- `cargo test`：假的录音小程序（一个 sh 脚本，照协议说话）加假的服务器，测录音、上传、断网、401、中断和接着录、换账号。
- `real_helper` 开头的 Rust 测试（`#[ignore]`）：真的录音小程序（`ZSMEET_RECORDER`，`--input-file`）加 App 的录音管理和假的服务器，录一遍、暂停、标记、结束、传完，用 ffprobe 检查服务器收到的分片接起来能完整解码，时长和原来的声音相差不到 50 毫秒。`make desktop-test` 和 CI 都会跑。
- 没法自动测的：真的麦克风、电脑声音和权限弹窗。用打包出来的 App（`make desktop-build`）试：第一次录音时两个弹窗都写着 zsmeet，说话时麦克风的音量条会动，放视频、开会时电脑声音的音量条会动，录出来两路都有。开发模式（`make desktop-dev`）下权限算在终端头上。

## 分阶段

1. **服务端**：App 令牌、授权页、设备管理、`desktop` 来源、`api` 版本号。
2. **桌面外壳**：Tauri 项目、本地代理和离线缓存、登录、钥匙串、菜单栏、平台差异、CI 打包。
3. **录音**：Swift 录音程序、上传、录音页和菜单栏的录音控制。已完成，真的麦克风和电脑声音还要在真机上试（见上面的「测试」）。
4. **收尾**：审查、在真机上试、文档、发布流程。

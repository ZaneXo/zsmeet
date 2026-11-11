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
  src-tauri/        Rust：本地代理、登录、钥匙串、菜单栏、录音管理（第三阶段）
  recorder/         Swift：录系统声音和麦克风的小程序（第三阶段）
web/src/platform/   网页版和桌面版不一样的地方（见下）
```

桌面版的界面就是 `web/` 用 `pnpm -C web build --mode desktop` 构建出来的，`VITE_PLATFORM=desktop`。

### 本地代理

Rust 在 App 进程里起一个只监听 `127.0.0.1` 随机端口的 HTTP 代理，界面里所有接口请求都发给它：

- 转发到配置好的服务器，加上 `Authorization: Bearer <App 令牌>`。令牌只在 Rust 这边（存在钥匙串里），界面拿不到。
- 请求要带每次启动随机生成的 `X-Zsmeet-App: <secret>`，没有的拒绝（别的程序、浏览器不能借它冒充你）。播放录音的 `<audio>` 加不了请求头，录音接口允许把 secret 放在查询参数 `_s` 里。
- 只接受来自 App 界面的跨域请求（`tauri://localhost`）。
- 原样转发流式响应和 `Range` 请求（录音能拖动、处理进度的推送能用）。
- **离线**：会议列表、会议详情、纪要、转写、模板、状态这些 GET 请求的最近一次成功结果存在本地；连不上服务器时返回存下的结果，并加上响应头 `X-Zsmeet-Offline: 1`，界面据此显示「离线，显示的是上次同步的内容」。录音文件不缓存。

### 登录（App 令牌）

不在 App 里输密码，统一在浏览器里登录（密码、飞书扫码都行），登录完把令牌交回 App：

1. 第一次打开时填服务器地址，App 用 `GET {服务器}/api/status` 检查。
2. 点「在浏览器里登录」：App 生成 PKCE 的 `code_verifier` 和 `state`，打开 `{服务器}/app/authorize?client=desktop&device=<电脑名>&challenge=<S256(code_verifier)>&state=<state>`。
3. 网页上没登录先登录；登录后显示「允许 zsmeet 桌面版（电脑名）登录你的账号吗？」，点「允许」：网页调用 `POST /api/apps/authorize`，拿到一次性的 `code`，跳到 `zsmeet://auth?code=<code>&state=<state>`。只允许跳到这一个地址。
4. App 收到这个链接（deep link），核对 `state`，用 `POST /api/apps/token {code, code_verifier}` 换出令牌，存进钥匙串。

### 服务端接口（第一阶段）

- `POST /api/apps/authorize`（要登录）：`{client: "desktop", device_name, code_challenge}` → `{code}`。`code` 只能用一次，2 分钟内有效，和 `code_challenge`、用户绑定。
- `POST /api/apps/token`（不用登录）：`{code, code_verifier}` → `{token, user}`。令牌形如 `zsm_` 加 32 字节随机数（base64url），数据库里只存它的 SHA-256。
- `Authorization: Bearer zsm_…` 在所有要登录的接口上都能用，和网页的登录状态一样有效（包括 `/api/meetings/{id}/audio`）。停用的账号、被吊销的令牌返回 401。
- `GET /api/me/devices`、`DELETE /api/me/devices/{id}`：账号页上列出已登录的 App（设备名、上次使用时间），可以退出某一台。`POST /api/apps/logout`（带令牌）：退出当前这台。
- 改密码、用重置链接设新密码、停用账号时，这个人的 App 令牌全部作废（和网页上其他设备的登录一起退出）。
- 会议来源多一种 `desktop`（桌面版录音），界面上显示「电脑录音」。
- `/api/status` 多一个 `api` 版本号（整数，从 1 开始），App 发现服务器太旧时提示升级服务器。

### 平台差异（`web/src/platform/`）

| | 网页版 | 桌面版 |
|---|---|---|
| 接口地址 | 同源 `/api` | 本地代理，带 `X-Zsmeet-App` |
| 路由 | BrowserRouter | HashRouter |
| 登录 | 登录页、Cookie | 填服务器地址 →「在浏览器里登录」；账号页可以退出、换服务器 |
| 录音的地址 | `/api/meetings/{id}/audio` | 代理地址加 `_s` |
| 下载（导出） | 浏览器下载 | 保存对话框 |
| 外部链接 | 新标签页 | 系统浏览器 |
| 录音 | 浏览器录音（MediaRecorder） | 原生录音（第三阶段） |
| 管理员设置 | 有 | 先只放「在网页里打开」的入口 |

### 录音（第三阶段）

- Swift 小程序 `zsmeet-recorder`：Core Audio 进程音频捕获录系统声音、AVAudioEngine 录麦克风，混成一路，编码成 AAC（ADTS），每 10 秒落一个分片到本地目录；支持暂停、继续、标记重点、电平。
- Rust 管理它，把分片按序号上传到 `PUT /api/meetings/{id}/chunks/{seq}`，断网就留在本地，恢复后接着传，App 重启也能接着传；结束时 `POST …/complete`。
- 菜单栏：录音时长、暂停、标记、结束。关掉窗口录音不停。

## 分阶段

1. **服务端**：App 令牌、授权页、设备管理、`desktop` 来源、`api` 版本号。
2. **桌面外壳**：Tauri 项目、本地代理和离线缓存、登录、钥匙串、菜单栏、平台差异、CI 打包。
3. **录音**：Swift 录音程序、上传、录音页和菜单栏的录音控制。
4. **收尾**：审查、在真机上试、文档、发布流程。

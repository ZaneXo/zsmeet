# 部署和运维

## 准备

需要一台装了 Docker 的服务器（x86 的 Linux，或者 Apple 芯片的 Mac 装 Docker Desktop / OrbStack），建议 4 核 CPU、8 GB 内存以上。

## 安装

```bash
git clone https://github.com/ZaneXo/zsmeet.git && cd zsmeet/deploy
cp .env.example .env
# 编辑 .env，填两个密码（文件里有生成方法）
docker compose up -d --build
```

不想在服务器上构建的话，可以用发布好的镜像（同一个镜像里有 linux/amd64 和 linux/arm64）：在 `.env` 里加一行 `ZSMEET_IMAGE=ghcr.io/zanexo/zsmeet:0.1.0`（换成 [Releases](https://github.com/ZaneXo/zsmeet/releases) 里最新的版本号），再用 `docker compose pull && docker compose up -d --no-build` 代替上面最后一行。

## 第一次打开

装好以后用浏览器打开 `http://服务器地址:8080`：

1. 第一次打开会让你创建管理员账号。
2. 进入「设置」，填写大模型的接口地址、模型名和 API Key，点「测试连接」确认能用后保存。
   任何 OpenAI 兼容的接口都可以。不配置也能用，只是只有转写、没有纪要。
   「上下文长度」选模型一次能读多少（看模型的说明，默认 32K；Ollama 的本地模型没调大 `num_ctx` 的话一般只有 8K 到 32K）。
   长会议放不进一次请求时，先分段提取要点再汇总成纪要：32K 时每段约 2 万字，8K 约 3000 字，64K 约 5 万字，再长也最多 6 万字一段（一次塞太多，本地模型要算很久）。
   各段要点合起来也放不下时（上下文小、会又特别长），先把相邻几段的要点合并精简，一层层合到放得下再汇总，所以会多调用几次大模型。
   「问会议」交给大模型的材料也按它算，32K 时约 1.2 万字。选得比模型实际的大，长会议的纪要可能写不出来或者漏掉内容。
3. 回到「会议」，开始录音，或者上传一段录音。
4. 进入「设置 → 成员」邀请同事（见[使用说明 → 成员](usage.md#成员)）。

第一次处理会议时，后台会下载约 300 MB 的识别模型（来自 Hugging Face，连不上时自动换国内镜像），之后不用再等。

## 升级

在 `zsmeet/deploy` 目录下执行 `git pull && docker compose up -d --build`，数据库会自动迁移。用发布好的镜像的话，把 `.env` 里 `ZSMEET_IMAGE` 的版本号改成新的，再执行 `docker compose pull && docker compose up -d --no-build`。

## 备份

数据在三个 Docker 数据卷里。`zsmeet_pgdata` 是数据库，`zsmeet_data` 是录音，这两个要备份；`zsmeet_models` 是识别模型，丢了会重新下载。另外请保管好 `.env` 里的 `ZSMEET_SECRET_KEY`，没有它就解不开数据库里保存的 API Key。

## HTTPS 和反向代理

浏览器只允许 HTTPS 网页使用麦克风，所以要用网页录音，就得在前面加一层 HTTPS 反向代理（Caddy、Nginx 等），并把 `.env` 里的 `ZSMEET_SECURE_COOKIE` 设为 `true`。只用 `http://服务器地址:8080` 打开的话，上传录音、查看纪要都正常，只是不能录音（在服务器本机上用 `http://localhost:8080` 打开可以录）。

反向代理要把访问者的 IP 用 `X-Forwarded-For` 带过来（Caddy 默认就带；Nginx 要加 `proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;`），还要在 `.env` 的 `ZSMEET_TRUSTED_PROXIES` 里写上代理连过来的地址，zsmeet 才按真实的访问者 IP 限制输错密码的次数。默认只信本机（`127.0.0.1`、`::1`）。用 docker compose 部署、代理装在同一台机器上时，代理是从 Docker 网桥的网关连进容器的：把网关的地址（`docker network inspect zsmeet_default` 里的 `Gateway`，比如 `172.18.0.1`）写进 `ZSMEET_TRUSTED_PROXIES`，同时设 `ZSMEET_PORT=127.0.0.1:8080`，让 zsmeet 的端口只有本机的代理能连，别人绕过代理直接连端口就没法假装成别的 IP。没配好时（内网地址带着 `X-Forwarded-For` 连过来、却不在名单里），zsmeet 不按 IP 限制、只按账号限制，并在日志里提醒一次；外网地址发来的 `X-Forwarded-For` 一律不信。

## 安全

在第一个人创建管理员之前，任何能访问这个地址的人都可以成为管理员，所以部署后请尽快完成初始化。对公网开放时一定要用 HTTPS（见上一条）。

## 管理员都登录不了时

没有管理员能登录了怎么办（唯一的管理员忘了密码，或者离职没交接）：在服务器上执行下面的命令。

```bash
cd zsmeet/deploy
docker compose exec api zsmeet reset-link 某人的邮箱   # 给这个账号生成重置密码的链接
docker compose exec api zsmeet make-admin 某人的邮箱   # 把这个账号设为管理员
docker compose exec api zsmeet unlock 某人的邮箱       # 解开这个账号的登录锁（密码输错太多次被锁住时）
```

## 处理要多久

识别在 CPU 上跑，不需要显卡。一组参考数据（Apple Silicon 笔记本上的 Docker，4 个线程，一段 20 分钟的两人对话）：

| 步骤 | 用时 |
|---|---|
| 接收与转码 | 9 秒 |
| 识别文字 | 13 秒 |
| 区分说话人 | 46 秒 |
| 合计（不含写纪要） | 约 1 分 10 秒 |

写纪要的时间取决于你用的大模型。普通的 x86 服务器会比这慢。

内存：worker 加载模型后常驻约 1 GB。另外一场录音会整段读进内存，每小时约占 230 MB，所以单场上限是 6 小时。

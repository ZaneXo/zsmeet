SHELL := /bin/bash
.DEFAULT_GOAL := help

# 开发和测试用的 Postgres 跑在 Docker 里，只监听本机
DEV_PG      := zsmeet-dev-pg
DEV_DB_URL  := postgres://postgres:dev@127.0.0.1:55432/zsmeet?sslmode=disable
TEST_DB_URL := postgres://postgres:dev@127.0.0.1:55432/postgres?sslmode=disable
SQLC_IMAGE  := sqlc/sqlc:1.30.0

# 本地运行时的配置。数据和模型放在 .dev/ 下，不进版本库
DEV_ENV := ZSMEET_DATABASE_URL='$(DEV_DB_URL)' \
           ZSMEET_SECRET_KEY=dev-secret-key-0123456789 \
           ZSMEET_DATA_DIR=.dev/data \
           ZSMEET_MODELS_DIR=.dev/models

.PHONY: help
help: ## 列出所有命令
	@grep -hE '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  make %-18s %s\n", $$1, $$2}'

# ---- 开发 ----

.PHONY: dev-db
dev-db: ## 启动开发用的 Postgres（Docker，端口 55432）
	@docker start $(DEV_PG) >/dev/null 2>&1 || \
		docker run -d --name $(DEV_PG) -e POSTGRES_PASSWORD=dev -e POSTGRES_DB=zsmeet -p 127.0.0.1:55432:5432 postgres:16 >/dev/null
	@until docker exec $(DEV_PG) pg_isready -q -U postgres; do sleep 0.5; done

.PHONY: dev-api
dev-api: dev-db ## 启动接口服务，监听 http://127.0.0.1:8080
	$(DEV_ENV) go run ./cmd/app serve

.PHONY: dev-worker
dev-worker: dev-db ## 启动后台处理（第一次会下载约 560 MB 的识别模型）
	$(DEV_ENV) go run ./cmd/app worker

.PHONY: dev-web
dev-web: ## 启动前端开发服务器，打开 http://127.0.0.1:5173
	pnpm -C web install
	pnpm -C web dev

# ---- 生成代码 ----

.PHONY: gen
gen: ## 重新生成数据库查询代码、接口文档、前端类型（改了 SQL 或接口以后运行）
	docker run --rm -v "$(CURDIR)":/src -w /src $(SQLC_IMAGE) generate
	go run ./cmd/app openapi > web/openapi.json
	pnpm -C web gen:api

.PHONY: api
api: ## 生成新接口的骨架：make api NAME=DeleteMeeting METHOD=DELETE ROUTE=/api/meetings/{id}
	@go run ./tools/newapi -name='$(NAME)' -method='$(METHOD)' -route='$(ROUTE)'

# ---- 检查 ----

.PHONY: test
test: dev-db ## 跑全部测试（需要 Docker 和 ffmpeg）
	ZSMEET_TEST_DATABASE_URL='$(TEST_DB_URL)' go test ./...
	pnpm -C web typecheck
	pnpm -C web test

.PHONY: check
check: ## 提交前检查：代码格式、静态检查、生成的前端类型是否最新
	@files="$$(gofmt -l cmd internal tools web/embed.go web/placeholder.go)"; \
		if [ -n "$$files" ]; then echo "这些文件需要 gofmt："; echo "$$files"; exit 1; fi
	go vet ./...
	@# 重新生成到一个被忽略的缓存位置，和现有的文件逐字节比较。不看 git 的状态：还没 git add 的正确改动不该报错
	@mkdir -p web/node_modules/.cache
	@# 生成器自己出错时（没装依赖、openapi.json 写坏了）把它的报错原样显示出来，不要说成「不是最新的」
	@pnpm -C web exec openapi-typescript openapi.json -o node_modules/.cache/schema.check.d.ts >/dev/null
	@cmp -s web/node_modules/.cache/schema.check.d.ts web/src/api/schema.d.ts || \
		{ echo "web/src/api/schema.d.ts 不是最新的，请运行 make gen 后一起提交"; exit 1; }
	@if grep -rnE 'import \{[^}]*[{ ,]Link[ ,}][^}]*\} from "react-router"' web/src | grep -v '^web/src/components/AppLink.tsx:'; then \
		echo "上面这些地方直接用了 react-router 的 Link，站内链接请改用 web/src/components/AppLink.tsx 里的 AppLink"; exit 1; fi

# ---- 构建 ----

.PHONY: build
build: ## 构建带网页的程序到 bin/zsmeet
	pnpm -C web install --frozen-lockfile
	pnpm -C web build
	go build -tags embedweb -o bin/zsmeet ./cmd/app

.PHONY: docker
docker: ## 构建 Docker 镜像 zsmeet:local（先把 deploy/.env.example 复制成 deploy/.env 并填好）
	docker compose -f deploy/docker-compose.yml build

# ---- 桌面版（macOS，desktop/，见 docs/desktop.md）----
# 需要 Rust（rustup）。界面是 web/ 按桌面版模式构建的（pnpm -C web build --mode desktop），录音小程序
# zsmeet-recorder（Swift，desktop/recorder/）由 desktop/scripts/build-recorder.sh 构建，Tauri 构建前都会自动做

.PHONY: desktop-dev
desktop-dev: ## 开发桌面版：打开 App 窗口，界面改了自动刷新
	pnpm -C desktop install --frozen-lockfile
	pnpm -C desktop tauri dev

.PHONY: desktop-build
desktop-build: ## 构建桌面版的 .app 和 DMG（本机的架构，ad-hoc 签名，不打自动更新用的更新包）
	pnpm -C desktop install --frozen-lockfile
	pnpm -C desktop tauri build

.PHONY: desktop-universal
desktop-universal: ## 构建 Apple 芯片和 Intel 通用的 DMG（和发布时一样，只是不打更新包：那要发布用的密钥，只在 GitHub 上有）
	rustup target add aarch64-apple-darwin x86_64-apple-darwin
	pnpm -C desktop install --frozen-lockfile
	pnpm -C desktop tauri build --target universal-apple-darwin

.PHONY: desktop-version
desktop-version: ## 发布前改桌面版的版本号：make desktop-version VERSION=0.2.0（tauri.conf.json、Cargo.toml、Cargo.lock 一起改）
	node desktop/scripts/set-version.mjs '$(VERSION)'
	@# 只更新 Cargo.lock 里 zsmeet-desktop 自己的版本，依赖一个都不动（发布时用 --locked 构建，没更新会失败）。
	@# 先不联网试；这台电脑上还没下载过依赖的索引时再联网
	cd desktop/src-tauri && { cargo update --workspace --offline || cargo update --workspace; }

.PHONY: desktop-test
desktop-test: ## 桌面版的检查和测试：录音小程序的测试，cargo fmt、clippy、cargo test，和真的录音小程序一起录一遍（要 ffmpeg），发布脚本的测试
	pnpm -C web install --frozen-lockfile
	pnpm -C web build --mode desktop
	@# 发布用的脚本：生成自动更新清单、核对更新包签名（见 .github/workflows/desktop.yml），改版本号
	node --test desktop/scripts/*.test.mjs
	@# 先构建录音小程序（Rust 编译时要它已经在 desktop/src-tauri/binaries/ 下），再跑它自己的测试
	desktop/recorder/test.sh
	cd desktop/src-tauri && cargo fmt --check && cargo clippy --all-targets --locked -- -D warnings && cargo test --locked
	@# 真的录音小程序（用测试音频代替麦克风和电脑声音，不要权限）+ App 的录音管理 + 假的服务器
	cd desktop/src-tauri && ZSMEET_RECORDER="$(CURDIR)/desktop/src-tauri/binaries/zsmeet-recorder-universal-apple-darwin" \
		cargo test --locked real_helper -- --ignored

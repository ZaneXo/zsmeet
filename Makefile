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
	@grep -hE '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  make %-11s %s\n", $$1, $$2}'

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

.PHONY: check
check: ## 提交前检查：代码格式、静态检查、生成的前端类型是否最新
	@files="$$(gofmt -l cmd internal tools web/embed.go web/placeholder.go)"; \
		if [ -n "$$files" ]; then echo "这些文件需要 gofmt："; echo "$$files"; exit 1; fi
	go vet ./...
	pnpm -C web gen:api >/dev/null
	@git diff --quiet -- web/src/api/schema.d.ts || \
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

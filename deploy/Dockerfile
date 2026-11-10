# syntax=docker/dockerfile:1
#
# 一个镜像，两种启动方式：zsmeet serve（网页和接口）、zsmeet worker（后台处理）。
# 国内构建慢的话，可以在 deploy/.env 里设置 GOPROXY、NPM_REGISTRY、APT_MIRROR。

# ---- 前端 ----
FROM node:24-bookworm-slim AS web
ARG NPM_REGISTRY=https://registry.npmjs.org
ENV COREPACK_NPM_REGISTRY=$NPM_REGISTRY
WORKDIR /src/web
RUN corepack enable
COPY web/package.json web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile --registry=$NPM_REGISTRY
COPY web/ ./
RUN pnpm build

# ---- 后端 ----
# 识别库 sherpa-onnx 通过 cgo 调用，所以要用带 C 编译器的镜像，不能交叉编译
FROM golang:1.26-bookworm AS build
ARG GOPROXY=https://proxy.golang.org,direct
ARG VERSION=dev
ENV GOPROXY=$GOPROXY CGO_ENABLED=1
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY web/embed.go web/placeholder.go ./web/
COPY --from=web /src/web/dist ./web/dist
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -tags embedweb -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /out/zsmeet ./cmd/app \
 && lib="$(go list -m -f '{{.Dir}}' github.com/k2-fsa/sherpa-onnx-go-linux)/lib/$(uname -m)-unknown-linux-gnu" \
 && mkdir /out/lib \
 && cp "$lib/libsherpa-onnx-c-api.so" "$lib/libonnxruntime.so" /out/lib/

# ---- 运行 ----
FROM debian:bookworm-slim
ARG APT_MIRROR=deb.debian.org
RUN sed -i "s|deb.debian.org|$APT_MIRROR|g" /etc/apt/sources.list.d/debian.sources \
 && apt-get update \
 && apt-get install -y --no-install-recommends ffmpeg ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --uid 10001 --no-create-home zsmeet \
 && mkdir -p /data /models \
 && chown zsmeet:zsmeet /data /models
COPY --from=build /out/lib/ /usr/local/lib/
RUN ldconfig
COPY --from=build /out/zsmeet /usr/local/bin/zsmeet

ENV ZSMEET_DATA_DIR=/data \
    ZSMEET_MODELS_DIR=/models \
    ZSMEET_LISTEN=:8080
USER zsmeet
EXPOSE 8080
VOLUME ["/data", "/models"]
ENTRYPOINT ["zsmeet"]
CMD ["serve"]

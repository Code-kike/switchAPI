# switchapi-hub 镜像：多阶段 —— SPA 构建 → Go 构建（embed 前端）→ 运行层。
# 运行层带 ca-certificates（LiteLLM 每日价格同步走 TLS）。权威数据全在 /data 卷。

FROM node:22-alpine AS web
RUN npm install -g pnpm@9
WORKDIR /src/web
COPY web/package.json web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm build

FROM golang:1.26-alpine AS build
# 网络受限环境可覆盖：docker build --build-arg GOPROXY=https://goproxy.cn,direct .
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist internal/hub/webui/dist
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/Code-kike/switchAPI/internal/shared/version.Version=${VERSION}" \
    -o /out/switchapi-hub ./cmd/hub

FROM alpine:3.21
RUN apk add --no-cache ca-certificates \
    && adduser -D -H -u 1000 switchapi \
    && mkdir -p /data && chown switchapi /data
COPY --from=build /out/switchapi-hub /usr/local/bin/switchapi-hub
USER switchapi
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["switchapi-hub", "-listen", ":8080", "-data", "/data"]

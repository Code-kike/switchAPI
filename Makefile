# switchAPI 构建入口 —— 各目标与 CI（.github/workflows/ci.yml）保持一致。
# 版本注入：make build VERSION=v0.1.0（默认 dev）。

MODULE  := github.com/Code-kike/switchAPI
VERSION ?= dev
LDFLAGS := -X $(MODULE)/internal/shared/version.Version=$(VERSION)
DIST    := dist

# 交叉编译矩阵（与 CI matrix 一致）
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.PHONY: build test vet cross clean

## build: 构建当前平台的 hub 与 agent 二进制到 dist/
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/switchapi-hub ./cmd/hub
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/switchapi-agent ./cmd/agent

## test: 运行全部测试
test:
	go test ./...

## vet: 静态检查（golangci-lint 配置落地前以 go vet 为准）
vet:
	go vet ./...

## cross: 三平台 × 双架构交叉编译（CGO_ENABLED=0），产物在 dist/<os>-<arch>/
cross:
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; ext=""; \
		if [ "$$os" = "windows" ]; then ext=".exe"; fi; \
		echo "==> $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" \
			-o $(DIST)/$$os-$$arch/switchapi-hub$$ext ./cmd/hub || exit 1; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" \
			-o $(DIST)/$$os-$$arch/switchapi-agent$$ext ./cmd/agent || exit 1; \
	done

## clean: 清理构建产物
clean:
	rm -rf $(DIST)

// Package version 提供构建期注入的版本信息，供 Hub 与 Agent 共用。
package version

// Version 是当前构建的版本号。
// 发布构建通过 -ldflags 注入，例如：
//
//	go build -ldflags "-X github.com/Code-kike/switchAPI/internal/shared/version.Version=v0.1.0" ./cmd/hub
//
// 未注入时保持开发期默认值 "dev"。
var Version = "dev"

// switchapi-agent —— 本地代理（Agent）入口。
//
// 子命令见 internal/agent/cli；--version 保持独立处理。
package main

import (
	"fmt"
	"os"

	"github.com/Code-kike/switchAPI/internal/agent/cli"
	"github.com/Code-kike/switchAPI/internal/shared/version"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-version") {
		fmt.Println("switchapi-agent " + version.Version)
		return
	}
	os.Exit(cli.Main(os.Args[1:]))
}

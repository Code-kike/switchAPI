// switchapi-hub —— 中心服务（Hub）入口。
//
// M0 骨架：仅支持 --version 与一行启动占位输出；
// 真实服务逻辑（存储、切换裁决、配置分发、Web 托管）自 M1 起实现。
package main

import (
	"flag"
	"fmt"

	"github.com/Code-kike/switchAPI/internal/shared/version"
)

func main() {
	showVersion := flag.Bool("version", false, "打印版本号后退出")
	flag.Parse()

	if *showVersion {
		fmt.Println("switchapi-hub " + version.Version)
		return
	}

	// M0 启动占位：说明当前二进制尚无服务能力，避免误以为 Hub 已在运行。
	fmt.Println("switchapi-hub " + version.Version + "：M0 骨架占位，服务逻辑将在 M1 实现")
}

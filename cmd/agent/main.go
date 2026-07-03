// switchapi-agent —— 本地代理（Agent）入口。
//
// M0 骨架：仅支持 --version 与一行启动占位输出；
// 真实代理逻辑（同格式直通转发、用量上报、配对、系统服务注册）自 M1 起实现。
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
		fmt.Println("switchapi-agent " + version.Version)
		return
	}

	// M0 启动占位：说明当前二进制尚无代理能力。
	fmt.Println("switchapi-agent " + version.Version + "：M0 骨架占位，代理逻辑将在 M1 实现")
}

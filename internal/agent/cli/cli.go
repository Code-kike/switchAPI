// Package cli implements the switchapi-agent subcommands: system-service
// lifecycle (kardianos), pairing, config takeover, and the foreground run
// body. One code path serves interactive and service modes via service.Run.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Code-kike/switchAPI/internal/agent/appconfig"
	"github.com/Code-kike/switchAPI/internal/agent/forward"
	"github.com/Code-kike/switchAPI/internal/agent/health"
	"github.com/Code-kike/switchAPI/internal/agent/hubclient"
	"github.com/Code-kike/switchAPI/internal/agent/usagebuf"
	"github.com/Code-kike/switchAPI/internal/shared/wire"
	"github.com/kardianos/service"
)

const usage = `switchapi-agent <命令> [参数]

命令：
  pair    --hub <URL> --code <配对码> [--name 设备名]   与 Hub 配对
  config  apply [--dry-run] | rollback                接管/回滚 CC 与 Codex 配置
  run     [-listen 127.0.0.1:9527]                    前台运行（服务体）
  install | uninstall | start | stop | status          系统服务管理
  --version                                            打印版本号`

// Main dispatches and returns the process exit code.
func Main(args []string) int {
	if len(args) == 0 {
		fmt.Println(usage)
		return 2
	}
	switch args[0] {
	case "pair":
		return pairCmd(args[1:])
	case "config":
		return configCmd(args[1:])
	case "run":
		return runCmd(args[1:])
	case "install", "uninstall", "start", "stop", "status":
		return serviceCmd(args[0])
	default:
		fmt.Println(usage)
		return 2
	}
}

func statePathFlag(fs *flag.FlagSet) *string {
	def, _ := hubclient.DefaultStatePath()
	return fs.String("state", def, "状态文件路径")
}

// ---- pair ----

func pairCmd(args []string) int {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	hub := fs.String("hub", "", "Hub 地址，如 http://192.168.1.10:8080")
	code := fs.String("code", "", "Web 控制台生成的一次性配对码")
	name := fs.String("name", "", "设备名（默认主机名）")
	statePath := statePathFlag(fs)
	if fs.Parse(args) != nil {
		return 2
	}
	if *hub == "" || *code == "" {
		fmt.Println("必须提供 --hub 与 --code")
		return 2
	}
	st, err := hubclient.Pair(*hub, *code, *name, *statePath)
	if err != nil {
		fmt.Println("配对失败：", err)
		return 1
	}
	fmt.Printf("配对成功：device_id=%s\n状态已写入 %s（0600）\n", st.DeviceID, *statePath)
	fmt.Println("下一步：`switchapi-agent config apply` 接管 CC/Codex，再 `switchapi-agent install` 注册系统服务")
	return 0
}

// ---- config ----

func configCmd(args []string) int {
	if len(args) == 0 {
		fmt.Println("用法：config apply [--dry-run] | config rollback")
		return 2
	}
	sub := args[0]
	fs := flag.NewFlagSet("config "+sub, flag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "只显示将要做的修改，不写入")
	listen := fs.String("listen", "127.0.0.1:9527", "Agent 转发监听地址（写入 CC/Codex 的 base_url）")
	statePath := statePathFlag(fs)
	if fs.Parse(args[1:]) != nil {
		return 2
	}

	st, err := hubclient.LoadState(*statePath)
	if err != nil {
		fmt.Println("读取状态失败（请先 agent pair）：", err)
		return 1
	}
	opts := appconfig.Options{ListenAddr: *listen, LocalToken: st.LocalToken}

	switch sub {
	case "apply":
		changes, warnings, err := appconfig.Apply(opts, *dry)
		for _, w := range warnings {
			fmt.Println("⚠ ", w)
		}
		if err != nil {
			fmt.Println("接管失败：", err)
			return 1
		}
		if len(changes) == 0 {
			fmt.Println("配置已是目标状态，无需修改")
			return 0
		}
		for _, c := range changes {
			fmt.Println("──", c.File)
			for _, l := range c.Lines {
				fmt.Println("   ", l)
			}
		}
		if *dry {
			fmt.Println("（dry-run：以上修改未写入）")
		} else {
			fmt.Println("接管完成：CC 热生效无需重启；运行中的 codex 会话请重启后生效")
		}
		return 0
	case "rollback":
		restored, err := appconfig.Rollback(opts)
		if err != nil {
			fmt.Println("回滚失败：", err)
			return 1
		}
		if len(restored) == 0 {
			fmt.Println("没有可回滚的备份")
			return 0
		}
		for _, r := range restored {
			fmt.Println("已恢复：", r)
		}
		return 0
	default:
		fmt.Println("未知子命令：", sub)
		return 2
	}
}

// ---- run（服务体） ----

type program struct {
	listen    string
	statePath string
	dbPath    string
	state     *hubclient.State

	cancel context.CancelFunc
	srv    *http.Server
	buf    *usagebuf.Queue
	done   chan struct{}
}

func (p *program) Start(_ service.Service) error {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})

	// Usage buffer: parsed usage is enqueued here (non-blocking) and pumped to
	// the Hub by the client. If it can't be opened, forwarding still proceeds —
	// usage reporting is best-effort relative to the proxy staying up.
	// 健康判定（M4）挂在同一 sink 链上：每条记录同时喂给 health.Tracker，
	// 达阈值经 client 上报（断连时本地临时降级）。
	var client *hubclient.Client
	tracker := health.New(health.DefaultConfig(), func(r wire.HealthReport) {
		if client != nil {
			client.ReportHealth(r)
		}
	})
	var sink forward.UsageSink
	if buf, err := usagebuf.Open(p.dbPath); err != nil {
		log.Printf("usagebuf 打开失败，用量上报本次禁用: %v", err)
		sink = tracker.Observe
	} else {
		p.buf = buf
		sink = func(u forward.Usage) {
			buf.Enqueue(u.ToRecord())
			tracker.Observe(u)
		}
	}

	fwd := forward.New(p.state.LocalToken, sink)
	client = hubclient.New(p.statePath, p.state, fwd)
	if p.buf != nil {
		client.UseQueue(p.buf)
	}
	go client.Run(ctx)

	p.srv = &http.Server{
		Addr:              p.listen,
		Handler:           fwd.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// WriteTimeout 必须为 0：SSE 长流不能被写超时切断（研究#6）。
	}
	go func() {
		defer close(p.done)
		log.Printf("switchapi-agent 转发器监听 %s", p.listen)
		if err := p.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("转发器退出: %v", err)
		}
	}()
	return nil
}

func (p *program) Stop(_ service.Service) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p.srv.Shutdown(shutdownCtx)
	p.cancel()
	select {
	case <-p.done:
	case <-time.After(6 * time.Second):
	}
	if p.buf != nil {
		p.buf.Close()
	}
	return nil
}

func runCmd(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:9527", "转发监听地址（仅允许回环地址）")
	statePath := statePathFlag(fs)
	if fs.Parse(args) != nil {
		return 2
	}
	if err := ensureLoopback(*listen); err != nil {
		fmt.Println(err)
		return 1
	}
	st, err := hubclient.LoadState(*statePath)
	if err != nil {
		fmt.Println("读取状态失败（请先 agent pair）：", err)
		return 1
	}
	if st.LocalToken == "" {
		fmt.Println("状态文件缺少本地 token，请重新 agent pair")
		return 1
	}

	prg := &program{listen: *listen, statePath: *statePath,
		dbPath: filepath.Join(filepath.Dir(*statePath), "agent.db"), state: st}
	svc, err := service.New(prg, svcConfig())
	if err != nil {
		fmt.Println(err)
		return 1
	}
	// service.Run：交互模式下运行 Start 并等待信号；服务模式下对接
	// systemd/launchd/SCM——一套代码两种形态。
	if err := svc.Run(); err != nil {
		fmt.Println(err)
		return 1
	}
	return 0
}

// ensureLoopback rejects non-loopback listen addresses (ADR-0005: Agent 仅
// 监听 127.0.0.1).
func ensureLoopback(listen string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("监听地址非法 %q: %w", listen, err)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("拒绝监听非回环地址 %q：Agent 只允许 127.0.0.1/localhost（ADR-0005）", listen)
	}
	return nil
}

// ---- 系统服务管理 ----

func svcConfig() *service.Config {
	opts := service.KeyValue{}
	switch runtime.GOOS {
	case "linux", "darwin":
		opts["UserService"] = true // systemd user unit / LaunchAgent，免管理员
	}
	if runtime.GOOS == "darwin" {
		opts["RunAtLoad"] = true // 登录即启动（研究#7）
	}
	return &service.Config{
		Name:        "switchapi-agent",
		DisplayName: "switchAPI Agent",
		Description: "switchAPI 本地代理守护进程：CC/Codex 请求经此直连当前供应商",
		Arguments:   []string{"run"},
		Option:      opts,
	}
}

func serviceCmd(action string) int {
	svc, err := service.New(&program{}, svcConfig())
	if err != nil {
		fmt.Println(err)
		return 1
	}
	if action == "status" {
		status, err := svc.Status()
		if err != nil {
			fmt.Println("状态查询失败：", err)
			return 1
		}
		switch status {
		case service.StatusRunning:
			fmt.Println("运行中")
		case service.StatusStopped:
			fmt.Println("已停止")
		default:
			fmt.Println("未知状态")
		}
		return 0
	}
	if err := service.Control(svc, action); err != nil {
		fmt.Printf("%s 失败：%v\n", action, err)
		if action == "install" && runtime.GOOS == "windows" {
			fmt.Println("提示：Windows 注册系统服务需要管理员权限（以管理员身份重试）")
		}
		return 1
	}
	fmt.Printf("%s 完成\n", action)
	if action == "install" {
		switch runtime.GOOS {
		case "linux":
			fmt.Println("提示：如需注销后继续运行，执行 `loginctl enable-linger $USER`")
		case "windows":
			fmt.Println("提示：服务以系统身份运行；CC/Codex 配置接管请在用户会话中执行 `config apply`")
		}
		fmt.Println("启动服务：switchapi-agent start")
	}
	return 0
}

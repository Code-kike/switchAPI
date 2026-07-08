// switchapi-hub —— 中心服务（Hub）入口。
//
// 职责（父 design.md §2/§3）：权威 SQLite、REST API、ws/agent 配置分发、
// ws/ui 实时通知与 Web 控制台托管（embed）、用量汇聚与计价。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Code-kike/switchAPI/internal/hub/api"
	"github.com/Code-kike/switchAPI/internal/hub/pricing"
	"github.com/Code-kike/switchAPI/internal/hub/realtime"
	"github.com/Code-kike/switchAPI/internal/hub/store"
	"github.com/Code-kike/switchAPI/internal/hub/webui"
	"github.com/Code-kike/switchAPI/internal/shared/cryptoutil"
	"github.com/Code-kike/switchAPI/internal/shared/version"
)

// litellmURL is the upstream price table refreshed daily (研究#4).
const litellmURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

func main() {
	showVersion := flag.Bool("version", false, "打印版本号后退出")
	listen := flag.String("listen", ":8080", "HTTP 监听地址")
	dataDir := flag.String("data", "", "数据目录（默认 ~/.switchapi-hub）")
	flag.Parse()

	if *showVersion {
		fmt.Println("switchapi-hub " + version.Version)
		return
	}
	if err := run(*listen, *dataDir); err != nil {
		log.Fatal(err)
	}
}

func run(listen, dataDir string) error {
	if dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("无法确定用户目录，请用 -data 指定数据目录: %w", err)
		}
		dataDir = filepath.Join(home, ".switchapi-hub")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}

	st, err := store.Open(filepath.Join(dataDir, "hub.db"))
	if err != nil {
		return fmt.Errorf("打开数据库失败: %w", err)
	}
	defer st.Close()

	masterKey, err := cryptoutil.LoadOrCreateMasterKey(filepath.Join(dataDir, "master.key"))
	if err != nil {
		return fmt.Errorf("主密钥加载失败: %w", err)
	}

	// 计价引擎：首启从内嵌快照灌入 pricing_base，随后每日 ETag 同步（研究#4）。
	if err := pricing.EnsureLoaded(st); err != nil {
		return fmt.Errorf("价格表初始化失败: %w", err)
	}
	resolver, err := pricing.NewResolver(st)
	if err != nil {
		return fmt.Errorf("计价解析器构建失败: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go pricing.SyncDaily(ctx, st, litellmURL, resolver)

	rt := realtime.New(st, masterKey)
	apiSrv := api.New(st, masterKey, rt, resolver)
	rt.SetUsageNotifier(apiSrv) // 用量入库 → ws/ui usage_tick

	root := http.NewServeMux()
	root.Handle("GET /api/v1/ws/agent", rt.Handler())
	root.Handle("/api/", apiSrv.Handler())
	root.Handle("GET /healthz", apiSrv.Handler())
	root.Handle("/", webui.Handler())

	srv := &http.Server{
		Addr:              listen,
		Handler:           root,
		ReadHeaderTimeout: 5 * time.Second,
		// WriteTimeout 保持 0：WS 长连接与未来的 SSE 都不能被写超时切断。
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("switchapi-hub %s 启动：listen=%s data=%s", version.Version, listen, dataDir)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Println("收到退出信号，优雅关闭中…")
		rt.CloseAll()    // WS 为 hijacked 连接，Shutdown 不会关它们
		apiSrv.CloseUI() // ws/ui 同理
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}
	return nil
}

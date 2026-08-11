// Relay —— 超轻量反向代理面板
//
// 控制面（本程序）与数据面（Caddy）分离：面板崩溃、升级、被 OOM 杀掉都不会
// 影响正在跑的流量。面板通过 Caddy 的 Admin API 原子地下发配置，配置有问题
// 时 Caddy 会整体拒绝并继续运行旧配置。
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"relay/internal/app"
	"relay/internal/caddy"
	"relay/internal/store"
	"relay/internal/web"
)

//go:embed web
var assets embed.FS

const version = "0.1.0"

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime)

	var (
		listen     = flag.String("listen", envOr("RELAY_LISTEN", ":81"), "面板监听地址")
		dataDir    = flag.String("data", envOr("RELAY_DATA_DIR", "./data"), "数据目录（数据库存放位置）")
		caddyAddr  = flag.String("caddy", envOr("RELAY_CADDY_ADMIN", "127.0.0.1:2019"), "Caddy Admin API 地址，支持 127.0.0.1:2019 或 unix//run/caddy/admin.sock")
		printVer   = flag.Bool("version", false, "显示版本号后退出")
	)
	flag.Parse()

	if *printVer {
		fmt.Println("relay", version)
		return
	}

	absData, err := filepath.Abs(*dataDir)
	if err != nil {
		log.Fatalf("数据目录路径无效: %v", err)
	}
	if err := os.MkdirAll(absData, 0o750); err != nil {
		log.Fatalf("创建数据目录失败: %v", err)
	}

	st, err := store.Open(filepath.Join(absData, "relay.db"))
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer st.Close()

	svc := &app.Service{Store: st, Caddy: caddy.New(*caddyAddr)}

	// 启动时把数据库里的站点同步给 Caddy。这样即使 Caddy 单独重启过、
	// 或者面板停机期间有人手改了配置，也能自动回到面板认可的状态。
	if err := svc.Sync(); err != nil {
		log.Printf("⚠ 启动同步失败（面板仍可使用，修好后在「配置」页点重新下发）: %v", err)
	} else {
		log.Printf("✓ 已同步配置到 Caddy (%s)", *caddyAddr)
	}

	sub, err := fs.Sub(assets, "web")
	if err != nil {
		log.Fatalf("加载内置前端资源失败: %v", err)
	}
	handler, err := web.New(svc, sub, version)
	if err != nil {
		log.Fatalf("初始化 Web 层失败: %v", err)
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	go func() {
		log.Printf("Relay %s 已启动，面板地址 http://localhost%s", version, normalizeListen(*listen))
		if st.UserCount() == 0 {
			log.Printf("首次运行：请打开上面的地址创建管理员账号")
		}
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("面板监听失败: %v", err)
		}
	}()

	// 后台定期清理过期会话，顺带保活。
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if err := st.CleanupSessions(); err != nil {
					log.Printf("清理过期会话失败: %v", err)
				}
			case <-stop:
				return
			}
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	close(stop)

	log.Println("正在退出……（Caddy 不受影响，流量继续）")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// normalizeListen 把 "0.0.0.0:81" 变成 ":81"，
// 纯粹是为了日志里那行地址能直接点开。
func normalizeListen(addr string) string {
	if len(addr) > 8 && addr[:8] == "0.0.0.0:" {
		return addr[7:]
	}
	return addr
}

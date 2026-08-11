// CaddyUI —— 超轻量反向代理面板
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
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"caddyui/internal/app"
	"caddyui/internal/caddy"
	"caddyui/internal/caddybin"
	"caddyui/internal/certs"
	"caddyui/internal/store"
	"caddyui/internal/web"
)

//go:embed web
var assets embed.FS

const version = "0.2.0"

// envOr 依次尝试给定的环境变量名。旧的 RELAY_* 留着是为了让从 Relay 升级上来
// 的机器不改 systemd 单元也能跑起来。
func envOr(def string, keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return def
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime)

	var (
		listen    = flag.String("listen", envOr(":81", "CADDYUI_LISTEN", "RELAY_LISTEN"), "面板监听地址")
		dataDir   = flag.String("data", envOr("./data", "CADDYUI_DATA_DIR", "RELAY_DATA_DIR"), "数据目录（数据库存放位置）")
		caddyAddr = flag.String("caddy", envOr("127.0.0.1:2019", "CADDYUI_CADDY_ADMIN", "RELAY_CADDY_ADMIN"), "Caddy Admin API 地址，支持 127.0.0.1:2019 或 unix//run/caddy/admin.sock")
		caddyData = flag.String("caddy-data", envOr("", "CADDYUI_CADDY_DATA"), "Caddy 数据目录（证书放在这里），留空自动探测")
		caddyBin  = flag.String("caddy-bin", envOr("", "CADDYUI_CADDY_BIN"), "Caddy 可执行文件路径，留空自动探测")
		printVer  = flag.Bool("version", false, "显示版本号后退出")
	)
	flag.Parse()

	if *printVer {
		fmt.Println("caddyui", version)
		return
	}

	absData, err := filepath.Abs(*dataDir)
	if err != nil {
		log.Fatalf("数据目录路径无效: %v", err)
	}
	if err := os.MkdirAll(absData, 0o750); err != nil {
		log.Fatalf("创建数据目录失败: %v", err)
	}

	st, err := store.Open(dbPath(absData))
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer st.Close()

	certLoc := certs.New(*caddyData)
	log.Printf("Caddy 证书目录：%s", certLoc.CertRoot())

	binMgr := caddybin.New(*caddyBin)
	if binMgr.BinPath == "" {
		log.Printf("⚠ 没找到 caddy 可执行文件，设置页看不到内核版本（可用 -caddy-bin 指定）")
	}

	svc := &app.Service{
		Store:  st,
		Caddy:  caddy.New(*caddyAddr),
		Certs:  certLoc,
		Binary: binMgr,
	}

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
		log.Printf("CaddyUI %s 已启动，面板地址 %s", version, panelURL(*listen))
		if st.UserCount() == 0 {
			log.Printf("首次运行：请打开上面的地址，用邮箱创建管理员账号")
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

// dbPath 决定数据库文件叫什么。
//
// 面板从 Relay 改名成 CaddyUI，新装的用 caddyui.db；但升级上来的机器数据目录里
// 躺着一个 relay.db，直接换名字等于把人家的站点和账号全弄丢。所以：目录里已经
// 有 relay.db 而没有 caddyui.db 时，继续用旧文件。
func dbPath(dir string) string {
	newPath := filepath.Join(dir, "caddyui.db")
	oldPath := filepath.Join(dir, "relay.db")

	if _, err := os.Stat(newPath); err == nil {
		return newPath
	}
	if _, err := os.Stat(oldPath); err == nil {
		log.Printf("沿用已有数据库 %s（从 Relay 升级上来的）", oldPath)
		return oldPath
	}
	return newPath
}

// panelURL 把监听地址变成一条能直接点开的日志。
//
//	:81 / 0.0.0.0:81  → http://localhost:81  （监听全部网卡，本机就能开）
//	127.0.0.1:18081   → http://127.0.0.1:18081
func panelURL(addr string) string {
	host, port, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return "http://localhost" + addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port)
}

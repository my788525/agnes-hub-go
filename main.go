// agnes-hub-go — Agnes AI 多账号聚合中转 + RPM 限流排队网关
//
// 一句话说明它解决什么：把「20 RPM 硬墙 + 弹超限提示 + 任务中断」
// 变成「服务端自己排队等候，客户端永远拿不到 429，任务不断线」，
// 并把多个独立账号聚合成线性可扩容的池，按模态分工承载文本 / 生图 / 生视频。
//
// 客户端只需要填一个模型名 agnes-auto，其余交给网关。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"agneshub/internal/config"
	"agneshub/internal/hub"
	"agneshub/internal/relay"
	"agneshub/internal/updater"
	"agneshub/internal/web"
)

var version = "1.0.12"

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	// 自更新助手模式：此时本进程唯一任务是等上一个进程退出、
	// 换掉被锁定的 exe、再把新版本拉起来，然后立刻结束。
	// 必须在任何初始化之前判断，否则助手会去抢端口。
	if updater.SwapHelperRequested() {
		os.Exit(updater.RunSwapHelper())
	}

	host := flag.String("host", env("AGNES_HUB_HOST", "127.0.0.1"), "监听地址（0.0.0.0 表示允许局域网访问）")
	port := flag.String("port", env("AGNES_HUB_PORT", "4142"), "监听端口")
	dataDir := flag.String("data", env("AGNES_HUB_DATA", ""), "数据目录（默认 ./data）")
	showVersion := flag.Bool("version", false, "打印版本后退出")
	flag.Parse()

	if *showVersion {
		fmt.Println("agnes-hub-go", version)
		return
	}

	if *dataDir == "" {
		exe, err := os.Executable()
		if err == nil {
			*dataDir = filepath.Join(filepath.Dir(exe), "data")
		} else {
			*dataDir = "data"
		}
	}
	if abs, err := filepath.Abs(*dataDir); err == nil {
		*dataDir = abs
	}

	store, err := config.NewStore(*dataDir)
	if err != nil {
		log.Fatalf("初始化数据目录失败：%v", err)
	}
	h := hub.New(store)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	h.StartMaintenance(ctx)

	srv := web.New(store, h, relay.BuildClient())
	srv.Version = version

	// 初始化自更新器
	exe, _ := os.Executable()
	updCfg := updater.Config{
		Repo:       "my788525/agnes-hub-go",
		BinaryName: "agnes-hub-go",
		DataDir:    *dataDir,
		// 检查频率：12 小时。自更新是「有就换」，没必要更勤；
		// 控制台上也可以随时手动点「检查更新」。
		CheckInterval: 12 * time.Hour,
	}
	upd := updater.New(updCfg, version, exe, nil)
	srv.SetUpdater(upd)
	upd.StartBackground(ctx, updCfg.CheckInterval)

	// 应用完更新后要真的退出：光置一个标志位没人看，
	// 必须有人把它翻译成取消信号，进程才会走到优雅关闭。
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if upd.NeedRestart() {
					fmt.Println("自更新已就位，正在退出以便替换二进制…")
					cancel()
					return
				}
			}
		}
	}()

	addr := net.JoinHostPort(*host, *port)
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv,
		ReadHeaderTimeout: 20 * time.Second,
		// 刻意不设 WriteTimeout / IdleTimeout 上限：
		// 视频提交与长回答的 SSE 流可能持续很久，超时会把正常任务掐断。
		IdleTimeout: 120 * time.Second,
	}

	settings := store.SettingsSnapshot()
	enabled := 0
	for _, a := range store.AccountsSnapshot() {
		if a.Enabled && a.APIKey != "" {
			enabled++
		}
	}

	fmt.Println("==============================================================")
	fmt.Printf("  agnes-hub-go %s  已启动\n", version)
	fmt.Println("--------------------------------------------------------------")
	fmt.Printf("  控制台    http://%s/console\n", displayAddr(*host, *port))
	fmt.Printf("  接口基址  http://%s/v1\n", displayAddr(*host, *port))
	fmt.Printf("  统一模型  %s（自动判定 文本/生图/生视频）\n", settings.AutoModelName)
	fmt.Printf("  数据目录  %s\n", *dataDir)
	fmt.Printf("  可用账号  %d\n", enabled)
	if settings.MustChangePassword {
		fmt.Println("  请使用安装向导设置的管理员密码登录控制台；若未设置，可通过重新安装向导填写新密码覆盖。")
	}
	if *host == "0.0.0.0" || *host == "" || *host == "::" {
		fmt.Println("  网络监听  IPv4(0.0.0.0) + IPv6(::) 双栈")
	}
	fmt.Println("--------------------------------------------------------------")
	fmt.Println("  关闭本窗口即停止服务。")
	fmt.Println("==============================================================")

	listeners, err := buildListeners(*host, *port)
	if err != nil {
		log.Fatalf("监听失败：%v", err)
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, c := context.WithTimeout(context.Background(), 8*time.Second)
		defer c()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	var wg sync.WaitGroup
	errCh := make(chan error, len(listeners))
	for _, ln := range listeners {
		wg.Add(1)
		go func(l net.Listener) {
			defer wg.Done()
			if e := httpSrv.Serve(l); e != nil && !errors.Is(e, http.ErrServerClosed) {
				errCh <- e
			}
		}(ln)
	}

	// 等到优雅关闭完成，或首个监听错误。
	select {
	case <-ctx.Done():
		wg.Wait()
	case e := <-errCh:
		log.Printf("监听错误：%v", e)
		cancel()
		wg.Wait()
	}
	fmt.Println("baiPiao-hub 已停止。")
}

// buildListeners 按平台拆分到 listener_linux.go / listener_other.go：
//   - Linux（飞牛 fnOS 部署目标）：host 为 0.0.0.0/空/:: 时同时监听
//     IPv4(0.0.0.0) 与 IPv6(::)，IPv6 套接字强制 V6ONLY=1，互不抢占端口，
//     满足外网 IPv6 域名直达 + 局域网 IPv4 访问。
//   - 其余平台（Windows / macOS 单机运行）：单套接字，行为与原版一致。

func displayAddr(host, port string) string {
	switch host {
	case "0.0.0.0", "::", "":
		return "127.0.0.1:" + port
	default:
		return net.JoinHostPort(host, port)
	}
}

// xrayclient-reversepanel：只做 VLESS 新版内置 reverse 的精简客户端（类似 frpc），
// 服务端使用官方完整版 Xray。
//
// 目录：
//
//	internal/xray    与上游 Xray-core 对接的适配层（唯一 import xray-core 的地方）
//	internal/config  本程序的配置格式
//	internal/app     内核生命周期、热重载
//	internal/health  本地服务健康检测
//	internal/panel   管理网页（HTTPS）
package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/tcpqueue/xrayclient-reversepanel/internal/app"
	"github.com/tcpqueue/xrayclient-reversepanel/internal/config"
	"github.com/tcpqueue/xrayclient-reversepanel/internal/panel"
	"github.com/tcpqueue/xrayclient-reversepanel/internal/xray"
)

// version 在发布构建时通过 -ldflags "-X main.version=v1.2.3" 写入。
var version = "dev"

const defaultInitListen = "127.0.0.1:7400"

// defaultConfigPath：环境变量 XRP_CONFIG 优先（Docker 镜像里设置了它），否则为当前目录的 config.json。
func defaultConfigPath() string {
	if p := os.Getenv("XRP_CONFIG"); p != "" {
		return p
	}
	return "config.json"
}

func main() {
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		os.Exit(runCLI(os.Args[1], os.Args[2:]))
	}
	cfgPath := flag.String("c", defaultConfigPath(), "配置文件路径")
	test := flag.Bool("test", false, "只检查配置，不启动")
	showVer := flag.Bool("version", false, "显示版本")
	webAddr := flag.String("web", "", "临时覆盖管理网页监听地址（不写入配置），如 127.0.0.1:7400")
	initListen := flag.String("init-listen", defaultInitListen, "首次运行（没有配置文件）时管理网页的监听地址")
	memStats := flag.Duration("memstats", 0, "按间隔打印 Go 内存统计，如 10s（调试用）")
	flag.Usage = usage
	flag.Parse()

	if *showVer {
		printVersion()
		return
	}

	c, err := config.Load(*cfgPath)
	if errors.Is(err, fs.ErrNotExist) && !*test {
		// 首次运行：生成带随机初始密码的配置，只起网页，让用户在网页里填写服务端
		var pw string
		if c, pw, err = initConfig(*cfgPath, *initListen); err != nil {
			fatal(err)
		}
		fmt.Fprintf(os.Stderr, "已生成配置文件 %s\n管理网页初始账号 %s 密码 %s（之后可用 `%s info` 查看）\n",
			*cfgPath, c.Web.User, pw, os.Args[0])
	} else if err != nil {
		fatal(err)
	}
	if *test {
		if err := xray.Check(c); err != nil {
			fatal(err)
		}
		fmt.Println("配置正确")
		return
	}
	// -web 只影响这次运行的网页监听，用一份拷贝，避免之后保存配置时把临时地址写进文件
	web := config.WebConfig{}
	if c.Web != nil {
		web = *c.Web
	}
	if *webAddr != "" {
		web.Listen = *webAddr
	}

	xray.InstallHooks()
	r := app.NewRunner(*cfgPath)
	webOn := web.Listen != ""
	if err := r.Start(c); err != nil {
		if !webOn {
			fatal(err)
		}
		fmt.Fprintln(os.Stderr, "内核未启动:", err, "（请在管理网页中完成配置）")
	} else {
		fmt.Fprintln(os.Stderr, "xrayclient-reversepanel", version, "已启动，Xray-core", xray.Version())
	}
	if webOn {
		if _, err := panel.Start(&web, r, version); err != nil {
			fatal(err)
		}
	}

	runtime.GC()
	debug.FreeOSMemory()

	// 定时把空闲堆归还系统：流量高峰过后 RSS 能尽快回落
	trim := c.Memory.TrimSec
	if trim == 0 {
		trim = 60
	}
	if trim > 0 {
		go func() {
			for range time.Tick(time.Duration(trim) * time.Second) {
				debug.FreeOSMemory()
			}
		}()
	}

	if *memStats > 0 {
		go func() {
			var m runtime.MemStats
			for range time.Tick(*memStats) {
				runtime.ReadMemStats(&m)
				fmt.Fprintf(os.Stderr, "[mem] heapAlloc=%.1fMB heapInuse=%.1fMB released=%.1fMB stack=%.1fMB sys=%.1fMB goroutines=%d gc=%d\n",
					mb(m.HeapAlloc), mb(m.HeapInuse), mb(m.HeapReleased), mb(m.StackInuse), mb(m.Sys), runtime.NumGoroutine(), m.NumGC)
			}
		}()
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	r.Stop()
}

func printVersion() {
	fmt.Printf("xrayclient-reversepanel %s (Xray-core %s, %s %s/%s)\n", version, xray.Version(), runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

func mb(v uint64) float64 { return float64(v) / (1 << 20) }

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "错误:", err)
	os.Exit(1)
}

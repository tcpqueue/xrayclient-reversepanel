// xray-lite：只做 VLESS 新版内置 reverse 的精简客户端（类似 frpc），
// 服务端使用官方完整版 Xray。
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
	"syscall"
	"time"

	"github.com/xtls/xray-core/core"
)

func main() {
	cfgPath := flag.String("c", "config.json", "配置文件路径")
	test := flag.Bool("test", false, "只检查配置，不启动")
	showVer := flag.Bool("version", false, "显示版本")
	webAddr := flag.String("web", "", "管理网页监听地址，如 127.0.0.1:7400（覆盖配置中的 web.listen）")
	memStats := flag.Duration("memstats", 0, "按间隔打印 Go 内存统计，如 10s（调试用）")
	flag.Parse()

	if *showVer {
		fmt.Println("xray-lite, based on Xray-core", core.Version())
		return
	}

	c, err := loadConfig(*cfgPath)
	firstRun := false
	if errors.Is(err, fs.ErrNotExist) && !*test {
		// 首次运行：没有配置文件就只起网页，让用户在网页里填写
		firstRun = true
		c = &Config{
			Servers: []ServerConfig{{Name: "服务端1", Security: "reality", Flow: "xtls-rprx-vision", Encryption: "none"}},
			Web:     &WebConfig{Listen: "127.0.0.1:7400"},
		}
	} else if err != nil {
		fatal(err)
	}
	if *webAddr != "" {
		if c.Web == nil {
			c.Web = &WebConfig{}
		}
		c.Web.Listen = *webAddr
	}
	if *test {
		if _, err := c.Build(); err != nil {
			fatal(err)
		}
		fmt.Println("配置正确")
		return
	}

	installHooks()
	r := &runner{path: *cfgPath}
	webOn := c.Web != nil && c.Web.Listen != ""

	if firstRun {
		r.cfg = c
		fmt.Fprintf(os.Stderr, "未找到配置文件 %s，请打开 http://%s 进行配置\n", *cfgPath, c.Web.Listen)
	} else if err := r.start(c); err != nil {
		if !webOn {
			fatal(err)
		}
		fmt.Fprintln(os.Stderr, "错误:", err, "（可在管理网页中修改配置）")
	} else {
		fmt.Fprintln(os.Stderr, "xray-lite 已启动，Xray-core", core.Version())
	}
	if webOn {
		if err := startWeb(c.Web, r); err != nil {
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
	r.stop()
}

func mb(v uint64) float64 { return float64(v) / (1 << 20) }

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "错误:", err)
	os.Exit(1)
}

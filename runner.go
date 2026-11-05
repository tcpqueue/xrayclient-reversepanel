package main

import (
	"fmt"
	"math"
	"os"
	"runtime"
	"runtime/debug"
	"sync"
	"time"

	"github.com/xtls/xray-core/core"
)

// runner 管理 Xray 内核实例的启动、停止与热重载。
type runner struct {
	mu        sync.Mutex
	path      string
	cfg       *Config
	insts     []*core.Instance // 每个启用的服务端一个实例
	startedAt time.Time
	lastErr   string
}

func (r *runner) running() (bool, *Config, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.insts) > 0, r.cfg, r.lastErr
}

func (r *runner) startLocked(c *Config) error {
	stats.setServers(c.Servers) // 即使配置有误也先登记，网页上能看到各服务端
	pb, err := c.Build()
	if err != nil {
		return err
	}
	if err := setLogLevel(c.LogLevel); err != nil {
		return err
	}
	applyMemory(c.Memory)
	var insts []*core.Instance
	t0 := time.Now()
	fail := func(name string, err error) error {
		if len(insts) > 0 { // 同 stopLocked：避开 VLESS reverse 监控任务延迟启动的竞态
			if d := 3*time.Second - time.Since(t0); d > 0 {
				time.Sleep(d)
			}
		}
		for _, x := range insts {
			x.Close()
		}
		stats.closeTunnels()
		return fmt.Errorf("服务端 %q 启动失败: %w", name, err)
	}
	for _, b := range pb {
		inst, err := core.New(b.Config)
		if err != nil {
			return fail(b.Name, err)
		}
		if err := inst.Start(); err != nil {
			inst.Close()
			return fail(b.Name, err)
		}
		insts = append(insts, inst)
	}
	r.insts, r.startedAt = insts, time.Now()
	health.apply(c.Allow) // 健康检测按新配置重启（配置没变的保留历史）
	return nil
}

func (r *runner) stopLocked() {
	if len(r.insts) == 0 {
		return
	}
	// Xray 的 VLESS reverse 会在实例创建 2 秒后才启动监控任务；
	// 若在此之前 Close，监控任务会在 Close 之后才启动并残留下来，所以至少等满 3 秒。
	if d := 3*time.Second - time.Since(r.startedAt); d > 0 {
		time.Sleep(d)
	}
	for _, x := range r.insts {
		x.Close()
	}
	r.insts = nil
	stats.closeTunnels() // 主动关掉旧的隧道连接，避免旧会话残留
	runtime.GC()
	debug.FreeOSMemory()
}

// start 启动内核（启动时调用）。
func (r *runner) start(c *Config) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg = c
	if err := r.startLocked(c); err != nil {
		r.lastErr = err.Error()
		return err
	}
	r.lastErr = ""
	return nil
}

// apply 用新配置热重载；失败则回滚到旧配置。成功后写回配置文件。
func (r *runner) apply(c *Config) error {
	if _, err := c.Build(); err != nil { // 先校验，避免无谓地停掉正在运行的实例
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	old := r.cfg
	if old != nil {
		c.Web = old.Web // 网页监听设置不允许从网页修改
	}
	r.stopLocked()
	if err := r.startLocked(c); err != nil {
		r.lastErr = err.Error()
		if old != nil && r.startLocked(old) == nil {
			return fmt.Errorf("新配置启动失败，已回滚: %w", err)
		}
		r.cfg = c
		return err
	}
	r.cfg, r.lastErr = c, ""
	if err := c.save(r.path); err != nil {
		return fmt.Errorf("已生效，但写入配置文件失败: %w", err)
	}
	return nil
}

// updateWeb 修改网页登录设置并写回配置文件（不重启内核）。
func (r *runner) updateWeb(f func(*WebConfig)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cfg == nil {
		return fmt.Errorf("没有配置")
	}
	if r.cfg.Web == nil {
		r.cfg.Web = &WebConfig{}
	}
	f(r.cfg.Web)
	return r.cfg.save(r.path)
}

func (r *runner) restart() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cfg == nil {
		return fmt.Errorf("没有配置")
	}
	r.stopLocked()
	if err := r.startLocked(r.cfg); err != nil {
		r.lastErr = err.Error()
		return err
	}
	r.lastErr = ""
	return nil
}

func (r *runner) stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, x := range r.insts {
		x.Close()
	}
	r.insts = nil
}

// applyMemory 设置 Go 运行时参数；环境变量 GOGC/GOMEMLIMIT/GOMAXPROCS 优先。
func applyMemory(m MemoryConfig) {
	if os.Getenv("GOGC") == "" {
		p := m.GCPercent
		if p == 0 {
			p = 50
		}
		debug.SetGCPercent(p)
	}
	if os.Getenv("GOMEMLIMIT") == "" {
		if m.LimitMB > 0 {
			debug.SetMemoryLimit(m.LimitMB << 20)
		} else {
			debug.SetMemoryLimit(math.MaxInt64) // 不限制
		}
	}
	if os.Getenv("GOMAXPROCS") == "" {
		if m.MaxProcs > 0 {
			runtime.GOMAXPROCS(m.MaxProcs)
		} else {
			runtime.SetDefaultGOMAXPROCS() // 0 = 使用全部 CPU 线程（会考虑容器的 CPU 限额）
		}
	}
}

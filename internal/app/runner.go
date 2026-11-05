// Package app 管理内核的启动、停止、热重载与配置保存。本包不直接引用 xray-core，
// 只通过 internal/xray 适配层操作内核。
package app

import (
	"fmt"
	"math"
	"os"
	"runtime"
	"runtime/debug"
	"sync"

	"github.com/tcpqueue/xrayclient-reversepanel/internal/config"
	"github.com/tcpqueue/xrayclient-reversepanel/internal/health"
	"github.com/tcpqueue/xrayclient-reversepanel/internal/xray"
)

type Runner struct {
	mu      sync.Mutex
	path    string
	cfg     *config.Config
	insts   *xray.Instances
	lastErr string
}

func NewRunner(path string) *Runner { return &Runner{path: path} }

// Path 返回配置文件路径。
func (r *Runner) Path() string { return r.path }

// Running 返回内核是否在运行、当前配置和最近一次启动错误。
func (r *Runner) Running() (bool, *config.Config, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.insts.Len() > 0, r.cfg, r.lastErr
}

func (r *Runner) startLocked(c *config.Config) error {
	applyMemory(c.Memory)
	x, err := xray.Start(c)
	if err != nil {
		return err
	}
	r.insts = x
	health.Apply(c.Allow)
	return nil
}

func (r *Runner) stopLocked() {
	if r.insts == nil {
		return
	}
	r.insts.Close()
	r.insts = nil
	runtime.GC()
	debug.FreeOSMemory()
}

// Start 启动内核（程序启动时调用）。失败时仍保留配置，可在网页里修改。
func (r *Runner) Start(c *config.Config) error {
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

// Apply 用新配置热重载；失败则回滚到旧配置。成功后写回配置文件。
// 网页登录与监听设置（web）不允许通过这个接口修改，始终保留当前值。
func (r *Runner) Apply(c *config.Config) error {
	if err := xray.Check(c); err != nil { // 先校验，避免无谓地停掉正在运行的实例
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	old := r.cfg
	c.Web = nil
	if old != nil {
		c.Web = old.Web
	}
	r.stopLocked()
	if err := r.startLocked(c); err != nil {
		r.lastErr = err.Error()
		if old != nil && r.startLocked(old) == nil {
			r.lastErr = ""
			return fmt.Errorf("新配置启动失败，已回滚: %w", err)
		}
		r.cfg = c
		return err
	}
	r.cfg, r.lastErr = c, ""
	if err := c.Save(r.path); err != nil {
		return fmt.Errorf("已生效，但写入配置文件失败: %w", err)
	}
	return nil
}

// UpdateWeb 修改网页设置并写回配置文件（不重启内核）。
func (r *Runner) UpdateWeb(f func(*config.WebConfig)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cfg == nil {
		return fmt.Errorf("没有配置")
	}
	// 拷贝一份再改，避免与正在读取 r.cfg.Web 的地方竞争
	nc := *r.cfg
	w := config.WebConfig{}
	if r.cfg.Web != nil {
		w = *r.cfg.Web
	}
	f(&w)
	nc.Web = &w
	if err := nc.Save(r.path); err != nil {
		return err
	}
	r.cfg = &nc
	return nil
}

func (r *Runner) Restart() error {
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

// Stop 在进程退出时调用，不等待。
func (r *Runner) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.insts.CloseNow()
	r.insts = nil
}

// applyMemory 设置 Go 运行时参数；环境变量 GOGC/GOMEMLIMIT/GOMAXPROCS 优先。
func applyMemory(m config.MemoryConfig) {
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

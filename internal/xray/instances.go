package xray

import (
	"fmt"
	"sync"
	"time"

	"github.com/tcpqueue/xrayclient-reversepanel/internal/config"
	clog "github.com/xtls/xray-core/common/log"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/transport/internet"
)

// 上游耦合点：VLESS reverse 会在实例创建约 2 秒后才启动监控任务；
// 若在此之前 Close，监控任务会在 Close 之后才启动并残留下来，所以至少等满这么久再关闭。
const reverseStartGuard = 3 * time.Second

var hooksOnce sync.Once

// InstallHooks 安装拨号器和日志钩子（进程内只需一次）。
func InstallHooks() {
	hooksOnce.Do(func() {
		internet.UseAlternativeSystemDialer(&trackingDialer{inner: &internet.DefaultSystemDialer{}})
		clog.RegisterHandler(logs)
	})
}

// Version 返回所依赖的 Xray-core 版本。
func Version() string { return core.Version() }

// Check 只校验配置，不启动。
func Check(c *config.Config) error {
	_, err := Build(c)
	return err
}

// Instances 是一组正在运行的 Xray 实例（每个启用的服务端一个）。
type Instances struct {
	list    []*core.Instance
	started time.Time
}

// Start 按配置启动全部服务端；任何一个失败都会关闭已启动的并返回错误。
func Start(c *config.Config) (*Instances, error) {
	SetServers(c.Servers) // 即使配置有误也先登记，网页上能看到各服务端
	built, err := Build(c)
	if err != nil {
		return nil, err
	}
	if err := setLogLevel(c.LogLevel); err != nil {
		return nil, err
	}
	x := &Instances{started: time.Now()}
	for _, b := range built {
		inst, err := core.New(b.Config)
		if err == nil {
			if err = inst.Start(); err != nil {
				inst.Close()
			}
		}
		if err != nil {
			x.Close()
			return nil, fmt.Errorf("服务端 %q 启动失败: %w", b.Name, err)
		}
		x.list = append(x.list, inst)
	}
	return x, nil
}

// Close 关闭全部实例和残留的隧道连接。wait=false 用于进程退出，不必避开启动竞态。
func (x *Instances) close(wait bool) {
	if x == nil {
		return
	}
	if wait && len(x.list) > 0 {
		if d := reverseStartGuard - time.Since(x.started); d > 0 {
			time.Sleep(d)
		}
	}
	for _, i := range x.list {
		i.Close()
	}
	x.list = nil
	stats.closeTunnels() // 主动关掉旧的隧道连接，避免旧会话残留
}

func (x *Instances) Close()    { x.close(true) }
func (x *Instances) CloseNow() { x.close(false) }
func (x *Instances) Len() int {
	if x == nil {
		return 0
	}
	return len(x.list)
}

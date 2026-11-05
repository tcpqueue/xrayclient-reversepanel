package xray

// 接管 Xray 日志（log.RegisterHandler）：识别反向控制通道建立事件，并保留最近日志给网页。

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	clog "github.com/xtls/xray-core/common/log"
)

type logHandler struct {
	level atomic.Int32 // 输出到控制台/网页的级别；-1 = 不输出
	mu    sync.Mutex
	ring  [200]string
	pos   int
	full  bool
}

var logs = &logHandler{}

// 上游耦合点：服务端 portal 建立 mux 后会先发一个到 udp:reverse:0 的控制连接，
// 客户端 VLESS outbound 收到后打出这行日志（proxy/vless/outbound 与 common/mux）。
// 上游改了这句日志，就只能看到“TCP 已连上”而看不到“反向隧道已建立”，upstream_test 会提示检查。
const reverseUpLog = "received request for udp:reverse:0"

// Severity 返回给 Xray 的过滤级别：至少 Info，这样才能收到反向通道建立的事件。
func (h *logHandler) Severity() clog.Severity {
	if l := clog.Severity(h.level.Load()); l > clog.Severity_Info {
		return l
	}
	return clog.Severity_Info
}

func (h *logHandler) Handle(msg clog.Message) {
	s := msg.String()
	sev := clog.Severity_Info
	if gm, ok := msg.(*clog.GeneralMessage); ok {
		sev = gm.Severity
	}
	// 日志里没有服务端信息：控制消息刚从某条隧道读进来，紧接着就打出这行日志，
	// 所以归给“最近一次收到数据”的那个服务端。
	if strings.Contains(s, reverseUpLog) {
		stats.mu.Lock()
		var best *serverStat
		for _, ss := range stats.servers {
			if len(ss.conns) > 0 && (best == nil || ss.lastRx.Load() > best.lastRx.Load()) {
				best = ss
			}
		}
		if best != nil {
			best.reverseUpAt = time.Now()
		}
		stats.mu.Unlock()
	}
	if sev <= clog.Severity_Warning && (strings.Contains(s, "REALITY") || strings.Contains(s, "proxy/vless/outbound")) {
		stats.mu.Lock()
		stats.lastErr, stats.lastErrAt = s, time.Now()
		stats.mu.Unlock()
	}
	lvl := h.level.Load()
	if lvl < 0 || int32(sev) > lvl {
		return
	}
	line := time.Now().Format("2006/01/02 15:04:05 ") + s
	fmt.Fprintln(os.Stderr, line)
	h.mu.Lock()
	h.ring[h.pos] = line
	h.pos = (h.pos + 1) % len(h.ring)
	if h.pos == 0 {
		h.full = true
	}
	h.mu.Unlock()
}

// RecentLogs 返回最近的日志（空时返回 [] 而不是 null）。
func RecentLogs() []string {
	h := logs
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.ring))
	if h.full {
		out = append(out, h.ring[h.pos:]...)
	}
	out = append(out, h.ring[:h.pos]...)
	return out
}

func setLogLevel(s string) error {
	l := int32(clog.Severity_Warning)
	switch strings.ToLower(s) {
	case "debug":
		l = int32(clog.Severity_Debug)
	case "info":
		l = int32(clog.Severity_Info)
	case "", "warning":
	case "error":
		l = int32(clog.Severity_Error)
	case "none":
		l = -1
	default:
		return fmt.Errorf(`"logLevel" 无效: %s`, s)
	}
	logs.level.Store(l)
	return nil
}

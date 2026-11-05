package main

// 出站对象（本地服务）健康检测：TCP / UDP / HTTP / HTTPS。
// 检测直接从本机拨号到本地服务，不经过 Xray，也不使用系统代理。

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	stdnet "net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type HealthConfig struct {
	Enabled     bool   `json:"enabled"`
	Type        string `json:"type"`                  // tcp | udp | http | https
	Target      string `json:"target,omitempty"`      // host:port；留空时使用出站目标（需为单个 IP:端口）
	Path        string `json:"path,omitempty"`        // http(s) 路径，默认 /
	Insecure    bool   `json:"insecure,omitempty"`    // https 忽略证书校验
	AnyStatus   bool   `json:"anyStatus,omitempty"`   // http(s) 任何状态码都算正常（否则 < 400 才算）
	Interval    int    `json:"interval,omitempty"`    // 秒，默认 30，最小 5
	Timeout     int    `json:"timeout,omitempty"`     // 秒，默认 5
	Payload     string `json:"payload,omitempty"`     // udp 探测内容；以 hex: 开头表示十六进制
	ExpectReply bool   `json:"expectReply,omitempty"` // udp 必须收到回复才算正常
}

const healthHistory = 30

// healthTarget 返回实际检测地址。
func (a *AllowEntry) healthTarget() (string, error) {
	h := a.Health
	if h.Target != "" {
		if _, _, err := stdnet.SplitHostPort(h.Target); err != nil {
			return "", fmt.Errorf(`出站 %q 的检测地址需要 host:端口`, outKeyOf(*a))
		}
		return h.Target, nil
	}
	t := strings.TrimSpace(a.Target)
	host, port, err := stdnet.SplitHostPort(t)
	if err != nil || strings.Contains(host, "/") || strings.Contains(port, "-") {
		return "", fmt.Errorf(`出站 %q 是网段或端口范围，请在健康检测里指定一个检测地址`, outKeyOf(*a))
	}
	return t, nil
}

func (h *HealthConfig) validate(a *AllowEntry) error {
	switch h.Type {
	case "tcp", "udp", "http", "https":
	default:
		return fmt.Errorf(`出站 %q 的检测类型只能是 tcp/udp/http/https`, outKeyOf(*a))
	}
	if _, err := a.healthTarget(); err != nil {
		return err
	}
	if h.Interval != 0 && h.Interval < 5 {
		return fmt.Errorf(`出站 %q 的检测间隔不能小于 5 秒`, outKeyOf(*a))
	}
	if h.Timeout < 0 || h.Timeout > 60 {
		return fmt.Errorf(`出站 %q 的检测超时需在 1–60 秒`, outKeyOf(*a))
	}
	if strings.HasPrefix(h.Payload, "hex:") {
		if _, err := hex.DecodeString(strings.TrimPrefix(h.Payload, "hex:")); err != nil {
			return fmt.Errorf(`出站 %q 的 UDP 探测内容不是有效的十六进制`, outKeyOf(*a))
		}
	}
	return nil
}

func outKeyOf(a AllowEntry) string {
	if a.Name != "" {
		return a.Name
	}
	return a.Target
}

// ---------------- 检测结果 ----------------

type healthState struct {
	mu        sync.Mutex
	cfg       HealthConfig
	target    string
	status    string // pending | up | down
	latencyMs int64
	lastCheck time.Time
	lastErr   string
	noReply   bool    // UDP：没收到回复也没被拒绝（无法确认对方一定在工作）
	fails     int64   // 累计失败次数
	history   []int64 // 最近若干次：延迟毫秒；失败为 -1
}

type HealthView struct {
	Type      string  `json:"type"`
	Target    string  `json:"target"`
	Status    string  `json:"status"`
	LatencyMs int64   `json:"latencyMs"`
	LastCheck int64   `json:"lastCheck"`
	LastErr   string  `json:"lastErr"`
	Fails     int64   `json:"fails"`
	History   []int64 `json:"history"`
	Interval  int     `json:"interval"`
	NoReply   bool    `json:"noReply"`
}

type healthManager struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	states map[string]*healthState // key: 出站名称（或目标）
}

var health = &healthManager{states: map[string]*healthState{}}

// apply 按新的出站列表重启检测任务；配置没变的检测保留历史。
func (m *healthManager) apply(allow []AllowEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		m.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	old := m.states
	m.states = map[string]*healthState{}
	for i := range allow {
		a := allow[i]
		if a.Health == nil || !a.Health.Enabled {
			continue
		}
		target, err := a.healthTarget()
		if err != nil {
			continue
		}
		key := outKeyOf(a)
		st := old[key]
		if st == nil || st.cfg != *a.Health || st.target != target {
			st = &healthState{status: "pending"}
		}
		st.cfg, st.target = *a.Health, target
		m.states[key] = st
		go st.run(ctx)
	}
}

func (m *healthManager) snapshot() map[string]HealthView {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]HealthView, len(m.states))
	for k, st := range m.states {
		st.mu.Lock()
		out[k] = HealthView{Type: st.cfg.Type, Target: st.target, Status: st.status, LatencyMs: st.latencyMs,
			LastCheck: ms(st.lastCheck), LastErr: st.lastErr, Fails: st.fails,
			History: append([]int64{}, st.history...), Interval: st.interval(), NoReply: st.noReply}
		st.mu.Unlock()
	}
	return out
}

func (st *healthState) interval() int {
	if st.cfg.Interval >= 5 {
		return st.cfg.Interval
	}
	return 30
}

func (st *healthState) run(ctx context.Context) {
	timeout := time.Duration(st.cfg.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	var client *http.Client
	if st.cfg.Type == "http" || st.cfg.Type == "https" {
		client = &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				Proxy:             nil, // 不走 HTTP(S)_PROXY 环境变量
				DisableKeepAlives: true,
				TLSClientConfig:   &tls.Config{InsecureSkipVerify: st.cfg.Insecure},
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	tick := time.NewTicker(time.Duration(st.interval()) * time.Second)
	defer tick.Stop()
	for {
		start := time.Now()
		var noReply bool
		err := st.check(ctx, client, timeout, &noReply)
		if ctx.Err() != nil {
			return
		}
		lat := time.Since(start).Milliseconds()
		if noReply {
			lat = 0 // 等待回复的那段时间不是延迟
		}
		st.mu.Lock()
		st.lastCheck = time.Now()
		if err != nil {
			st.status, st.lastErr, st.latencyMs = "down", err.Error(), 0
			st.fails++
			st.history = append(st.history, -1)
		} else {
			st.status, st.lastErr, st.latencyMs = "up", "", lat
			st.history = append(st.history, lat)
		}
		st.noReply = err == nil && noReply
		if len(st.history) > healthHistory {
			st.history = st.history[len(st.history)-healthHistory:]
		}
		st.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (st *healthState) check(ctx context.Context, client *http.Client, timeout time.Duration, noReply *bool) error {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	switch st.cfg.Type {
	case "tcp":
		var d stdnet.Dialer
		c, err := d.DialContext(cctx, "tcp", st.target)
		if err != nil {
			return shortErr(err)
		}
		c.Close()
		return nil

	case "udp":
		var d stdnet.Dialer
		c, err := d.DialContext(cctx, "udp", st.target)
		if err != nil {
			return shortErr(err)
		}
		defer c.Close()
		payload := []byte("\n")
		if p := st.cfg.Payload; p != "" {
			if strings.HasPrefix(p, "hex:") {
				payload, _ = hex.DecodeString(strings.TrimPrefix(p, "hex:"))
			} else {
				payload = []byte(p)
			}
		}
		if _, err := c.Write(payload); err != nil {
			return shortErr(err)
		}
		wait := timeout
		if !st.cfg.ExpectReply && wait > 1500*time.Millisecond {
			wait = 1500 * time.Millisecond // 不要求回复时只等一小会儿，看有没有“端口不可达”
		}
		c.SetReadDeadline(time.Now().Add(wait))
		buf := make([]byte, 512)
		_, err = c.Read(buf)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(strings.ToLower(err.Error()), "refused") ||
			strings.Contains(err.Error(), "forcibly closed"):
			return errors.New("端口不可达（没有程序在监听）")
		case isTimeout(err):
			if st.cfg.ExpectReply {
				return errors.New("超时未收到回复")
			}
			*noReply = true
			return nil // 没回复、也没被拒绝：视为正常（Linux 上端口没人监听会收到“拒绝”；Windows 上 Go 屏蔽了这个错误）
		default:
			return shortErr(err)
		}

	case "http", "https":
		path := st.cfg.Path
		if path == "" {
			path = "/"
		}
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		req, err := http.NewRequestWithContext(cctx, http.MethodGet, st.cfg.Type+"://"+st.target+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "xray-lite-health")
		resp, err := client.Do(req)
		if err != nil {
			return shortErr(err)
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if !st.cfg.AnyStatus && resp.StatusCode >= 400 {
			return errors.New("HTTP 状态码 " + strconv.Itoa(resp.StatusCode))
		}
		return nil
	}
	return errors.New("未知检测类型")
}

func isTimeout(err error) bool {
	var ne stdnet.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// shortErr 把常见网络错误翻译成简短中文。
func shortErr(err error) error {
	s := err.Error()
	l := strings.ToLower(s)
	switch {
	case strings.Contains(l, "refused"):
		return errors.New("连接被拒绝（服务未启动或端口不对）")
	case isTimeout(err) || strings.Contains(l, "deadline exceeded"):
		return errors.New("超时")
	case strings.Contains(l, "certificate"):
		return errors.New("证书校验失败（可勾选“忽略证书校验”）：" + s)
	case strings.Contains(l, "no route") || strings.Contains(l, "unreachable"):
		return errors.New("网络不可达")
	case strings.Contains(l, "server gave http response to https client"):
		return errors.New("目标是 HTTP，不是 HTTPS")
	case strings.Contains(l, "first record does not look like a tls handshake"):
		return errors.New("目标不是 HTTPS 服务")
	}
	return err
}

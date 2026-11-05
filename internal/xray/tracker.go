package xray

// 连接状态追踪：替换 Xray 的系统拨号器（internet.UseAlternativeSystemDialer）。
// 反向连接的上下文里带着入站 tag = 该服务端的 reverse tag，据此区分流量属于哪个服务端；
// 出站 tag 为 direct-N 的是“连本地服务”，其余是“连服务端的隧道”。
// 反向代理走 Mux，本来就不会 splice，所以用 stat.CounterConnection 包一层不影响性能。
// 所有累计值自程序启动起计算，重载配置不清零。

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tcpqueue/xrayclient-reversepanel/internal/config"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/stat"
)

// ---------------- 计数器 ----------------

// counter 实现 stats.Counter；可选地同时累加到上级计数器（总流量），并记录最后一次有数据的时间。
type counter struct {
	n      atomic.Int64
	parent *counter
	last   *atomic.Int64 // unix 纳秒
}

func (c *counter) Value() int64      { return c.n.Load() }
func (c *counter) Set(v int64) int64 { return c.n.Swap(v) }
func (c *counter) Add(v int64) int64 {
	if v > 0 {
		if c.last != nil {
			c.last.Store(time.Now().UnixNano())
		}
		if c.parent != nil {
			c.parent.Add(v)
		}
	}
	return c.n.Add(v) - v
}

// closeHook 在连接关闭时执行一次回调，并记录最后一次读到数据的时间；读写字节计数交给外层的 stat.CounterConnection。
type closeHook struct {
	net.Conn
	once     sync.Once
	onClose  func()
	created  time.Time
	lastRead atomic.Int64 // unix 纳秒
	closed   atomic.Bool
}

func (c *closeHook) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.lastRead.Store(time.Now().UnixNano())
	}
	return n, err
}

func (c *closeHook) Close() error {
	c.closed.Store(true)
	c.once.Do(c.onClose)
	return c.Conn.Close()
}

// alive：未关闭，且最近 35 秒内收到过数据（服务端约 10 秒一次心跳）或刚建立不到 15 秒。
// 用活跃度判断而不是只靠 Close 回调，因为 Xray 内部个别路径会绕过外层包装直接关闭底层连接。
func (c *closeHook) alive(now time.Time) bool {
	if c.closed.Load() {
		return false
	}
	return now.Sub(time.Unix(0, c.lastRead.Load())) < 35*time.Second || now.Sub(c.created) < 15*time.Second
}

// ---------------- 状态 ----------------

type serverStat struct {
	Name, Tag, Addr, Security string
	Disabled                  bool

	dials, fails int64
	lastErr      string
	lastErrAt    time.Time
	lastDialOK   time.Time
	reverseUpAt  time.Time // 最近一次收到该服务端的反向控制通道
	conns        map[*closeHook]struct{}
	rx, tx       counter
	lastRx       atomic.Int64
}

type targetStat struct {
	Server   string // 所属服务端名称
	Addr     string
	Dials    int64
	Fails    int64
	LastErr  string
	LastTime time.Time
	active   atomic.Int64 // 当前打开的 TCP 连接数
	rx, tx   counter
}

type tracker struct {
	mu sync.Mutex

	servers map[string]*serverStat // key: reverse tag
	order   []string               // 当前配置中的服务端顺序（tag）
	targets map[string]*targetStat // key: tag + "|" + addr

	totalRx, totalTx counter // 全部隧道合计
	lastErr          string  // 日志里的最近一条隧道相关错误（无法区分服务端）
	lastErrAt        time.Time

	tunnelConns   [32]net.Conn // 最近的隧道连接，重载时主动关闭，防止旧连接残留
	tunnelConnIdx int
}

var stats = &tracker{servers: map[string]*serverStat{}, targets: map[string]*targetStat{}}

func (t *tracker) newServerStat(name, tag string) *serverStat {
	ss := &serverStat{Name: name, Tag: tag}
	ss.rx.parent, ss.tx.parent = &t.totalRx, &t.totalTx
	ss.rx.last = &ss.lastRx
	t.servers[tag] = ss
	return ss
}

// SetServers 在每次（重新）启动内核前调用；同 tag 的服务端保留累计值，已删除的服务端丢弃。
func SetServers(list []config.ServerConfig) {
	t := stats
	t.mu.Lock()
	defer t.mu.Unlock()
	t.order = t.order[:0]
	keep := map[string]bool{}
	for _, s := range list {
		tag := s.LocalTag()
		keep[tag] = true
		ss := t.servers[tag]
		if ss == nil {
			ss = t.newServerStat(s.Name, tag)
		}
		ss.Name, ss.Tag, ss.Addr, ss.Security, ss.Disabled = s.Name, tag, s.Server, s.Security, s.Disabled
		ss.reverseUpAt, ss.lastDialOK, ss.lastErr = time.Time{}, time.Time{}, ""
		ss.lastRx.Store(0)
		t.order = append(t.order, tag)
	}
	for tag := range t.servers {
		if !keep[tag] {
			delete(t.servers, tag) // 仍在关闭中的连接持有的是 *serverStat 指针，不受影响
		}
	}
	t.lastErr = ""
}

// liveTunnels 统计当前活着的隧道连接，并丢掉 2 分钟没有任何数据的僵尸条目（调用方持有 t.mu）。
func (ss *serverStat) liveTunnels(now time.Time) int64 {
	var n int64
	for h := range ss.conns {
		switch {
		case h.alive(now):
			n++
		case h.closed.Load() || now.Sub(time.Unix(0, h.lastRead.Load())) > 2*time.Minute && now.Sub(h.created) > 2*time.Minute:
			delete(ss.conns, h)
		}
	}
	return n
}

// rememberTunnel 记录最近的隧道连接（调用方持有 t.mu）。环形缓冲被覆盖的旧连接若仍在用，
// 由 Xray 自己负责关闭；这里只是重载时的兜底。
func (t *tracker) rememberTunnel(c net.Conn) {
	t.tunnelConns[t.tunnelConnIdx%len(t.tunnelConns)] = c
	t.tunnelConnIdx++
}

// closeTunnels 关闭记录在案的隧道连接（关闭已关闭的连接是无害的）。
func (t *tracker) closeTunnels() {
	t.mu.Lock()
	conns := t.tunnelConns
	t.tunnelConns = [32]net.Conn{}
	t.mu.Unlock()
	for _, c := range conns {
		if c != nil {
			c.Close()
		}
	}
}

// ---------------- 拨号器 ----------------

type trackingDialer struct{ inner internet.SystemDialer }

func (d *trackingDialer) DestIpAddress() net.IP { return d.inner.DestIpAddress() }

func (d *trackingDialer) Dial(ctx context.Context, src net.Address, dest net.Destination, sockopt *internet.SocketConfig) (net.Conn, error) {
	outTag, inTag := "", ""
	if obs := session.OutboundsFromContext(ctx); len(obs) > 0 {
		outTag = obs[len(obs)-1].Tag
	}
	if in := session.InboundFromContext(ctx); in != nil {
		inTag = in.Tag
	}
	conn, err := d.inner.Dial(ctx, src, dest, sockopt)
	now := time.Now()
	t := stats

	if strings.HasPrefix(outTag, "direct-") {
		return t.dialTarget(inTag, dest, conn, err, now)
	}

	// 隧道：reverse 的连接由 VLESS 直接发起，出站 tag 为空，入站 tag 就是该服务端的 reverse tag
	t.mu.Lock()
	ss := t.servers[inTag]
	if ss == nil { // 理论上不会发生；兜底记到一个匿名条目，避免丢统计
		ss = t.newServerStat(inTag, inTag)
	}
	ss.dials++
	if err != nil {
		ss.fails++
		ss.lastErr, ss.lastErrAt = err.Error(), now
		t.mu.Unlock()
		return nil, err
	}
	ss.lastDialOK = now
	if dest.Network != net.Network_TCP {
		t.rememberTunnel(conn)
		t.mu.Unlock()
		return conn, nil
	}
	hook := &closeHook{Conn: conn, created: now}
	hook.onClose = func() {
		t.mu.Lock()
		delete(ss.conns, hook)
		if ss.liveTunnels(time.Now()) == 0 {
			ss.reverseUpAt = time.Time{} // 隧道全部断开，等待重新建立
		}
		t.mu.Unlock()
	}
	if ss.conns == nil {
		ss.conns = map[*closeHook]struct{}{}
	}
	ss.conns[hook] = struct{}{}
	t.rememberTunnel(hook) // 记录带关闭钩子的连接：重载时主动关闭也会正确扣减计数
	t.mu.Unlock()
	return &stat.CounterConnection{Connection: hook, ReadCounter: &ss.rx, WriteCounter: &ss.tx}, nil
}

func (t *tracker) dialTarget(inTag string, dest net.Destination, conn net.Conn, err error, now time.Time) (net.Conn, error) {
	addr := dest.NetAddr()
	if dest.Network == net.Network_UDP {
		addr = "udp:" + addr
	}
	key := inTag + "|" + addr
	t.mu.Lock()
	ts := t.targets[key]
	if ts == nil {
		if len(t.targets) >= 256 { // 防止目标过多导致无限增长：丢掉没有活动连接的旧条目
			for k, v := range t.targets {
				if v.active.Load() == 0 {
					delete(t.targets, k)
				}
			}
		}
		name := inTag
		if ss := t.servers[inTag]; ss != nil {
			name = ss.Name
		}
		ts = &targetStat{Server: name, Addr: addr}
		t.targets[key] = ts
	}
	ts.Dials++
	ts.LastTime = now
	if err != nil {
		ts.Fails++
		ts.LastErr = err.Error()
	} else {
		ts.LastErr = ""
	}
	t.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if dest.Network != net.Network_TCP {
		return conn, nil
	}
	ts.active.Add(1)
	hook := &closeHook{Conn: conn, onClose: func() { ts.active.Add(-1) }}
	return &stat.CounterConnection{Connection: hook, ReadCounter: &ts.rx, WriteCounter: &ts.tx}, nil
}

// ---------------- 快照（给网页） ----------------

type ServerView struct {
	Name      string `json:"name"`
	Tag       string `json:"tag"`
	Addr      string `json:"addr"`
	Security  string `json:"security"`
	State     string `json:"state"` // online / connecting / offline / disabled / stopped
	Detail    string `json:"detail"`
	Since     int64  `json:"since"`
	LastRx    int64  `json:"lastRx"`
	Tunnels   int64  `json:"tunnels"` // 当前打开的隧道连接数
	Dials     int64  `json:"dials"`
	Fails     int64  `json:"fails"`
	LastErr   string `json:"lastErr"`
	LastErrAt int64  `json:"lastErrAt"`
	Rx        int64  `json:"rx"`
	Tx        int64  `json:"tx"`
}

type TargetView struct {
	Server   string `json:"server"`
	Addr     string `json:"addr"`
	Name     string `json:"name"`
	Network  string `json:"network"`
	Active   int64  `json:"active"` // 当前 TCP 连接数；UDP 为 -1（不统计）
	Dials    int64  `json:"dials"`
	Fails    int64  `json:"fails"`
	LastErr  string `json:"lastErr"`
	LastTime int64  `json:"lastTime"`
	Rx       int64  `json:"rx"`
	Tx       int64  `json:"tx"`
}

type Snapshot struct {
	State       string       `json:"state"` // online / partial / connecting / offline / stopped
	Detail      string       `json:"detail"`
	Servers     []ServerView `json:"servers"`
	Targets     []TargetView `json:"targets"`
	Rx          int64        `json:"rx"` // 全部隧道累计
	Tx          int64        `json:"tx"`
	Active      int64        `json:"active"` // 全部本地服务当前 TCP 连接
	Conns       int64        `json:"conns"`  // 全部本地服务累计连接
	LastErr     string       `json:"lastErr"`
	LastErrAt   int64        `json:"lastErrAt"`
	CoreRunning bool         `json:"coreRunning"`
}

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// TakeSnapshot 返回当前连接状态。
func TakeSnapshot(running bool, cfg *config.Config) Snapshot {
	t := stats
	t.mu.Lock()
	defer t.mu.Unlock()
	v := Snapshot{
		Servers: []ServerView{}, Targets: []TargetView{}, // 空时返回 [] 而不是 null
		Rx: t.totalRx.Value(), Tx: t.totalTx.Value(),
		LastErr: t.lastErr, LastErrAt: ms(t.lastErrAt), CoreRunning: running,
	}
	online, enabled := 0, 0
	now := time.Now()
	for _, tag := range t.order {
		ss := t.servers[tag]
		if ss == nil {
			continue
		}
		sv := ServerView{Name: ss.Name, Tag: ss.Tag, Addr: ss.Addr, Security: ss.Security,
			Since: ms(ss.reverseUpAt), Tunnels: ss.liveTunnels(now), Dials: ss.dials, Fails: ss.fails,
			LastErr: ss.lastErr, LastErrAt: ms(ss.lastErrAt), Rx: ss.rx.Value(), Tx: ss.tx.Value()}
		lastRx := time.Unix(0, ss.lastRx.Load())
		if ss.lastRx.Load() != 0 {
			sv.LastRx = lastRx.UnixMilli()
		}
		switch {
		case ss.Disabled:
			sv.State, sv.Detail = "disabled", "已停用"
		case !running:
			sv.State, sv.Detail = "stopped", "内核未运行"
		case !ss.reverseUpAt.IsZero() && sv.Tunnels > 0 && now.Sub(lastRx) < 30*time.Second:
			// 服务端心跳约 10 秒一次，30 秒内收到过数据即认为在线
			sv.State, sv.Detail = "online", "反向隧道已建立"
		case sv.Tunnels > 0 && now.Sub(ss.lastDialOK) < 15*time.Second:
			sv.State, sv.Detail = "connecting", "TCP 已连上，等待服务端建立反向通道"
		case ss.dials == 0:
			sv.State, sv.Detail = "connecting", "正在连接服务端"
		default:
			sv.State, sv.Detail = "offline", "未连上或认证失败，请检查地址、UUID、REALITY/encryption 参数"
		}
		if !ss.Disabled {
			enabled++
			if sv.State == "online" {
				online++
			}
		}
		v.Servers = append(v.Servers, sv)
	}
	switch {
	case !running:
		v.State, v.Detail = "stopped", "内核未运行"
	case enabled > 0 && online == enabled:
		v.State, v.Detail = "online", fmt.Sprintf("全部 %d 个服务端在线", enabled)
	case online > 0:
		v.State, v.Detail = "partial", fmt.Sprintf("%d / %d 个服务端在线", online, enabled)
	default:
		v.State, v.Detail = "offline", "没有在线的服务端"
	}
	for _, ts := range t.targets {
		tv := TargetView{Server: ts.Server, Addr: ts.Addr, Network: "tcp", Active: ts.active.Load(), Dials: ts.Dials, Fails: ts.Fails,
			LastErr: ts.LastErr, LastTime: ms(ts.LastTime), Rx: ts.rx.Value(), Tx: ts.tx.Value()}
		if strings.HasPrefix(ts.Addr, "udp:") {
			tv.Network, tv.Active = "udp", -1
		} else {
			v.Active += tv.Active
		}
		v.Conns += tv.Dials
		if cfg != nil {
			tv.Name = cfg.NameFor(ts.Addr)
		}
		v.Targets = append(v.Targets, tv)
	}
	sort.Slice(v.Targets, func(i, j int) bool {
		if v.Targets[i].Server != v.Targets[j].Server {
			return v.Targets[i].Server < v.Targets[j].Server
		}
		return v.Targets[i].Addr < v.Targets[j].Addr
	})
	return v
}

package main

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	stdnet "net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/xtls/xray-core/app/dispatcher"
	"github.com/xtls/xray-core/app/policy"
	"github.com/xtls/xray-core/app/proxyman"
	"github.com/xtls/xray-core/common/geodata"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/common/uuid"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/proxy/freedom"
	"github.com/xtls/xray-core/proxy/vless"
	vlessout "github.com/xtls/xray-core/proxy/vless/outbound"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/reality"
	"github.com/xtls/xray-core/transport/internet/tcp"
	"github.com/xtls/xray-core/transport/internet/tls"
)

// Config 是 xrayclient-reversepanel 自己的极简配置（不经过 Xray 的 infra/conf）。
type Config struct {
	Servers  []ServerConfig `json:"servers"`
	Allow    []AllowEntry   `json:"allow"`    // 允许服务端访问的本地目标
	LogLevel string         `json:"logLevel"` // debug|info|warning|error|none
	Memory   MemoryConfig   `json:"memory"`
	Web      *WebConfig     `json:"web,omitempty"`
}

// ServerConfig 是一个服务端（一条 VLESS 反向隧道）。
type ServerConfig struct {
	Name       string         `json:"name"`               // 唯一名称，allow 中按名称引用
	Disabled   bool           `json:"disabled,omitempty"` // 暂停这个服务端
	Server     string         `json:"server"`             // host:port
	UUID       string         `json:"uuid"`               // 与服务端 VLESS 用户一致
	Flow       string         `json:"flow"`               // "" / "xtls-rprx-vision" / "xtls-rprx-vision-udp443"
	Testseed   []uint32       `json:"testseed,omitempty"` // Vision 填充参数（4 个数），空=默认 [900,500,900,256]
	Tag        string         `json:"tag,omitempty"`      // 可选：客户端本地 reverse tag，不需要与服务端一致；留空自动生成
	Encryption string         `json:"encryption"`         // "none" 或 "mlkem768x25519plus.native.0rtt.<客户端公钥>"
	Security   string         `json:"security"`           // "reality" 或 "none"
	Reality    *RealityConfig `json:"reality,omitempty"`
}

// AllowEntry 是一个允许被服务端访问的本地服务。
// 兼容直接写字符串 "127.0.0.1:22"。Name/RemotePort 只用于网页展示和生成服务端配置片段。
type AllowEntry struct {
	Target     string        `json:"target"`            // "IP[/前缀]:端口[-端口]"
	Network    string        `json:"network,omitempty"` // "tcp"（默认）/ "udp" / "tcp,udp"
	Name       string        `json:"name,omitempty"`
	RemotePort int           `json:"remotePort,omitempty"` // 服务端对外端口（仅用于生成服务端配置）
	Servers    []string      `json:"servers,omitempty"`    // 允许哪些服务端访问（按名称），空=全部
	Health     *HealthConfig `json:"health,omitempty"`     // 健康检测（可选）
}

func (a *AllowEntry) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*a = AllowEntry{Target: s}
		return nil
	}
	type plain AllowEntry
	return json.Unmarshal(b, (*plain)(a))
}

func (a *AllowEntry) allowedFor(server string) bool {
	if len(a.Servers) == 0 {
		return true
	}
	for _, s := range a.Servers {
		if s == server {
			return true
		}
	}
	return false
}

type WebConfig struct {
	Listen       string `json:"listen"`                 // 如 "127.0.0.1:7400"
	User         string `json:"user,omitempty"`         // 默认 admin
	PasswordHash string `json:"passwordHash,omitempty"` // 网页里修改密码后写入（PBKDF2）
	Password     string `json:"password,omitempty"`     // 旧版明文密码，仍兼容；都不填则为默认 admin
}

type RealityConfig struct {
	ServerName    string `json:"serverName"`
	Password      string `json:"password"` // 服务端私钥对应的公钥（x25519 输出里的 Password/PublicKey）
	ShortID       string `json:"shortId"`
	Fingerprint   string `json:"fingerprint"` // 默认 chrome
	SpiderX       string `json:"spiderX"`
	Mldsa65Verify string `json:"mldsa65Verify,omitempty"` // 可选：ML-DSA-65 公钥（分享链接里的 pqv）
}

type MemoryConfig struct {
	LimitMB   int64 `json:"limitMB"`   // Go 软内存上限（GOMEMLIMIT），0=不设
	GCPercent int   `json:"gcPercent"` // GOGC，0=使用默认 50
	BufferKB  int32 `json:"bufferKB"`  // 每连接缓冲，默认 16KB（官方在 amd64 上默认 512KB）
	MaxProcs  int   `json:"maxProcs"`  // GOMAXPROCS，0=不改
	TrimSec   int   `json:"trimSec"`   // 每隔多少秒归还空闲内存给系统，0=默认 60，负数=关闭
}

func (c *Config) save(path string) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// nameFor 返回某个实际连接目标（"ip:port" 或 "udp:ip:port"）匹配到的 allow 项名称。
func (c *Config) nameFor(addr string) string {
	isUDP := strings.HasPrefix(addr, "udp:")
	host, portStr, err := stdnet.SplitHostPort(strings.TrimPrefix(addr, "udp:"))
	if err != nil {
		return ""
	}
	ip := stdnet.ParseIP(host)
	port, _ := strconv.ParseUint(portStr, 10, 16)
	for _, a := range c.Allow {
		n, from, to, err := parseAllow(a.Target)
		nw := strings.ToLower(a.Network)
		netOK := (isUDP && strings.Contains(nw, "udp")) || (!isUDP && (nw == "" || strings.Contains(nw, "tcp")))
		if err == nil && netOK && ip != nil && n.Contains(ip) && uint64(from) <= port && port <= uint64(to) {
			if a.Name != "" {
				return a.Name
			}
			return a.Target
		}
	}
	return ""
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Config{}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("解析配置失败: %w", err)
	}
	// 兼容旧版单服务端格式：顶层的 server/uuid/tag/... 转成 servers[0]
	if len(c.Servers) == 0 {
		var legacy ServerConfig
		if json.Unmarshal(b, &legacy) == nil && (legacy.Server != "" || legacy.UUID != "" || legacy.Tag != "") {
			legacy.Name = "默认"
			c.Servers = []ServerConfig{legacy}
		}
	}
	return c, nil
}

// 每个服务端独立运行一个 Xray 实例：出站只有 direct-N（放行出口，默认出站）和 tunnel-N（连服务端）。
// 这样不需要 app/router（它会连带引入完整 DNS 模块，约 5MB），服务端之间也天然隔离。
func tunnelTag(i int) string { return "tunnel-" + strconv.Itoa(i) }
func directTag(i int) string { return "direct-" + strconv.Itoa(i) }

type builtServer struct {
	Name   string
	Config *core.Config
}

// Build 校验配置并为每个启用的服务端生成一份 Xray 配置。
func (c *Config) Build() ([]builtServer, error) {
	if len(c.Servers) == 0 {
		return nil, fmt.Errorf(`至少需要配置一个服务端`)
	}
	if len(c.Allow) == 0 {
		return nil, fmt.Errorf(`"allow" 为空：反向流量默认全部拦截，至少写一个本地目标，如 "127.0.0.1:22"`)
	}
	names, tags := map[string]bool{}, map[string]bool{}
	for i := range c.Servers {
		s := &c.Servers[i]
		s.Name = strings.TrimSpace(s.Name)
		if s.Name == "" {
			return nil, fmt.Errorf(`第 %d 个服务端缺少名称`, i+1)
		}
		if names[s.Name] {
			return nil, fmt.Errorf(`服务端名称 %q 重复`, s.Name)
		}
		names[s.Name] = true

		if tags[s.localTag()] {
			return nil, fmt.Errorf(`tag %q 被多个服务端使用，请删掉 tag 让程序自动生成，或改成互不相同`, s.localTag())
		}
		tags[s.localTag()] = true
	}
	for i := range c.Allow {
		a := &c.Allow[i]
		if a.Health != nil && a.Health.Enabled {
			if err := a.Health.validate(a); err != nil {
				return nil, err
			}
		}
		for _, n := range a.Servers {
			if !names[n] {
				return nil, fmt.Errorf(`本地服务 %q 引用了不存在的服务端 %q`, a.Target, n)
			}
		}
	}

	switch strings.ToLower(c.LogLevel) {
	case "", "debug", "info", "warning", "error", "none":
	default:
		return nil, fmt.Errorf(`"logLevel" 无效: %s`, c.LogLevel)
	}
	if c.Web != nil && c.Web.Listen != "" {
		if _, _, err := stdnet.SplitHostPort(c.Web.Listen); err != nil {
			return nil, fmt.Errorf(`"web.listen" 需要 host:port`)
		}
	}
	bufKB := c.Memory.BufferKB
	if bufKB == 0 {
		bufKB = 16
	}

	apps := func() []*serial.TypedMessage {
		return []*serial.TypedMessage{
			serial.ToTypedMessage(&dispatcher.Config{}),
			serial.ToTypedMessage(&proxyman.OutboundConfig{}),
			serial.ToTypedMessage(&policy.Config{
				Level: map[uint32]*policy.Policy{
					0: {Buffer: &policy.Policy_Buffer{Connection: bufKB * 1024}},
				},
			}),
		}
	}
	var out []builtServer
	for i := range c.Servers {
		s := &c.Servers[i]
		if s.Disabled {
			continue
		}
		tunnel, err := s.buildOutbound(tunnelTag(i))
		if err != nil {
			return nil, fmt.Errorf(`服务端 %q: %w`, s.Name, err)
		}
		var mine []AllowEntry
		for _, a := range c.Allow {
			if a.allowedFor(s.Name) {
				mine = append(mine, a)
			}
		}
		allow, err := buildAllowRules(mine)
		if err != nil {
			return nil, err
		}
		out = append(out, builtServer{Name: s.Name, Config: &core.Config{
			App: apps(),
			Outbound: []*core.OutboundHandlerConfig{
				// 第一个出站是默认出站：服务端推下来的反向连接都走它。
				// 只放行分配给这个服务端的目标（vless-reverse 来的流量默认全部拦截，这是 Xray 内置行为）。
				// 绝不能把 VLESS 放第一个，否则反向流量会被送回服务端形成回环。
				{
					Tag:            directTag(i),
					SenderSettings: serial.ToTypedMessage(&proxyman.SenderConfig{}),
					ProxySettings: serial.ToTypedMessage(&freedom.Config{
						DomainStrategy: internet.DomainStrategy_AS_IS,
						FinalRules:     allow,
					}),
				},
				tunnel,
			},
		}})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf(`所有服务端都已停用`)
	}
	return out, nil
}

// localTag 是客户端本地使用的 reverse tag。它不会发给服务端（线上只携带“这是反向连接”的命令，
// 服务端用它自己配置里该用户的 reverse.tag），所以不必与服务端一致；留空时按名称自动生成。
// 它只用于在本地区分流量来自哪个服务端，因此各服务端之间不能重复。
func (s *ServerConfig) localTag() string {
	if s.Tag != "" {
		return s.Tag
	}
	return "rvs:" + s.Name
}

// buildOutbound 生成连接这个服务端的 VLESS 出站（带新版 reverse）。
func (s *ServerConfig) buildOutbound(tag string) (*core.OutboundHandlerConfig, error) {
	u, err := uuid.ParseString(s.UUID)
	if err != nil {
		return nil, fmt.Errorf(`"uuid" 无效: %w`, err)
	}
	account := &vless.Account{
		Id:      u.String(),
		Flow:    s.Flow,
		Reverse: &vless.Reverse{Tag: s.localTag()},
	}
	switch s.Flow {
	case "", vless.XRV, vless.XRV + "-udp443":
	default:
		return nil, fmt.Errorf(`"flow" 只支持 ""、"%s" 或 "%s-udp443"`, vless.XRV, vless.XRV)
	}
	switch len(s.Testseed) {
	case 0:
	case 4:
		if s.Flow == "" {
			return nil, fmt.Errorf(`"testseed" 只在 flow 为 Vision 时有效`)
		}
		if s.Testseed[1] == 0 || s.Testseed[3] == 0 {
			return nil, fmt.Errorf(`"testseed" 第 2、4 个数必须大于 0`)
		}
		account.Testseed = s.Testseed
	default:
		return nil, fmt.Errorf(`"testseed" 需要 4 个数，如 [900,500,900,256]`)
	}
	if err := parseEncryption(s.Encryption, account); err != nil {
		return nil, err
	}

	host, portStr, err := stdnet.SplitHostPort(s.Server)
	if err != nil {
		return nil, fmt.Errorf(`"server" 需要 host:port: %w`, err)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil || port == 0 {
		return nil, fmt.Errorf(`"server" 端口无效: %s`, portStr)
	}

	stream := &internet.StreamConfig{
		ProtocolName: "tcp",
		TransportSettings: []*internet.TransportConfig{{
			ProtocolName: "tcp",
			Settings:     serial.ToTypedMessage(&tcp.Config{}),
		}},
	}
	switch strings.ToLower(s.Security) {
	case "reality":
		rc, err := s.Reality.build()
		if err != nil {
			return nil, err
		}
		tm := serial.ToTypedMessage(rc)
		stream.SecurityType = tm.Type
		stream.SecuritySettings = []*serial.TypedMessage{tm}
	case "", "none":
		if account.Encryption == "none" {
			return nil, fmt.Errorf(`security 为 none 时必须配置 "encryption"（否则流量是明文）`)
		}
	default:
		return nil, fmt.Errorf(`"security" 只支持 reality 或 none`)
	}

	return &core.OutboundHandlerConfig{
		Tag:            tag,
		SenderSettings: serial.ToTypedMessage(&proxyman.SenderConfig{StreamSettings: stream}),
		ProxySettings: serial.ToTypedMessage(&vlessout.Config{
			Vnext: &protocol.ServerEndpoint{
				Address: net.NewIPOrDomain(net.ParseAddress(host)),
				Port:    uint32(port),
				User:    &protocol.User{Account: serial.ToTypedMessage(account)},
			},
		}),
	}, nil
}

// parseEncryption 复刻 infra/conf/vless.go 中客户端 encryption 字符串的解析。
// 格式：mlkem768x25519plus.<native|xorpub|random>.<1rtt|0rtt>[.padding...].<公钥...>
func parseEncryption(s string, a *vless.Account) error {
	if s == "" || s == "none" {
		a.Encryption = "none"
		return nil
	}
	p := strings.Split(s, ".")
	if len(p) < 4 || p[0] != "mlkem768x25519plus" {
		return fmt.Errorf(`"encryption" 格式无效`)
	}
	switch p[1] {
	case "native":
	case "xorpub":
		a.XorMode = 1
	case "random":
		a.XorMode = 2
	default:
		return fmt.Errorf(`"encryption" 模式只能是 native/xorpub/random`)
	}
	switch p[2] {
	case "1rtt":
	case "0rtt":
		a.Seconds = 1
	default:
		return fmt.Errorf(`"encryption" 只能是 1rtt/0rtt`)
	}
	padding := 0
	for _, r := range p[3:] {
		if len(r) < 20 {
			padding += len(r) + 1
			continue
		}
		if b, _ := base64.RawURLEncoding.DecodeString(r); len(b) != 32 && len(b) != 1184 {
			return fmt.Errorf(`"encryption" 中的公钥无效`)
		}
	}
	rest := s[27+len(p[2]):]
	if padding > 0 {
		a.Padding = rest[:padding-1]
		rest = rest[padding:]
	}
	a.Encryption = rest
	return nil
}

func (r *RealityConfig) build() (*reality.Config, error) {
	if r == nil {
		return nil, fmt.Errorf(`security 为 reality 时需要 "reality" 配置`)
	}
	c := &reality.Config{ServerName: r.ServerName}
	c.Fingerprint = strings.ToLower(r.Fingerprint)
	if c.Fingerprint == "" {
		c.Fingerprint = "chrome"
	}
	if c.Fingerprint == "unsafe" || c.Fingerprint == "hellogolang" || tls.GetFingerprint(c.Fingerprint) == nil {
		return nil, fmt.Errorf(`reality "fingerprint" 无效: %s`, r.Fingerprint)
	}
	var err error
	if c.PublicKey, err = base64.RawURLEncoding.DecodeString(r.Password); err != nil || len(c.PublicKey) != 32 {
		return nil, fmt.Errorf(`reality "password" 无效`)
	}
	if len(r.ShortID) > 16 {
		return nil, fmt.Errorf(`reality "shortId" 过长`)
	}
	c.ShortId = make([]byte, 8)
	if _, err = hex.Decode(c.ShortId, []byte(r.ShortID)); err != nil {
		return nil, fmt.Errorf(`reality "shortId" 无效`)
	}
	spiderX := r.SpiderX
	if spiderX == "" {
		spiderX = "/"
	}
	if spiderX[0] != '/' {
		return nil, fmt.Errorf(`reality "spiderX" 必须以 / 开头`)
	}
	// 与官方一致：从 spiderX 的查询参数中取出 p/c/t/i/r 填入 SpiderY
	c.SpiderY = make([]int64, 10)
	u, _ := url.Parse(spiderX)
	q := u.Query()
	for i, k := range []string{"p", "c", "t", "i", "r"} {
		if v := q.Get(k); v != "" {
			lo, hi, ok := strings.Cut(v, "-")
			if !ok {
				hi = lo
			}
			c.SpiderY[i*2], _ = strconv.ParseInt(lo, 10, 64)
			c.SpiderY[i*2+1], _ = strconv.ParseInt(hi, 10, 64)
		}
		q.Del(k)
	}
	u.RawQuery = q.Encode()
	c.SpiderX = u.String()
	if r.Mldsa65Verify != "" {
		if c.Mldsa65Verify, err = base64.RawURLEncoding.DecodeString(r.Mldsa65Verify); err != nil || len(c.Mldsa65Verify) != 1952 {
			return nil, fmt.Errorf(`reality "mldsa65Verify" 无效`)
		}
	}
	return c, nil
}

// parseAllow 解析 "IP[/前缀]:端口[-端口]"。
func parseAllow(item string) (*stdnet.IPNet, uint32, uint32, error) {
	i := strings.LastIndex(item, ":")
	if i <= 0 {
		return nil, 0, 0, fmt.Errorf(`allow 项 %q 需要 IP:端口`, item)
	}
	hostPart := strings.Trim(item[:i], "[]")
	portPart := item[i+1:]

	var ipnet *stdnet.IPNet
	if strings.Contains(hostPart, "/") {
		_, n, err := stdnet.ParseCIDR(hostPart)
		if err != nil {
			return nil, 0, 0, fmt.Errorf(`allow 项 %q 的网段无效`, item)
		}
		ipnet = n
	} else {
		ip := stdnet.ParseIP(hostPart)
		if ip == nil {
			return nil, 0, 0, fmt.Errorf(`allow 项 %q 只能写 IP（不支持域名）`, item)
		}
		bits := 128
		if ip4 := ip.To4(); ip4 != nil {
			ip, bits = ip4, 32
		}
		ipnet = &stdnet.IPNet{IP: ip, Mask: stdnet.CIDRMask(bits, bits)}
	}

	lo, hi, ok := strings.Cut(portPart, "-")
	if !ok {
		hi = lo
	}
	from, err1 := strconv.ParseUint(lo, 10, 16)
	to, err2 := strconv.ParseUint(hi, 10, 16)
	if err1 != nil || err2 != nil || from == 0 || to < from {
		return nil, 0, 0, fmt.Errorf(`allow 项 %q 的端口无效`, item)
	}
	return ipnet, uint32(from), uint32(to), nil
}

// buildAllowRules 把 allow 列表转成 freedom 的 Allow 规则。
// 服务端发来的目标若不在列表中，会被 freedom 默认的 "vless-reverse 全拦截" 规则挡住。
func buildAllowRules(list []AllowEntry) ([]*freedom.FinalRuleConfig, error) {
	var rules []*freedom.FinalRuleConfig
	for _, item := range list {
		ipnet, from, to, err := parseAllow(strings.TrimSpace(item.Target))
		if err != nil {
			return nil, err
		}
		ones, _ := ipnet.Mask.Size()
		ipBytes := ipnet.IP
		if ip4 := ipBytes.To4(); ip4 != nil {
			ipBytes = ip4
		}
		var networks []net.Network
		switch strings.ToLower(strings.ReplaceAll(item.Network, " ", "")) {
		case "", "tcp":
			networks = []net.Network{net.Network_TCP}
		case "udp":
			networks = []net.Network{net.Network_UDP}
		case "tcp,udp", "udp,tcp", "tcp+udp":
			networks = []net.Network{net.Network_TCP, net.Network_UDP}
		default:
			return nil, fmt.Errorf(`allow 项 %q 的 network 只能是 tcp、udp 或 tcp,udp`, item.Target)
		}
		rules = append(rules, &freedom.FinalRuleConfig{
			Action:   freedom.RuleAction_Allow,
			Networks: networks,
			PortList: &net.PortList{Range: []*net.PortRange{{From: from, To: to}}},
			Ip: []*geodata.IPRule{{Value: &geodata.IPRule_Custom{Custom: &geodata.CIDRRule{
				Cidr: &geodata.CIDR{Ip: ipBytes, Prefix: uint32(ones)},
			}}}},
		})
	}
	return rules, nil
}

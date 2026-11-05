// Package xray 是与上游 Xray-core 对接的唯一一层（适配层）。
//
// 项目里只有这个目录 import github.com/xtls/xray-core；跟进上游时只需要看这里，
// 并运行 upstream_test.go（它用官方 infra/conf 生成同样的配置来对比）。
// 与上游耦合的地方见仓库根目录 UPSTREAM.md。
package xray

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	stdnet "net"
	"net/url"
	"strconv"
	"strings"

	"github.com/tcpqueue/xrayclient-reversepanel/internal/config"
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

// 每个服务端独立运行一个 Xray 实例：出站只有 direct-N（放行出口，默认出站）和 tunnel-N（连服务端）。
// 这样不需要 app/router（它会连带引入完整 DNS 模块，约 5MB），服务端之间也天然隔离。
func tunnelTag(i int) string { return "tunnel-" + strconv.Itoa(i) }
func directTag(i int) string { return "direct-" + strconv.Itoa(i) }

type Built struct {
	Name   string
	Config *core.Config
}

// Build 校验配置并为每个启用的服务端生成一份 Xray 配置。
func Build(c *config.Config) ([]Built, error) {
	if err := c.Validate(); err != nil {
		return nil, err
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
	var out []Built
	for i := range c.Servers {
		s := &c.Servers[i]
		if s.Disabled {
			continue
		}
		tunnel, err := buildOutbound(s, tunnelTag(i))
		if err != nil {
			return nil, fmt.Errorf(`服务端 %q: %w`, s.Name, err)
		}
		var mine []config.AllowEntry
		for _, a := range c.Allow {
			if a.AllowedFor(s.Name) {
				mine = append(mine, a)
			}
		}
		allow, err := buildAllowRules(mine)
		if err != nil {
			return nil, err
		}
		out = append(out, Built{Name: s.Name, Config: &core.Config{
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

// buildAccount 生成 VLESS 用户（带新版 reverse）。对应上游 infra/conf/vless.go 的客户端部分。
func buildAccount(s *config.ServerConfig) (*vless.Account, error) {
	u, err := uuid.ParseString(s.UUID)
	if err != nil {
		return nil, fmt.Errorf(`"uuid" 无效: %w`, err)
	}
	account := &vless.Account{
		Id:      u.String(),
		Flow:    s.Flow,
		Reverse: &vless.Reverse{Tag: s.LocalTag()},
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
	return account, nil
}

// buildOutbound 生成连接这个服务端的 VLESS 出站。
func buildOutbound(s *config.ServerConfig, tag string) (*core.OutboundHandlerConfig, error) {
	account, err := buildAccount(s)
	if err != nil {
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
		rc, err := buildReality(s.Reality)
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

// parseEncryption 复刻上游 infra/conf/vless.go 中客户端 encryption 字符串的解析。
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
	padding, keys := 0, 0
	for _, r := range p[3:] {
		if len(r) < 20 {
			padding += len(r) + 1
			continue
		}
		if b, _ := base64.RawURLEncoding.DecodeString(r); len(b) != 32 && len(b) != 1184 {
			return fmt.Errorf(`"encryption" 中的公钥无效`)
		}
		keys++
	}
	if keys == 0 {
		return fmt.Errorf(`"encryption" 缺少公钥`)
	}
	rest := s[len(p[0])+len(p[1])+len(p[2])+3:]
	if padding > 0 {
		a.Padding = rest[:padding-1]
		rest = rest[padding:]
	}
	a.Encryption = rest
	return nil
}

// buildReality 对应上游 infra/conf/transport_security.go 中 REALITY 客户端部分。
func buildReality(r *config.RealityConfig) (*reality.Config, error) {
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
	// 与上游一致：从 spiderX 的查询参数中取出 p/c/t/i/r 填入 SpiderY
	c.SpiderY = make([]int64, 10)
	u, err := url.Parse(spiderX)
	if err != nil {
		return nil, fmt.Errorf(`reality "spiderX" 无效: %w`, err)
	}
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

// buildAllowRules 把 allow 列表转成 freedom 的 Allow 规则。
// 服务端发来的目标若不在列表中，会被 freedom 默认的 "vless-reverse 全拦截" 规则挡住。
func buildAllowRules(list []config.AllowEntry) ([]*freedom.FinalRuleConfig, error) {
	var rules []*freedom.FinalRuleConfig
	for _, item := range list {
		ipnet, from, to, err := config.ParseAllow(strings.TrimSpace(item.Target))
		if err != nil {
			return nil, err
		}
		ones, _ := ipnet.Mask.Size()
		ipBytes := ipnet.IP
		if ip4 := ipBytes.To4(); ip4 != nil {
			ipBytes = ip4
		}
		useTCP, useUDP, err := config.ParseNetwork(item.Network)
		if err != nil {
			return nil, fmt.Errorf(`allow 项 %q: %w`, item.Target, err)
		}
		var networks []net.Network
		if useTCP {
			networks = append(networks, net.Network_TCP)
		}
		if useUDP {
			networks = append(networks, net.Network_UDP)
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
	// 最后显式加一条“其余全部拦截”（空条件匹配一切）。上游对 vless-reverse 默认就是全拦截，
	// 这里再写一遍，即使上游以后改了默认规则，放行范围也不会扩大。
	rules = append(rules, &freedom.FinalRuleConfig{Action: freedom.RuleAction_Block})
	return rules, nil
}

// Package config 是 xrayclient-reversepanel 自己的配置格式：类型定义、读写和与 Xray 无关的校验。
// 本包不引用 xray-core；把配置翻译成 Xray protobuf 的工作在 internal/xray 中完成。
package config

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	stdnet "net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

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

type WebConfig struct {
	Listen       string `json:"listen"`                 // 如 "0.0.0.0:7400"（强制 HTTPS）
	User         string `json:"user,omitempty"`         // 默认 admin
	PasswordHash string `json:"passwordHash,omitempty"` // 网页里修改密码后写入（PBKDF2）
	// Password 是明文密码：安装/重置时生成的随机初始密码保存在这里，便于用 info 命令查看；
	// 在网页里修改密码后会换成 passwordHash 并清空此项。三者都不填时为默认 admin / admin。
	Password string `json:"password,omitempty"`
	Cert     string `json:"cert,omitempty"` // 可选：自己的证书路径；留空使用自动生成的自签名证书
	Key      string `json:"key,omitempty"`
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
	MaxProcs  int   `json:"maxProcs"`  // GOMAXPROCS，0=使用全部 CPU 线程
	TrimSec   int   `json:"trimSec"`   // 每隔多少秒归还空闲内存给系统，0=默认 60，负数=关闭
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

// AllowedFor 判断这个本地服务是否分配给了某个服务端。
func (a *AllowEntry) AllowedFor(server string) bool {
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

// Key 是本地服务在网页和健康检测里的标识：有名称用名称，否则用目标。
func (a *AllowEntry) Key() string {
	if a.Name != "" {
		return a.Name
	}
	return a.Target
}

// LocalTag 是客户端本地使用的 reverse tag。它不会发给服务端（线上只携带“这是反向连接”的命令，
// 服务端用它自己配置里该用户的 reverse.tag），所以不必与服务端一致；留空时按名称自动生成。
// 它只用于在本地区分流量来自哪个服务端，因此各服务端之间不能重复。
func (s *ServerConfig) LocalTag() string {
	if s.Tag != "" {
		return s.Tag
	}
	return "rvs:" + s.Name
}

// Load 读取配置文件；兼容旧版单服务端格式（顶层的 server/uuid/tag/... 转成 servers[0]）。
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Config{}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("解析配置失败: %w", err)
	}
	if len(c.Servers) == 0 {
		var legacy ServerConfig
		if json.Unmarshal(b, &legacy) == nil && (legacy.Server != "" || legacy.UUID != "" || legacy.Tag != "") {
			legacy.Name = "默认"
			c.Servers = []ServerConfig{legacy}
		}
	}
	return c, nil
}

// Save 原子地写入配置文件（权限 0600：里面有 UUID 和网页密码）。
func (c *Config) Save(path string) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// NewDefault 生成首次运行用的配置：一个待填写的服务端，以及随机的网页初始密码。
func NewDefault(listen string) (*Config, string) {
	pw := RandomPassword(12)
	return &Config{
		Servers: []ServerConfig{{Name: "服务端1", Security: "reality", Flow: "xtls-rprx-vision", Encryption: "none"}},
		Web:     &WebConfig{Listen: listen, User: "admin", Password: pw},
	}, pw
}

// RandomPassword 生成不含易混淆字符的随机密码。
func RandomPassword(n int) string {
	const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, n)
	rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)] // 256 % 56 的偏差可以忽略
	}
	return string(b)
}

// Validate 做与 Xray 无关的校验（名称、引用、目标格式、健康检测、日志级别、网页监听）。
// 服务端连接参数（UUID、flow、REALITY、encryption）由 internal/xray 在生成配置时校验。
func (c *Config) Validate() error {
	if len(c.Servers) == 0 {
		return fmt.Errorf(`至少需要配置一个服务端`)
	}
	if len(c.Allow) == 0 {
		return fmt.Errorf(`"allow" 为空：反向流量默认全部拦截，至少写一个本地目标，如 "127.0.0.1:22"`)
	}
	names, tags := map[string]bool{}, map[string]bool{}
	for i := range c.Servers {
		s := &c.Servers[i]
		s.Name = strings.TrimSpace(s.Name)
		if s.Name == "" {
			return fmt.Errorf(`第 %d 个服务端缺少名称`, i+1)
		}
		if names[s.Name] {
			return fmt.Errorf(`服务端名称 %q 重复`, s.Name)
		}
		names[s.Name] = true
		if tags[s.LocalTag()] {
			return fmt.Errorf(`tag %q 被多个服务端使用，请删掉 tag 让程序自动生成，或改成互不相同`, s.LocalTag())
		}
		tags[s.LocalTag()] = true
	}
	for i := range c.Allow {
		a := &c.Allow[i]
		a.Target = strings.TrimSpace(a.Target)
		if _, _, _, err := ParseAllow(a.Target); err != nil {
			return err
		}
		if _, _, err := ParseNetwork(a.Network); err != nil {
			return fmt.Errorf(`allow 项 %q: %w`, a.Target, err)
		}
		if a.Health != nil && a.Health.Enabled {
			if err := a.Health.Validate(a); err != nil {
				return err
			}
		}
		for _, n := range a.Servers {
			if !names[n] {
				return fmt.Errorf(`本地服务 %q 引用了不存在的服务端 %q`, a.Target, n)
			}
		}
	}
	switch strings.ToLower(c.LogLevel) {
	case "", "debug", "info", "warning", "error", "none":
	default:
		return fmt.Errorf(`"logLevel" 无效: %s`, c.LogLevel)
	}
	if c.Web != nil && c.Web.Listen != "" {
		if _, _, err := stdnet.SplitHostPort(c.Web.Listen); err != nil {
			return fmt.Errorf(`"web.listen" 需要 host:port`)
		}
	}
	return nil
}

// ParseNetwork 解析 allow 项的 network。
func ParseNetwork(s string) (tcp, udp bool, err error) {
	switch strings.ToLower(strings.ReplaceAll(s, " ", "")) {
	case "", "tcp":
		return true, false, nil
	case "udp":
		return false, true, nil
	case "tcp,udp", "udp,tcp", "tcp+udp":
		return true, true, nil
	}
	return false, false, fmt.Errorf(`network 只能是 tcp、udp 或 tcp,udp`)
}

// ParseAllow 解析 "IP[/前缀]:端口[-端口]"。
func ParseAllow(item string) (*stdnet.IPNet, uint32, uint32, error) {
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

// NameFor 返回某个实际连接目标（"ip:port" 或 "udp:ip:port"）匹配到的 allow 项名称。
func (c *Config) NameFor(addr string) string {
	isUDP := strings.HasPrefix(addr, "udp:")
	host, portStr, err := stdnet.SplitHostPort(strings.TrimPrefix(addr, "udp:"))
	if err != nil {
		return ""
	}
	ip := stdnet.ParseIP(host)
	port, _ := strconv.ParseUint(portStr, 10, 16)
	for i := range c.Allow {
		a := &c.Allow[i]
		n, from, to, err := ParseAllow(a.Target)
		tcpOK, udpOK, err2 := ParseNetwork(a.Network)
		if err != nil || err2 != nil || ip == nil {
			continue
		}
		if (isUDP && udpOK || !isUDP && tcpOK) && n.Contains(ip) && uint64(from) <= port && port <= uint64(to) {
			return a.Key()
		}
	}
	return ""
}

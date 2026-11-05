package config

import (
	"encoding/hex"
	"fmt"
	stdnet "net"
	"strings"
)

// HealthConfig 是本地服务的健康检测设置（检测逻辑在 internal/health）。
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

// HealthTarget 返回实际检测地址。
func (a *AllowEntry) HealthTarget() (string, error) {
	h := a.Health
	if h.Target != "" {
		if _, _, err := stdnet.SplitHostPort(h.Target); err != nil {
			return "", fmt.Errorf(`出站 %q 的检测地址需要 host:端口`, a.Key())
		}
		return h.Target, nil
	}
	t := strings.TrimSpace(a.Target)
	host, port, err := stdnet.SplitHostPort(t)
	if err != nil || strings.Contains(host, "/") || strings.Contains(port, "-") {
		return "", fmt.Errorf(`出站 %q 是网段或端口范围，请在健康检测里指定一个检测地址`, a.Key())
	}
	return t, nil
}

func (h *HealthConfig) Validate(a *AllowEntry) error {
	switch h.Type {
	case "tcp", "udp", "http", "https":
	default:
		return fmt.Errorf(`出站 %q 的检测类型只能是 tcp/udp/http/https`, a.Key())
	}
	if _, err := a.HealthTarget(); err != nil {
		return err
	}
	if h.Interval != 0 && h.Interval < 5 {
		return fmt.Errorf(`出站 %q 的检测间隔不能小于 5 秒`, a.Key())
	}
	if h.Timeout < 0 || h.Timeout > 60 {
		return fmt.Errorf(`出站 %q 的检测超时需在 1–60 秒`, a.Key())
	}
	if strings.HasPrefix(h.Payload, "hex:") {
		if _, err := hex.DecodeString(strings.TrimPrefix(h.Payload, "hex:")); err != nil {
			return fmt.Errorf(`出站 %q 的 UDP 探测内容不是有效的十六进制`, a.Key())
		}
	}
	return nil
}

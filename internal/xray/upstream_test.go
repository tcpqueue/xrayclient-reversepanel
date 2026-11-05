package xray

// 跟进上游 Xray-core 的“哨兵”测试。升级 go.mod 中的 xray-core 版本后运行：
//
//	go test ./internal/xray/
//
// 1. 用官方 infra/conf（官方 JSON 配置加载器）生成同样的 VLESS 出站，与本项目直接生成的 protobuf 逐字段对比；
//    不一致说明上游改了配置语义（例如 encryption、REALITY、reverse 的字段），需要同步修改 build.go。
// 2. 检查上游源码中本项目依赖的几处行为（reverse 启动延迟、vless-reverse 的入站名、反向通道日志）。
// infra/conf 只在测试里引用，不会进入最终二进制。

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tcpqueue/xrayclient-reversepanel/internal/config"
	"github.com/xtls/xray-core/app/proxyman"
	"github.com/xtls/xray-core/infra/conf"
	"github.com/xtls/xray-core/proxy/vless"
	vlessout "github.com/xtls/xray-core/proxy/vless/outbound"
	"github.com/xtls/xray-core/transport/internet/reality"
	"google.golang.org/protobuf/proto"
)

const (
	testUUID = "b831381d-6324-4d53-ad4f-8cda48b30811"
	testPBK  = "Z84J2IelR9ch3k8VtlVhhs5ycBUlXA7wHBWcBrjqnAw" // 32 字节 x25519 公钥（base64url）
	// mlkem768x25519plus 客户端串：x25519 公钥，带一个 padding 段
	testEnc = "mlkem768x25519plus.native.0rtt.Z84J2IelR9ch3k8VtlVhhs5ycBUlXA7wHBWcBrjqnAw"
)

type upstreamCase struct {
	name string
	srv  config.ServerConfig
	// 官方客户端出站 JSON（简化写法，reverse 只支持这种写法）
	official string
}

func cases() []upstreamCase {
	return []upstreamCase{
		{
			name: "reality+vision",
			srv: config.ServerConfig{Name: "a", Server: "example.com:443", UUID: testUUID, Flow: "xtls-rprx-vision",
				Encryption: "none", Security: "reality", Tag: "rvs:a",
				Reality: &config.RealityConfig{ServerName: "www.example.com", Password: testPBK, ShortID: "0123abcd", Fingerprint: "chrome", SpiderX: "/path?p=10-20&c=1&x=y"}},
			official: `{"protocol":"vless","tag":"t","settings":{"address":"example.com","port":443,"id":"` + testUUID + `","flow":"xtls-rprx-vision","encryption":"none","reverse":{"tag":"rvs:a"}},
				"streamSettings":{"network":"raw","security":"reality","realitySettings":{"serverName":"www.example.com","password":"` + testPBK + `","shortId":"0123abcd","fingerprint":"chrome","spiderX":"/path?p=10-20&c=1&x=y"}}}`,
		},
		{
			name: "reality+testseed",
			srv: config.ServerConfig{Name: "b", Server: "1.2.3.4:8443", UUID: testUUID, Flow: "xtls-rprx-vision-udp443", Testseed: []uint32{900, 500, 900, 256},
				Encryption: "none", Security: "reality", Tag: "rvs:b",
				Reality: &config.RealityConfig{ServerName: "a.com", Password: testPBK, ShortID: "", Fingerprint: "firefox"}},
			official: `{"protocol":"vless","tag":"t","settings":{"address":"1.2.3.4","port":8443,"id":"` + testUUID + `","flow":"xtls-rprx-vision-udp443","testseed":[900,500,900,256],"encryption":"none","reverse":{"tag":"rvs:b"}},
				"streamSettings":{"network":"raw","security":"reality","realitySettings":{"serverName":"a.com","password":"` + testPBK + `","fingerprint":"firefox"}}}`,
		},
		{
			name: "vless-encryption",
			srv: config.ServerConfig{Name: "c", Server: "example.org:80", UUID: testUUID, Flow: "xtls-rprx-vision",
				Encryption: testEnc, Security: "none", Tag: "rvs:c"},
			official: `{"protocol":"vless","tag":"t","settings":{"address":"example.org","port":80,"id":"` + testUUID + `","flow":"xtls-rprx-vision","encryption":"` + testEnc + `","reverse":{"tag":"rvs:c"}},
				"streamSettings":{"network":"raw"}}`,
		},
		{
			name: "vless-encryption+padding+xorpub",
			srv: config.ServerConfig{Name: "d", Server: "example.org:80", UUID: testUUID,
				Encryption: "mlkem768x25519plus.xorpub.1rtt.100-111-1111.75-0-111." + testPBK, Security: "none", Tag: "rvs:d"},
			official: `{"protocol":"vless","tag":"t","settings":{"address":"example.org","port":80,"id":"` + testUUID + `","encryption":"mlkem768x25519plus.xorpub.1rtt.100-111-1111.75-0-111.` + testPBK + `","reverse":{"tag":"rvs:d"}},
				"streamSettings":{"network":"raw"}}`,
		},
	}
}

func officialOutbound(t *testing.T, js string) (*vless.Account, *reality.Config) {
	t.Helper()
	var oc conf.OutboundDetourConfig
	if err := json.Unmarshal([]byte(js), &oc); err != nil {
		t.Fatalf("官方 JSON 解析失败: %v", err)
	}
	h, err := oc.Build()
	if err != nil {
		t.Fatalf("官方 infra/conf 构建失败: %v", err)
	}
	return extract(t, h.ProxySettings.GetValue(), h.SenderSettings.GetValue())
}

func extract(t *testing.T, proxyRaw, senderRaw []byte) (*vless.Account, *reality.Config) {
	t.Helper()
	oc := &vlessout.Config{}
	if err := proto.Unmarshal(proxyRaw, oc); err != nil {
		t.Fatal(err)
	}
	acc := &vless.Account{}
	if err := proto.Unmarshal(oc.Vnext.User.Account.Value, acc); err != nil {
		t.Fatal(err)
	}
	sc := &proxyman.SenderConfig{}
	if err := proto.Unmarshal(senderRaw, sc); err != nil {
		t.Fatal(err)
	}
	var rc *reality.Config
	if ss := sc.StreamSettings; ss != nil && len(ss.SecuritySettings) > 0 {
		rc = &reality.Config{}
		if err := proto.Unmarshal(ss.SecuritySettings[0].Value, rc); err != nil {
			t.Fatal(err)
		}
	}
	return acc, rc
}

func TestMatchesUpstreamConfigLoader(t *testing.T) {
	for _, tc := range cases() {
		t.Run(tc.name, func(t *testing.T) {
			wantAcc, wantRC := officialOutbound(t, tc.official)
			h, err := buildOutbound(&tc.srv, "t")
			if err != nil {
				t.Fatalf("本项目构建失败: %v", err)
			}
			gotAcc, gotRC := extract(t, h.ProxySettings.GetValue(), h.SenderSettings.GetValue())
			if !proto.Equal(gotAcc, wantAcc) {
				t.Errorf("VLESS 用户与官方不一致\n本项目: %v\n官方:   %v", gotAcc, wantAcc)
			}
			if !proto.Equal(gotRC, wantRC) {
				t.Errorf("REALITY 配置与官方不一致\n本项目: %v\n官方:   %v", gotRC, wantRC)
			}
		})
	}
}

// 官方遇到“只有 padding 没有公钥”的 encryption 会越界 panic；本项目应返回错误。
func TestEncryptionWithoutKey(t *testing.T) {
	var a vless.Account
	if err := parseEncryption("mlkem768x25519plus.native.0rtt.100-111-1111", &a); err == nil {
		t.Fatal("应当报错")
	}
}

func TestSpiderXInvalid(t *testing.T) {
	_, err := buildReality(&config.RealityConfig{Password: testPBK, SpiderX: "/%zz"})
	if err == nil {
		t.Fatal("应当报错")
	}
}

// ---------------- 上游源码中的行为依赖 ----------------

func upstreamDir(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/xtls/xray-core").Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		t.Skip("找不到 xray-core 源码目录（需要 go 命令和模块缓存）")
	}
	return strings.TrimSpace(string(out))
}

func readUpstream(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(upstreamDir(t), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("上游文件 %s 不存在了，请检查对应逻辑是否被移动: %v", rel, err)
	}
	return string(b)
}

// instances.go 的 reverseStartGuard 必须大于上游 reverse 监控任务的启动延迟。
func TestReverseStartDelay(t *testing.T) {
	src := readUpstream(t, "proxy/vless/outbound/outbound.go")
	m := regexp.MustCompile(`time\.Sleep\((\d+) \* time\.Second\)\s*\n\s*handler\.reverse\.Start\(\)`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("上游 reverse 的延迟启动写法变了：检查 instances.go 中 reverseStartGuard 是否还需要/是否足够")
	}
	n, _ := strconv.Atoi(m[1])
	if time.Duration(n)*time.Second >= reverseStartGuard {
		t.Fatalf("上游延迟 %ds，reverseStartGuard (%v) 需要更大", n, reverseStartGuard)
	}
}

// 反向连接的入站名是 vless-reverse；freedom 对它默认全部拦截（本项目另外显式追加了全拦截规则）。
func TestReverseInboundName(t *testing.T) {
	if !strings.Contains(readUpstream(t, "proxy/vless/outbound/outbound.go"), `Name: "vless-reverse"`) {
		t.Error("上游 reverse 入站名变了：检查 tracker.go 的服务端识别和 freedom 默认规则")
	}
	if !regexp.MustCompile(`case "vless-reverse":\s*\n\s*return defaultBlockAllRule`).MatchString(readUpstream(t, "proxy/freedom/freedom.go")) {
		t.Error("上游 freedom 对 vless-reverse 的默认规则变了（本项目已显式追加全拦截，但请确认放行规则语义未变）")
	}
}

// loghook.go 通过这句日志判断“反向隧道已建立”。
func TestReverseUpLog(t *testing.T) {
	if !strings.Contains(readUpstream(t, "common/mux/server.go"), `errors.LogInfo(ctx, "received request for ", meta.Target)`) {
		t.Error("上游 mux 的 “received request for” 日志变了：检查 loghook.go 的 reverseUpLog")
	}
}

// 显式的全拦截规则必须在最后，且放行规则在它之前。
func TestAllowRulesEndWithBlock(t *testing.T) {
	rules, err := buildAllowRules([]config.AllowEntry{{Target: "127.0.0.1:22"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 || rules[1].Action.String() != "Block" || rules[1].Ip != nil || rules[1].PortList != nil || len(rules[1].Networks) != 0 {
		t.Fatalf("规则不对: %v", rules)
	}
}

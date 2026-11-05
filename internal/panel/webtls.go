package panel

// 管理网页强制 HTTPS：
//   - 没有证书时自动生成自签名证书（ECDSA P-256，825 天，到期前 30 天自动续期）。
//   - 同一个端口上：TLS 握手走 HTTPS；明文 HTTP 请求只返回一个跳转到 https:// 的响应，不会暴露任何接口。

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	stdnet "net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tcpqueue/xrayclient-reversepanel/internal/config"
)

const (
	selfSignedOrg  = "xrayclient-reversepanel self-signed"
	certValidDays  = 825 // 苹果系统对 TLS 证书有效期的上限
	certRenewAhead = 30 * 24 * time.Hour
)

// CertPaths 返回证书和私钥路径，以及是否为默认（可自动生成）的路径。
func CertPaths(wc *config.WebConfig, cfgPath string) (cert, key string, auto bool) {
	if wc != nil && wc.Cert != "" && wc.Key != "" {
		return wc.Cert, wc.Key, false
	}
	dir := filepath.Join(filepath.Dir(cfgPath), "tls")
	return filepath.Join(dir, "web.crt"), filepath.Join(dir, "web.key"), true
}

// LoadOrCreateCert 读取证书；默认路径下没有证书、或自签名证书快到期时自动（重新）生成。
func LoadOrCreateCert(certFile, keyFile string, auto bool) (tls.Certificate, error) {
	c, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err == nil {
		if auto && isSelfSigned(c.Leaf) && time.Until(c.Leaf.NotAfter) < certRenewAhead {
			fmt.Fprintln(os.Stderr, "管理网页自签名证书即将到期，重新生成")
			return GenerateCert(certFile, keyFile)
		}
		return c, nil
	}
	if !auto {
		return tls.Certificate{}, fmt.Errorf("读取管理网页证书失败: %w", err)
	}
	_, e1 := os.Stat(certFile)
	_, e2 := os.Stat(keyFile)
	if errors.Is(e1, os.ErrNotExist) || errors.Is(e2, os.ErrNotExist) {
		return GenerateCert(certFile, keyFile)
	}
	return tls.Certificate{}, fmt.Errorf("管理网页证书无效（可运行 gen-cert 重新生成）: %w", err)
}

// ReadCert 只读取证书，不会生成。
func ReadCert(certFile, keyFile string) (*x509.Certificate, error) {
	c, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return c.Leaf, nil
}

func isSelfSigned(c *x509.Certificate) bool {
	return c != nil && len(c.Subject.Organization) > 0 && c.Subject.Organization[0] == selfSignedOrg
}

var dnsLabel = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

// GenerateCert 生成自签名证书，SAN 包含 localhost、主机名和本机所有网卡地址。
func GenerateCert(certFile, keyFile string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "xrayclient-reversepanel", Organization: []string{selfSignedOrg}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(certValidDays * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []stdnet.IP{stdnet.IPv4(127, 0, 0, 1), stdnet.IPv6loopback},
	}
	if h, _ := os.Hostname(); h != "" && h != "localhost" && dnsLabel.MatchString(strings.Split(h, ".")[0]) {
		tmpl.DNSNames = append(tmpl.DNSNames, h)
	}
	for _, ip := range localIPs() {
		tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := os.MkdirAll(filepath.Dir(certFile), 0o700); err != nil {
		return tls.Certificate{}, fmt.Errorf("创建证书目录失败: %w", err)
	}
	if err := writeFileAtomic(keyFile, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, fmt.Errorf("写入私钥失败: %w", err)
	}
	if err := writeFileAtomic(certFile, certPEM, 0o644); err != nil {
		return tls.Certificate{}, fmt.Errorf("写入证书失败: %w", err)
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

func writeFileAtomic(path string, b []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, perm); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil { // WriteFile 不会修改已存在文件的权限
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// localIPs 返回本机网卡上的单播地址（不含回环和 IPv6 链路本地地址）。
func localIPs() []stdnet.IP {
	var out []stdnet.IP
	addrs, _ := stdnet.InterfaceAddrs()
	for _, a := range addrs {
		n, ok := a.(*stdnet.IPNet)
		if !ok || n.IP.IsLoopback() || n.IP.IsLinkLocalUnicast() || n.IP.IsMulticast() {
			continue
		}
		if ip4 := n.IP.To4(); ip4 != nil {
			out = append(out, ip4)
		} else {
			out = append(out, n.IP)
		}
	}
	return out
}

type CertInfo struct {
	File       string   `json:"file"`
	SelfSigned bool     `json:"selfSigned"`
	Subject    string   `json:"subject"`
	NotAfter   int64    `json:"notAfter"`
	Names      []string `json:"names"`
	SHA256     string   `json:"sha256"`
}

// CertInfoOf 返回证书的展示信息。
func CertInfoOf(file string, c *x509.Certificate) CertInfo {
	if c == nil {
		return CertInfo{File: file}
	}
	sum := sha256.Sum256(c.Raw)
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	var fp []string
	for i := 0; i < len(h); i += 2 {
		fp = append(fp, h[i:i+2])
	}
	names := append([]string{}, c.DNSNames...)
	for _, ip := range c.IPAddresses {
		names = append(names, ip.String())
	}
	return CertInfo{File: file, SelfSigned: isSelfSigned(c), Subject: c.Subject.CommonName,
		NotAfter: c.NotAfter.UnixMilli(), Names: names, SHA256: strings.Join(fp, ":")}
}

// LoginURLs 根据监听地址给出可以在浏览器里打开的地址。
func LoginURLs(listen string) []string {
	host, port, err := stdnet.SplitHostPort(listen)
	if err != nil {
		return nil
	}
	fmtURL := func(h string) string { return "https://" + stdnet.JoinHostPort(h, port) + "/" }
	if host != "" && host != "0.0.0.0" && host != "::" {
		return []string{fmtURL(host)}
	}
	var out []string
	for _, ip := range localIPs() {
		if host == "0.0.0.0" && ip.To4() == nil {
			continue // 只监听 IPv4
		}
		out = append(out, fmtURL(ip.String()))
	}
	return append(out, fmtURL("127.0.0.1"))
}

// ---------------- 同端口 HTTPS + HTTP 跳转 ----------------

// sniffListener 看连接的第一个字节：0x16 是 TLS 握手，否则当作明文 HTTP（只会被重定向到 https）。
// 判断放在独立的 goroutine 里，慢客户端不会卡住 Accept。
type sniffListener struct {
	inner   stdnet.Listener
	tlsConf *tls.Config
	ch      chan stdnet.Conn
	done    chan struct{}
	once    sync.Once
}

func newSniffListener(inner stdnet.Listener, conf *tls.Config) *sniffListener {
	l := &sniffListener{inner: inner, tlsConf: conf, ch: make(chan stdnet.Conn), done: make(chan struct{})}
	go l.loop()
	return l
}

func (l *sniffListener) loop() {
	sem := make(chan struct{}, 128) // 同时等待首字节的连接数上限
	for {
		c, err := l.inner.Accept()
		if err != nil {
			var ne stdnet.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			l.Close()
			return
		}
		select {
		case sem <- struct{}{}:
		default:
			c.Close() // 太多半开连接，直接丢弃
			continue
		}
		go func() {
			defer func() { <-sem }()
			l.classify(c)
		}()
	}
}

func (l *sniffListener) classify(c stdnet.Conn) {
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReaderSize(c, 16)
	b, err := br.Peek(1)
	c.SetReadDeadline(time.Time{})
	if err != nil {
		c.Close()
		return
	}
	var out stdnet.Conn = &peekedConn{Conn: c, r: br}
	if b[0] == 0x16 {
		out = tls.Server(out, l.tlsConf)
	}
	select {
	case l.ch <- out:
	case <-l.done:
		c.Close()
	}
}

func (l *sniffListener) Accept() (stdnet.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, stdnet.ErrClosed
	}
}

func (l *sniffListener) Close() error {
	l.once.Do(func() {
		close(l.done)
		l.inner.Close()
	})
	return nil
}

func (l *sniffListener) Addr() stdnet.Addr { return l.inner.Addr() }

type peekedConn struct {
	stdnet.Conn
	r *bufio.Reader
}

func (c *peekedConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// Package panel 是管理网页：HTTPS 服务、登录、接口和前端资源。本包不直接引用 xray-core。
package panel

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	stdnet "net"
	"net/http"
	"net/url"
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tcpqueue/xrayclient-reversepanel/internal/app"
	"github.com/tcpqueue/xrayclient-reversepanel/internal/config"
	"github.com/tcpqueue/xrayclient-reversepanel/internal/health"
	"github.com/tcpqueue/xrayclient-reversepanel/internal/xray"
)

//go:embed web/index.html web/theme.js web/app.js web/app.css web/vendor/vue.js web/vendor/bootstrap.css web/vendor/LICENSES.txt
var webFS embed.FS

const cookieName = "xl_session"

// 静态文件：首次请求时压缩一次并缓存（全部约 100KB）
var gzCache sync.Map // name -> []byte

// index.html 中的资源地址带上内容哈希（app.js?v=xxxx），升级后浏览器一定会取新文件，
// 不会出现“新页面 + 旧脚本”的错配；资源本身仍可长期缓存。
var (
	indexOnce sync.Once
	indexHTML []byte
)

func versionedIndex() []byte {
	indexOnce.Do(func() {
		b, _ := webFS.ReadFile("web/index.html")
		for _, name := range []string{"theme.js", "app.js", "app.css", "vendor/vue.js", "vendor/bootstrap.css"} {
			raw, _ := webFS.ReadFile("web/" + name)
			sum := sha256.Sum256(raw)
			v := hex.EncodeToString(sum[:4])
			b = bytes.ReplaceAll(b, []byte(`"`+name+`"`), []byte(`"`+name+"?v="+v+`"`))
		}
		indexHTML = b
	})
	return indexHTML
}

func serveStatic(w http.ResponseWriter, req *http.Request, name string) {
	raw, err := webFS.ReadFile("web/" + name)
	if name == "index.html" {
		raw, err = versionedIndex(), nil
	}
	if err != nil {
		http.NotFound(w, req)
		return
	}
	ct := map[string]string{".html": "text/html; charset=utf-8", ".js": "text/javascript; charset=utf-8",
		".css": "text/css; charset=utf-8", ".txt": "text/plain; charset=utf-8"}[path.Ext(name)]
	w.Header().Set("Content-Type", ct)
	if name == "index.html" {
		w.Header().Set("Cache-Control", "no-store")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=86400")
	}
	w.Header().Set("Vary", "Accept-Encoding")
	if !strings.Contains(req.Header.Get("Accept-Encoding"), "gzip") {
		w.Write(raw)
		return
	}
	gz, ok := gzCache.Load(name)
	if !ok {
		var b bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&b, gzip.BestCompression)
		zw.Write(raw)
		zw.Close()
		gz, _ = gzCache.LoadOrStore(name, b.Bytes())
	}
	w.Header().Set("Content-Encoding", "gzip")
	w.Write(gz.([]byte))
}

// Server 是管理网页（强制 HTTPS）。监听地址可以在运行中更换。
type Server struct {
	mu       sync.Mutex
	r        *app.Runner
	handler  http.Handler
	tlsConf  *tls.Config
	certFile string
	leaf     *x509.Certificate
	addr     string
	ln       stdnet.Listener
	srv      *http.Server
}

// Start 启动管理网页；version 为程序版本号（显示在网页上）。
func Start(wc *config.WebConfig, r *app.Runner, version string) (*Server, error) {
	auth := newAuth(wc)
	if _, isDef := auth.info(); isDef {
		fmt.Fprintln(os.Stderr, "提示: 管理网页正在使用默认账号密码 admin / admin，请登录后尽快修改")
	}
	certFile, keyFile, auto := CertPaths(wc, r.Path())
	cert, err := LoadOrCreateCert(certFile, keyFile, auto)
	if err != nil {
		return nil, err
	}
	ws := &Server{r: r, certFile: certFile, leaf: cert.Leaf, tlsConf: &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"http/1.1"},
	}}

	mux := http.NewServeMux()

	// ---------- 静态页面（不含任何敏感信息，无需登录） ----------
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, req *http.Request) { serveStatic(w, req, "index.html") })
	for _, f := range []string{"theme.js", "app.js", "app.css", "vendor/vue.js", "vendor/bootstrap.css", "vendor/LICENSES.txt"} {
		name := f
		mux.HandleFunc("GET /"+name, func(w http.ResponseWriter, req *http.Request) { serveStatic(w, req, name) })
	}

	// ---------- 登录 ----------
	mux.HandleFunc("GET /api/session", func(w http.ResponseWriter, req *http.Request) {
		user, isDef := auth.info()
		c, _ := req.Cookie(cookieName)
		ok := c != nil && auth.valid(c.Value)
		out := map[string]any{"loggedIn": ok}
		if ok {
			out["user"], out["defaultPassword"] = user, isDef
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("POST /api/login", func(w http.ResponseWriter, req *http.Request) {
		var in struct{ User, Password string }
		if json.NewDecoder(io.LimitReader(req.Body, 4096)).Decode(&in) != nil {
			httpErr(w, "请求格式错误")
			return
		}
		ip, _, _ := stdnet.SplitHostPort(req.RemoteAddr)
		tok, err := auth.login(ip, in.User, in.Password)
		if err != nil {
			httpErr(w, err.Error())
			return
		}
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: tok, Path: "/", HttpOnly: true, Secure: true,
			SameSite: http.SameSiteStrictMode, MaxAge: int(sessionTTL.Seconds())})
		_, isDef := auth.info()
		writeJSON(w, map[string]any{"ok": true, "defaultPassword": isDef})
	})
	mux.HandleFunc("POST /api/logout", func(w http.ResponseWriter, req *http.Request) {
		if c, _ := req.Cookie(cookieName); c != nil {
			auth.logout(c.Value)
		}
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
		writeJSON(w, map[string]any{"ok": true})
	})

	// ---------- 以下接口需要登录 ----------
	api := http.NewServeMux()
	api.HandleFunc("POST /api/password", func(w http.ResponseWriter, req *http.Request) {
		var in struct{ Old, User, New string }
		if json.NewDecoder(io.LimitReader(req.Body, 4096)).Decode(&in) != nil {
			httpErr(w, "请求格式错误")
			return
		}
		c, _ := req.Cookie(cookieName)
		user, hash, err := auth.change(c.Value, in.Old, in.User, in.New)
		if err != nil {
			httpErr(w, err.Error())
			return
		}
		if err := r.UpdateWeb(func(wc *config.WebConfig) { wc.User, wc.PasswordHash, wc.Password = user, hash, "" }); err != nil {
			httpErr(w, "密码已在本次运行中生效，但写入配置文件失败: "+err.Error())
			return
		}
		writeJSON(w, map[string]string{"ok": "密码已修改"})
	})
	api.HandleFunc("GET /api/status", func(w http.ResponseWriter, _ *http.Request) {
		on, cfg, lastErr := r.Running()
		v := statusView{Snapshot: xray.TakeSnapshot(on, cfg)}
		if !on && lastErr != "" {
			v.Detail = "内核未运行：" + lastErr
		}
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		v.HeapMB, v.SysMB = round1(mb(m.HeapAlloc)), round1(mb(m.Sys-m.HeapReleased))
		v.RssMB = round1(mb(rssBytes()))
		v.Goroutines = runtime.NumGoroutine()
		v.MaxProcs = runtime.GOMAXPROCS(0)
		v.Health = health.Snapshot()
		v.Uptime = int64(time.Since(started).Seconds())
		v.Version = xray.Version()
		v.AppVersion = version
		writeJSON(w, v)
	})
	api.HandleFunc("GET /api/config", func(w http.ResponseWriter, _ *http.Request) {
		_, cfg, _ := r.Running()
		if cfg == nil {
			cfg = &config.Config{}
		}
		out := *cfg
		out.Web = nil
		writeJSON(w, out)
	})
	api.HandleFunc("PUT /api/config", func(w http.ResponseWriter, req *http.Request) {
		c := &config.Config{}
		if err := json.NewDecoder(io.LimitReader(req.Body, 1<<20)).Decode(c); err != nil {
			httpErr(w, "配置格式错误: "+err.Error())
			return
		}
		if err := r.Apply(c); err != nil {
			httpErr(w, err.Error())
			return
		}
		writeJSON(w, map[string]string{"ok": "已保存并应用"})
	})
	api.HandleFunc("POST /api/restart", func(w http.ResponseWriter, _ *http.Request) {
		if err := r.Restart(); err != nil {
			httpErr(w, err.Error())
			return
		}
		writeJSON(w, map[string]string{"ok": "已重新连接"})
	})
	api.HandleFunc("POST /api/gen", func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		out, err := xray.Generate(q.Get("kind"), q.Get("auth"), q.Get("mode"))
		if err != nil {
			httpErr(w, err.Error())
			return
		}
		writeJSON(w, out)
	})
	api.HandleFunc("GET /api/logs", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, xray.RecentLogs())
	})
	api.HandleFunc("GET /api/web", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, ws.info())
	})
	api.HandleFunc("PUT /api/web", func(w http.ResponseWriter, req *http.Request) {
		var in struct{ Listen string }
		if json.NewDecoder(io.LimitReader(req.Body, 4096)).Decode(&in) != nil {
			httpErr(w, "请求格式错误")
			return
		}
		addr, err := NormalizeListen(in.Listen)
		if err != nil {
			httpErr(w, err.Error())
			return
		}
		if err := ws.rebind(addr); err != nil {
			httpErr(w, err.Error())
			return
		}
		if err := r.UpdateWeb(func(wc *config.WebConfig) { wc.Listen = addr }); err != nil {
			httpErr(w, "已切换到 "+addr+"，但写入配置文件失败（重启后会恢复原地址）: "+err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": "管理网页已改为监听 " + addr, "listen": addr})
	})
	mux.Handle("/api/", requireLogin(auth, api))

	ws.handler = guard(mux)
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if err := ws.serveLocked(wc.Listen); err != nil {
		return nil, err
	}
	fmt.Fprintln(os.Stderr, "管理网页（HTTPS，自签名证书，浏览器首次打开需确认）:")
	for _, u := range LoginURLs(wc.Listen) {
		fmt.Fprintln(os.Stderr, "  "+u)
	}
	return ws, nil
}

func (ws *Server) info() map[string]any {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	_, inDocker := os.Stat("/.dockerenv")
	return map[string]any{"listen": ws.addr, "urls": LoginURLs(ws.addr), "cert": CertInfoOf(ws.certFile, ws.leaf), "docker": inDocker == nil}
}

// serveLocked 在 addr 上启动一个新的 http.Server（调用方持有 ws.mu）。
func (ws *Server) serveLocked(addr string) error {
	raw, err := stdnet.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("管理网页监听 %s 失败: %w", addr, err)
	}
	ln := newSniffListener(raw, ws.tlsConf)
	srv := &http.Server{
		Handler:           ws.handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          log.New(io.Discard, "", 0), // 浏览器拒绝自签名证书时会产生大量握手错误日志
	}
	go srv.Serve(ln)
	ws.addr, ws.ln, ws.srv = addr, ln, srv
	return nil
}

// rebind 切换监听地址：先在新地址上监听成功，再关闭旧的；
// 端口相同（只换了 IP）时必须先关旧的，失败则恢复原地址。
// 旧的 server 延迟几秒再关闭，保证正在处理的这次请求能把响应发回去。
func (ws *Server) rebind(addr string) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if addr == ws.addr {
		return nil
	}
	oldAddr, oldLn, oldSrv := ws.addr, ws.ln, ws.srv
	if err := ws.serveLocked(addr); err != nil {
		_, p1, _ := stdnet.SplitHostPort(addr)
		_, p2, _ := stdnet.SplitHostPort(oldAddr)
		if p1 != p2 {
			return err
		}
		oldLn.Close()
		if err = ws.serveLocked(addr); err != nil {
			if e := ws.serveLocked(oldAddr); e != nil {
				return fmt.Errorf("%v；恢复原地址 %s 也失败: %v", err, oldAddr, e)
			}
			return err
		}
	}
	oldLn.Close()
	go func() {
		time.Sleep(3 * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		oldSrv.Shutdown(ctx)
	}()
	return nil
}

// NormalizeListen 校验并规范监听地址；只写端口时表示监听全部 IPv4 地址。
func NormalizeListen(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("监听地址不能为空")
	}
	if !strings.Contains(s, ":") {
		s = "0.0.0.0:" + s
	}
	host, port, err := stdnet.SplitHostPort(s)
	if err != nil {
		return "", fmt.Errorf("监听地址需要 IP:端口，如 0.0.0.0:7400 或 [::]:7400")
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return "", fmt.Errorf("端口需要在 1–65535 之间")
	}
	if host != "" && host != "localhost" && stdnet.ParseIP(host) == nil {
		return "", fmt.Errorf("监听地址只能写 IP（如 0.0.0.0、127.0.0.1、::）")
	}
	return stdnet.JoinHostPort(host, strconv.Itoa(p)), nil
}

func requireLogin(auth *authState, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		c, _ := req.Cookie(cookieName)
		if c == nil || !auth.valid(c.Value) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "未登录或登录已过期"})
			return
		}
		next.ServeHTTP(w, req)
	})
}

// guard：明文 HTTP 一律跳转到 https://（不处理任何接口）；拒绝跨站写请求（配合 SameSite=Strict Cookie 防 CSRF）。
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.TLS == nil {
			w.Header().Set("Connection", "close")
			http.Redirect(w, req, "https://"+req.Host+req.URL.RequestURI(), http.StatusFound)
			return
		}
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			if o := req.Header.Get("Origin"); o != "" {
				if ou, err := url.Parse(o); err != nil || ou.Host != req.Host {
					http.Error(w, "cross-origin request rejected", http.StatusForbidden)
					return
				}
			}
			if req.ContentLength != 0 && !strings.HasPrefix(req.Header.Get("Content-Type"), "application/json") {
				http.Error(w, "need application/json", http.StatusUnsupportedMediaType)
				return
			}
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		// Vue 运行时编译模板需要 unsafe-eval；不允许内联脚本和任何外部资源
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, req)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }

func mb(v uint64) float64 { return float64(v) / (1 << 20) }

var started = time.Now()

// statusView 是 /api/status 的返回：连接状态 + 进程信息。
type statusView struct {
	xray.Snapshot
	Uptime     int64                  `json:"uptime"`
	Version    string                 `json:"version"`    // Xray-core 版本
	AppVersion string                 `json:"appVersion"` // 本程序版本
	RssMB      float64                `json:"rssMB"`      // 进程实际占用（Linux /proc/self/statm），其他系统为 0
	HeapMB     float64                `json:"heapMB"`
	SysMB      float64                `json:"sysMB"`
	Goroutines int                    `json:"goroutines"`
	MaxProcs   int                    `json:"maxProcs"`
	Health     map[string]health.View `json:"health"` // key: 出站名称（或目标）
}

// rssBytes 读取进程常驻内存（RSS），与 top/ps 看到的一致；非 Linux 返回 0。
func rssBytes() uint64 {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 0
	}
	pages, _ := strconv.ParseUint(f[1], 10, 64)
	return pages * uint64(os.Getpagesize())
}

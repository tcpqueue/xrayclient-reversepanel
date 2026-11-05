package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	stdnet "net"
	"net/http"
	"net/url"
	"os"
	"path"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/xtls/xray-core/core"
)

//go:embed web/index.html web/app.js web/app.css web/vendor/vue.js web/vendor/bootstrap.css web/vendor/LICENSES.txt
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
		for _, name := range []string{"app.js", "app.css", "vendor/vue.js", "vendor/bootstrap.css"} {
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

func startWeb(wc *WebConfig, r *runner) error {
	auth := newAuth(wc)
	if _, isDef := auth.info(); isDef {
		fmt.Fprintln(os.Stderr, "提示: 管理网页正在使用默认账号密码 admin / admin，请登录后尽快修改")
	}
	ln, err := stdnet.Listen("tcp", wc.Listen)
	if err != nil {
		return fmt.Errorf("管理网页监听失败: %w", err)
	}
	fmt.Fprintf(os.Stderr, "管理网页: http://%s\n", wc.Listen)

	mux := http.NewServeMux()

	// ---------- 静态页面（不含任何敏感信息，无需登录） ----------
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, req *http.Request) { serveStatic(w, req, "index.html") })
	for _, f := range []string{"app.js", "app.css", "vendor/vue.js", "vendor/bootstrap.css", "vendor/LICENSES.txt"} {
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
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: tok, Path: "/", HttpOnly: true,
			SameSite: http.SameSiteStrictMode, MaxAge: int(sessionTTL.Seconds())})
		_, isDef := auth.info()
		writeJSON(w, map[string]any{"ok": true, "defaultPassword": isDef})
	})
	mux.HandleFunc("POST /api/logout", func(w http.ResponseWriter, req *http.Request) {
		if c, _ := req.Cookie(cookieName); c != nil {
			auth.logout(c.Value)
		}
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
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
		if err := r.updateWeb(func(wc *WebConfig) { wc.User, wc.PasswordHash, wc.Password = user, hash, "" }); err != nil {
			httpErr(w, "密码已在本次运行中生效，但写入配置文件失败: "+err.Error())
			return
		}
		writeJSON(w, map[string]string{"ok": "密码已修改"})
	})
	api.HandleFunc("GET /api/status", func(w http.ResponseWriter, _ *http.Request) {
		on, cfg, lastErr := r.running()
		v := stats.snapshot(on, cfg)
		if !on && lastErr != "" {
			v.Detail = "内核未运行：" + lastErr
		}
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		v.HeapMB, v.SysMB = round1(mb(m.HeapAlloc)), round1(mb(m.Sys-m.HeapReleased))
		v.RssMB = round1(mb(rssBytes()))
		v.Goroutines = runtime.NumGoroutine()
		v.MaxProcs = runtime.GOMAXPROCS(0)
		v.Health = health.snapshot()
		v.Uptime = int64(time.Since(stats.started).Seconds())
		v.Version = core.Version()
		writeJSON(w, v)
	})
	api.HandleFunc("GET /api/config", func(w http.ResponseWriter, _ *http.Request) {
		_, cfg, _ := r.running()
		if cfg == nil {
			cfg = &Config{}
		}
		out := *cfg
		out.Web = nil
		writeJSON(w, out)
	})
	api.HandleFunc("PUT /api/config", func(w http.ResponseWriter, req *http.Request) {
		c := &Config{}
		if err := json.NewDecoder(io.LimitReader(req.Body, 1<<20)).Decode(c); err != nil {
			httpErr(w, "配置格式错误: "+err.Error())
			return
		}
		if err := r.apply(c); err != nil {
			httpErr(w, err.Error())
			return
		}
		writeJSON(w, map[string]string{"ok": "已保存并应用"})
	})
	api.HandleFunc("POST /api/restart", func(w http.ResponseWriter, _ *http.Request) {
		if err := r.restart(); err != nil {
			httpErr(w, err.Error())
			return
		}
		writeJSON(w, map[string]string{"ok": "已重新连接"})
	})
	api.HandleFunc("POST /api/gen", func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		out, err := generate(q.Get("kind"), q.Get("auth"), q.Get("mode"))
		if err != nil {
			httpErr(w, err.Error())
			return
		}
		writeJSON(w, out)
	})
	api.HandleFunc("GET /api/logs", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, logs.recent())
	})
	mux.Handle("/api/", requireLogin(auth, api))

	srv := &http.Server{
		Handler:           guard(mux),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	go srv.Serve(ln)
	return nil
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

// guard：拒绝跨站写请求（配合 SameSite=Strict Cookie 防 CSRF）。
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
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

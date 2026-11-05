package main

// 管理网页登录：账号密码（默认 admin / admin）+ 会话 Cookie。
// 密码以 PBKDF2-SHA256 加盐哈希保存在配置的 web.passwordHash 中；
// 兼容旧版明文 web.password，修改密码后自动换成哈希。

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultUser     = "admin"
	defaultPassword = "admin"
	sessionTTL      = 7 * 24 * time.Hour
	maxSessions     = 32
	pbkdf2Iter      = 120000
)

type authState struct {
	mu        sync.Mutex
	user      string
	hash      string // pbkdf2-sha256$iter$salt$key
	plain     string // 旧版明文密码（只用于兼容）
	isDefault bool
	sessions  map[string]time.Time // token -> 过期时间
	fails     map[string]*failInfo // 按 IP 记录登录失败
}

type failInfo struct {
	n     int
	until time.Time
}

func newAuth(wc *WebConfig) *authState {
	a := &authState{sessions: map[string]time.Time{}, fails: map[string]*failInfo{}}
	a.user = wc.User
	if a.user == "" {
		a.user = defaultUser
	}
	switch {
	case wc.PasswordHash != "":
		a.hash = wc.PasswordHash
	case wc.Password != "":
		a.plain = wc.Password
	default:
		a.hash = hashPassword(defaultPassword)
		a.isDefault = true
	}
	return a
}

func hashPassword(pw string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	key, _ := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, 32)
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iter, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

func checkHash(stored, pw string) bool {
	p := strings.Split(stored, "$")
	if len(p) != 4 || p[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(p[1])
	salt, err1 := base64.RawStdEncoding.DecodeString(p[2])
	want, err2 := base64.RawStdEncoding.DecodeString(p[3])
	if err != nil || err1 != nil || err2 != nil || iter < 1000 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

func (a *authState) verify(user, pw string) bool {
	a.mu.Lock()
	u, h, plain := a.user, a.hash, a.plain
	a.mu.Unlock()
	okUser := subtle.ConstantTimeCompare([]byte(user), []byte(u)) == 1
	var okPw bool
	if plain != "" {
		okPw = subtle.ConstantTimeCompare([]byte(pw), []byte(plain)) == 1
	} else {
		okPw = checkHash(h, pw)
	}
	return okUser && okPw
}

// login 校验账号密码并创建会话；返回 token，或错误信息。
func (a *authState) login(ip, user, pw string) (string, error) {
	a.mu.Lock()
	f := a.fails[ip]
	if f != nil && time.Now().Before(f.until) {
		wait := time.Until(f.until).Round(time.Second)
		a.mu.Unlock()
		return "", fmt.Errorf("失败次数过多，请 %v 后再试", wait)
	}
	a.mu.Unlock()

	if !a.verify(user, pw) {
		a.mu.Lock()
		if len(a.fails) > 256 {
			a.fails = map[string]*failInfo{}
		}
		if f = a.fails[ip]; f == nil {
			f = &failInfo{}
			a.fails[ip] = f
		}
		f.n++
		if f.n >= 5 {
			f.n, f.until = 0, time.Now().Add(time.Minute)
		}
		a.mu.Unlock()
		time.Sleep(500 * time.Millisecond)
		return "", fmt.Errorf("账号或密码错误")
	}

	b := make([]byte, 32)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	a.mu.Lock()
	delete(a.fails, ip)
	now := time.Now()
	for k, exp := range a.sessions { // 清理过期会话
		if now.After(exp) {
			delete(a.sessions, k)
		}
	}
	if len(a.sessions) >= maxSessions { // 超出上限时丢掉最早过期的
		var oldK string
		var oldT time.Time
		for k, exp := range a.sessions {
			if oldK == "" || exp.Before(oldT) {
				oldK, oldT = k, exp
			}
		}
		delete(a.sessions, oldK)
	}
	a.sessions[tok] = now.Add(sessionTTL)
	a.mu.Unlock()
	return tok, nil
}

func (a *authState) valid(tok string) bool {
	if tok == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[tok]
	if !ok || time.Now().After(exp) {
		delete(a.sessions, tok)
		return false
	}
	return true
}

func (a *authState) logout(tok string) {
	a.mu.Lock()
	delete(a.sessions, tok)
	a.mu.Unlock()
}

// change 修改账号密码；成功后只保留当前会话。返回新的 web 配置字段。
func (a *authState) change(keepTok, oldPw, newUser, newPw string) (user, hash string, err error) {
	a.mu.Lock()
	cur := a.user
	a.mu.Unlock()
	if !a.verify(cur, oldPw) {
		time.Sleep(500 * time.Millisecond)
		return "", "", fmt.Errorf("原密码错误")
	}
	newUser = strings.TrimSpace(newUser)
	if newUser == "" {
		newUser = cur
	}
	if len(newPw) < 6 {
		return "", "", fmt.Errorf("新密码至少 6 位")
	}
	if newPw == defaultPassword {
		return "", "", fmt.Errorf("新密码不能是默认密码")
	}
	h := hashPassword(newPw)
	a.mu.Lock()
	a.user, a.hash, a.plain, a.isDefault = newUser, h, "", false
	for k := range a.sessions {
		if k != keepTok {
			delete(a.sessions, k)
		}
	}
	a.mu.Unlock()
	return newUser, h, nil
}

func (a *authState) info() (user string, isDefault bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.user, a.isDefault
}

package web

import (
	"context"
	"encoding/base64"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"caddyui/internal/app"
	"caddyui/internal/store"
)

// 会话 cookie 的名字与有效期。
//
// 名字跟着面板一起从 relay_ 改成了 caddyui_，代价是升级后所有人要重新登录一次。
const (
	sessionCookie = "caddyui_session"
	sessionTTL    = 14 * 24 * time.Hour
	maxDockerForm = 4 << 20
)

type ctxKey int

const (
	ctxSession ctxKey = iota
	ctxUser
)

func sessionFrom(ctx context.Context) *store.Session {
	v, _ := ctx.Value(ctxSession).(*store.Session)
	return v
}

func userFrom(ctx context.Context) *store.User {
	v, _ := ctx.Value(ctxUser).(*store.User)
	return v
}

// auth 是登录门禁。顺带在 POST 请求上做 CSRF 校验，省得每个 handler 自己写。
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			s.redirectToLogin(w, r)
			return
		}
		sess, err := s.svc.Store.LookupSession(c.Value)
		if err != nil {
			clearSessionCookie(w)
			s.redirectToLogin(w, r)
			return
		}
		user, err := s.svc.Store.UserByID(sess.UserID)
		if err != nil {
			clearSessionCookie(w)
			s.redirectToLogin(w, r)
			return
		}

		if r.Method == http.MethodPost {
			if strings.HasPrefix(r.URL.Path, "/docker") {
				r.Body = http.MaxBytesReader(w, r.Body, maxDockerForm)
			}
			if err := r.ParseForm(); err != nil {
				http.Error(w, "表单太大或格式不正确", http.StatusBadRequest)
				return
			}
			if !checkOrigin(r) || r.PostFormValue("csrf") != sess.CSRF {
				http.Error(w, "请求校验失败，请返回上一页刷新后重试", http.StatusForbidden)
				return
			}
		}

		ctx := context.WithValue(r.Context(), ctxSession, sess)
		ctx = context.WithValue(ctx, ctxUser, user)
		next(w, r.WithContext(ctx))
	}
}

// redirectToLogin 未登录时的去向：还没建过账号就去初始化页。
func (s *Server) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	if s.svc.Store.UserCount() == 0 {
		redirect(w, r, "/setup")
		return
	}
	redirect(w, r, "/login")
}

// checkOrigin 做同源检查，作为 CSRF 的第二道防线，也覆盖登录/初始化这两个
// 还没有会话 token 的表单。
func checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// 有些环境不发 Origin，退而求其次看 Referer；两个都没有就放行，
		// 否则命令行 curl 之类的正常用法会被挡掉。
		ref := r.Header.Get("Referer")
		if ref == "" {
			return true
		}
		u, err := url.Parse(ref)
		if err != nil {
			return false
		}
		return u.Host == r.Host
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == r.Host
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// 面板初次访问多半是 http://ip:81，这时候不能加 Secure，否则
		// cookie 根本存不下来。等用户把面板挂到域名上走 https 就自动加上。
		Secure:  r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
		Expires: expires,
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

// ---------- 初始化 ----------

func (s *Server) handleSetupForm(w http.ResponseWriter, r *http.Request) {
	if s.svc.Store.UserCount() > 0 {
		redirect(w, r, "/login")
		return
	}
	s.render(w, r, "setup", nil)
}

func (s *Server) handleSetupSubmit(w http.ResponseWriter, r *http.Request) {
	if s.svc.Store.UserCount() > 0 {
		redirect(w, r, "/login")
		return
	}
	if err := r.ParseForm(); err != nil || !checkOrigin(r) {
		http.Error(w, "请求校验失败", http.StatusBadRequest)
		return
	}

	email := store.NormalizeEmail(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	confirm := r.PostFormValue("confirm")

	fail := func(msg string) {
		s.render(w, r, "setup", map[string]any{"Error": msg, "Username": email})
	}
	if password != confirm {
		fail("两次输入的密码不一致")
		return
	}
	user, err := s.svc.Store.CreateUser(email, password)
	if err != nil {
		fail(err.Error())
		return
	}

	// 注册邮箱直接当证书联系邮箱用，省掉「设置里还有一格要填」这一步。
	//
	// Let's Encrypt 会往这个地址发证书续期失败的告警，这是唯一能提前知道
	// 证书要出问题的渠道，不该指望用户自己想起来去填。设置页里随时能改。
	if err := s.svc.Store.SetSetting(app.SettingACMEEmail, email); err != nil {
		log.Printf("写入 ACME 联系邮箱失败（不影响使用，可在设置页手动填）: %v", err)
	}

	if err := s.startSession(w, r, user.ID); err != nil {
		flashErr(w, "账号已创建，但自动登录失败：%v", err)
		redirect(w, r, "/login")
		return
	}
	flashOK(w, "账号创建成功，%s 已自动设为证书联系邮箱。现在去添加第一个站点吧。", email)
	redirect(w, r, "/sites")
}

// ---------- 登录 / 退出 ----------

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if s.svc.Store.UserCount() == 0 {
		redirect(w, r, "/setup")
		return
	}
	// 已经登录的直接进主页。
	if c, err := r.Cookie(sessionCookie); err == nil {
		if _, err := s.svc.Store.LookupSession(c.Value); err == nil {
			redirect(w, r, "/sites")
			return
		}
	}
	s.render(w, r, "login", nil)
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || !checkOrigin(r) {
		http.Error(w, "请求校验失败", http.StatusBadRequest)
		return
	}

	username := r.PostFormValue("username")
	password := r.PostFormValue("password")

	fail := func(msg string) {
		s.render(w, r, "login", map[string]any{"Error": msg, "Username": username})
	}

	// 简单的登录限速：同一个 IP 5 分钟内最多试 10 次。面板可能直接暴露在
	// 公网上，这条不能省。
	if !s.logins.allow(clientIP(r), 10, 5*time.Minute) {
		fail("尝试次数过多，请 5 分钟后再试")
		return
	}

	user, err := s.svc.Store.Authenticate(username, password)
	if err != nil {
		if errors.Is(err, store.ErrBadCredentials) {
			fail("邮箱或密码错误")
			return
		}
		fail("登录失败：" + err.Error())
		return
	}

	s.logins.reset(clientIP(r))
	if err := s.startSession(w, r, user.ID); err != nil {
		fail("创建会话失败：" + err.Error())
		return
	}
	redirect(w, r, "/sites")
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, userID int64) error {
	sess, err := s.svc.Store.CreateSession(userID, sessionTTL)
	if err != nil {
		return err
	}
	setSessionCookie(w, r, sess.Token, time.Unix(sess.ExpiresAt, 0))
	return nil
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if sess := sessionFrom(r.Context()); sess != nil {
		_ = s.svc.Store.DeleteSession(sess.Token)
	}
	clearSessionCookie(w)
	redirect(w, r, "/login")
}

// ---------- 登录限速 ----------

type limiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func newLimiter() *limiter {
	return &limiter{hits: make(map[string][]time.Time)}
}

func (l *limiter) allow(key string, max int, window time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := time.Now().Add(-window)
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	// 顺手清掉其它 key 的陈旧记录，免得 map 无限长大。
	if len(l.hits) > 1024 {
		for k, v := range l.hits {
			if len(v) == 0 || v[len(v)-1].Before(cutoff) {
				delete(l.hits, k)
			}
		}
	}
	if len(kept) >= max {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, time.Now())
	return true
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, key)
}

// clientIP 取访问者 IP。面板通常被 Caddy 反代，这时候 RemoteAddr 是 127.0.0.1，
// 只有在这种情况下才信任 X-Forwarded-For。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if first, _, ok := strings.Cut(xff, ","); ok {
				return strings.TrimSpace(first)
			}
			return strings.TrimSpace(xff)
		}
	}
	return host
}

// ---------- flash 编解码 ----------

func encodeFlash(kind, msg string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(kind + "|" + msg))
}

func decodeFlash(v string) (kind, msg string, ok bool) {
	raw, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		return "", "", false
	}
	k, m, found := strings.Cut(string(raw), "|")
	if !found {
		return "", "", false
	}
	switch k {
	case "ok", "err", "warn":
	default:
		return "", "", false
	}
	return k, m, true
}

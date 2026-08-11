// Package web 是面板的 HTTP 层：路由、模板渲染、会话与 CSRF。
package web

import (
	"bytes"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"time"

	"caddyui/internal/app"
	"caddyui/internal/store"
)

// Server 持有渲染面板所需的一切。
type Server struct {
	svc     *app.Service
	assets  fs.FS
	tmpl    map[string]*template.Template
	version string
	logins  *limiter
}

// 每个页面模板都会和 layout.html 一起解析。
var pages = []string{"setup", "login", "sites", "site_form", "config", "settings"}

// New 构造面板的 http.Handler。
func New(svc *app.Service, assets fs.FS, version string) (http.Handler, error) {
	s := &Server{
		svc:     svc,
		assets:  assets,
		version: version,
		logins:  newLimiter(),
	}
	if err := s.parseTemplates(); err != nil {
		return nil, err
	}
	return s.routes(), nil
}

func (s *Server) parseTemplates() error {
	funcs := template.FuncMap{
		"fmtTime": func(ts int64) string {
			if ts == 0 {
				return "-"
			}
			return time.Unix(ts, 0).Format("2006-01-02 15:04")
		},
		"since": func(ts int64) string {
			d := time.Since(time.Unix(ts, 0))
			switch {
			case d < time.Minute:
				return "刚刚"
			case d < time.Hour:
				return fmt.Sprintf("%d 分钟前", int(d.Minutes()))
			case d < 24*time.Hour:
				return fmt.Sprintf("%d 小时前", int(d.Hours()))
			default:
				return fmt.Sprintf("%d 天前", int(d.Hours()/24))
			}
		},
		"join":      func(parts []string, sep string) string { return strings.Join(parts, sep) },
		"hasPrefix": strings.HasPrefix,

		// fmtDate 只要日期，用在证书有效期上——精确到分钟没有意义。
		"fmtDate": func(t time.Time) string {
			if t.IsZero() {
				return "-"
			}
			return t.Format("2006-01-02")
		},
	}

	s.tmpl = make(map[string]*template.Template, len(pages))
	for _, p := range pages {
		t, err := template.New("layout.html").Funcs(funcs).
			ParseFS(s.assets, "templates/layout.html", "templates/"+p+".html")
		if err != nil {
			return fmt.Errorf("解析模板 %s: %w", p, err)
		}
		s.tmpl[p] = t
	}
	return nil
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	staticFS, err := fs.Sub(s.assets, "static")
	if err != nil {
		log.Fatalf("加载静态资源失败: %v", err)
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/",
		cacheStatic(http.FileServerFS(staticFS))))

	// 未登录可访问
	mux.HandleFunc("GET /setup", s.handleSetupForm)
	mux.HandleFunc("POST /setup", s.handleSetupSubmit)
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLoginSubmit)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})

	// 需要登录
	mux.HandleFunc("POST /logout", s.auth(s.handleLogout))
	mux.HandleFunc("GET /{$}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/sites", http.StatusSeeOther)
	}))

	mux.HandleFunc("GET /sites", s.auth(s.handleSiteList))
	mux.HandleFunc("GET /sites/new", s.auth(s.handleSiteNewForm))
	mux.HandleFunc("POST /sites/new", s.auth(s.handleSiteCreate))
	mux.HandleFunc("GET /sites/{id}/edit", s.auth(s.handleSiteEditForm))
	mux.HandleFunc("POST /sites/{id}/edit", s.auth(s.handleSiteUpdate))
	mux.HandleFunc("POST /sites/{id}/toggle", s.auth(s.handleSiteToggle))
	mux.HandleFunc("POST /sites/{id}/delete", s.auth(s.handleSiteDelete))

	mux.HandleFunc("GET /config", s.auth(s.handleConfig))
	mux.HandleFunc("POST /config/apply", s.auth(s.handleConfigApply))
	mux.HandleFunc("POST /config/rollback/{id}", s.auth(s.handleConfigRollback))

	mux.HandleFunc("GET /settings", s.auth(s.handleSettings))
	mux.HandleFunc("POST /settings/acme", s.auth(s.handleSettingsACME))
	mux.HandleFunc("POST /settings/password", s.auth(s.handleSettingsPassword))
	mux.HandleFunc("POST /settings/caddy/check", s.auth(s.handleCaddyCheck))
	mux.HandleFunc("POST /settings/caddy/upgrade", s.auth(s.handleCaddyUpgrade))

	// 主题切换不需要登录：登录页和初始化页上也有这个按钮。
	mux.HandleFunc("POST /theme", s.handleThemePublic)

	return securityHeaders(mux)
}

// handleThemePublic 是 /theme 的入口。它绕开了 auth 中间件（登录页也要能切主题），
// 所以 CSRF 那套得在这里自己补上。
//
// 这个端点本身没什么可攻击的——最坏的情况是别人让你的面板变成深色——但同源
// 检查基本是白送的，加上没有坏处。
func (s *Server) handleThemePublic(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || !checkOrigin(r) {
		http.Error(w, "请求校验失败", http.StatusBadRequest)
		return
	}
	s.handleTheme(w, r)
}

// securityHeaders 给所有响应加上基础安全头。CSP 能开到 'self' 这么严，是因为
// 面板没有任何外部资源、没有内联脚本和内联样式。
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy",
			"default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 静态资源是编进二进制的，同一个版本内不会变。
		w.Header().Set("Cache-Control", "public, max-age=3600")
		next.ServeHTTP(w, r)
	})
}

// render 渲染一个页面。先渲染到内存再写出去，这样模板出错时不会吐半张残页。
func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, data map[string]any) {
	t, ok := s.tmpl[page]
	if !ok {
		log.Printf("未知模板: %s", page)
		http.Error(w, "内部错误", http.StatusInternalServerError)
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	data["Version"] = s.version
	data["Path"] = r.URL.Path
	data["Theme"] = themeFrom(r)
	data["Flash"] = takeFlash(w, r)
	if sess := sessionFrom(r.Context()); sess != nil {
		data["CSRF"] = sess.CSRF
	}
	if u := userFrom(r.Context()); u != nil {
		data["User"] = u
	}

	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		log.Printf("渲染模板 %s 失败: %v", page, err)
		http.Error(w, "内部错误", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

// ---------- Flash 消息 ----------

const flashCookie = "caddyui_flash"

// Flash 是一条一次性提示。
type Flash struct {
	Kind    string // ok | err | warn
	Message string
}

func setFlash(w http.ResponseWriter, kind, msg string) {
	// Caddy 的报错可能很长，cookie 有 4KB 上限，截一下。
	if len(msg) > 1200 {
		msg = msg[:1200] + "……"
	}
	http.SetCookie(w, &http.Cookie{
		Name:     flashCookie,
		Value:    encodeFlash(kind, msg),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   60,
	})
}

func takeFlash(w http.ResponseWriter, r *http.Request) *Flash {
	c, err := r.Cookie(flashCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	http.SetCookie(w, &http.Cookie{
		Name: flashCookie, Value: "", Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
	kind, msg, ok := decodeFlash(c.Value)
	if !ok {
		return nil
	}
	return &Flash{Kind: kind, Message: msg}
}

// flashOK / flashErr 是最常用的两个快捷方式。
func flashOK(w http.ResponseWriter, format string, a ...any) {
	setFlash(w, "ok", fmt.Sprintf(format, a...))
}

func flashErr(w http.ResponseWriter, format string, a ...any) {
	setFlash(w, "err", fmt.Sprintf(format, a...))
}

func flashWarn(w http.ResponseWriter, format string, a ...any) {
	setFlash(w, "warn", fmt.Sprintf(format, a...))
}

// ---------- 小工具 ----------

// redirect 是 303 跳转的简写，POST 之后统一用它，避免刷新重复提交。
func redirect(w http.ResponseWriter, r *http.Request, path string) {
	http.Redirect(w, r, path, http.StatusSeeOther)
}

// notFound 渲染一个朴素的 404。
func notFound(w http.ResponseWriter) {
	http.Error(w, "页面不存在", http.StatusNotFound)
}

// storeErrMessage 把存储层的错误转成能直接给用户看的话。
func storeErrMessage(err error) string {
	if err == store.ErrNotFound {
		return "记录不存在，可能已经被删除了"
	}
	return err.Error()
}

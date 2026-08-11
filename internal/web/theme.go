package web

import (
	"net/http"
	"time"
)

// 主题存在 cookie 里而不是 localStorage，是为了让服务端在渲染 HTML 时就能把
// data-theme 写进 <html> 标签。这样页面第一帧就是对的颜色，不会先闪一下白底
// 再变黑（localStorage 方案要么闪，要么得靠内联脚本——而面板的 CSP 严到
// 不允许内联脚本，加 hash 例外只会让这一条防线变松）。
//
// 没有 cookie 时值为空，CSS 交给 prefers-color-scheme 处理，跟随系统。
const themeCookie = "caddyui_theme"

// 合法的主题值。空字符串表示「跟随系统」。
const (
	themeLight = "light"
	themeDark  = "dark"
)

// themeFrom 读出当前主题，非法值一律当作跟随系统。
func themeFrom(r *http.Request) string {
	c, err := r.Cookie(themeCookie)
	if err != nil {
		return ""
	}
	switch c.Value {
	case themeLight, themeDark:
		return c.Value
	}
	return ""
}

// handleTheme 切换主题。
//
// 走表单 POST 而不是纯前端，是为了在禁用 JS 的浏览器上也能用：没有 JS 时这就是
// 一次普通的提交 + 跳回原页面；有 JS 时前端会先就地改掉 <html> 的属性，
// 页面不闪，请求在后台把 cookie 落下来。
func (s *Server) handleTheme(w http.ResponseWriter, r *http.Request) {
	next := r.PostFormValue("theme")
	switch next {
	case themeLight, themeDark:
		http.SetCookie(w, &http.Cookie{
			Name:     themeCookie,
			Value:    next,
			Path:     "/",
			HttpOnly: false, // 前端要读它来决定下一次点击切到哪个主题
			SameSite: http.SameSiteLaxMode,
			Expires:  time.Now().AddDate(1, 0, 0),
		})
	default:
		// 其它任何值都当作「恢复跟随系统」，把 cookie 删掉。
		http.SetCookie(w, &http.Cookie{
			Name: themeCookie, Value: "", Path: "/",
			SameSite: http.SameSiteLaxMode, MaxAge: -1,
		})
	}

	// 回到用户点击时所在的页面。只接受站内的相对路径，防止被当成开放重定向。
	back := r.PostFormValue("back")
	if len(back) == 0 || back[0] != '/' || (len(back) > 1 && back[1] == '/') {
		back = "/sites"
	}
	redirect(w, r, back)
}

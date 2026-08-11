package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"relay/internal/app"
	"relay/internal/store"
)

// handleConfig 展示当前应该生效的 Caddyfile，以及历史版本。
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	caddyfile, err := s.svc.Render()
	if err != nil {
		flashErr(w, "生成配置失败：%v", err)
		caddyfile = nil
	}
	versions, err := s.svc.Store.ConfigVersions(30)
	if err != nil {
		flashErr(w, "读取历史版本失败：%v", err)
	}
	s.render(w, r, "config", map[string]any{
		"Caddyfile": string(caddyfile),
		"Versions":  versions,
		"Status":    s.svc.Status(),
	})
}

// handleConfigApply 手动重新下发一次。Caddy 重启过、或者上一次下发失败修好了
// 之后，点这个按钮。
func (s *Server) handleConfigApply(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.Apply("手动重新下发"); err != nil {
		flashErr(w, "下发失败：%v ｜ 线上仍在运行上一份配置。", err)
	} else {
		flashOK(w, "配置已重新下发并生效。")
	}
	redirect(w, r, "/config")
}

// handleConfigRollback 回滚到某个历史版本。
//
// 这是「一直都能用」的兜底手段：改崩了不用 SSH 上去翻配置文件，点一下就回去了。
func (s *Server) handleConfigRollback(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		notFound(w)
		return
	}
	if err := s.svc.ApplyVersion(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			flashErr(w, "这个版本不存在，可能已经被历史记录清理掉了。")
		} else {
			flashErr(w, "回滚失败：%v", err)
		}
		redirect(w, r, "/config")
		return
	}
	flashWarn(w, "已回滚到版本 #%d 并生效。注意：面板里的站点列表没有跟着变，"+
		"下次保存任何站点都会重新下发当前列表、覆盖这次回滚。", id)
	redirect(w, r, "/config")
}

// ---------- 设置 ----------

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "settings", map[string]any{
		"ACMEEmail": s.svc.Store.Setting(app.SettingACMEEmail, ""),
		"ACMECA":    s.svc.Store.Setting(app.SettingACMECA, ""),
		"Status":    s.svc.Status(),
	})
}

func (s *Server) handleSettingsACME(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(r.PostFormValue("acme_email"))
	ca := strings.TrimSpace(r.PostFormValue("acme_ca"))

	// 邮箱和 CA 地址会原样进 Caddyfile 当作一个 token，含空白或大括号会
	// 破坏配置结构，直接拒掉。
	if strings.ContainsAny(email, " \t\r\n{}\"'#") {
		flashErr(w, "邮箱格式不正确。")
		redirect(w, r, "/settings")
		return
	}
	if email != "" && !strings.Contains(email, "@") {
		flashErr(w, "邮箱格式不正确。")
		redirect(w, r, "/settings")
		return
	}
	if ca != "" && !strings.HasPrefix(ca, "https://") {
		flashErr(w, "ACME 目录地址必须以 https:// 开头。")
		redirect(w, r, "/settings")
		return
	}
	if strings.ContainsAny(ca, " \t\r\n{}\"'#") {
		flashErr(w, "ACME 目录地址不能包含空格。")
		redirect(w, r, "/settings")
		return
	}

	if err := s.svc.Store.SetSetting(app.SettingACMEEmail, email); err != nil {
		flashErr(w, "保存失败：%v", err)
		redirect(w, r, "/settings")
		return
	}
	if err := s.svc.Store.SetSetting(app.SettingACMECA, ca); err != nil {
		flashErr(w, "保存失败：%v", err)
		redirect(w, r, "/settings")
		return
	}

	if err := s.svc.Apply("修改证书设置"); err != nil {
		flashWarn(w, "设置已保存，但下发失败：%v ｜ 线上不受影响。", err)
	} else {
		flashOK(w, "证书设置已保存并生效。")
	}
	redirect(w, r, "/settings")
}

func (s *Server) handleSettingsPassword(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	if user == nil {
		redirect(w, r, "/login")
		return
	}

	oldPw := r.PostFormValue("old_password")
	newPw := r.PostFormValue("new_password")
	confirm := r.PostFormValue("confirm_password")

	if newPw != confirm {
		flashErr(w, "两次输入的新密码不一致。")
		redirect(w, r, "/settings")
		return
	}
	if err := s.svc.Store.ChangePassword(user.ID, oldPw, newPw); err != nil {
		flashErr(w, "修改密码失败：%v", err)
		redirect(w, r, "/settings")
		return
	}

	// 改完密码把所有会话踢掉，包括当前这个，逼一次重新登录。
	_ = s.svc.Store.DeleteUserSessions(user.ID)
	clearSessionCookie(w)
	flashOK(w, "密码已修改，请用新密码重新登录。")
	redirect(w, r, "/login")
}

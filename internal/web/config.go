package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"caddyui/internal/app"
	"caddyui/internal/caddybin"
	"caddyui/internal/store"
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
	data := map[string]any{
		"ACMEEmail": s.svc.Store.Setting(app.SettingACMEEmail, ""),
		"ACMECA":    s.svc.Store.Setting(app.SettingACMECA, ""),
		"Status":    s.svc.Status(),
	}
	if s.svc.Certs != nil {
		data["CertRoot"] = s.svc.Certs.CertRoot()
		data["CertAvailable"] = s.svc.Certs.Available()
		data["DataDir"] = s.svc.Certs.Dir
	}
	s.addCaddyVersionData(data)
	s.render(w, r, "settings", data)
}

// addCaddyVersionData 填「Caddy 内核」那一块要用的数据。
//
// 这里只读缓存、不主动打 GitHub：设置页可能被频繁打开，而 GitHub 未认证接口
// 是每小时 60 次。想看最新的就点「检查更新」。
func (s *Server) addCaddyVersionData(data map[string]any) {
	bin := s.svc.Binary
	if bin == nil {
		return
	}

	data["CaddyBinPath"] = bin.BinPath
	if cur, err := bin.CurrentVersion(); err == nil {
		data["CaddyVersion"] = cur
	} else {
		data["CaddyVersionErr"] = err.Error()
	}

	if rel := bin.CachedLatest(); rel != nil {
		data["CaddyLatest"] = rel
		if cur, ok := data["CaddyVersion"].(string); ok {
			data["CaddyOutdated"] = caddybin.Newer(cur, rel.Version)
		}
	}

	ok, why := bin.HelperAvailable()
	data["CaddyUpgradable"] = ok
	data["CaddyUpgradeBlocked"] = why

	if job := bin.Job(); job.State != caddybin.StateIdle {
		data["CaddyJob"] = job
	}
}

// handleCaddyCheck 主动查一次官方最新版本。
func (s *Server) handleCaddyCheck(w http.ResponseWriter, r *http.Request) {
	if s.svc.Binary == nil {
		redirect(w, r, "/settings")
		return
	}
	rel, err := s.svc.Binary.Latest(true)
	if err != nil {
		flashErr(w, "检查更新失败：%v", err)
		redirect(w, r, "/settings")
		return
	}

	cur, cerr := s.svc.Binary.CurrentVersion()
	switch {
	case cerr != nil:
		flashOK(w, "官方最新版本是 %s。（读不到本机 Caddy 版本：%v）", rel.Version, cerr)
	case caddybin.Newer(cur, rel.Version):
		flashOK(w, "有新版本：%s → %s。", cur, rel.Version)
	default:
		flashOK(w, "当前 %s 已经是最新版本。", cur)
	}
	redirect(w, r, "/settings")
}

// handleCaddyUpgrade 触发一次升级。
//
// 真正干活的是 root 拥有的助手脚本，面板只是通过 sudo 把它叫起来 ——
// 权限边界的理由写在 deploy/upgrade-caddy.sh 的注释里。
//
// 任务是异步跑的：下载加重启在慢机器上可能要一分钟以上，而且升级过程中 Caddy
// 会重启，如果用户正是通过 Caddy 反代访问面板的，同步请求会被连带掐断，
// 结果就看不到了。这里立刻返回，状态在设置页上轮询着看。
func (s *Server) handleCaddyUpgrade(w http.ResponseWriter, r *http.Request) {
	if s.svc.Binary == nil {
		redirect(w, r, "/settings")
		return
	}
	if err := s.svc.Binary.StartUpgrade(); err != nil {
		flashErr(w, "升级没能开始：%v", err)
		redirect(w, r, "/settings")
		return
	}
	flashWarn(w, "升级已开始，大约需要 10~60 秒。期间 Caddy 会重启一次，网站会短暂中断几秒。"+
		"刷新本页查看结果。")
	redirect(w, r, "/settings")
}

func (s *Server) handleSettingsACME(w http.ResponseWriter, r *http.Request) {
	ca := strings.TrimSpace(r.PostFormValue("acme_ca"))

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

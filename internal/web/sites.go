package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"relay/internal/store"
)

func (s *Server) handleSiteList(w http.ResponseWriter, r *http.Request) {
	sites, err := s.svc.Store.Sites()
	if err != nil {
		flashErr(w, "读取站点列表失败：%v", err)
		sites = nil
	}
	s.render(w, r, "sites", map[string]any{
		"Sites":  sites,
		"Status": s.svc.Status(),
	})
}

func (s *Server) handleSiteNewForm(w http.ResponseWriter, r *http.Request) {
	// 新站点的默认值：开 HTTPS、强制跳转、转发到本机 http。这是绝大多数人
	// 想要的，理想情况下只需要填域名和端口两个格子。
	s.renderSiteForm(w, r, &store.Site{
		UpstreamScheme: "http",
		UpstreamHost:   "127.0.0.1",
		Enabled:        true,
		HTTPS:          true,
		ForceHTTPS:     true,
	}, "", true)
}

func (s *Server) handleSiteEditForm(w http.ResponseWriter, r *http.Request) {
	site, ok := s.siteFromPath(w, r)
	if !ok {
		return
	}
	s.renderSiteForm(w, r, site, "", false)
}

// renderSiteForm 渲染新增/编辑表单。校验失败时会带着用户刚填的内容重新渲染，
// 不让人白填一遍。
func (s *Server) renderSiteForm(w http.ResponseWriter, r *http.Request, site *store.Site, errMsg string, isNew bool) {
	title := "编辑站点"
	action := fmt.Sprintf("/sites/%d/edit", site.ID)
	if isNew {
		title, action = "添加站点", "/sites/new"
	}
	// 表单里域名一行一个更好编辑。
	domainsText := strings.ReplaceAll(site.Domains, ",", "\n")

	s.render(w, r, "site_form", map[string]any{
		"Site":        site,
		"DomainsText": domainsText,
		"Title":       title,
		"Action":      action,
		"IsNew":       isNew,
		"Error":       errMsg,
	})
}

func (s *Server) handleSiteCreate(w http.ResponseWriter, r *http.Request) {
	site, err := s.siteFromForm(r, &store.Site{Enabled: true})
	if err != nil {
		s.renderSiteForm(w, r, site, err.Error(), true)
		return
	}
	if err := s.svc.Store.CreateSite(site); err != nil {
		s.renderSiteForm(w, r, site, err.Error(), true)
		return
	}
	s.applyAndFlash(w, "新增站点 "+site.PrimaryDomain(), site)
	redirect(w, r, "/sites")
}

func (s *Server) handleSiteUpdate(w http.ResponseWriter, r *http.Request) {
	existing, ok := s.siteFromPath(w, r)
	if !ok {
		return
	}
	site, err := s.siteFromForm(r, existing)
	if err != nil {
		s.renderSiteForm(w, r, site, err.Error(), false)
		return
	}
	if err := s.svc.Store.UpdateSite(site); err != nil {
		s.renderSiteForm(w, r, site, err.Error(), false)
		return
	}
	s.applyAndFlash(w, "修改站点 "+site.PrimaryDomain(), site)
	redirect(w, r, "/sites")
}

func (s *Server) handleSiteToggle(w http.ResponseWriter, r *http.Request) {
	site, ok := s.siteFromPath(w, r)
	if !ok {
		return
	}
	want := !site.Enabled
	if err := s.svc.Store.SetSiteEnabled(site.ID, want); err != nil {
		flashErr(w, "操作失败：%s", storeErrMessage(err))
		redirect(w, r, "/sites")
		return
	}
	verb := "停用"
	if want {
		verb = "启用"
	}
	site.Enabled = want
	s.applyAndFlash(w, verb+"站点 "+site.PrimaryDomain(), nil)
	redirect(w, r, "/sites")
}

func (s *Server) handleSiteDelete(w http.ResponseWriter, r *http.Request) {
	site, ok := s.siteFromPath(w, r)
	if !ok {
		return
	}
	if err := s.svc.Store.DeleteSite(site.ID); err != nil {
		flashErr(w, "删除失败：%s", storeErrMessage(err))
		redirect(w, r, "/sites")
		return
	}
	s.applyAndFlash(w, "删除站点 "+site.PrimaryDomain(), nil)
	redirect(w, r, "/sites")
}

// applyAndFlash 下发配置并给出一条人话提示。
//
// 关键设计：下发失败不代表数据丢了，也不代表线上挂了。Caddy 会整体拒绝新配置
// 并继续跑旧的，用户的编辑也已经存进库里。提示语必须把这两件事讲清楚，
// 否则小白看到红字会以为网站已经炸了。
func (s *Server) applyAndFlash(w http.ResponseWriter, reason string, site *store.Site) {
	if err := s.svc.Apply(reason); err != nil {
		flashWarn(w, "改动已保存，但下发到 Caddy 失败：%v ｜ 线上仍在运行上一份配置，网站不受影响。修正后可在「配置」页点「重新下发」。", err)
		return
	}
	if site != nil && site.HTTPS && site.Enabled {
		flashOK(w, "%s 已生效。首次访问 HTTPS 时 Caddy 会自动申请证书，可能要等几秒钟。", reason)
		return
	}
	flashOK(w, "%s 已生效。", reason)
}

// siteFromPath 从 URL 里取出 {id} 对应的站点。
func (s *Server) siteFromPath(w http.ResponseWriter, r *http.Request) (*store.Site, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		notFound(w)
		return nil, false
	}
	site, err := s.svc.Store.SiteByID(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			notFound(w)
			return nil, false
		}
		http.Error(w, "读取站点失败: "+err.Error(), http.StatusInternalServerError)
		return nil, false
	}
	return site, true
}

// siteFromForm 把表单读进 Site 结构。base 提供那些表单里没有的字段
// （ID、创建时间、以及编辑时没重填的 basic auth 密码）。
func (s *Server) siteFromForm(r *http.Request, base *store.Site) (*store.Site, error) {
	site := *base // 拷贝一份，不动传进来的

	site.Domains = r.PostFormValue("domains")
	site.Note = strings.TrimSpace(r.PostFormValue("note"))
	site.Advanced = strings.TrimSpace(r.PostFormValue("advanced"))
	site.HTTPS = r.PostFormValue("https") != ""
	site.ForceHTTPS = r.PostFormValue("force_https") != ""
	site.SkipTLSVerify = r.PostFormValue("skip_tls_verify") != ""

	scheme, host, port, err := parseUpstream(
		r.PostFormValue("upstream_host"),
		r.PostFormValue("upstream_scheme"),
		r.PostFormValue("upstream_port"),
	)
	if err != nil {
		// 出错也要把用户填的原样塞回去，让他看到自己写了什么。
		site.UpstreamHost = strings.TrimSpace(r.PostFormValue("upstream_host"))
		return &site, err
	}
	site.UpstreamScheme, site.UpstreamHost, site.UpstreamPort = scheme, host, port

	// 访问密码：用户名清空就整个关掉；密码留空表示「不改」。
	site.BasicUser = strings.TrimSpace(r.PostFormValue("basic_user"))
	if site.BasicUser == "" {
		site.BasicHash = ""
	} else if pw := r.PostFormValue("basic_pass"); pw != "" {
		if len(pw) < 4 {
			return &site, errors.New("访问密码至少 4 位")
		}
		hash, err := store.HashPassword(pw)
		if err != nil {
			return &site, fmt.Errorf("生成访问密码失败: %w", err)
		}
		site.BasicHash = hash
	} else if site.BasicHash == "" {
		return &site, errors.New("填了访问用户名，还需要设置一个密码")
	}

	return &site, nil
}

// parseUpstream 尽量猜对用户想填什么。这三种写法都能识别，因为小白最常见的
// 动作就是直接把浏览器地址栏里的东西粘过来：
//
//	127.0.0.1              → 端口取自旁边的端口框
//	127.0.0.1:8080         → 自带端口
//	http://127.0.0.1:8080/ → 自带协议和端口，路径丢弃
//	[::1]:8080             → IPv6
func parseUpstream(rawHost, rawScheme, rawPort string) (scheme, host string, port int, err error) {
	scheme = strings.ToLower(strings.TrimSpace(rawScheme))
	if scheme != "https" {
		scheme = "http"
	}

	h := strings.TrimSpace(rawHost)
	if h == "" {
		return "", "", 0, errors.New("请填写转发目标地址")
	}

	if i := strings.Index(h, "://"); i >= 0 {
		scheme = strings.ToLower(h[:i])
		h = h[i+3:]
	}
	if i := strings.IndexAny(h, "/?#"); i >= 0 {
		h = h[:i] // 路径部分直接丢掉，反代是整站转发
	}

	switch {
	case strings.HasPrefix(h, "["): // [::1]:8080
		end := strings.Index(h, "]")
		if end < 0 {
			return "", "", 0, errors.New("IPv6 地址的方括号没闭合")
		}
		rest := h[end+1:]
		h = h[1:end]
		if after, ok := strings.CutPrefix(rest, ":"); ok {
			if p, e := strconv.Atoi(after); e == nil {
				port = p
			}
		}
	case strings.Count(h, ":") == 1: // host:port
		i := strings.Index(h, ":")
		if p, e := strconv.Atoi(h[i+1:]); e == nil {
			port = p
			h = h[:i]
		}
	}
	// 超过一个冒号说明是没加方括号的裸 IPv6，那就没有端口部分。

	if port == 0 {
		ps := strings.TrimSpace(rawPort)
		if ps == "" {
			return "", "", 0, errors.New("请填写端口")
		}
		p, e := strconv.Atoi(ps)
		if e != nil {
			return "", "", 0, errors.New("端口必须是数字")
		}
		port = p
	}
	if scheme != "http" && scheme != "https" {
		return "", "", 0, fmt.Errorf("不支持的协议 %q，只能是 http 或 https", scheme)
	}
	return scheme, h, port, nil
}

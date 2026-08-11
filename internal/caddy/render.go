package caddy

import (
	"fmt"
	"strings"

	"caddyui/internal/store"
)

// RenderOptions 是生成 Caddyfile 需要的全部输入。
type RenderOptions struct {
	AdminAddr string // 必须和面板连接的 admin 地址一致，否则下发后面板会失联
	ACMEEmail string // 证书快过期时 Let's Encrypt 会往这里发提醒，可以留空
	ACMECA    string // 自定义 ACME 目录，留空用默认（Let's Encrypt，失败自动切 ZeroSSL）
	Sites     []*store.Site
}

// Render 把站点列表渲染成 Caddyfile。
//
// 这里用 strings.Builder 而不是 text/template，是为了对每一处用户输入的落点
// 都有明确掌控——域名和主机名在入库时已经被白名单正则过滤过，端口是整数，
// bcrypt 哈希加引号，只有「高级配置」是原样透传（那是用户主动进专家模式）。
func Render(opts RenderOptions) []byte {
	var b strings.Builder

	b.WriteString("# 本文件由 CaddyUI 面板自动生成，手动修改会在下次保存时被覆盖。\n")
	b.WriteString("# 需要自定义的内容请写进对应站点的「高级配置」。\n\n")

	// ---- 全局配置 ----
	b.WriteString("{\n")
	if addr := sanitizeToken(opts.AdminAddr); addr != "" {
		b.WriteString("\tadmin " + addr + "\n")
	}
	if email := sanitizeToken(opts.ACMEEmail); email != "" {
		b.WriteString("\temail " + email + "\n")
	}
	if ca := sanitizeToken(opts.ACMECA); ca != "" {
		b.WriteString("\tacme_ca " + ca + "\n")
	}
	b.WriteString("}\n")

	if len(opts.Sites) == 0 {
		b.WriteString("\n# 还没有启用任何站点。\n")
		return []byte(b.String())
	}

	// ---- 各个站点 ----
	for _, site := range opts.Sites {
		b.WriteString("\n")
		if note := oneLine(site.Note); note != "" {
			b.WriteString("# " + note + "\n")
		}
		b.WriteString(addressLine(site) + " {\n")

		if site.HasBasicAuth() {
			b.WriteString("\tbasic_auth {\n")
			// 哈希加引号：bcrypt 里有 $ 和 . 等字符，加引号最保险。
			b.WriteString(fmt.Sprintf("\t\t%s %q\n", site.BasicUser, site.BasicHash))
			b.WriteString("\t}\n")
		}

		b.WriteString("\treverse_proxy " + site.UpstreamURL() + " {\n")
		// Caddy 会自动带上 X-Forwarded-For / -Proto / -Host，X-Real-IP 得自己加，
		// 很多后端（各种 PHP 面板、Gitea 之类）认这个头。这里是覆盖写入，
		// 顺带把客户端伪造的 X-Real-IP 挡掉。
		//
		// 用 {client_ip} 而不是老写法 {remote_host}：没有前置代理时两者结果一样，
		// 套了 CDN／SLB 时前者会按全局 trusted_proxies 还原出真实访客 IP。
		b.WriteString("\t\theader_up X-Real-IP {client_ip}\n")
		if site.UpstreamScheme == "https" && site.SkipTLSVerify {
			b.WriteString("\t\ttransport http {\n")
			b.WriteString("\t\t\ttls_insecure_skip_verify\n")
			b.WriteString("\t\t}\n")
		}
		b.WriteString("\t}\n")

		if adv := strings.TrimSpace(site.Advanced); adv != "" {
			b.WriteString("\n")
			b.WriteString(indent(adv, "\t"))
			b.WriteString("\n")
		}

		b.WriteString("}\n")
	}

	return []byte(b.String())
}

// addressLine 拼出站点块的地址行，决定这个站点走 http 还是 https。
//
//	裸域名          → Caddy 自动申请证书，并把 http 301 跳到 https
//	http:// + https:// → 两个都监听，不跳转
//	只有 http://    → 纯 http，不碰证书
func addressLine(site *store.Site) string {
	domains := site.DomainList()
	parts := make([]string, 0, len(domains)*2)

	switch {
	case !site.HTTPS:
		for _, d := range domains {
			parts = append(parts, "http://"+d)
		}
	case site.ForceHTTPS:
		parts = append(parts, domains...)
	default:
		for _, d := range domains {
			parts = append(parts, "http://"+d, "https://"+d)
		}
	}
	return strings.Join(parts, ", ")
}

// indent 给多行文本每行加前缀，空行保持空行。
func indent(s, prefix string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			lines[i] = ""
			continue
		}
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

// sanitizeToken 保证一个值能安全地当作 Caddyfile 的单个 token：不含空白、
// 大括号和引号。这些值来自启动参数和设置页，属于运维自己填的，这里只是兜底。
func sanitizeToken(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.ContainsAny(s, " \t\r\n{}\"'#") {
		return ""
	}
	return s
}

// oneLine 把备注压成安全的单行注释。
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

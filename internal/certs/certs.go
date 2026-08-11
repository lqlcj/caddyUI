// Package certs 在本机磁盘上定位 Caddy 已经签发的证书。
//
// 面板和 Caddy 跑在同一台机器、同一个系统用户下，所以可以直接读 Caddy 的
// 数据目录。这里只读取、只展示路径和有效期，绝不把证书内容或私钥吐到页面上。
//
// Caddy 的存放规则（v2）：
//
//	<数据目录>/certificates/<签发者>/<域名>/<域名>.crt
//	<数据目录>/certificates/<签发者>/<域名>/<域名>.key
//
// 其中「签发者」形如 acme-v02.api.letsencrypt.org-directory，
// 通配符域名 *.example.com 的目录名会被写成 wildcard_.example.com。
package certs

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Info 是一个域名的证书情况。没找到文件时 Found 为 false，其余字段无意义。
type Info struct {
	Domain   string
	Found    bool
	CertPath string
	KeyPath  string

	// 下面这些来自解析证书本体，解析失败时保持零值。
	Issuer    string
	NotBefore time.Time
	NotAfter  time.Time
	Parsed    bool
}

// Expired 证书是否已经过期。
func (i *Info) Expired() bool {
	return i.Parsed && time.Now().After(i.NotAfter)
}

// DaysLeft 距离过期还有多少天，已过期返回负数。
func (i *Info) DaysLeft() int {
	if !i.Parsed {
		return 0
	}
	return int(time.Until(i.NotAfter).Hours() / 24)
}

// Expiring 是否进入了需要留意的窗口。Caddy 会在剩余 1/3 有效期时自动续期，
// 正常情况下永远不会走到这里；真走到了说明续期在出问题。
func (i *Info) Expiring() bool {
	return i.Parsed && !i.Expired() && i.DaysLeft() < 14
}

// Locator 按域名查证书。Dir 是 Caddy 的数据目录（不是 certificates 子目录）。
type Locator struct {
	Dir string
}

// New 构造一个 Locator。dir 传空时自动探测。
func New(dir string) *Locator {
	if strings.TrimSpace(dir) == "" {
		dir = DetectDataDir()
	}
	return &Locator{Dir: strings.TrimSpace(dir)}
}

// CertRoot 返回证书总目录，界面上用来告诉用户「东西都在这下面」。
func (l *Locator) CertRoot() string {
	if l.Dir == "" {
		return ""
	}
	return filepath.Join(l.Dir, "certificates")
}

// Available 数据目录是否真的存在且能读。装在别的机器上、或者权限不对时为 false，
// 界面上据此把整块证书信息藏起来，而不是显示一堆猜出来的假路径。
func (l *Locator) Available() bool {
	root := l.CertRoot()
	if root == "" {
		return false
	}
	f, err := os.Open(root)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

// Lookup 查一个域名的证书。查不到时返回 Found=false 的结果，不返回错误——
// 「还没签发」是完全正常的状态，不该在界面上显示成故障。
func (l *Locator) Lookup(domain string) Info {
	info := Info{Domain: domain}

	root := l.CertRoot()
	if root == "" {
		return info
	}

	// 每个签发者一个目录，挨个找。同一个域名理论上只会落在一个签发者下面，
	// 但换过 CA 的话可能留着旧的，那就取有效期最晚的那张。
	issuers, err := os.ReadDir(root)
	if err != nil {
		return info
	}

	subject := storageSubject(domain)
	var best Info

	for _, issuer := range issuers {
		if !issuer.IsDir() {
			continue
		}
		dir := filepath.Join(root, issuer.Name(), subject)
		crt := filepath.Join(dir, subject+".crt")
		key := filepath.Join(dir, subject+".key")

		if !fileExists(crt) {
			continue
		}
		cand := Info{
			Domain:   domain,
			Found:    true,
			CertPath: crt,
			Issuer:   issuer.Name(),
		}
		if fileExists(key) {
			cand.KeyPath = key
		}
		if leaf := parseLeaf(crt); leaf != nil {
			cand.Parsed = true
			cand.NotBefore = leaf.NotBefore
			cand.NotAfter = leaf.NotAfter
			if cn := leaf.Issuer.CommonName; cn != "" {
				cand.Issuer = cn
			}
		}
		if !best.Found || cand.NotAfter.After(best.NotAfter) {
			best = cand
		}
	}
	if best.Found {
		return best
	}
	return info
}

// LookupAll 批量查询，顺序和传入的域名一致。
func (l *Locator) LookupAll(domains []string) []Info {
	out := make([]Info, 0, len(domains))
	for _, d := range domains {
		out = append(out, l.Lookup(d))
	}
	return out
}

// storageSubject 把域名转成 Caddy 在磁盘上用的目录名。
// 通配符 *.example.com 存成 wildcard_.example.com，因为 * 在很多文件系统上不合法。
func storageSubject(domain string) string {
	if strings.HasPrefix(domain, "*") {
		return "wildcard_" + strings.TrimPrefix(domain, "*")
	}
	return domain
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// parseLeaf 从 PEM 文件里取出第一张证书（叶子证书）。文件里通常还跟着中间证书，
// 那些不关心。读不动或者格式不对就返回 nil，调用方按「路径有、详情没有」处理。
func parseLeaf(path string) *x509.Certificate {
	// 证书文件都很小，2MB 的上限纯粹是防止读到意外的大文件。
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > 2<<20 {
		return nil
	}
	for {
		block, rest := pem.Decode(raw)
		if block == nil {
			return nil
		}
		if block.Type == "CERTIFICATE" {
			leaf, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil
			}
			return leaf
		}
		raw = rest
	}
}

// DetectDataDir 猜 Caddy 的数据目录，规则和 Caddy 自己的一致。
//
// 面板和 Caddy 用同一个系统用户跑（install.sh 就是这么装的），所以
// $HOME/.local/share/caddy 这一条在生产环境下总是对的。后面几个候选是给
// 手动部署、或者面板以 root 跑的情况兜底。
func DetectDataDir() string {
	if v := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); v != "" {
		return filepath.Join(v, "caddy")
	}

	var candidates []string
	if home := strings.TrimSpace(os.Getenv("HOME")); home != "" {
		candidates = append(candidates, filepath.Join(home, ".local", "share", "caddy"))
	}
	candidates = append(candidates,
		filepath.Join("/var", "lib", "caddy", ".local", "share", "caddy"),
		filepath.Join("/root", ".local", "share", "caddy"),
		filepath.Join("/var", "lib", "caddy"),
	)

	// 优先返回真的装着证书的那个。
	for _, c := range candidates {
		if st, err := os.Stat(filepath.Join(c, "certificates")); err == nil && st.IsDir() {
			return c
		}
	}
	// 一个都没命中就返回第一个候选，界面上会显示「目录不存在」。
	if len(candidates) > 0 {
		return candidates[0]
	}
	return ""
}

// Issuers 列出数据目录下所有签发者，设置页用来展示。
func (l *Locator) Issuers() []string {
	root := l.CertRoot()
	if root == "" {
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

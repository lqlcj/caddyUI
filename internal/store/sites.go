package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"
)

// Site 是一条反向代理规则：一组域名 → 一个上游地址。
type Site struct {
	ID             int64
	Domains        string // 逗号分隔，已规范化为小写
	UpstreamScheme string // http | https
	UpstreamHost   string
	UpstreamPort   int
	Enabled        bool
	HTTPS          bool // 自动申请并使用证书
	ForceHTTPS     bool // http 请求 301 跳到 https
	SkipTLSVerify  bool // 上游是自签证书时用
	BasicUser      string
	BasicHash      string // bcrypt，Caddy 直接吃这个格式
	Advanced       string // 原样插入站点块的 Caddyfile 片段
	Note           string
	CreatedAt      int64
	UpdatedAt      int64
}

// DomainList 把逗号分隔的域名拆成切片。
func (s *Site) DomainList() []string {
	if s.Domains == "" {
		return nil
	}
	return strings.Split(s.Domains, ",")
}

// PrimaryDomain 返回第一个域名，用于界面上做标题。
func (s *Site) PrimaryDomain() string {
	l := s.DomainList()
	if len(l) == 0 {
		return ""
	}
	return l[0]
}

// UpstreamAddr 返回 host:port，IPv6 会自动加方括号。
func (s *Site) UpstreamAddr() string {
	host := s.UpstreamHost
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		host = "[" + host + "]"
	}
	return fmt.Sprintf("%s:%d", host, s.UpstreamPort)
}

// UpstreamURL 返回带协议的完整上游地址。
func (s *Site) UpstreamURL() string {
	return s.UpstreamScheme + "://" + s.UpstreamAddr()
}

// HasBasicAuth 是否开启了访问密码。
func (s *Site) HasBasicAuth() bool {
	return s.BasicUser != "" && s.BasicHash != ""
}

// SiteLink 是列表页上的一个域名条目。通配符域名没法直接点开，所以要区分。
type SiteLink struct {
	Domain   string
	URL      string
	Linkable bool
}

// Links 返回这个站点的全部域名及其可访问地址。
func (s *Site) Links() []SiteLink {
	scheme := "http"
	if s.HTTPS {
		scheme = "https"
	}
	domains := s.DomainList()
	out := make([]SiteLink, 0, len(domains))
	for _, d := range domains {
		l := SiteLink{Domain: d, Linkable: !strings.HasPrefix(d, "*.")}
		if l.Linkable {
			l.URL = scheme + "://" + d
		}
		out = append(out, l)
	}
	return out
}

var labelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidateDomain 校验单个域名。这一层是防 Caddyfile 注入的关键：只放行
// 字母数字短横线和点，其它字符（尤其是空格、大括号、换行）一律拒绝。
func ValidateDomain(d string) error {
	if d == "" {
		return errors.New("域名不能为空")
	}
	if len(d) > 253 {
		return fmt.Errorf("域名 %q 太长了", d)
	}
	labels := strings.Split(d, ".")
	for i, l := range labels {
		if i == 0 && l == "*" && len(labels) > 1 {
			continue // 通配符 *.example.com
		}
		if !labelRe.MatchString(l) {
			return fmt.Errorf("域名 %q 格式不正确", d)
		}
	}
	return nil
}

// NormalizeDomains 接受用户输入的一堆域名（换行、逗号、空格随便分隔），
// 返回去重、小写、逗号分隔的规范形式。
func NormalizeDomains(raw string) (string, error) {
	fields := strings.FieldsFunc(strings.ToLower(raw), func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t' || r == ';'
	})
	var (
		out  []string
		seen = map[string]bool{}
	)
	for _, f := range fields {
		f = strings.TrimSuffix(f, ".") // 容忍用户粘进来的 FQDN 尾点
		if f == "" || seen[f] {
			continue
		}
		if err := ValidateDomain(f); err != nil {
			return "", err
		}
		seen[f] = true
		out = append(out, f)
	}
	if len(out) == 0 {
		return "", errors.New("请至少填写一个域名")
	}
	if len(out) > 50 {
		return "", errors.New("单个站点最多 50 个域名")
	}
	return strings.Join(out, ","), nil
}

// ValidateUpstreamHost 上游可以是域名，也可以是 IP。
func ValidateUpstreamHost(h string) error {
	if h == "" {
		return errors.New("转发目标地址不能为空")
	}
	if net.ParseIP(h) != nil {
		return nil
	}
	if err := ValidateDomain(strings.ToLower(h)); err != nil {
		return errors.New("转发目标只能填 IP 或域名，不要带 http:// 和端口")
	}
	return nil
}

// Validate 在写库之前做一次完整校验。
func (s *Site) Validate() error {
	normalized, err := NormalizeDomains(s.Domains)
	if err != nil {
		return err
	}
	s.Domains = normalized

	s.UpstreamHost = strings.TrimSpace(s.UpstreamHost)
	if err := ValidateUpstreamHost(s.UpstreamHost); err != nil {
		return err
	}
	if s.UpstreamPort < 1 || s.UpstreamPort > 65535 {
		return errors.New("端口必须在 1~65535 之间")
	}
	if s.UpstreamScheme != "http" && s.UpstreamScheme != "https" {
		return errors.New("上游协议只能是 http 或 https")
	}
	if s.BasicUser != "" {
		if strings.ContainsAny(s.BasicUser, " \t\n\r\"{}") {
			return errors.New("访问密码的用户名不能包含空格或引号")
		}
		if s.BasicHash == "" {
			return errors.New("填了访问用户名就必须设置对应的密码")
		}
	}
	// 高级片段原样进 Caddyfile，这里只做一次括号配平检查，防止手滑把站点块
	// 提前闭合导致后面所有站点被吞掉。真正的语法校验交给 Caddy。
	if depth := braceDepth(s.Advanced); depth != 0 {
		return fmt.Errorf("高级配置里的大括号没有配平（相差 %d 个）", depth)
	}
	if !s.HTTPS {
		s.ForceHTTPS = false // 没证书谈何强制跳转
	}
	return nil
}

// braceDepth 统计未闭合的大括号数量，跳过 # 注释和引号里的内容。
func braceDepth(s string) int {
	depth := 0
	inQuote := false
	inComment := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inComment:
			if c == '\n' {
				inComment = false
			}
		case c == '"':
			inQuote = !inQuote
		case inQuote:
			// 引号里的都不算
		case c == '#':
			inComment = true
		case c == '{':
			depth++
		case c == '}':
			depth--
		}
	}
	return depth
}

const siteCols = `id, domains, upstream_scheme, upstream_host, upstream_port,
	enabled, https, force_https, skip_tls_verify, basic_user, basic_hash,
	advanced, note, created_at, updated_at`

func scanSite(row interface{ Scan(...any) error }) (*Site, error) {
	var s Site
	err := row.Scan(&s.ID, &s.Domains, &s.UpstreamScheme, &s.UpstreamHost, &s.UpstreamPort,
		&s.Enabled, &s.HTTPS, &s.ForceHTTPS, &s.SkipTLSVerify, &s.BasicUser, &s.BasicHash,
		&s.Advanced, &s.Note, &s.CreatedAt, &s.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// Sites 返回全部站点，按创建时间排序。
func (s *Store) Sites() ([]*Site, error) {
	rows, err := s.db.Query(`SELECT ` + siteCols + ` FROM sites ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Site
	for rows.Next() {
		site, err := scanSite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, site)
	}
	return out, rows.Err()
}

// EnabledSites 只返回启用的站点，用于生成 Caddyfile。
func (s *Store) EnabledSites() ([]*Site, error) {
	all, err := s.Sites()
	if err != nil {
		return nil, err
	}
	out := make([]*Site, 0, len(all))
	for _, site := range all {
		if site.Enabled {
			out = append(out, site)
		}
	}
	return out, nil
}

// SiteByID 取单个站点。
func (s *Store) SiteByID(id int64) (*Site, error) {
	return scanSite(s.db.QueryRow(`SELECT `+siteCols+` FROM sites WHERE id = ?`, id))
}

// checkDomainConflict 确认这些域名没有被别的站点占用。excludeID 传站点自己的
// ID（新建时传 0），免得编辑时和自己冲突。
func (s *Store) checkDomainConflict(domains string, excludeID int64) error {
	existing, err := s.Sites()
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, d := range strings.Split(domains, ",") {
		want[d] = true
	}
	for _, site := range existing {
		if site.ID == excludeID {
			continue
		}
		for _, d := range site.DomainList() {
			if want[d] {
				return fmt.Errorf("域名 %s 已经被站点「%s」占用了", d, site.PrimaryDomain())
			}
		}
	}
	return nil
}

// CreateSite 新建站点。
func (s *Store) CreateSite(site *Site) error {
	if err := site.Validate(); err != nil {
		return err
	}
	if err := s.checkDomainConflict(site.Domains, 0); err != nil {
		return err
	}
	now := time.Now().Unix()
	site.CreatedAt, site.UpdatedAt = now, now
	res, err := s.db.Exec(
		`INSERT INTO sites(domains, upstream_scheme, upstream_host, upstream_port,
			enabled, https, force_https, skip_tls_verify, basic_user, basic_hash,
			advanced, note, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		site.Domains, site.UpstreamScheme, site.UpstreamHost, site.UpstreamPort,
		site.Enabled, site.HTTPS, site.ForceHTTPS, site.SkipTLSVerify,
		site.BasicUser, site.BasicHash, site.Advanced, site.Note, now, now)
	if err != nil {
		return err
	}
	site.ID, _ = res.LastInsertId()
	return nil
}

// UpdateSite 更新站点。
func (s *Store) UpdateSite(site *Site) error {
	if err := site.Validate(); err != nil {
		return err
	}
	if err := s.checkDomainConflict(site.Domains, site.ID); err != nil {
		return err
	}
	site.UpdatedAt = time.Now().Unix()
	res, err := s.db.Exec(
		`UPDATE sites SET domains=?, upstream_scheme=?, upstream_host=?, upstream_port=?,
			enabled=?, https=?, force_https=?, skip_tls_verify=?, basic_user=?, basic_hash=?,
			advanced=?, note=?, updated_at=?
		 WHERE id=?`,
		site.Domains, site.UpstreamScheme, site.UpstreamHost, site.UpstreamPort,
		site.Enabled, site.HTTPS, site.ForceHTTPS, site.SkipTLSVerify,
		site.BasicUser, site.BasicHash, site.Advanced, site.Note, site.UpdatedAt, site.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetSiteEnabled 启用/停用站点。
func (s *Store) SetSiteEnabled(id int64, enabled bool) error {
	res, err := s.db.Exec(`UPDATE sites SET enabled = ?, updated_at = ? WHERE id = ?`,
		enabled, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteSite 删除站点。
func (s *Store) DeleteSite(id int64) error {
	res, err := s.db.Exec(`DELETE FROM sites WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

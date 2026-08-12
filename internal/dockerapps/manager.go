package dockerapps

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	maxComposeSize  = 2 << 20
	maxEnvSize      = 512 << 10
	maxArchiveSize  = 200 << 20
	maxArchiveFiles = 5000
	maxRetainedJobs = 32
	jobRetention    = 24 * time.Hour
)

var composeCandidates = []string{
	"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml",
}

// Manager owns CaddyUI's on-disk Compose application catalog and delegates
// Docker operations to HelperClient.
type Manager struct {
	Root   string
	Helper *HelperClient
	HTTP   *http.Client

	jobsMu   sync.Mutex
	jobs     map[string]Job
	global   bool
	mutation sync.Mutex
}

func New(root, socketPath string) *Manager {
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dialer.DialContext(ctx, "tcp4", address)
		if err == nil {
			return conn, nil
		}
		return dialer.DialContext(ctx, network, address)
	}
	return &Manager{
		Root:   root,
		Helper: NewHelperClient(socketPath),
		HTTP: &http.Client{
			Timeout:   45 * time.Second,
			Transport: transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return errors.New("重定向次数过多")
				}
				return nil
			},
		},
		jobs: make(map[string]Job),
	}
}

func (m *Manager) EnsureRoot() error {
	if strings.TrimSpace(m.Root) == "" {
		return errors.New("Docker 应用目录未配置")
	}
	return os.MkdirAll(m.Root, 0o750)
}

func (m *Manager) Apps() ([]*App, error) {
	if err := m.EnsureRoot(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(m.Root)
	if err != nil {
		return nil, err
	}
	apps := make([]*App, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !ValidAppName(entry.Name()) {
			continue
		}
		app, err := m.App(entry.Name())
		if err != nil {
			continue
		}
		apps = append(apps, app)
	}
	sortApps(apps)
	return apps, nil
}

func (m *Manager) App(name string) (*App, error) {
	if !ValidAppName(name) {
		return nil, errors.New("应用名称不正确")
	}
	dir, err := m.appDir(name)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(dir, metadataFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	var app App
	if err := json.Unmarshal(b, &app); err != nil {
		return nil, fmt.Errorf("应用元数据损坏: %w", err)
	}
	if app.Name != name || !ValidAppName(app.Name) {
		return nil, errors.New("应用元数据中的名称不正确")
	}
	if app.ComposeRel == "" {
		app.ComposeRel = "compose.yaml"
	}
	if app.EnvRel == "" {
		app.EnvRel = ".env"
	}
	app.Dir = dir
	if _, err := safeJoin(dir, app.ComposeRel); err != nil {
		return nil, fmt.Errorf("Compose 路径不安全: %w", err)
	}
	return &app, nil
}

func (m *Manager) appDir(name string) (string, error) {
	if !ValidAppName(name) {
		return "", errors.New("应用名称只能使用小写字母、数字、横线或下划线")
	}
	root, err := filepath.Abs(m.Root)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, name)
	if filepath.Dir(dir) != root {
		return "", errors.New("应用目录越界")
	}
	return dir, nil
}

func safeJoin(root, rel string) (string, error) {
	rel = filepath.Clean(filepath.FromSlash(strings.TrimSpace(rel)))
	if rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("路径必须位于应用目录内")
	}
	full := filepath.Join(root, rel)
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	fullAbs, err := filepath.Abs(full)
	if err != nil {
		return "", err
	}
	if fullAbs != rootAbs && !strings.HasPrefix(fullAbs, rootAbs+string(filepath.Separator)) {
		return "", errors.New("路径越界")
	}
	return fullAbs, nil
}

func (m *Manager) Compose(app *App) (string, error) {
	p, err := safeJoin(app.Dir, app.ComposeRel)
	if err != nil {
		return "", err
	}
	b, err := readLimitedFile(p, maxComposeSize)
	return string(b), err
}

func (m *Manager) Env(app *App) (string, error) {
	p, err := safeJoin(app.Dir, app.EnvRel)
	if err != nil {
		return "", err
	}
	b, err := readLimitedFile(p, maxEnvSize)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return string(b), err
}

func readLimitedFile(name string, max int64) ([]byte, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errors.New("文件太大")
	}
	return b, nil
}

// Draft is the editable import result shown before anything is installed.
type Draft struct {
	Name        string
	DisplayName string
	Compose     string
	Env         string
	Source      Source
}

func (m *Manager) Prepare(ctx context.Context, raw string) (*Draft, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("请粘贴 GitHub 地址或 Compose 内容")
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return m.prepareGitHub(ctx, raw)
	}
	if len(raw) > maxComposeSize {
		return nil, errors.New("Compose 内容太大")
	}
	if !looksLikeCompose(raw) {
		return nil, errors.New("没有识别到 Compose 内容；通常应该包含 services:")
	}
	return &Draft{Name: "docker-app", DisplayName: "Docker 应用", Compose: ensureTrailingNewline(raw)}, nil
}

func (m *Manager) CheckAvailable(ctx context.Context) error {
	info, err := m.Helper.Info(ctx, m.Root)
	if err != nil {
		return err
	}
	if !info.Available {
		return errors.New(info.Error)
	}
	return nil
}

func looksLikeCompose(raw string) bool {
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == "services:" {
			return true
		}
	}
	return false
}

func (m *Manager) prepareGitHub(ctx context.Context, raw string) (*Draft, error) {
	source, err := parseGitHubURL(raw)
	if err != nil {
		return nil, err
	}
	if source.Ref == "" {
		source.Ref, err = m.defaultGitHubBranch(ctx, source.Owner, source.Repo)
		if err != nil {
			return nil, err
		}
	}
	if source.ComposePath == "" {
		source.ComposePath, err = m.findGitHubCompose(ctx, source)
		if err != nil {
			return nil, err
		}
	}
	compose, err := m.fetchRawGitHub(ctx, source, maxComposeSize)
	if err != nil {
		return nil, fmt.Errorf("下载 Compose 文件失败: %w", err)
	}
	if !looksLikeCompose(string(compose)) {
		return nil, errors.New("这个 GitHub 文件看起来不是 Docker Compose 配置")
	}

	env := ""
	composeDir := path.Dir(source.ComposePath)
	for _, name := range []string{".env.example", ".env.sample", "example.env"} {
		test := source
		test.ComposePath = path.Join(composeDir, name)
		if b, err := m.fetchRawGitHub(ctx, test, maxEnvSize); err == nil {
			env = string(b)
			break
		}
	}
	display := source.Repo
	return &Draft{
		Name:        Slugify(source.Repo),
		DisplayName: display,
		Compose:     ensureTrailingNewline(string(compose)),
		Env:         ensureTrailingNewline(env),
		Source:      source,
	}, nil
}

func (m *Manager) findGitHubCompose(ctx context.Context, source Source) (string, error) {
	for _, candidate := range composeCandidates {
		test := source
		test.ComposePath = candidate
		if _, err := m.fetchRawGitHub(ctx, test, maxComposeSize); err == nil {
			return candidate, nil
		}
	}

	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/trees/%s?recursive=1",
		url.PathEscape(source.Owner), url.PathEscape(source.Repo), url.QueryEscape(source.Ref))
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "CaddyUI")
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("搜索 GitHub Compose 文件失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errors.New("仓库根目录没有找到 Compose；请粘贴具体 compose.yaml 文件地址")
	}
	var tree struct {
		Truncated bool `json:"truncated"`
		Tree      []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&tree); err != nil {
		return "", err
	}
	if tree.Truncated {
		return "", errors.New("GitHub 仓库文件太多，无法自动定位 Compose；请粘贴具体 compose.yaml 文件地址")
	}
	var matches []string
	for _, item := range tree.Tree {
		if item.Type != "blob" {
			continue
		}
		base := path.Base(strings.ToLower(item.Path))
		for _, candidate := range composeCandidates {
			if base == strings.ToLower(candidate) && strings.Count(item.Path, "/") <= 2 {
				matches = append(matches, item.Path)
			}
		}
	}
	if len(matches) == 0 {
		return "", errors.New("仓库里没有找到常见的 Compose 文件；请查看项目文档后粘贴具体文件地址")
	}
	sort.Slice(matches, func(i, j int) bool {
		depthI, depthJ := strings.Count(matches[i], "/"), strings.Count(matches[j], "/")
		if depthI != depthJ {
			return depthI < depthJ
		}
		return len(matches[i]) < len(matches[j])
	})
	return matches[0], nil
}

func parseGitHubURL(raw string) (Source, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.EqualFold(u.Hostname(), "github.com") {
		return Source{}, errors.New("目前只支持 github.com 项目地址；也可以直接粘贴 Compose 内容")
	}
	parts := strings.Split(strings.Trim(strings.TrimSuffix(u.Path, ".git"), "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return Source{}, errors.New("GitHub 地址不完整")
	}
	s := Source{URL: raw, Owner: parts[0], Repo: parts[1]}
	if len(parts) >= 5 && (parts[2] == "blob" || parts[2] == "tree") {
		s.Ref = parts[3]
		if parts[2] == "blob" {
			s.ComposePath = strings.Join(parts[4:], "/")
		}
	}
	return s, nil
}

func (m *Manager) defaultGitHubBranch(ctx context.Context, owner, repo string) (string, error) {
	u := fmt.Sprintf("https://api.github.com/repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo))
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "CaddyUI")
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("读取 GitHub 仓库信息失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub 返回 %s（私有仓库暂不支持）", resp.Status)
	}
	var meta struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&meta); err != nil {
		return "", err
	}
	if meta.DefaultBranch == "" {
		return "", errors.New("GitHub 没有返回默认分支")
	}
	return meta.DefaultBranch, nil
}

func (m *Manager) fetchRawGitHub(ctx context.Context, source Source, max int64) ([]byte, error) {
	u := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s",
		url.PathEscape(source.Owner), url.PathEscape(source.Repo),
		url.PathEscape(source.Ref), strings.Join(escapePath(source.ComposePath), "/"))
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Header.Set("User-Agent", "CaddyUI")
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub 返回 %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errors.New("文件太大")
	}
	return b, nil
}

func escapePath(p string) []string {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return parts
}

func ensureTrailingNewline(s string) string {
	if s == "" || strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

func validateDraft(d Draft) (Draft, error) {
	d.Name = Slugify(d.Name)
	d.DisplayName = strings.TrimSpace(d.DisplayName)
	d.Compose = strings.TrimSpace(d.Compose)
	if !ValidAppName(d.Name) {
		return d, errors.New("应用名称不正确")
	}
	if d.DisplayName == "" {
		d.DisplayName = d.Name
	}
	if len(d.DisplayName) > 100 {
		return d, errors.New("显示名称太长")
	}
	if len(d.Compose) == 0 || len(d.Compose) > maxComposeSize || !looksLikeCompose(d.Compose) {
		return d, errors.New("Compose 内容不正确，必须包含 services:")
	}
	if len(d.Env) > maxEnvSize {
		return d, errors.New("环境变量文件太大")
	}
	d.Compose = ensureTrailingNewline(d.Compose)
	d.Env = ensureTrailingNewline(d.Env)
	return d, nil
}

func (m *Manager) SaveDraft(ctx context.Context, draft Draft) (*App, error) {
	draft, err := validateDraft(draft)
	if err != nil {
		return nil, err
	}
	if m.Busy() {
		return nil, errors.New("已有 Docker 操作正在进行，请稍后再安装")
	}
	m.mutation.Lock()
	defer m.mutation.Unlock()
	if m.Busy() {
		return nil, errors.New("已有 Docker 操作正在进行，请稍后再安装")
	}
	if err := m.EnsureRoot(); err != nil {
		return nil, err
	}
	finalDir, err := m.appDir(draft.Name)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(finalDir); err == nil {
		return nil, errors.New("同名应用已经存在")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	tmp, err := os.MkdirTemp(m.Root, ".import-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	composeRel := "compose.yaml"
	projectRoot := tmp
	if draft.Source.IsGitHub() {
		if err := m.downloadGitHubArchive(ctx, draft.Source, tmp); err != nil {
			return nil, err
		}
		composeSourceRel := filepath.ToSlash(filepath.Clean(filepath.FromSlash(draft.Source.ComposePath)))
		composeDir := filepath.ToSlash(filepath.Dir(filepath.FromSlash(composeSourceRel)))
		composeRel = composeSourceRel
		if composeDir != "." {
			composeRel = filepath.ToSlash(filepath.Join(composeDir, "compose.caddyui.yaml"))
		}
	}
	composePath, err := safeJoin(projectRoot, composeRel)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(composePath), 0o750); err != nil {
		return nil, err
	}
	if err := os.WriteFile(composePath, []byte(draft.Compose), 0o640); err != nil {
		return nil, err
	}
	envRel := filepath.ToSlash(filepath.Join(filepath.Dir(filepath.FromSlash(composeRel)), ".env"))
	if draft.Env != "" {
		envPath, err := safeJoin(projectRoot, envRel)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(envPath, []byte(draft.Env), 0o640); err != nil {
			return nil, err
		}
	}

	now := time.Now().Unix()
	app := &App{
		Version: metadataV1, Name: draft.Name, DisplayName: draft.DisplayName,
		Source: draft.Source, ComposeRel: composeRel, EnvRel: envRel,
		Managed: true, CreatedAt: now, UpdatedAt: now, Dir: finalDir,
	}
	if err := writeMetadata(tmp, app); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, finalDir); err != nil {
		return nil, err
	}
	return app, nil
}

func writeMetadata(dir string, app *App) error {
	b, err := json.MarshalIndent(app, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(filepath.Join(dir, metadataFile), b, 0o640)
}

func (m *Manager) downloadGitHubArchive(ctx context.Context, source Source, dest string) error {
	u := fmt.Sprintf("https://codeload.github.com/%s/%s/tar.gz/%s",
		url.PathEscape(source.Owner), url.PathEscape(source.Repo), url.PathEscape(source.Ref))
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Header.Set("User-Agent", "CaddyUI")
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("下载 GitHub 项目失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载 GitHub 项目失败: %s", resp.Status)
	}
	gz, err := gzip.NewReader(io.LimitReader(resp.Body, maxArchiveSize+1))
	if err != nil {
		return fmt.Errorf("解压 GitHub 项目失败: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := 0
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("解压 GitHub 项目失败: %w", err)
		}
		files++
		if files > maxArchiveFiles {
			return errors.New("GitHub 项目文件过多，停止导入")
		}
		parts := strings.SplitN(filepath.ToSlash(h.Name), "/", 2)
		if len(parts) < 2 || parts[1] == "" {
			continue
		}
		rel := parts[1]
		target, err := safeJoin(dest, rel)
		if err != nil {
			return fmt.Errorf("GitHub 项目包含不安全路径: %w", err)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if h.Size < 0 || h.Size > maxArchiveSize-total {
				return errors.New("GitHub 项目太大，停止导入")
			}
			total += h.Size
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return err
			}
			mode := os.FileMode(0o640)
			if h.FileInfo().Mode()&0o111 != 0 {
				mode = 0o750
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(f, tr, h.Size)
			closeErr := f.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		case tar.TypeSymlink, tar.TypeLink:
			// Links can point outside the project tree. They are uncommon in
			// Compose repositories, so skipping them is the safest behavior.
			continue
		}
	}
	return nil
}

func (m *Manager) UpdateFiles(app *App, displayName, compose, env string) error {
	if m.Busy() {
		return errors.New("已有 Docker 操作正在进行，请稍后再保存配置")
	}
	m.mutation.Lock()
	defer m.mutation.Unlock()
	if m.Busy() {
		return errors.New("已有 Docker 操作正在进行，请稍后再保存配置")
	}
	draft, err := validateDraft(Draft{Name: app.Name, DisplayName: displayName, Compose: compose, Env: env})
	if err != nil {
		return err
	}
	composePath, err := safeJoin(app.Dir, app.ComposeRel)
	if err != nil {
		return err
	}
	envPath, err := safeJoin(app.Dir, app.EnvRel)
	if err != nil {
		return err
	}
	oldCompose, err := os.ReadFile(composePath)
	if err != nil {
		return err
	}
	oldEnv, envReadErr := os.ReadFile(envPath)
	if envReadErr != nil && !errors.Is(envReadErr, os.ErrNotExist) {
		return envReadErr
	}
	oldApp := *app
	rollback := func() {
		_ = os.WriteFile(composePath, oldCompose, 0o640)
		if envReadErr == nil {
			_ = os.WriteFile(envPath, oldEnv, 0o640)
		} else {
			_ = os.Remove(envPath)
		}
		*app = oldApp
		_ = writeMetadata(app.Dir, app)
	}
	if err := os.WriteFile(composePath, []byte(draft.Compose), 0o640); err != nil {
		return err
	}
	if draft.Env == "" {
		if err := os.Remove(envPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			rollback()
			return err
		}
	} else if err := os.WriteFile(envPath, []byte(draft.Env), 0o640); err != nil {
		rollback()
		return err
	}
	app.DisplayName = draft.DisplayName
	app.UpdatedAt = time.Now().Unix()
	if err := writeMetadata(app.Dir, app); err != nil {
		rollback()
		return err
	}
	return nil
}

func (m *Manager) RefreshFromGitHub(ctx context.Context, app *App) (*Draft, error) {
	if !app.Source.IsGitHub() {
		return nil, errors.New("这个应用不是从 GitHub 导入的")
	}
	compose, err := m.fetchRawGitHub(ctx, app.Source, maxComposeSize)
	if err != nil {
		return nil, fmt.Errorf("下载 GitHub Compose 失败: %w", err)
	}
	env, err := m.Env(app)
	if err != nil {
		return nil, err
	}
	return &Draft{
		Name: app.Name, DisplayName: app.DisplayName, Compose: ensureTrailingNewline(string(compose)),
		Env: env, Source: app.Source,
	}, nil
}

func (m *Manager) RefreshRepository(ctx context.Context, app *App) error {
	if !app.Source.IsGitHub() {
		return errors.New("这个应用不是从 GitHub 导入的")
	}
	if m.Busy() {
		return errors.New("已有 Docker 操作正在进行，请稍后再同步项目文件")
	}
	m.mutation.Lock()
	defer m.mutation.Unlock()
	if m.Busy() {
		return errors.New("已有 Docker 操作正在进行，请稍后再同步项目文件")
	}
	tmp, err := os.MkdirTemp(m.Root, ".refresh-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := m.downloadGitHubArchive(ctx, app.Source, tmp); err != nil {
		return err
	}
	return copyMissingFiles(tmp, app.Dir)
}

func copyMissingFiles(src, dst string) error {
	return filepath.WalkDir(src, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil || rel == "." {
			return err
		}
		target, err := safeJoin(dst, filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		if _, err := os.Lstat(target); err == nil {
			if entry.IsDir() {
				return nil
			}
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o640)
		if info, err := entry.Info(); err == nil && info.Mode()&0o111 != 0 {
			mode = 0o750
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			in.Close()
			return err
		}
		_, copyErr := io.Copy(out, in)
		inErr := in.Close()
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if inErr != nil {
			return inErr
		}
		return closeErr
	})
}

// Adopt discovers Compose projects that already exist under the managed root.
// This covers files placed there by backup/restore or normal docker compose
// use without requiring terminal commands in the UI.
func (m *Manager) Adopt() (int, error) {
	if m.Busy() {
		return 0, errors.New("已有 Docker 操作正在进行，请稍后再扫描")
	}
	m.mutation.Lock()
	defer m.mutation.Unlock()
	if m.Busy() {
		return 0, errors.New("已有 Docker 操作正在进行，请稍后再扫描")
	}
	if err := m.EnsureRoot(); err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(m.Root)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() || !ValidAppName(entry.Name()) {
			continue
		}
		dir, err := m.appDir(entry.Name())
		if err != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, metadataFile)); err == nil {
			continue
		}
		composeRel := ""
		for _, candidate := range composeCandidates {
			if st, err := os.Stat(filepath.Join(dir, candidate)); err == nil && !st.IsDir() {
				composeRel = candidate
				break
			}
		}
		if composeRel == "" {
			continue
		}
		now := time.Now().Unix()
		app := &App{
			Version: metadataV1, Name: entry.Name(), DisplayName: entry.Name(),
			ComposeRel: composeRel, EnvRel: ".env", CreatedAt: now, UpdatedAt: now,
		}
		if err := writeMetadata(dir, app); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

func (m *Manager) Ref(app *App) (HelperAppRef, error) {
	composeFile, err := safeJoin(app.Dir, app.ComposeRel)
	if err != nil {
		return HelperAppRef{}, err
	}
	envFile, err := safeJoin(app.Dir, app.EnvRel)
	if err != nil {
		return HelperAppRef{}, err
	}
	if _, err := os.Stat(envFile); errors.Is(err, os.ErrNotExist) {
		envFile = ""
	} else if err != nil {
		return HelperAppRef{}, err
	}
	projectDir := app.Dir
	if app.Source.IsGitHub() {
		sourceDir := filepath.ToSlash(filepath.Dir(filepath.FromSlash(app.Source.ComposePath)))
		if sourceDir != "." {
			projectDir, err = safeJoin(app.Dir, sourceDir)
			if err != nil {
				return HelperAppRef{}, err
			}
		}
	}
	return HelperAppRef{
		Name: app.Name, AppDir: app.Dir, ProjectDir: projectDir,
		ComposeFile: composeFile, EnvFile: envFile,
	}, nil
}

func (m *Manager) Info(ctx context.Context) DockerInfo {
	info, err := m.Helper.Info(ctx, m.Root)
	if err != nil {
		info.Available = false
		if info.Error == "" {
			info.Error = err.Error()
		}
	}
	return info
}

func (m *Manager) Statuses(ctx context.Context, apps []*App) (map[string][]Container, map[string]string) {
	refs := make([]HelperAppRef, 0, len(apps))
	errs := make(map[string]string)
	for _, app := range apps {
		ref, err := m.Ref(app)
		if err != nil {
			errs[app.Name] = err.Error()
			continue
		}
		refs = append(refs, ref)
	}
	resp, err := m.Helper.Do(ctx, HelperRequest{Action: "statuses", Apps: refs})
	if err != nil {
		errs["*"] = err.Error()
		return nil, errs
	}
	for k, v := range resp.StatusErrors {
		errs[k] = v
	}
	return resp.Statuses, errs
}

func (m *Manager) Containers(ctx context.Context, app *App) ([]Container, error) {
	ref, err := m.Ref(app)
	if err != nil {
		return nil, err
	}
	resp, err := m.Helper.Do(ctx, HelperRequest{Action: "ps", App: &ref})
	if err != nil {
		return nil, err
	}
	return resp.Containers, nil
}

func (m *Manager) Logs(ctx context.Context, app *App, tail int) (string, error) {
	ref, err := m.Ref(app)
	if err != nil {
		return "", err
	}
	resp, err := m.Helper.Do(ctx, HelperRequest{Action: "logs", App: &ref, Tail: tail})
	if resp != nil {
		return resp.Output, err
	}
	return "", err
}

func (m *Manager) Validate(ctx context.Context, app *App) (string, error) {
	ref, err := m.Ref(app)
	if err != nil {
		return "", err
	}
	return m.Helper.Run(ctx, "validate", ref)
}

func (m *Manager) Images(ctx context.Context) ([]Image, error) {
	resp, err := m.Helper.Do(ctx, HelperRequest{Action: "images"})
	if err != nil {
		return nil, err
	}
	sort.Slice(resp.Images, func(i, j int) bool { return resp.Images[i].Name() < resp.Images[j].Name() })
	return resp.Images, nil
}

func (m *Manager) RemoveImage(ctx context.Context, image string) (string, error) {
	image = strings.TrimSpace(image)
	if image == "" || len(image) > 300 || strings.ContainsAny(image, "\x00\r\n") {
		return "", errors.New("镜像名称不正确")
	}
	resp, err := m.Helper.Do(ctx, HelperRequest{Action: "image-remove", Image: image})
	if resp != nil {
		return resp.Output, err
	}
	return "", err
}

func (m *Manager) PullImage(ctx context.Context, image string) (string, error) {
	image = strings.TrimSpace(image)
	if image == "" || len(image) > 300 || strings.HasPrefix(image, "-") || strings.ContainsAny(image, "\x00\r\n\t ") {
		return "", errors.New("镜像名称不正确，例如 nginx:latest")
	}
	resp, err := m.Helper.Do(ctx, HelperRequest{Action: "image-pull", Image: image})
	if resp != nil {
		return resp.Output, err
	}
	return "", err
}

func (m *Manager) PruneImages(ctx context.Context) (string, error) {
	resp, err := m.Helper.Do(ctx, HelperRequest{Action: "image-prune"})
	if resp != nil {
		return resp.Output, err
	}
	return "", err
}

func (m *Manager) StartJob(key, action string, fn func(context.Context) (string, error)) error {
	if m.Busy() {
		return errors.New("已有 Docker 操作正在进行，请稍后刷新")
	}
	m.mutation.Lock()
	if err := m.beginOperation(key, action, true); err != nil {
		m.mutation.Unlock()
		return err
	}

	go func() {
		defer m.mutation.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		output, err := fn(ctx)
		if len(output) > 64<<10 {
			output = output[len(output)-(64<<10):]
		}
		m.finishOperation(key, output, err, true)
	}()
	return nil
}

// RunJob serializes a request-bound Docker mutation with background jobs.
// Synchronous operations such as deleting an image still need the same global
// lock, otherwise they can race a deploy that is already using Docker.
func (m *Manager) RunJob(ctx context.Context, key, action string, fn func(context.Context) (string, error)) (string, error) {
	if m.Busy() {
		return "", errors.New("已有 Docker 操作正在进行，请稍后刷新")
	}
	m.mutation.Lock()
	defer m.mutation.Unlock()
	if err := m.beginOperation(key, action, true); err != nil {
		return "", err
	}
	output, err := fn(ctx)
	if len(output) > 64<<10 {
		output = output[len(output)-(64<<10):]
	}
	m.finishOperation(key, output, err, true)
	return output, err
}

func (m *Manager) beginOperation(key, action string, trackJob bool) error {
	m.jobsMu.Lock()
	defer m.jobsMu.Unlock()
	if m.global {
		return errors.New("已有 Docker 操作正在进行，请稍后刷新")
	}
	if trackJob {
		now := time.Now()
		m.pruneJobsLocked(now)
		if old, ok := m.jobs[key]; ok && old.Running() {
			return errors.New("这个应用已有操作正在进行，请稍后刷新")
		}
		m.jobs[key] = Job{Key: key, Action: action, State: JobRunning, StartedAt: now}
		m.pruneJobsLocked(now)
	}
	m.global = true
	return nil
}

func (m *Manager) finishOperation(key, output string, err error, trackJob bool) {
	m.jobsMu.Lock()
	defer m.jobsMu.Unlock()
	if trackJob {
		job := m.jobs[key]
		job.Output = output
		job.FinishedAt = time.Now()
		if err != nil {
			job.State, job.Error = JobFailed, err.Error()
		} else {
			job.State = JobOK
		}
		m.jobs[key] = job
		m.pruneJobsLocked(job.FinishedAt)
	}
	m.global = false
}

// pruneJobsLocked keeps transient Docker task output small and bounded. The
// UI only needs recent results; Compose files and Docker remain the source of
// truth. Running work is never removed.
func (m *Manager) pruneJobsLocked(now time.Time) {
	cutoff := now.Add(-jobRetention)
	for key, job := range m.jobs {
		if !job.Running() && !job.FinishedAt.IsZero() && job.FinishedAt.Before(cutoff) {
			delete(m.jobs, key)
		}
	}
	if len(m.jobs) <= maxRetainedJobs {
		return
	}
	type completedJob struct {
		key      string
		finished time.Time
	}
	completed := make([]completedJob, 0, len(m.jobs))
	for key, job := range m.jobs {
		if job.Running() {
			continue
		}
		finished := job.FinishedAt
		if finished.IsZero() {
			finished = job.StartedAt
		}
		completed = append(completed, completedJob{key: key, finished: finished})
	}
	sort.Slice(completed, func(i, j int) bool { return completed[i].finished.Before(completed[j].finished) })
	for _, job := range completed {
		if len(m.jobs) <= maxRetainedJobs {
			break
		}
		delete(m.jobs, job.key)
	}
}

func (m *Manager) Job(key string) *Job {
	m.jobsMu.Lock()
	defer m.jobsMu.Unlock()
	m.pruneJobsLocked(time.Now())
	job, ok := m.jobs[key]
	if !ok {
		return nil
	}
	copy := job
	return &copy
}

func (m *Manager) ActiveJob() *Job {
	m.jobsMu.Lock()
	defer m.jobsMu.Unlock()
	m.pruneJobsLocked(time.Now())
	for _, job := range m.jobs {
		if job.Running() {
			copy := job
			return &copy
		}
	}
	return nil
}

func (m *Manager) Jobs() map[string]Job {
	m.jobsMu.Lock()
	defer m.jobsMu.Unlock()
	m.pruneJobsLocked(time.Now())
	out := make(map[string]Job, len(m.jobs))
	for k, v := range m.jobs {
		out[k] = v
	}
	return out
}

func (m *Manager) Busy() bool {
	m.jobsMu.Lock()
	defer m.jobsMu.Unlock()
	return m.global
}

func (m *Manager) RemoveApp(app *App) error {
	m.mutation.Lock()
	defer m.mutation.Unlock()
	if !app.Managed {
		return errors.New("只允许自动清理本次安装创建的临时应用目录")
	}
	dir, err := m.appDir(app.Name)
	if err != nil {
		return err
	}
	if dir != app.Dir {
		return errors.New("应用目录不匹配")
	}
	return removeImportDir(dir)
}

func removeImportDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.IsDir() {
			if err := removeImportDir(path); err != nil {
				return err
			}
		} else if err := os.Remove(path); err != nil {
			return err
		}
	}
	return os.Remove(dir)
}

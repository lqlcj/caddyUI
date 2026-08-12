package dockerapps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxHelperOutput = 1 << 20
	maxLogOutput    = 256 << 10
)

// HelperServer is the narrowly-scoped privileged side of Docker application
// management. It accepts only a fixed list of Docker/Compose operations and
// never invokes a shell.
type HelperServer struct {
	SocketPath  string
	AppsRoot    string
	SocketGroup string

	dockerMu sync.RWMutex
	docker   string
	opMu     sync.Mutex
}

func NewHelperServer(socketPath, appsRoot, socketGroup string) *HelperServer {
	return &HelperServer{SocketPath: socketPath, AppsRoot: appsRoot, SocketGroup: socketGroup}
}

func (h *HelperServer) Serve(ctx context.Context) error {
	if h.SocketPath == "" || h.AppsRoot == "" {
		return errors.New("Docker 助手的 socket 和应用目录不能为空")
	}
	root, err := filepath.Abs(h.AppsRoot)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return fmt.Errorf("创建 Docker 应用目录: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	h.AppsRoot = filepath.Clean(root)
	docker, _ := findDocker()
	h.dockerMu.Lock()
	h.docker = docker
	h.dockerMu.Unlock()

	socket, err := filepath.Abs(h.SocketPath)
	if err != nil {
		return err
	}
	h.SocketPath = socket
	if err := os.MkdirAll(filepath.Dir(socket), 0o755); err != nil {
		return err
	}
	if st, err := os.Lstat(socket); err == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("拒绝覆盖非 socket 文件 %s", socket)
		}
		if err := os.Remove(socket); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	ln, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("监听 Docker 助手 socket: %w", err)
	}
	defer func() {
		_ = ln.Close()
		_ = os.Remove(socket)
	}()
	if err := os.Chmod(socket, 0o660); err != nil {
		return err
	}
	if h.SocketGroup != "" {
		group, err := user.LookupGroup(h.SocketGroup)
		if err != nil {
			return fmt.Errorf("查找 socket 用户组 %s: %w", h.SocketGroup, err)
		}
		gid, err := strconv.Atoi(group.Gid)
		if err != nil {
			return err
		}
		if err := os.Chown(socket, 0, gid); err != nil {
			return fmt.Errorf("设置 Docker 助手 socket 权限: %w", err)
		}
	}

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go h.handleConn(ctx, conn)
	}
}

func findDocker() (string, error) {
	for _, candidate := range []string{"/usr/bin/docker", "/usr/local/bin/docker"} {
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	if p, err := exec.LookPath("docker"); err == nil {
		return p, nil
	}
	return "", errors.New("没有找到 docker 命令")
}

func (h *HelperServer) handleConn(parent context.Context, conn net.Conn) {
	defer conn.Close()
	if err := authorizePeer(conn); err != nil {
		h.writeResponse(conn, HelperResponse{OK: false, Error: "拒绝未授权的本机进程: " + err.Error()})
		return
	}
	_ = conn.SetDeadline(time.Now().Add(31 * time.Minute))
	var req HelperRequest
	if err := json.NewDecoder(io.LimitReader(conn, 1<<20)).Decode(&req); err != nil {
		h.writeResponse(conn, HelperResponse{OK: false, Error: "请求格式错误: " + err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Minute)
	defer cancel()
	resp := h.execute(ctx, req)
	h.writeResponse(conn, resp)
}

func (h *HelperServer) writeResponse(w io.Writer, resp HelperResponse) {
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *HelperServer) execute(ctx context.Context, req HelperRequest) HelperResponse {
	if helperMutation(req.Action) {
		h.opMu.Lock()
		defer h.opMu.Unlock()
	}
	switch req.Action {
	case "engine-install", "engine-update":
		out, err := h.installDocker(ctx)
		if err != nil {
			return failResponse(err, out)
		}
		return HelperResponse{OK: true, Output: out}
	case "info":
		return h.info(ctx)
	case "statuses":
		statuses := make(map[string][]Container, len(req.Apps))
		errs := make(map[string]string)
		for _, ref := range req.Apps {
			clean, err := h.validateRef(ref)
			if err != nil {
				errs[ref.Name] = err.Error()
				continue
			}
			containers, err := h.ps(ctx, clean)
			if err != nil {
				errs[clean.Name] = err.Error()
				continue
			}
			statuses[clean.Name] = containers
		}
		return HelperResponse{OK: true, Statuses: statuses, StatusErrors: errs}
	case "images":
		if err := h.ensureDocker(); err != nil {
			return failResponse(err, "")
		}
		images, err := h.images(ctx)
		if err != nil {
			return failResponse(err, "")
		}
		return HelperResponse{OK: true, Images: images}
	case "image-remove", "image-pull":
		if err := h.ensureDocker(); err != nil {
			return failResponse(err, "")
		}
		image := strings.TrimSpace(req.Image)
		if !validImageArg(image) {
			return failResponse(errors.New("镜像名称不正确"), "")
		}
		if req.Action == "image-remove" && !h.imageExists(ctx, image) {
			return failResponse(errors.New("这个镜像不在当前镜像列表中，请刷新页面后重试"), "")
		}
		verb := "rm"
		if req.Action == "image-pull" {
			verb = "pull"
		}
		out, err := h.run(ctx, "", "image", verb, "--", image)
		if err != nil {
			return failResponse(err, out)
		}
		return HelperResponse{OK: true, Output: out}
	case "image-prune":
		if err := h.ensureDocker(); err != nil {
			return failResponse(err, "")
		}
		out, err := h.run(ctx, "", "image", "prune", "-f")
		if err != nil {
			return failResponse(err, out)
		}
		return HelperResponse{OK: true, Output: out}
	}

	if req.App == nil {
		return failResponse(errors.New("缺少应用信息"), "")
	}
	if err := h.ensureDocker(); err != nil {
		return failResponse(err, "")
	}
	ref, err := h.validateRef(*req.App)
	if err != nil {
		return failResponse(err, "")
	}

	switch req.Action {
	case "validate":
		out, err := h.compose(ctx, ref, "config", "--quiet")
		if err != nil {
			return failResponse(err, out)
		}
		if strings.TrimSpace(out) == "" {
			out = "Compose 配置检查通过"
		}
		return HelperResponse{OK: true, Output: out}
	case "deploy", "update":
		out, err := h.deploy(ctx, ref)
		if err != nil {
			return failResponse(err, out)
		}
		return HelperResponse{OK: true, Output: out}
	case "up":
		out, err := h.compose(ctx, ref, "up", "-d", "--build", "--remove-orphans")
		if err != nil {
			return failResponse(err, out)
		}
		return HelperResponse{OK: true, Output: out}
	case "stop":
		out, err := h.compose(ctx, ref, "stop")
		if err != nil {
			return failResponse(err, out)
		}
		return HelperResponse{OK: true, Output: out}
	case "restart":
		out, err := h.compose(ctx, ref, "restart")
		if err != nil {
			return failResponse(err, out)
		}
		return HelperResponse{OK: true, Output: out}
	case "down":
		out, err := h.compose(ctx, ref, "down", "--remove-orphans")
		if err != nil {
			return failResponse(err, out)
		}
		return HelperResponse{OK: true, Output: out}
	case "uninstall":
		out, err := h.compose(ctx, ref, "down", "--volumes", "--remove-orphans", "--rmi", "local")
		if err != nil {
			return failResponse(err, out)
		}
		if err := h.removeAppDir(ref); err != nil {
			return HelperResponse{OK: false, Error: "删除项目目录失败: " + err.Error(), Output: out}
		}
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		out += "已删除容器、网络、Compose 卷、项目本地构建镜像和项目目录\n"
		return HelperResponse{OK: true, Output: out}
	case "ps":
		containers, err := h.ps(ctx, ref)
		if err != nil {
			return failResponse(err, "")
		}
		return HelperResponse{OK: true, Containers: containers}
	case "logs":
		tail := req.Tail
		if tail < 20 || tail > 2000 {
			tail = 300
		}
		out, err := h.composeWithLimit(ctx, ref, maxLogOutput, true, "logs", "--no-color", "--timestamps", "--tail", strconv.Itoa(tail))
		if err != nil {
			return failResponse(err, out)
		}
		return HelperResponse{OK: true, Output: out}
	default:
		return failResponse(errors.New("不支持的 Docker 操作"), "")
	}
}

func helperMutation(action string) bool {
	switch action {
	case "engine-install", "engine-update", "image-remove", "image-pull", "image-prune",
		"deploy", "update", "up", "stop", "restart", "down", "uninstall", "validate":
		return true
	default:
		return false
	}
}

func (h *HelperServer) removeAppDir(ref HelperAppRef) error {
	root := filepath.Clean(h.AppsRoot)
	dir := filepath.Clean(ref.AppDir)
	expected := filepath.Join(root, ref.Name)
	if dir == root || dir != expected || !inside(root, dir) {
		return errors.New("拒绝删除 Docker 应用目录之外的路径")
	}
	return os.RemoveAll(dir)
}

func failResponse(err error, output string) HelperResponse {
	msg := err.Error()
	if strings.TrimSpace(output) != "" {
		msg = lastNonEmptyLine(output)
	}
	return HelperResponse{OK: false, Error: msg, Output: output}
}

func (h *HelperServer) info(ctx context.Context) HelperResponse {
	if err := h.ensureDocker(); err != nil {
		return HelperResponse{OK: false, Error: err.Error(), Info: &DockerInfo{Available: false, AppsRoot: h.AppsRoot, Error: err.Error()}}
	}
	dockerVersion, err := h.run(ctx, "", "version", "--format", "{{.Server.Version}}")
	if err != nil {
		msg := "Docker 不可用: " + lastNonEmptyLine(dockerVersion)
		return HelperResponse{OK: false, Error: msg, Info: &DockerInfo{Available: false, AppsRoot: h.AppsRoot, Error: msg}}
	}
	composeVersion, err := h.run(ctx, "", "compose", "version", "--short")
	if err != nil {
		msg := "没有可用的 Docker Compose V2: " + lastNonEmptyLine(composeVersion)
		return HelperResponse{OK: false, Error: msg, Info: &DockerInfo{Available: false, AppsRoot: h.AppsRoot, Error: msg}}
	}
	return HelperResponse{OK: true, Info: &DockerInfo{
		Available: true, DockerVersion: strings.TrimSpace(dockerVersion),
		ComposeVersion: strings.TrimSpace(composeVersion), AppsRoot: h.AppsRoot,
	}}
}

func (h *HelperServer) ensureDocker() error {
	h.dockerMu.RLock()
	docker := h.docker
	h.dockerMu.RUnlock()
	if docker != "" {
		return nil
	}
	docker, err := findDocker()
	if err != nil {
		return errors.New("服务器还没有安装 Docker Engine 和 Compose V2")
	}
	h.dockerMu.Lock()
	h.docker = docker
	h.dockerMu.Unlock()
	return nil
}

func (h *HelperServer) installDocker(ctx context.Context) (string, error) {
	st, err := os.Stat(DockerInstallerPath)
	if err != nil || st.IsDir() || st.Mode()&0o111 == 0 {
		return "", errors.New("Docker 安装助手不存在，重新执行 CaddyUI 一键安装脚本即可补上")
	}
	if st.Mode().Perm()&0o022 != 0 {
		return "", errors.New("Docker 安装助手可被非 root 用户修改，拒绝执行")
	}
	cmd := exec.CommandContext(ctx, DockerInstallerPath)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	buf := &limitedBuffer{limit: maxHelperOutput}
	cmd.Stdout, cmd.Stderr = buf, buf
	err = cmd.Run()
	out := buf.String()
	if err == nil {
		docker, findErr := findDocker()
		if findErr != nil {
			err = findErr
		} else {
			h.dockerMu.Lock()
			h.docker = docker
			h.dockerMu.Unlock()
		}
	}
	return out, err
}

func (h *HelperServer) validateRef(ref HelperAppRef) (HelperAppRef, error) {
	if !ValidAppName(ref.Name) {
		return ref, errors.New("应用名称不正确")
	}
	expectedDir := filepath.Join(h.AppsRoot, ref.Name)
	appDir, err := h.cleanExistingPath(ref.AppDir, true)
	if err != nil {
		return ref, err
	}
	if appDir != expectedDir {
		return ref, errors.New("应用目录与名称不匹配")
	}
	projectDir, err := h.cleanExistingPath(ref.ProjectDir, true)
	if err != nil {
		return ref, err
	}
	composeFile, err := h.cleanExistingPath(ref.ComposeFile, false)
	if err != nil {
		return ref, err
	}
	if !inside(appDir, projectDir) || !inside(appDir, composeFile) || filepath.Dir(composeFile) != projectDir {
		return ref, errors.New("Compose 文件必须位于应用目录内")
	}
	envFile := ""
	if ref.EnvFile != "" {
		envFile, err = h.cleanExistingPath(ref.EnvFile, false)
		if err != nil {
			return ref, err
		}
		if !inside(appDir, envFile) {
			return ref, errors.New("环境变量文件必须位于应用目录内")
		}
	}
	if err := validateMetadata(appDir, ref); err != nil {
		return ref, err
	}
	ref.AppDir, ref.ProjectDir, ref.ComposeFile, ref.EnvFile = appDir, projectDir, composeFile, envFile
	return ref, nil
}

func validateMetadata(appDir string, ref HelperAppRef) error {
	b, err := os.ReadFile(filepath.Join(appDir, metadataFile))
	if err != nil {
		return errors.New("应用元数据不存在")
	}
	var app App
	if err := json.Unmarshal(b, &app); err != nil {
		return errors.New("应用元数据损坏")
	}
	if app.Name != ref.Name {
		return errors.New("应用元数据名称不匹配")
	}
	if app.ComposeRel == "" {
		app.ComposeRel = "compose.yaml"
	}
	if app.EnvRel == "" {
		app.EnvRel = ".env"
	}
	composeFile, err := safeJoin(appDir, app.ComposeRel)
	if err != nil {
		return err
	}
	if filepath.Clean(composeFile) != ref.ComposeFile {
		return errors.New("Compose 路径与应用元数据不匹配")
	}
	expectedProjectDir := appDir
	if app.Source.IsGitHub() {
		sourceDir := filepath.ToSlash(filepath.Dir(filepath.FromSlash(app.Source.ComposePath)))
		if sourceDir != "." {
			expectedProjectDir, err = safeJoin(appDir, sourceDir)
			if err != nil {
				return err
			}
		}
	}
	if filepath.Clean(expectedProjectDir) != ref.ProjectDir {
		return errors.New("项目目录与应用元数据不匹配")
	}
	if ref.EnvFile != "" {
		envFile, err := safeJoin(appDir, app.EnvRel)
		if err != nil {
			return err
		}
		if filepath.Clean(envFile) != ref.EnvFile {
			return errors.New("环境变量路径与应用元数据不匹配")
		}
	}
	return nil
}

func (h *HelperServer) cleanExistingPath(raw string, wantDir bool) (string, error) {
	if raw == "" || !filepath.IsAbs(raw) {
		return "", errors.New("Docker 助手只接受绝对路径")
	}
	clean := filepath.Clean(raw)
	real, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", fmt.Errorf("路径不存在: %w", err)
	}
	real, err = filepath.Abs(real)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if wantDir != st.IsDir() {
		return "", errors.New("路径类型不正确")
	}
	if !inside(h.AppsRoot, real) {
		return "", errors.New("路径超出 Docker 应用目录")
	}
	return filepath.Clean(real), nil
}

func inside(root, target string) bool {
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	return target == root || strings.HasPrefix(target, root+string(filepath.Separator))
}

func validImageArg(image string) bool {
	return image != "" && len(image) <= 300 && !strings.HasPrefix(image, "-") &&
		!strings.ContainsAny(image, "\x00\r\n\t ")
}

func (h *HelperServer) composeArgs(ref HelperAppRef, args ...string) []string {
	base := []string{"compose", "--project-name", ref.Name, "--project-directory", ref.ProjectDir}
	if ref.EnvFile != "" {
		base = append(base, "--env-file", ref.EnvFile)
	}
	base = append(base, "-f", ref.ComposeFile)
	return append(base, args...)
}

func (h *HelperServer) compose(ctx context.Context, ref HelperAppRef, args ...string) (string, error) {
	return h.run(ctx, ref.ProjectDir, h.composeArgs(ref, args...)...)
}

func (h *HelperServer) composeWithLimit(ctx context.Context, ref HelperAppRef, limit int, keepTail bool, args ...string) (string, error) {
	return h.runLimited(ctx, ref.ProjectDir, limit, keepTail, h.composeArgs(ref, args...)...)
}

func (h *HelperServer) deploy(ctx context.Context, ref HelperAppRef) (string, error) {
	var out strings.Builder
	writeStep := func(title string, args ...string) error {
		out.WriteString("==> " + title + "\n")
		part, err := h.compose(ctx, ref, args...)
		out.WriteString(part)
		if part != "" && !strings.HasSuffix(part, "\n") {
			out.WriteByte('\n')
		}
		return err
	}
	if err := writeStep("检查 Compose 配置", "config", "--quiet"); err != nil {
		return out.String(), err
	}
	if err := writeStep("拉取可用镜像", "pull", "--ignore-buildable"); err != nil {
		return out.String(), err
	}
	if err := writeStep("创建并启动容器", "up", "-d", "--build", "--remove-orphans"); err != nil {
		return out.String(), err
	}
	return out.String(), nil
}

func (h *HelperServer) ps(ctx context.Context, ref HelperAppRef) ([]Container, error) {
	out, err := h.compose(ctx, ref, "ps", "--all", "--format", "json")
	if err != nil {
		return nil, fmt.Errorf("读取容器状态失败: %s", lastNonEmptyLine(out))
	}
	return decodeContainers(out)
}

func decodeContainers(raw string) ([]Container, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var list []Container
	if err := json.Unmarshal([]byte(raw), &list); err == nil {
		return list, nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	for {
		var c Container
		if err := dec.Decode(&c); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("解析 Docker 状态失败: %w", err)
		}
		list = append(list, c)
	}
	return list, nil
}

func (h *HelperServer) images(ctx context.Context) ([]Image, error) {
	out, err := h.run(ctx, "", "image", "ls", "--format", "{{json .}}")
	if err != nil {
		return nil, fmt.Errorf("读取镜像列表失败: %s", lastNonEmptyLine(out))
	}
	var images []Image
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var image Image
		if err := json.Unmarshal([]byte(line), &image); err != nil {
			return nil, fmt.Errorf("解析镜像列表失败: %w", err)
		}
		images = append(images, image)
	}
	return images, nil
}

func (h *HelperServer) imageExists(ctx context.Context, wanted string) bool {
	images, err := h.images(ctx)
	if err != nil {
		return false
	}
	for _, image := range images {
		if image.Name() == wanted || image.ID == wanted {
			return true
		}
	}
	return false
}

func (h *HelperServer) run(ctx context.Context, dir string, args ...string) (string, error) {
	return h.runLimited(ctx, dir, maxHelperOutput, false, args...)
}

func (h *HelperServer) runLimited(ctx context.Context, dir string, limit int, keepTail bool, args ...string) (string, error) {
	h.dockerMu.RLock()
	docker := h.docker
	h.dockerMu.RUnlock()
	if docker == "" {
		return "", errors.New("没有找到 docker 命令")
	}
	cmd := exec.CommandContext(ctx, docker, args...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root",
		"LC_ALL=C",
		"COMPOSE_ANSI=never",
		"DOCKER_CLI_HINTS=false",
	}
	buf := &limitedBuffer{limit: limit, keepTail: keepTail}
	cmd.Stdout, cmd.Stderr = buf, buf
	err := cmd.Run()
	out := buf.String()
	if buf.truncated {
		if keepTail {
			out = "……输出过长，只保留最后面的内容……\n" + out
		} else {
			out += "\n……输出过长，只保留前面的内容……\n"
		}
	}
	if ctx.Err() != nil {
		return out, fmt.Errorf("Docker 操作超时: %w", ctx.Err())
	}
	if err != nil {
		return out, err
	}
	return out, nil
}

type limitedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	keepTail  bool
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	original := len(p)
	if b.keepTail {
		if b.limit <= 0 {
			b.truncated = true
			return original, nil
		}
		if len(p) >= b.limit {
			b.buf.Reset()
			_, _ = b.buf.Write(p[len(p)-b.limit:])
			b.truncated = true
			return original, nil
		}
		overflow := b.buf.Len() + len(p) - b.limit
		if overflow > 0 {
			current := b.buf.Bytes()
			copy(current, current[overflow:])
			b.buf.Truncate(len(current) - overflow)
			b.truncated = true
		}
		_, _ = b.buf.Write(p)
		return original, nil
	}
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		return original, nil
	}
	if len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	_, _ = b.buf.Write(p)
	return original, nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.buf.String()
	if !b.keepTail || !b.truncated {
		return out
	}
	start := 0
	for start < len(out) && out[start]&0xc0 == 0x80 {
		start++
	}
	return out[start:]
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return "Docker 命令执行失败"
}

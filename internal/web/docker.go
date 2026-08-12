package web

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"caddyui/internal/dockerapps"
)

type dockerAppCard struct {
	App        *dockerapps.App
	Containers []dockerapps.Container
	Running    int
	Total      int
	Ports      []dockerapps.Publisher
	Error      string
	Job        *dockerapps.Job
	Busy       bool
	BusyAction string
}

func (s *Server) dockerManager() (*dockerapps.Manager, error) {
	if s.svc.Docker == nil {
		return nil, errors.New("Docker 应用功能未配置")
	}
	return s.svc.Docker, nil
}

func (s *Server) handleDockerApps(w http.ResponseWriter, r *http.Request) {
	m, err := s.dockerManager()
	if err != nil {
		s.render(w, r, "docker_apps", map[string]any{"DockerError": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	info := m.Info(ctx)
	installOK, installWhy := m.InstallerAvailable()
	engineJob := m.Job("@engine")
	apps, err := m.Apps()
	if err != nil {
		flashErr(w, "读取 Docker 应用失败：%v", err)
	}
	statuses := map[string][]dockerapps.Container{}
	statusErrors := map[string]string{}
	busy := m.Busy()
	activeJob := m.ActiveJob()
	busyAction := "Docker 操作进行中"
	if activeJob != nil {
		busyAction = activeJob.Action
	}
	if info.Available && len(apps) > 0 {
		statuses, statusErrors = m.Statuses(ctx, apps)
	}
	cards := make([]dockerAppCard, 0, len(apps))
	for _, app := range apps {
		containers := statuses[app.Name]
		card := dockerAppCard{App: app, Containers: containers, Total: len(containers), Error: statusErrors[app.Name], Job: m.Job(app.Name)}
		card.Busy, card.BusyAction = busy, busyAction
		for _, c := range containers {
			if c.Running() {
				card.Running++
			}
			for _, p := range c.Publishers {
				if p.PublishedPort > 0 {
					card.Ports = append(card.Ports, p)
				}
			}
		}
		cards = append(cards, card)
	}
	s.render(w, r, "docker_apps", map[string]any{
		"Docker": info, "Cards": cards, "InstallerAvailable": installOK,
		"InstallerBlocked": installWhy, "EngineJob": engineJob, "ActiveJob": activeJob, "EngineBusy": busy,
	})
}

func (s *Server) handleDockerEngineInstall(w http.ResponseWriter, r *http.Request) {
	m, err := s.dockerManager()
	if err == nil {
		err = m.StartEngineInstall()
	}
	if err != nil {
		flashErr(w, "Docker 安装没能开始：%v", err)
	} else {
		flashWarn(w, "正在后台安装或修复 Docker，通常需要 1~5 分钟。请稍后刷新本页。")
	}
	redirect(w, r, "/docker")
}

func (s *Server) handleDockerScan(w http.ResponseWriter, r *http.Request) {
	m, err := s.dockerManager()
	count := 0
	if err == nil {
		count, err = m.Adopt()
	}
	if err != nil {
		flashErr(w, "扫描失败：%v", err)
	} else if count == 0 {
		flashOK(w, "扫描完成，没有发现新的 Compose 应用。")
	} else {
		flashOK(w, "已导入 %d 个现有 Compose 应用。", count)
	}
	redirect(w, r, "/docker")
}

func (s *Server) handleDockerImportForm(w http.ResponseWriter, r *http.Request) {
	m, _ := s.dockerManager()
	data := map[string]any{}
	if m != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		data["Docker"] = m.Info(ctx)
		cancel()
	}
	s.render(w, r, "docker_import", data)
}

func (s *Server) handleDockerImportPrepare(w http.ResponseWriter, r *http.Request) {
	m, err := s.dockerManager()
	if err != nil {
		flashErr(w, "%v", err)
		redirect(w, r, "/docker/import")
		return
	}
	checkCtx, checkCancel := context.WithTimeout(r.Context(), 5*time.Second)
	err = m.CheckAvailable(checkCtx)
	checkCancel()
	if err != nil {
		flashErr(w, "Docker 暂不可用：%v", err)
		redirect(w, r, "/docker")
		return
	}
	raw := r.PostFormValue("source")
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	draft, err := m.Prepare(ctx, raw)
	if err != nil {
		s.render(w, r, "docker_import", map[string]any{"Error": err.Error(), "SourceText": raw})
		return
	}
	s.renderDockerEditor(w, r, nil, *draft, true, "")
}

func (s *Server) renderDockerEditor(w http.ResponseWriter, r *http.Request, app *dockerapps.App, draft dockerapps.Draft, installing bool, errMsg string) {
	hints := dockerapps.Analyze(draft.Compose, draft.Env)
	title := "确认安装"
	action := "/docker/install"
	if !installing {
		title = "编辑 " + app.Title()
		action = "/docker/apps/" + app.Name + "/edit"
	}
	s.render(w, r, "docker_edit", map[string]any{
		"Title": title, "Action": action, "Installing": installing,
		"App": app, "Draft": draft, "Hints": hints, "Error": errMsg,
	})
}

func draftFromDockerForm(r *http.Request) (dockerapps.Draft, error) {
	draft := dockerapps.Draft{
		Name: strings.TrimSpace(r.PostFormValue("name")), DisplayName: strings.TrimSpace(r.PostFormValue("display_name")),
		Compose: r.PostFormValue("compose"), Env: r.PostFormValue("env"),
		Source: dockerapps.Source{
			URL: r.PostFormValue("source_url"), Owner: r.PostFormValue("source_owner"), Repo: r.PostFormValue("source_repo"),
			Ref: r.PostFormValue("source_ref"), ComposePath: r.PostFormValue("source_compose_path"),
		},
	}
	portValues := map[string]string{}
	portOriginals := map[string]string{}
	composeEnvValues := map[string]string{}
	composeEnvOriginals := map[string]string{}
	envValues := map[string]string{}
	envOriginals := map[string]string{}
	for key, values := range r.PostForm {
		if len(values) == 0 {
			continue
		}
		if id, ok := strings.CutPrefix(key, "port__"); ok {
			index, service, found := strings.Cut(id, "__")
			if found {
				portValues[service+":"+index] = values[0]
			}
		}
		if id, ok := strings.CutPrefix(key, "portorig__"); ok {
			index, service, found := strings.Cut(id, "__")
			if found {
				portOriginals[service+":"+index] = values[0]
			}
		}
		if id, ok := strings.CutPrefix(key, "cenv__"); ok {
			entry, serviceIndex, found := strings.Cut(id, "__")
			if found {
				composeEnvValues[serviceIndex+":"+entry] = values[0]
			}
		}
		if id, ok := strings.CutPrefix(key, "cenvorig__"); ok {
			entry, serviceIndex, found := strings.Cut(id, "__")
			if found {
				composeEnvOriginals[serviceIndex+":"+entry] = values[0]
			}
		}
		if envKey, ok := strings.CutPrefix(key, "env__"); ok {
			envValues[envKey] = values[0]
		}
		if envKey, ok := strings.CutPrefix(key, "envorig__"); ok {
			envOriginals[envKey] = values[0]
		}
	}
	portChanges := map[string]string{}
	for key, value := range portValues {
		if value != portOriginals[key] {
			portChanges[key] = value
		}
	}
	envChanges := map[string]string{}
	for key, value := range envValues {
		if value != envOriginals[key] {
			envChanges[key] = value
		}
	}
	composeEnvChanges := map[string]string{}
	for key, value := range composeEnvValues {
		if value != composeEnvOriginals[key] {
			composeEnvChanges[key] = value
		}
	}
	var err error
	if draft.Compose, err = dockerapps.ApplyPortChanges(draft.Compose, portChanges); err != nil {
		return draft, err
	}
	if draft.Compose, err = dockerapps.ApplyComposeEnvChanges(draft.Compose, composeEnvChanges); err != nil {
		return draft, err
	}
	if draft.Env, err = dockerapps.ApplyEnvChanges(draft.Env, envChanges); err != nil {
		return draft, err
	}
	return draft, nil
}

func (s *Server) handleDockerInstall(w http.ResponseWriter, r *http.Request) {
	m, err := s.dockerManager()
	if err != nil {
		flashErr(w, "%v", err)
		redirect(w, r, "/docker")
		return
	}
	checkCtx, checkCancel := context.WithTimeout(r.Context(), 5*time.Second)
	err = m.CheckAvailable(checkCtx)
	checkCancel()
	if err != nil {
		flashErr(w, "Docker 暂不可用：%v", err)
		redirect(w, r, "/docker")
		return
	}
	draft, err := draftFromDockerForm(r)
	if err != nil {
		s.renderDockerEditor(w, r, nil, draft, true, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	app, err := m.SaveDraft(ctx, draft)
	cancel()
	if err != nil {
		s.renderDockerEditor(w, r, nil, draft, true, err.Error())
		return
	}
	ref, err := m.Ref(app)
	if err != nil {
		_ = m.RemoveApp(app)
		s.renderDockerEditor(w, r, nil, draft, true, err.Error())
		return
	}
	if err := m.StartJob(app.Name, "正在安装", func(ctx context.Context) (string, error) {
		return m.Helper.Run(ctx, "deploy", ref)
	}); err != nil {
		_ = m.RemoveApp(app)
		s.renderDockerEditor(w, r, nil, draft, true, err.Error())
		return
	}
	flashWarn(w, "已保存 %s，正在后台拉取镜像并启动。第一次安装可能需要几分钟，请刷新查看状态。", app.Title())
	redirect(w, r, "/docker/apps/"+app.Name)
}

func (s *Server) dockerAppFromPath(w http.ResponseWriter, r *http.Request) (*dockerapps.Manager, *dockerapps.App, bool) {
	m, err := s.dockerManager()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return nil, nil, false
	}
	app, err := m.App(r.PathValue("name"))
	if errors.Is(err, os.ErrNotExist) {
		notFound(w)
		return nil, nil, false
	}
	if err != nil {
		http.Error(w, "读取 Docker 应用失败: "+err.Error(), http.StatusInternalServerError)
		return nil, nil, false
	}
	return m, app, true
}

func (s *Server) handleDockerAppDetail(w http.ResponseWriter, r *http.Request) {
	m, app, ok := s.dockerAppFromPath(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	containers, statusErr := m.Containers(ctx, app)
	compose, composeErr := m.Compose(app)
	logs := ""
	var logsErr error
	if len(containers) > 0 {
		logs, logsErr = m.Logs(ctx, app, 300)
	}
	hints := dockerapps.Analyze(compose, "")
	job := m.Job(app.Name)
	s.render(w, r, "docker_app", map[string]any{
		"App": app, "Containers": containers, "StatusError": errorText(statusErr),
		"Compose": compose, "ComposeError": errorText(composeErr), "Logs": logs, "LogsError": errorText(logsErr),
		"Hints": hints, "Job": job, "Busy": m.Busy(),
	})
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *Server) handleDockerAppEditForm(w http.ResponseWriter, r *http.Request) {
	m, app, ok := s.dockerAppFromPath(w, r)
	if !ok {
		return
	}
	compose, err := m.Compose(app)
	if err != nil {
		flashErr(w, "读取 Compose 失败：%v", err)
		redirect(w, r, "/docker/apps/"+app.Name)
		return
	}
	env, err := m.Env(app)
	if err != nil {
		flashErr(w, "读取环境变量失败：%v", err)
		redirect(w, r, "/docker/apps/"+app.Name)
		return
	}
	s.renderDockerEditor(w, r, app, dockerapps.Draft{Name: app.Name, DisplayName: app.DisplayName, Compose: compose, Env: env, Source: app.Source}, false, "")
}

func (s *Server) handleDockerAppGitHubRefresh(w http.ResponseWriter, r *http.Request) {
	m, app, ok := s.dockerAppFromPath(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	draft, err := m.RefreshFromGitHub(ctx, app)
	cancel()
	if err != nil {
		flashErr(w, "重新读取 GitHub 配置失败：%v", err)
		redirect(w, r, "/docker/apps/"+app.Name+"/edit")
		return
	}
	s.renderDockerEditor(w, r, app, *draft, false, "已重新读取 GitHub 上的 Compose，请检查你的端口和密码后再保存。")
}

func (s *Server) handleDockerAppEdit(w http.ResponseWriter, r *http.Request) {
	m, app, ok := s.dockerAppFromPath(w, r)
	if !ok {
		return
	}
	draft, err := draftFromDockerForm(r)
	draft.Name, draft.Source = app.Name, app.Source
	if err != nil {
		s.renderDockerEditor(w, r, app, draft, false, err.Error())
		return
	}
	oldCompose, oldComposeErr := m.Compose(app)
	oldEnv, oldEnvErr := m.Env(app)
	oldDisplay := app.DisplayName
	if oldComposeErr != nil || oldEnvErr != nil {
		flashErr(w, "保存前读取旧配置失败，已取消修改：%v %v", oldComposeErr, oldEnvErr)
		redirect(w, r, "/docker/apps/"+app.Name)
		return
	}
	if err := m.UpdateFiles(app, draft.DisplayName, draft.Compose, draft.Env); err != nil {
		s.renderDockerEditor(w, r, app, draft, false, err.Error())
		return
	}
	if app.Source.IsGitHub() {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		err := m.RefreshRepository(ctx, app)
		cancel()
		if err != nil {
			_ = m.UpdateFiles(app, oldDisplay, oldCompose, oldEnv)
			flashErr(w, "同步 GitHub 项目文件失败，服务器上的旧配置已恢复：%v", err)
			redirect(w, r, "/docker/apps/"+app.Name+"/edit")
			return
		}
	}
	validateCtx, validateCancel := context.WithTimeout(r.Context(), 2*time.Minute)
	_, err = m.Validate(validateCtx, app)
	validateCancel()
	if err != nil {
		_ = m.UpdateFiles(app, oldDisplay, oldCompose, oldEnv)
		flashErr(w, "Compose 检查未通过，服务器上的旧配置已恢复：%v", err)
		redirect(w, r, "/docker/apps/"+app.Name+"/edit")
		return
	}
	flashOK(w, "配置已保存并检查通过。点击“重新部署”让改动生效。")
	redirect(w, r, "/docker/apps/"+app.Name)
}

func dockerActionLabel(action string) string {
	switch action {
	case "up":
		return "正在启动"
	case "stop":
		return "正在停止"
	case "restart":
		return "正在重启"
	case "update":
		return "正在更新镜像并重新部署"
	case "down":
		return "正在停止并移除容器"
	default:
		return "正在处理"
	}
}

func (s *Server) handleDockerAppAction(w http.ResponseWriter, r *http.Request) {
	m, app, ok := s.dockerAppFromPath(w, r)
	if !ok {
		return
	}
	action := r.PathValue("action")
	switch action {
	case "up", "stop", "restart", "update", "down":
	default:
		notFound(w)
		return
	}
	ref, err := m.Ref(app)
	if err == nil {
		err = m.StartJob(app.Name, dockerActionLabel(action), func(ctx context.Context) (string, error) {
			return m.Helper.Run(ctx, action, ref)
		})
	}
	if err != nil {
		flashErr(w, "操作没能开始：%v", err)
	} else {
		flashWarn(w, "%s，稍后刷新查看结果。", dockerActionLabel(action))
	}
	redirect(w, r, "/docker/apps/"+app.Name)
}

func (s *Server) handleDockerAppDelete(w http.ResponseWriter, r *http.Request) {
	m, app, ok := s.dockerAppFromPath(w, r)
	if !ok {
		return
	}
	ref, err := m.Ref(app)
	if err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		_, err = m.RunJob(ctx, app.Name, "正在卸载 "+app.Title(), func(jobCtx context.Context) (string, error) {
			output, runErr := m.Helper.Run(jobCtx, "down", ref)
			if runErr != nil {
				return output, runErr
			}
			archive, archiveErr := m.ArchiveAppLocked(app)
			if archiveErr != nil {
				return output, archiveErr
			}
			if output != "" && !strings.HasSuffix(output, "\n") {
				output += "\n"
			}
			return output + "项目文件已归档到 " + archive, nil
		})
		cancel()
	}
	if err != nil {
		flashErr(w, "卸载失败，应用文件没有删除：%v", err)
		redirect(w, r, "/docker/apps/"+app.Name)
		return
	}
	flashOK(w, "已卸载 %s。命名卷和项目目录中的数据均已保留。", app.Title())
	redirect(w, r, "/docker")
}

func (s *Server) handleDockerImages(w http.ResponseWriter, r *http.Request) {
	m, err := s.dockerManager()
	if err != nil {
		s.render(w, r, "docker_images", map[string]any{"Error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	info := m.Info(ctx)
	var images []dockerapps.Image
	if info.Available {
		images, err = m.Images(ctx)
	}
	job := m.Job("@images")
	s.render(w, r, "docker_images", map[string]any{"Docker": info, "Images": images, "Error": errorText(err), "Job": job, "Busy": m.Busy()})
}

func (s *Server) handleDockerImagePull(w http.ResponseWriter, r *http.Request) {
	m, err := s.dockerManager()
	image := strings.TrimSpace(r.PostFormValue("image"))
	if err == nil {
		err = m.StartJob("@images", "正在拉取镜像 "+image, func(ctx context.Context) (string, error) { return m.PullImage(ctx, image) })
	}
	if err != nil {
		flashErr(w, "拉取没能开始：%v", err)
	} else {
		flashWarn(w, "正在后台拉取 %s，请稍后刷新。", image)
	}
	redirect(w, r, "/docker/images")
}

func (s *Server) handleDockerImageRemove(w http.ResponseWriter, r *http.Request) {
	m, err := s.dockerManager()
	image := strings.TrimSpace(r.PostFormValue("image"))
	if err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		_, err = m.RunJob(ctx, "@images", "正在删除镜像 "+image, func(jobCtx context.Context) (string, error) {
			return m.RemoveImage(jobCtx, image)
		})
		cancel()
	}
	if err != nil {
		flashErr(w, "删除镜像失败：%v。正在使用的镜像不能删除。", err)
	} else {
		flashOK(w, "镜像 %s 已删除。", image)
	}
	redirect(w, r, "/docker/images")
}

func (s *Server) handleDockerImagePrune(w http.ResponseWriter, r *http.Request) {
	m, err := s.dockerManager()
	if err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		_, err = m.RunJob(ctx, "@images", "正在清理悬空镜像", func(jobCtx context.Context) (string, error) {
			return m.PruneImages(jobCtx)
		})
		cancel()
	}
	if err != nil {
		flashErr(w, "清理失败：%v", err)
	} else {
		flashOK(w, "已清理悬空镜像，不会删除正在使用的镜像。")
	}
	redirect(w, r, "/docker/images")
}

// portFieldName maps the service/index key to an HTML form field without
// relying on user-controlled arbitrary names.
func portFieldName(service string, index int) string {
	return "port__" + strconv.Itoa(index) + "__" + service
}

func portOriginalFieldName(service string, index int) string {
	return "portorig__" + strconv.Itoa(index) + "__" + service
}

func composeEnvFieldName(serviceIndex, entryIndex int) string {
	return "cenv__" + strconv.Itoa(entryIndex) + "__" + strconv.Itoa(serviceIndex)
}

func composeEnvOriginalFieldName(serviceIndex, entryIndex int) string {
	return "cenvorig__" + strconv.Itoa(entryIndex) + "__" + strconv.Itoa(serviceIndex)
}

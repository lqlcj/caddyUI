package web

import (
	"os"
	"strings"
	"testing"

	"caddyui/internal/dockerapps"
)

func TestAllTemplatesParse(t *testing.T) {
	assets := os.DirFS("../../web")
	s := &Server{assets: assets}
	if err := s.parseTemplates(); err != nil {
		t.Fatal(err)
	}
	for _, page := range pages {
		if s.tmpl[page] == nil {
			t.Fatalf("template %q was not parsed", page)
		}
	}
}

func TestDockerEditorShowsFullLoopbackBinding(t *testing.T) {
	assets := os.DirFS("../../web")
	s := &Server{assets: assets}
	if err := s.parseTemplates(); err != nil {
		t.Fatal(err)
	}
	data := map[string]any{
		"Title": "确认安装", "Action": "/docker/install", "Installing": true,
		"Draft": dockerapps.Draft{Name: "demo", DisplayName: "Demo", Compose: "services:\n  app:\n    image: demo\n    ports:\n      - 127.0.0.1:16688:16688\n"},
		"Hints": dockerapps.Analyze("services:\n  app:\n    image: demo\n    ports:\n      - 127.0.0.1:16688:16688\n", ""),
	}
	var out strings.Builder
	if err := s.tmpl["docker_edit"].ExecuteTemplate(&out, "layout.html", data); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `value="127.0.0.1:16688"`) || !strings.Contains(out.String(), "容器 16688/tcp") {
		t.Fatalf("loopback binding was not rendered clearly:\n%s", out.String())
	}
	for _, want := range []string{"data-docker-heavy-page", "data-compose-sync", "提交时服务器也会应用"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("Docker editor memory control missing %q:\n%s", want, out.String())
		}
	}
}

func TestDockerAppTemplateWarnsAboutPermanentUninstall(t *testing.T) {
	assets := os.DirFS("../../web")
	s := &Server{assets: assets}
	if err := s.parseTemplates(); err != nil {
		t.Fatal(err)
	}
	data := map[string]any{
		"App":        &dockerapps.App{Name: "demo", DisplayName: "Demo"},
		"Containers": []dockerapps.Container{},
	}
	var out strings.Builder
	if err := s.tmpl["docker_app"].ExecuteTemplate(&out, "layout.html", data); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{"彻底卸载并删除数据", "Compose 创建的卷", "共享镜像不会自动删除", "此操作无法撤销"} {
		if !strings.Contains(html, want) {
			t.Fatalf("docker uninstall warning missing %q:\n%s", want, html)
		}
	}
	for _, want := range []string{"data-docker-heavy-page", "/docker/apps/demo/logs", "/docker/apps/demo/compose", "按需加载"} {
		if !strings.Contains(html, want) {
			t.Fatalf("Docker lazy text control missing %q:\n%s", want, html)
		}
	}
}

func TestDockerAppsTemplateShowsEngineUpdateButton(t *testing.T) {
	assets := os.DirFS("../../web")
	s := &Server{assets: assets}
	if err := s.parseTemplates(); err != nil {
		t.Fatal(err)
	}
	data := map[string]any{
		"Docker":             dockerapps.DockerInfo{Available: true, DockerVersion: "29.7.2", ComposeVersion: "v2.40.0", AppsRoot: "/var/lib/caddyui/docker-apps"},
		"InstallerAvailable": true,
	}
	var out strings.Builder
	if err := s.tmpl["docker_apps"].ExecuteTemplate(&out, "layout.html", data); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "/docker/engine/update") || !strings.Contains(out.String(), "更新 Docker / Compose") {
		t.Fatalf("Docker update action missing:\n%s", out.String())
	}
}

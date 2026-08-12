package dockerapps

import (
	"context"
	"errors"
	"os"
	"runtime"
)

// DockerInstallerPath is installed root-owned and accepts no arguments. Only
// the isolated root Docker helper can execute it; the web process cannot.
const DockerInstallerPath = "/usr/local/lib/caddyui/install-docker.sh"

func (m *Manager) InstallerAvailable() (bool, string) {
	if runtime.GOOS != "linux" {
		return false, "一键安装 Docker 只支持 Linux"
	}
	if st, err := os.Stat(DockerInstallerPath); err != nil || st.IsDir() || st.Mode()&0o111 == 0 {
		return false, "Docker 安装助手没有安装，重新执行 CaddyUI 一键安装脚本即可补上"
	}
	return true, ""
}

func (m *Manager) StartEngineInstall() error {
	if ok, why := m.InstallerAvailable(); !ok {
		return errors.New(why)
	}
	return m.StartJob("@engine", "正在安装或修复 Docker", func(ctx context.Context) (string, error) {
		resp, err := m.Helper.Do(ctx, HelperRequest{Action: "engine-install"})
		if resp != nil {
			return resp.Output, err
		}
		return "", err
	})
}

func (m *Manager) StartEngineUpdate() error {
	if ok, why := m.InstallerAvailable(); !ok {
		return errors.New(why)
	}
	return m.StartJob("@engine", "正在更新 Docker Engine 和 Compose", func(ctx context.Context) (string, error) {
		resp, err := m.Helper.Do(ctx, HelperRequest{Action: "engine-update"})
		if resp != nil {
			return resp.Output, err
		}
		return "", err
	})
}

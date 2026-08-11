// Package app 把存储层和 Caddy 粘在一起：渲染配置、下发、记录版本、回滚。
package app

import (
	"errors"
	"fmt"

	"caddyui/internal/caddy"
	"caddyui/internal/caddybin"
	"caddyui/internal/certs"
	"caddyui/internal/store"
)

// 设置项的 key。
const (
	SettingACMEEmail = "acme_email"
	SettingACMECA    = "acme_ca"
)

// Service 是面板的业务门面。
type Service struct {
	Store  *store.Store
	Caddy  *caddy.Client
	Certs  *certs.Locator
	Binary *caddybin.Manager
}

// Render 生成当前应该生效的 Caddyfile，但不下发。
func (s *Service) Render() ([]byte, error) {
	sites, err := s.Store.EnabledSites()
	if err != nil {
		return nil, err
	}
	return caddy.Render(caddy.RenderOptions{
		AdminAddr: s.Caddy.Addr(),
		ACMEEmail: s.Store.Setting(SettingACMEEmail, ""),
		ACMECA:    s.Store.Setting(SettingACMECA, ""),
		Sites:     sites,
	}), nil
}

// Apply 渲染当前站点并下发给 Caddy，同时记录一个配置版本。
//
// 注意调用顺序：业务改动先写库，再调 Apply。万一 Caddy 拒绝了新配置，用户在
// 面板里的编辑不会丢（还在库里），线上也没受影响（Caddy 保持旧配置），用户
// 改完重试即可。这比"下发失败就回滚数据库"对用户友好得多。
func (s *Service) Apply(reason string) error {
	caddyfile, err := s.Render()
	if err != nil {
		return err
	}

	loadErr := s.Caddy.Load(caddyfile)

	detail := ""
	if loadErr != nil {
		detail = loadErr.Error()
	}
	if _, err := s.Store.AddConfigVersion(string(caddyfile), loadErr == nil, reason, detail); err != nil {
		// 版本记不上不该阻断主流程，配置该生效还是生效了。
		if loadErr == nil {
			return fmt.Errorf("配置已生效，但历史版本记录失败: %w", err)
		}
	}
	return loadErr
}

// ApplyVersion 回滚到某个历史版本。
func (s *Service) ApplyVersion(id int64) error {
	v, err := s.Store.ConfigVersionByID(id)
	if err != nil {
		return err
	}
	if err := s.Caddy.Load([]byte(v.Caddyfile)); err != nil {
		return err
	}
	_, _ = s.Store.AddConfigVersion(v.Caddyfile, true,
		fmt.Sprintf("回滚到 #%d", id), "")
	return nil
}

// Sync 在面板启动时把库里的站点推给 Caddy，不记录版本历史（避免每次重启都
// 塞一条一模一样的记录）。
func (s *Service) Sync() error {
	caddyfile, err := s.Render()
	if err != nil {
		return err
	}
	return s.Caddy.Load(caddyfile)
}

// ---------- 状态 ----------

// Status 是页面顶部展示的 Caddy 连接状态。
type Status struct {
	Addr      string
	Connected bool
	Error     string
}

// Status 探测一次 Caddy。
func (s *Service) Status() Status {
	st := Status{Addr: s.Caddy.Addr()}
	if err := s.Caddy.Ping(); err != nil {
		st.Error = err.Error()
		if errors.Is(err, caddy.ErrUnreachable) {
			st.Error = "连接不上 Caddy，请确认 Caddy 进程正在运行"
		}
		return st
	}
	st.Connected = true
	return st
}

// Package caddy 负责和 Caddy 的 Admin API 打交道，以及把面板里的站点渲染成
// Caddyfile。
//
// 这里只有两个关键点：
//  1. POST /load 是原子的。Caddy 会完整校验并 provision 新配置，任何一步出错
//     都整体回滚，旧配置继续跑。所以面板永远不可能把线上配置改成半残状态。
//  2. Caddy 自己会把生效的配置存成 autosave.json，配合 `caddy run --resume`，
//     即使面板和数据库全丢了，反代也能自己起来。
package caddy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Client 是 Caddy Admin API 的客户端。
type Client struct {
	addr string // 原始地址，同时也会写进生成的 Caddyfile
	base string
	hc   *http.Client
}

// New 创建客户端。addr 支持两种写法：
//   - "127.0.0.1:2019"          —— TCP，跨平台，适合本地调试
//   - "unix//run/caddy/admin.sock" —— unix socket，生产环境推荐，
//     连本地端口扫描都碰不到它
func New(addr string) *Client {
	addr = strings.TrimSpace(addr)
	c := &Client{addr: addr}

	transport := &http.Transport{
		MaxIdleConns:        4,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
	}

	if path, ok := unixPath(addr); ok {
		c.base = "http://unix"
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}
	} else {
		c.base = "http://" + addr
	}

	c.hc = &http.Client{Transport: transport, Timeout: 60 * time.Second}
	return c
}

// unixPath 识别 Caddy 的 "unix/<绝对路径>" 写法。注意 Caddy 官方文档里写作
// unix//run/caddy/admin.sock —— 前缀是 "unix/"，后面紧跟绝对路径。
func unixPath(addr string) (string, bool) {
	if !strings.HasPrefix(addr, "unix/") {
		return "", false
	}
	return strings.TrimPrefix(addr, "unix/"), true
}

// Addr 返回配置的 admin 地址。生成 Caddyfile 时必须原样写回去，否则 Caddy
// 会把 admin 端点挪回默认位置，面板就失联了。
func (c *Client) Addr() string { return c.addr }

// APIError 是 Caddy 返回的非 2xx 响应。
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("Caddy 返回 HTTP %d", e.Status)
	}
	return e.Message
}

// ErrUnreachable 表示压根连不上 Caddy。
var ErrUnreachable = errors.New("连接不上 Caddy Admin API")

func (c *Client) do(ctx context.Context, method, path, contentType string, body []byte) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w（%s）：请确认 Caddy 正在运行、admin 地址配置正确",
			ErrUnreachable, c.addr)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &APIError{Status: resp.StatusCode, Message: extractError(respBody)}
	}
	return respBody, nil
}

// extractError 从 Caddy 的 {"error":"..."} 里挖出人话。
func extractError(body []byte) string {
	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &payload) == nil && payload.Error != "" {
		return payload.Error
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 800 {
		s = s[:800] + "……"
	}
	return s
}

// Load 下发一份 Caddyfile。这是原子操作：要么整份生效，要么原封不动地
// 保持旧配置，不存在中间状态。
func (c *Client) Load(caddyfile []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := c.do(ctx, http.MethodPost, "/load", "text/caddyfile", caddyfile)
	return err
}

// Adapt 只把 Caddyfile 转成 JSON 而不生效，用来做保存前的语法预检。
func (c *Client) Adapt(caddyfile []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := c.do(ctx, http.MethodPost, "/adapt", "text/caddyfile", caddyfile)
	return err
}

// Ping 检查 Caddy 是否活着。
func (c *Client) Ping() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.do(ctx, http.MethodGet, "/config/", "", nil)
	return err
}

// CurrentConfig 返回 Caddy 当前实际在跑的配置（JSON）。用来和面板生成的配置
// 做比对，确认有没有被别的东西改过。
func (c *Client) CurrentConfig() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return c.do(ctx, http.MethodGet, "/config/", "", nil)
}

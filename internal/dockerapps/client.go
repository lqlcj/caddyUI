package dockerapps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// HelperClient talks to the root-owned Docker helper over a local Unix socket.
// Keeping this boundary explicit lets the main web process continue running as
// the unprivileged caddy user.
type HelperClient struct {
	SocketPath string
	Timeout    time.Duration
}

func NewHelperClient(socketPath string) *HelperClient {
	return &HelperClient{SocketPath: socketPath, Timeout: 20 * time.Minute}
}

func (c *HelperClient) Do(ctx context.Context, req HelperRequest) (*HelperResponse, error) {
	if c == nil || c.SocketPath == "" {
		return nil, errors.New("Docker 助手未配置")
	}
	if _, ok := ctx.Deadline(); !ok && c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}

	d := net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "unix", c.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("连接 Docker 助手失败: %w", err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	enc := json.NewEncoder(conn)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(req); err != nil {
		return nil, fmt.Errorf("发送 Docker 请求: %w", err)
	}
	if unix, ok := conn.(*net.UnixConn); ok {
		_ = unix.CloseWrite()
	}

	var resp HelperResponse
	dec := json.NewDecoder(io.LimitReader(conn, 4<<20))
	if err := dec.Decode(&resp); err != nil {
		return nil, fmt.Errorf("读取 Docker 助手响应: %w", err)
	}
	if !resp.OK {
		if resp.Error == "" {
			resp.Error = "Docker 操作失败"
		}
		return &resp, errors.New(resp.Error)
	}
	return &resp, nil
}

func (c *HelperClient) Info(ctx context.Context, appsRoot string) (DockerInfo, error) {
	resp, err := c.Do(ctx, HelperRequest{Action: "info", AppsRoot: appsRoot})
	if err != nil {
		info := DockerInfo{AppsRoot: appsRoot, Error: err.Error()}
		if resp != nil && resp.Info != nil {
			info = *resp.Info
			if info.Error == "" {
				info.Error = err.Error()
			}
		}
		return info, err
	}
	if resp.Info == nil {
		return DockerInfo{AppsRoot: appsRoot}, errors.New("Docker 助手没有返回状态")
	}
	return *resp.Info, nil
}

func (c *HelperClient) Run(ctx context.Context, action string, app HelperAppRef) (string, error) {
	resp, err := c.Do(ctx, HelperRequest{Action: action, App: &app})
	if resp != nil && resp.Output != "" {
		return resp.Output, err
	}
	return "", err
}

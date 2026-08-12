//go:build linux

package dockerapps

import (
	"errors"
	"net"
	"os"
	"os/user"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// authorizePeer verifies the effective UID at the other end of the Unix
// socket. Socket permissions are the first layer; SO_PEERCRED prevents a
// compromised unrelated local account from being accepted even if filesystem
// ACLs are accidentally loosened later.
func authorizePeer(conn net.Conn) error {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return errors.New("连接不是 Unix socket")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return err
	}
	var cred *unix.Ucred
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		cred, socketErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if socketErr != nil {
		return socketErr
	}
	account, err := user.Lookup("caddy")
	if err != nil {
		return err
	}
	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil {
		return err
	}
	if cred == nil || (cred.Uid != uint32(uid) && cred.Uid != 0) {
		return errors.New("调用方不是 caddy 用户")
	}
	if cred.Uid != 0 {
		cgroup, err := os.ReadFile("/proc/" + strconv.Itoa(int(cred.Pid)) + "/cgroup")
		if err != nil {
			return nil // 部分容器/旧内核会隐藏 /proc/<pid>/cgroup，保留 UID 校验兜底。
		}
		text := string(cgroup)
		// cgroup v1 可能只有用户级目录、无法可靠带出 unit 名；只有能看见
		// systemd unit 信息时才做第二层严格校验。
		if strings.Contains(text, ".service") && !strings.Contains(text, "caddyui.service") {
			return errors.New("调用进程不属于 caddyui.service")
		}
	}
	return nil
}

//go:build !linux

package dockerapps

import (
	"errors"
	"net"
)

func authorizePeer(conn net.Conn) error {
	return errors.New("Docker 助手只支持 Linux")
}

//go:build linux

package docker

import (
	"fmt"
	"net"
	"syscall"
)

func unixPeerPID(connection *net.UnixConn) (int, error) {
	raw, err := connection.SyscallConn()
	if err != nil {
		return 0, err
	}
	var credential *syscall.Ucred
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		credential, socketErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if socketErr != nil {
		return 0, socketErr
	}
	if credential == nil || credential.Pid <= 0 {
		return 0, fmt.Errorf("Unix peer returned invalid PID")
	}
	return int(credential.Pid), nil
}

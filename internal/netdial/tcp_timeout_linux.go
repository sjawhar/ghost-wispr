//go:build linux

// Package netdial holds socket options for connections the app dials itself.
package netdial

import (
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// TCPUserTimeout returns a net.Dialer Control that sets TCP_USER_TIMEOUT on a
// dialled socket: the kernel ends the connection when sent data stays
// unacknowledged that long, also against a peer that keeps its window at
// zero. Without it such a write blocks with no end.
func TCPUserTimeout(timeout time.Duration) func(network, address string, c syscall.RawConn) error {
	return func(_, _ string, c syscall.RawConn) error {
		var setErr error
		if err := c.Control(func(fd uintptr) {
			setErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT, int(timeout.Milliseconds()))
		}); err != nil {
			return err
		}
		return setErr
	}
}

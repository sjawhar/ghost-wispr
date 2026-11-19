//go:build !linux

// Package netdial holds socket options for connections the app dials itself.
package netdial

import (
	"syscall"
	"time"
)

// TCPUserTimeout has no socket option off Linux, so it returns no Control;
// each write's deadline still bounds a write there.
func TCPUserTimeout(time.Duration) func(network, address string, c syscall.RawConn) error {
	return nil
}

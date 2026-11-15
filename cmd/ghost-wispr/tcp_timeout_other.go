//go:build !linux

package main

import (
	"syscall"
	"time"
)

// tcpUserTimeout has no socket option off Linux; each write's deadline still
// bounds a Deepgram write there.
func tcpUserTimeout(time.Duration) func(network, address string, c syscall.RawConn) error {
	return nil
}

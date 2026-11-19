//go:build linux

package netdial

import (
	"net"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestTCPUserTimeoutSetsTheSocketOption fails if a socket dialled with
// TCPUserTimeout does not carry TCP_USER_TIMEOUT, in milliseconds: without it
// a write to a peer that stopped taking data blocks with no end.
func TestTCPUserTimeoutSetsTheSocketOption(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	dialer := net.Dialer{Control: TCPUserTimeout(1500 * time.Millisecond)}
	conn, err := dialer.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	raw, err := conn.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var got int
	var getErr error
	if err := raw.Control(func(fd uintptr) {
		got, getErr = unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT)
	}); err != nil {
		t.Fatal(err)
	}
	if getErr != nil {
		t.Fatalf("read TCP_USER_TIMEOUT: %v", getErr)
	}
	if got != 1500 {
		t.Fatalf("TCP_USER_TIMEOUT = %d ms, want 1500", got)
	}
}

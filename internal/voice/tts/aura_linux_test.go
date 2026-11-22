//go:build linux

package tts

import (
	"context"
	"net"
	"testing"

	"golang.org/x/sys/unix"
)

// TestAuraSocketHasTCPUserTimeout fails if the Aura-2 socket does not carry
// TCP_USER_TIMEOUT equal to its write timeout, so the kernel gives up on
// unacknowledged data as soon as a write's deadline does.
func TestAuraSocketHasTCPUserTimeout(t *testing.T) {
	f := startSpeak(t, speakBack(nil))
	s, err := newTestAura(t, f).(*Aura).dial(context.Background())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	tcp, ok := s.raw.(*net.TCPConn)
	if !ok {
		t.Fatalf("the socket's connection is %T, want *net.TCPConn", s.raw)
	}
	raw, err := tcp.SyscallConn()
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
	if want := int(auraWriteTimeout.Milliseconds()); got != want {
		t.Fatalf("TCP_USER_TIMEOUT = %d ms, want the write timeout's %d", got, want)
	}
}

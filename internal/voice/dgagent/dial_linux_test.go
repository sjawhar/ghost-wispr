//go:build linux

package dgagent

import (
	"net"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// TestTheSocketHasTCPUserTimeout fails if the Voice Agent socket does not
// carry TCP_USER_TIMEOUT equal to its write timeout, so the kernel gives up
// on unacknowledged data as soon as a write's deadline does, as the unit's
// other Deepgram sockets do.
func TestTheSocketHasTCPUserTimeout(t *testing.T) {
	s := startStandIn(t)
	sess, _, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: tools(nil)})
	tcp, ok := sess.(*session).conn.NetConn().(*net.TCPConn)
	if !ok {
		t.Fatalf("the socket's connection is %T, want *net.TCPConn", sess.(*session).conn.NetConn())
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
	if want := int(writeTimeout.Milliseconds()); got != want {
		t.Fatalf("TCP_USER_TIMEOUT = %d ms, want the write timeout's %d", got, want)
	}
}

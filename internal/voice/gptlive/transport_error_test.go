package gptlive

import (
	"bufio"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/voice"
)

const transportPlanted = "quokkaplantedroomword"

// An answer to GPT-Live's socket handshake that net/http cannot read fails
// Open without quoting it.
func TestAMalformedHandshakeAnswerQuotesNothing(t *testing.T) {
	address := malformedHTTPServer(t, "HTTP/1.1 "+transportPlanted+"400 Bad\r\n\r\n")
	v, err := New(Config{URL: "ws://" + address + "/v1/live", Token: func(context.Context) (string, error) { return "access-token", nil }})
	if err != nil {
		t.Fatal(err)
	}
	if s, err := v.Open(context.Background(), testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{}))); err == nil || strings.Contains(err.Error(), transportPlanted) {
		if s != nil {
			_ = s.Close()
		}
		t.Fatalf("Open = %v, want a failure that quotes nothing the server sent", err)
	}
}

// malformedHTTPServer answers every connection's request with answer, then
// closes it, and returns its address.
func malformedHTTPServer(t *testing.T, answer string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				reader := bufio.NewReader(conn)
				for {
					line, err := reader.ReadString('\n')
					if err != nil || line == "\r\n" {
						break
					}
				}
				_, _ = io.WriteString(conn, answer)
			}()
		}
	}()
	return listener.Addr().String()
}

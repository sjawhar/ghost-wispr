package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	interfaces "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/interfaces"
)

// An answer to the live socket's handshake that net/http cannot read fails
// the dial without quoting it: its text is Deepgram's, or whoever answered.
func TestAMalformedDeepgramHandshakeAnswerQuotesNothing(t *testing.T) {
	const planted = "quokkaplantedroomword"
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
				_, _ = io.WriteString(conn, "HTTP/1.1 "+planted+"400 Bad\r\n\r\n")
			}()
		}
	}()

	_, err = dialDeepgramLive(context.Background(), "test-key",
		&interfaces.ClientOptions{Host: "ws://" + listener.Addr().String()},
		&interfaces.LiveTranscriptionOptions{Model: "nova-3", Language: "en-US", Encoding: "linear16", SampleRate: 16000, Channels: 1},
		transcriptCallback{}, time.Second)
	if err == nil || strings.Contains(err.Error(), planted) {
		t.Fatalf("dial = %v, want a failure that quotes nothing the server sent", err)
	}
}

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// A replacement or shutdown must cancel a reader blocked behind the audio
// consumer, not merely close the network connection it is no longer reading.
func TestReplacingAnAudioSocketReleasesItsBlockedReader(t *testing.T) {
	for _, replace := range []bool{false, true} {
		name := "shutdown"
		if replace {
			name = "replacement"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			parsed := make(chan struct{}, 2)
			finished := make(chan struct{}, 2)
			in := make(chan audioIn)
			sock := newAudioSocket([]byte(audiosocketKey), in, func() time.Time {
				parsed <- struct{}{}
				return audiosocketNow
			})
			sock.Register("nonce-1")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sock.ServeHTTP(w, r.WithContext(ctx))
				finished <- struct{}{}
			}))
			defer srv.Close()
			defer sock.closeConn()
			url := "ws" + strings.TrimPrefix(srv.URL, "http") + audioSocketPath + audiosocketSecret1
			first, _, err := websocket.DefaultDialer.Dial(url, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = first.Close() }()
			if err := first.WriteMessage(websocket.TextMessage, []byte(`{"trigger":"realtime_audio.mixed","data":{"chunk":"AQACAA=="}}`)); err != nil {
				t.Fatal(err)
			}
			select {
			case <-parsed:
			case <-time.After(time.Second):
				t.Fatal("the first socket never decoded its audio")
			}
			if replace {
				second, _, err := websocket.DefaultDialer.Dial(url, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = second.Close() }()
			} else {
				sock.closeConn()
			}
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Fatal("the old audio reader remained blocked after its connection closed")
			}
			select {
			case a := <-in:
				t.Fatalf("the closed socket delivered stale audio: %v", a.PCM)
			default:
			}
		})
	}
}

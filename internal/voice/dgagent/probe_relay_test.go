package dgagent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dvonthenen/websocket"
)

// probeRelay passes the probe's session through a local relay to Deepgram
// and records in path, one JSON line per message Deepgram sends, when it
// arrived: each audio frame's size, and each text message's type and role.
// The lines show how far a reply's audio comes ahead of its own text, which
// bounds the hold after an injection's last text (holdTail).
func probeRelay(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	start := time.Now()
	record := func(line map[string]any) {
		line["ms"] = float64(time.Since(start).Microseconds()) / 1000
		b, _ := json.Marshal(line)
		mu.Lock()
		defer mu.Unlock()
		_, _ = f.Write(append(b, '\n'))
	}
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := http.Header{}
		header.Set("Authorization", r.Header.Get("Authorization"))
		server, resp, err := websocket.DefaultDialer.DialContext(r.Context(), DefaultURL, header)
		if resp != nil {
			_ = resp.Body.Close()
		}
		if err != nil {
			http.Error(w, "the relay could not reach Deepgram", http.StatusBadGateway)
			return
		}
		client, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			_ = server.Close()
			return
		}
		closeBoth := func() { _ = client.Close(); _ = server.Close() }
		go func() {
			defer closeBoth()
			for {
				kind, data, err := server.ReadMessage()
				if err != nil {
					return
				}
				if kind == websocket.BinaryMessage {
					record(map[string]any{"down": "audio", "bytes": len(data)})
				} else {
					var m struct{ Type, Role string }
					_ = json.Unmarshal(data, &m)
					record(map[string]any{"down": m.Type, "role": m.Role})
				}
				if client.WriteMessage(kind, data) != nil {
					return
				}
			}
		}()
		defer closeBoth()
		for {
			kind, data, err := client.ReadMessage()
			if err != nil {
				return
			}
			if kind == websocket.TextMessage {
				var m struct{ Type string }
				_ = json.Unmarshal(data, &m)
				record(map[string]any{"up": m.Type})
			}
			if server.WriteMessage(kind, data) != nil {
				return
			}
		}
	}))
	t.Cleanup(func() {
		srv.Close()
		mu.Lock()
		defer mu.Unlock()
		_ = f.Close()
	})
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/agent/converse"
}

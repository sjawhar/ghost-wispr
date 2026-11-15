package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// originTestHandler is the unit's handler with every state-changing hook the
// origin tests call recording that it ran.
func originTestHandler(t *testing.T, called *[]string, cfgStore ...*config.Store) http.Handler {
	t.Helper()
	store := &apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{"s1": {ID: "s1", Status: storage.SessionEnded}},
		segments:       map[string][]transcribe.Segment{},
	}
	record := func(name string) { *called = append(*called, name) }
	handler, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{
		Pause:  func() error { record("pause"); return nil },
		Resume: func() error { record("resume"); return nil },
		EndSession: func(context.Context) error {
			record("end")
			return nil
		},
		DeleteSegmentText: func(context.Context, string, int64, int) error {
			record("delete text")
			return nil
		},
	}, "", nil, cfgStore...)
	if err != nil {
		t.Fatalf("build handler: %v", err)
	}
	return handler
}

// tailnetConfig is a configuration naming the unit's tailnet address.
func tailnetConfig(t *testing.T) *config.Store {
	t.Helper()
	cfgStore, _, err := config.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfgStore.Update(func(c *config.Config) { c.PublicURL = "https://ghost-wispr.example.ts.net" }); err != nil {
		t.Fatal(err)
	}
	return cfgStore
}

// stateChangingRequests are requests a page could send across sites: a
// resume ends someone's pause, the others change or delete transcripts.
var stateChangingRequests = []struct {
	method, path, hook string
}{
	{http.MethodPost, "/api/resume", "resume"},
	{http.MethodPost, "/api/pause", "pause"},
	{http.MethodPost, "/api/session/end", "end"},
	{http.MethodDelete, "/api/sessions/s1/segments/1?mode=text&revision=0", "delete text"},
	{http.MethodPatch, "/api/sessions/s1", ""},
	{http.MethodDelete, "/api/sessions/s1", ""},
	{http.MethodPut, "/api/sessions/s1", ""},
}

// TestForeignOriginCannotChangeState fails if a page on another site, open in
// a tailnet member's browser, can end a pause or change a transcript: every
// POST, PATCH, PUT and DELETE whose Origin is not the unit's own is refused
// with 403 before any handler runs.
func TestForeignOriginCannotChangeState(t *testing.T) {
	for _, origin := range []string{"https://evil.example", "http://evil.example", "null", "https://example.com.evil.example", "http://example.com:8081"} {
		for _, request := range stateChangingRequests {
			var called []string
			handler := originTestHandler(t, &called)
			r := httptest.NewRequest(request.method, request.path, strings.NewReader(`{"title":"x"}`))
			r.Header.Set("Origin", origin)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Errorf("%s %s from Origin %q = %d, want 403: %s", request.method, request.path, origin, w.Code, w.Body.String())
			}
			if len(called) != 0 {
				t.Errorf("%s %s from Origin %q ran %v", request.method, request.path, origin, called)
			}
		}
	}
}

// TestOwnOriginCanChangeState fails if the origin check refuses the unit's own
// page, which sends its own origin with every POST, PATCH and DELETE, or a
// client that is not a browser page and sends no Origin at all.
func TestOwnOriginCanChangeState(t *testing.T) {
	for _, origin := range []string{"http://example.com", "https://example.com", "HTTP://EXAMPLE.COM", ""} {
		for _, request := range stateChangingRequests {
			if request.hook == "" {
				continue
			}
			var called []string
			handler := originTestHandler(t, &called)
			r := httptest.NewRequest(request.method, request.path, nil)
			if origin != "" {
				r.Header.Set("Origin", origin)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code == http.StatusForbidden {
				t.Errorf("%s %s from Origin %q was refused: %s", request.method, request.path, origin, w.Body.String())
			}
			if len(called) != 1 || called[0] != request.hook {
				t.Errorf("%s %s from Origin %q ran %v, want [%s]", request.method, request.path, origin, called, request.hook)
			}
		}
	}
}

// TestSafeRequestsIgnoreOrigin fails if the origin check refuses a GET, which
// changes nothing; the WebSocket upgrade is checked on its own.
func TestSafeRequestsIgnoreOrigin(t *testing.T) {
	var called []string
	handler := originTestHandler(t, &called)
	r := httptest.NewRequest(http.MethodGet, "/api/sessions/s1", nil)
	r.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET from a foreign origin = %d, want 200: %s", w.Code, w.Body.String())
	}
}

// TestForeignOriginCannotOpenWebSocket fails if a page on another site can
// open /ws and read the live transcript as it is spoken.
func TestForeignOriginCannotOpenWebSocket(t *testing.T) {
	var called []string
	server := httptest.NewServer(originTestHandler(t, &called))
	t.Cleanup(server.Close)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	for _, origin := range []string{"https://evil.example", "null", "http://evil.example"} {
		conn, response, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": {origin}})
		if err == nil {
			_ = conn.Close()
			t.Fatalf("WebSocket from Origin %q opened", origin)
		}
		if response == nil || response.StatusCode != http.StatusForbidden {
			t.Fatalf("WebSocket from Origin %q: response %v, err %v; want 403", origin, response, err)
		}
	}

	conn, response, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": {server.URL}})
	if err != nil {
		t.Fatalf("WebSocket from the unit's own origin %q: %v (response %v)", server.URL, err, response)
	}
	_ = conn.Close()
}

// TestTailnetOriginIsTheUnitsOwn fails if the unit refuses its own page at
// its tailnet address when a proxy in front of it passes another Host, or
// accepts that address under another scheme.
func TestTailnetOriginIsTheUnitsOwn(t *testing.T) {
	cases := []struct {
		origin string
		want   bool
	}{
		{"https://ghost-wispr.example.ts.net", true},
		{"https://GHOST-WISPR.example.ts.net:443", true},
		{"http://ghost-wispr.example.ts.net", false},
		{"https://ghost-wispr.example.ts.net:8443", false},
		{"https://other.example.ts.net", false},
	}
	for _, tc := range cases {
		var called []string
		handler := originTestHandler(t, &called, tailnetConfig(t))
		r := httptest.NewRequest(http.MethodPost, "/api/resume", nil)
		r.Host = "127.0.0.1:8080"
		r.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if allowed := w.Code != http.StatusForbidden; allowed != tc.want {
			t.Errorf("POST /api/resume from Origin %q via Host 127.0.0.1:8080 = %d, want allowed %t", tc.origin, w.Code, tc.want)
		}
	}

	var called []string
	server := httptest.NewServer(originTestHandler(t, &called, tailnetConfig(t)))
	t.Cleanup(server.Close)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": {"https://ghost-wispr.example.ts.net"}})
	if err != nil {
		t.Fatalf("WebSocket from the tailnet address: %v (response %v)", err, response)
	}
	_ = conn.Close()
	if _, response, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": {"http://ghost-wispr.example.ts.net"}}); err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("WebSocket from the tailnet address over http: response %v, err %v; want 403", response, err)
	}
}

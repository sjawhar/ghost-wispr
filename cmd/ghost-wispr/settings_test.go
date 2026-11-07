package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// Two settings changes saved together (from the settings page: a silence
// timeout of 10s, then 20s) run their callbacks at the same time. The app's
// running settings (summarizer, batch transcriber, log level, Envoy, silence
// timeout) followed whichever callback finished last, which was not always
// the last change saved. They now follow the last change saved.
func TestSettingsFollowTheLastOfTwoConcurrentChanges(t *testing.T) {
	cfgStore, _, err := config.NewStore("")
	if err != nil {
		t.Fatalf("config store: %v", err)
	}
	db, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var mu sync.Mutex
	applied := ""
	applying, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	onConfigChange(cfgStore, func(cfg *config.Config) {
		if cfg.SilenceTimeout == "10s" {
			once.Do(func() { close(applying) })
			<-release // applying a settings change can be slow (an API client)
		}
		mu.Lock()
		defer mu.Unlock()
		applied = cfg.SilenceTimeout
	})
	h, err := server.Handler(fstest.MapFS{"index.html": {Data: []byte("ok")}}, server.NewHub(), db, &server.ControlHooks{}, "", nil, cfgStore)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	patch := func(body string) <-chan struct{} {
		return goDone(func() {
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodPatch, "/api/config", strings.NewReader(body)))
			if rr.Code != http.StatusOK {
				t.Errorf("PATCH %s: %d %s", body, rr.Code, rr.Body.String())
			}
		})
	}

	first := patch(`{"silence_timeout":"10s"}`)
	<-applying
	second := patch(`{"silence_timeout":"20s"}`)
	for deadline := time.Now().Add(testWait); cfgStore.Get().SilenceTimeout != "20s"; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the second change was never saved")
		}
	}
	time.Sleep(20 * time.Millisecond) // the second change's callback runs, if it can
	close(release)
	if !returnedWithin(first, testWait) || !returnedWithin(second, testWait) {
		t.Fatal("settings changes still running")
	}

	mu.Lock()
	defer mu.Unlock()
	if applied != "20s" {
		t.Errorf("the app runs with silence timeout %q, but the last change saved was 20s", applied)
	}
}

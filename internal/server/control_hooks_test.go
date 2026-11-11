package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// TestPauseControlFailureIsReported fails if a caller is told that a pause is
// durable after its state write fails.
func TestPauseControlFailureIsReported(t *testing.T) {
	store := &apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}
	statusChanged := false
	handler, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{
		Pause:           func() error { return errors.New("write capture state") },
		OnStatusChanged: func(paused bool) { statusChanged = paused },
	}, "", nil)
	if err != nil {
		t.Fatalf("build handler: %v", err)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/pause", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("pause failure status = %d, want %d: %s", response.Code, http.StatusInternalServerError, response.Body.String())
	}
	if !statusChanged {
		t.Fatal("pause failure did not notify clients that capture stopped")
	}
}

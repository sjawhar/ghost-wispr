package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

func TestDeleteSegmentAPIRefusesWhileASpliceIsPending(t *testing.T) {
	store := &apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{"s1": {ID: "s1", Status: storage.SessionEnded}},
		segments:       map[string][]transcribe.Segment{},
	}
	hook := func(context.Context, string, int64) error { return storage.ErrSegmentSplicePending }
	handler, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{DeleteSegment: hook}, "", nil)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/sessions/s1/segments/42", nil))
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "previous segment deletion") {
		t.Fatalf("status = %d body=%s, want 409 naming the pending deletion", response.Code, response.Body.String())
	}
}

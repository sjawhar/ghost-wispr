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

type restoreInProgressError struct{}

func (restoreInProgressError) Error() string { return "recording restore in progress" }

func (restoreInProgressError) RecordingRestoreInProgress() {}

func TestDeleteSegmentAPIRefusesWhileASpliceIsPending(t *testing.T) {
	store := &apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{"s1": {ID: "s1", Status: storage.SessionEnded, RefinementStatus: storage.RefinementCompleted, RefinedRevision: 1}},
		segments:       map[string][]transcribe.Segment{},
	}
	hook := func(context.Context, string, int64, int) error { return storage.ErrSegmentSplicePending }
	handler, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{DeleteSegment: hook, DeleteSegmentText: hook}, "", nil)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	for _, mode := range []string{"text", "text_and_audio"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/sessions/s1/segments/42?mode="+mode+"&revision=1", nil))
		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "A previous segment deletion is still being recovered. Try again after it completes.") {
			t.Fatalf("mode %s: status = %d body=%s, want 409 naming the pending deletion", mode, response.Code, response.Body.String())
		}
	}
}

func TestDeleteSegmentAPIRefusesWhileRecordingRestores(t *testing.T) {
	store := &apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{"s1": {ID: "s1", Status: storage.SessionEnded, RefinementStatus: storage.RefinementCompleted, RefinedRevision: 1}},
		segments:       map[string][]transcribe.Segment{},
	}
	hook := func(context.Context, string, int64, int) error { return restoreInProgressError{} }
	handler, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{DeleteSegment: hook, DeleteSegmentText: hook}, "", nil)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/sessions/s1/segments/42?mode=text_and_audio&revision=1", nil))
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "Restoring the recording; try again shortly.") {
		t.Fatalf("status = %d body=%s, want 409 naming the recording restore", response.Code, response.Body.String())
	}
}

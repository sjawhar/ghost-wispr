package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

func TestRetryRefinementRespondsBeforeSlowRecordingRestore(t *testing.T) {
	store := &apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions: map[string]storage.Session{
			"s1": {ID: "s1", Status: storage.SessionEnded, RefinementStatus: storage.RefinementNoAudio},
		},
		segments: map[string][]transcribe.Segment{},
	}
	restoring := make(chan struct{})
	releaseRestore := make(chan struct{})
	refined := make(chan struct{})
	handler, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{
		RetryRefinement: func(context.Context, string) error {
			close(restoring)
			<-releaseRestore
			close(refined)
			return nil
		},
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	responded := make(chan struct{})
	go func() {
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/sessions/s1/retry-refinement", nil))
		close(responded)
	}()
	<-restoring

	select {
	case <-responded:
		if response.Code != http.StatusAccepted {
			t.Fatalf("retry refinement response = %d %s, want 202", response.Code, response.Body.String())
		}
	case <-time.After(100 * time.Millisecond):
		close(releaseRestore)
		<-responded
		t.Fatal("retry refinement waited for a slow recording restore")
	}

	close(releaseRestore)
	select {
	case <-refined:
	case <-time.After(time.Second):
		t.Fatal("retry refinement did not run after restoring the recording")
	}
}

func TestRetryRefinementReturnsRecordingAvailabilityError(t *testing.T) {
	store := &apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions: map[string]storage.Session{
			"s1": {ID: "s1", Status: storage.SessionEnded, RefinementStatus: storage.RefinementNoAudio},
		},
		segments: map[string][]transcribe.Segment{},
	}
	retried := make(chan struct{}, 1)
	handler, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{
		RefinementRecordingAvailable: func(context.Context, string) error {
			return errors.New("the recording was removed from this unit and its Drive folder is deleted")
		},
		RetryRefinement: func(context.Context, string) error {
			retried <- struct{}{}
			return nil
		},
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/sessions/s1/retry-refinement", nil))
	if response.Code != http.StatusConflict {
		t.Fatalf("retry refinement unavailable recording = %d %s, want 409", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "Drive folder is deleted") {
		t.Fatalf("retry refinement unavailable recording = %s, want the availability reason", response.Body.String())
	}
	select {
	case <-retried:
		t.Fatal("retry refinement started despite no available recording")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestRetryRefinementRefusesNeverRecordedSession(t *testing.T) {
	store := &apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions: map[string]storage.Session{
			"s1": {ID: "s1", Status: storage.SessionEnded, RefinementStatus: storage.RefinementNoAudio},
		},
		segments: map[string][]transcribe.Segment{},
	}
	retried := make(chan struct{}, 1)
	handler, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{
		RefinementRecordingAvailable: func(context.Context, string) error {
			return errors.New("no recording is available on this unit or Drive")
		},
		RetryRefinement: func(context.Context, string) error {
			retried <- struct{}{}
			return nil
		},
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/sessions/s1/retry-refinement", nil))
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "no recording is available") {
		t.Fatalf("retry refinement for a never-recorded session = %d %s, want 409 naming the missing recording", response.Code, response.Body.String())
	}
	select {
	case <-retried:
		t.Fatal("retry refinement started for a never-recorded session")
	case <-time.After(100 * time.Millisecond):
	}
}

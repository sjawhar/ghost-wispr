package gdrive

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/api/googleapi"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

type fakeUploader struct {
	folderID string
	err      error
}

func (f *fakeUploader) Upload(_ context.Context, _ string, _ []SyncFile) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.folderID, nil
}

type syncStateUpdate struct {
	state         string
	driveFolderID string
	retryCount    int
	errorMessage  string
	hasAttempt    bool
}

type fakeOrchestratorStore struct {
	session storage.Session

	segments []transcribe.Segment
	updates  []syncStateUpdate
}

func (f *fakeOrchestratorStore) GetSession(id string) (storage.Session, error) {
	if id != f.session.ID {
		return storage.Session{}, errors.New("not found")
	}
	return f.session, nil
}

func (f *fakeOrchestratorStore) GetSegments(_ string) ([]transcribe.Segment, error) {
	return f.segments, nil
}

func (f *fakeOrchestratorStore) UpdateSyncStatus(_, _, _ string) error {
	return nil
}

func (f *fakeOrchestratorStore) UpdateSyncState(_ string, state, driveFolderID string, retryCount int, lastSyncAttempt *time.Time, errorMessage string) error {
	f.updates = append(f.updates, syncStateUpdate{
		state:         state,
		driveFolderID: driveFolderID,
		retryCount:    retryCount,
		errorMessage:  errorMessage,
		hasAttempt:    lastSyncAttempt != nil,
	})
	f.session.SyncState = state
	f.session.RetryCount = retryCount
	f.session.ErrorMessage = errorMessage
	f.session.GDriveFolderID = driveFolderID
	f.session.LastSyncAttempt = lastSyncAttempt
	return nil
}

func TestOrchestratorSyncSessionSuccessTransitions(t *testing.T) {
	started := time.Date(2026, 3, 23, 10, 0, 0, 0, time.UTC)
	store := &fakeOrchestratorStore{
		session: storage.Session{
			ID:         "session-1",
			Title:      "Test session",
			StartedAt:  started,
			Summary:    "summary",
			SyncState:  storage.SyncStatePending,
			RetryCount: 2,
		},
		segments: []transcribe.Segment{{Speaker: 0, Text: "hello", Timestamp: started}},
	}
	uploader := &fakeUploader{folderID: "folder-123"}

	o := NewOrchestratorWithUploader(uploader, store)
	if err := o.SyncSession(context.Background(), "session-1"); err != nil {
		t.Fatalf("sync session: %v", err)
	}

	if len(store.updates) != 2 {
		t.Fatalf("expected 2 sync state updates, got %d", len(store.updates))
	}
	if store.updates[0].state != storage.SyncStateSyncing {
		t.Fatalf("expected first state %q, got %q", storage.SyncStateSyncing, store.updates[0].state)
	}
	if store.updates[1].state != storage.SyncStateSynced {
		t.Fatalf("expected second state %q, got %q", storage.SyncStateSynced, store.updates[1].state)
	}
	if store.updates[1].driveFolderID != "folder-123" {
		t.Fatalf("expected drive folder id folder-123, got %q", store.updates[1].driveFolderID)
	}
}

func TestOrchestratorSyncSessionSchedulesRetryOnFailure(t *testing.T) {
	started := time.Date(2026, 3, 23, 10, 0, 0, 0, time.UTC)
	store := &fakeOrchestratorStore{
		session: storage.Session{
			ID:         "session-2",
			Title:      "Retry session",
			StartedAt:  started,
			Summary:    "summary",
			SyncState:  storage.SyncStatePending,
			RetryCount: 0,
		},
		segments: []transcribe.Segment{{Speaker: 0, Text: "hello", Timestamp: started}},
	}
	uploader := &fakeUploader{err: errors.New("temporary upload error")}

	o := NewOrchestratorWithUploader(uploader, store)
	err := o.SyncSession(context.Background(), "session-2")
	if err == nil {
		t.Fatal("expected sync session to return retryable error")
	}

	if len(store.updates) != 3 {
		t.Fatalf("expected 3 updates (SYNCING->FAILED->RETRY_SCHEDULED), got %d", len(store.updates))
	}
	if store.updates[2].state != storage.SyncStateRetryScheduled {
		t.Fatalf("expected final state %q, got %q", storage.SyncStateRetryScheduled, store.updates[2].state)
	}
	if store.updates[2].retryCount != 1 {
		t.Fatalf("expected retry_count=1, got %d", store.updates[2].retryCount)
	}
	if !store.updates[2].hasAttempt {
		t.Fatal("expected retry schedule to record last_sync_attempt")
	}
}

func TestOrchestratorSyncSessionFailsPermanentlyAfterRetryExhaustion(t *testing.T) {
	started := time.Date(2026, 3, 23, 10, 0, 0, 0, time.UTC)
	store := &fakeOrchestratorStore{
		session: storage.Session{
			ID:         "session-3",
			Title:      "Retry exhausted session",
			StartedAt:  started,
			Summary:    "summary",
			SyncState:  storage.SyncStateRetryScheduled,
			RetryCount: 5,
		},
		segments: []transcribe.Segment{{Speaker: 0, Text: "hello", Timestamp: started}},
	}
	uploader := &fakeUploader{err: errors.New("persistent upload error")}

	o := NewOrchestratorWithUploader(uploader, store)
	err := o.SyncSession(context.Background(), "session-3")
	if err == nil {
		t.Fatal("expected sync session to fail")
	}

	if len(store.updates) < 2 {
		t.Fatalf("expected at least 2 updates, got %d", len(store.updates))
	}
	last := store.updates[len(store.updates)-1]
	if last.state != storage.SyncStateFailed {
		t.Fatalf("expected final state %q, got %q", storage.SyncStateFailed, last.state)
	}
	if last.retryCount != 5 {
		t.Fatalf("expected retry_count to remain 5, got %d", last.retryCount)
	}
}

func TestOrchestratorSyncSessionSoftDeletesRemoteDeletion(t *testing.T) {
	started := time.Date(2026, 3, 23, 10, 0, 0, 0, time.UTC)
	store := &fakeOrchestratorStore{
		session: storage.Session{
			ID:        "session-4",
			Title:     "Remote deleted session",
			StartedAt: started,
			Summary:   "summary",
			SyncState: storage.SyncStatePending,
		},
		segments: []transcribe.Segment{{Speaker: 0, Text: "hello", Timestamp: started}},
	}
	uploader := &fakeUploader{err: &googleapi.Error{Code: 404}}

	o := NewOrchestratorWithUploader(uploader, store)
	err := o.SyncSession(context.Background(), "session-4")
	if err == nil {
		t.Fatal("expected sync session to return remote deletion error")
	}

	last := store.updates[len(store.updates)-1]
	if last.state != storage.SyncStateRemoteDeleted {
		t.Fatalf("expected final state %q, got %q", storage.SyncStateRemoteDeleted, last.state)
	}
}

func TestOrchestratorSyncSessionAlreadySyncedIsNoop(t *testing.T) {
	started := time.Date(2026, 3, 23, 10, 0, 0, 0, time.UTC)
	store := &fakeOrchestratorStore{
		session: storage.Session{
			ID:        "session-already-synced",
			Title:     "Already synced session",
			StartedAt: started,
			Summary:   "summary",
			SyncState: storage.SyncStateSynced,
		},
		segments: []transcribe.Segment{{Speaker: 0, Text: "hello", Timestamp: started}},
	}
	uploader := &fakeUploader{folderID: "folder-xyz"}

	o := NewOrchestratorWithUploader(uploader, store)
	if err := o.SyncSession(context.Background(), "session-already-synced"); err != nil {
		t.Fatalf("expected re-syncing an already-synced session to be a no-op, got error: %v", err)
	}

	if len(store.updates) != 0 {
		t.Fatalf("expected no sync state updates for already-synced session, got %d", len(store.updates))
	}
}

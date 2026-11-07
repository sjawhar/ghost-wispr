package gdrive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/api/googleapi"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

type fakeUploader struct {
	folderID string
	err      error
	uploads  int      // new folders created
	into     []string // folders uploaded into
}

// Upload returns the folder it created even when an upload into it failed, as
// the real Syncer does.
func (f *fakeUploader) Upload(_ context.Context, _ string, _ []SyncFile) (string, error) {
	f.uploads++
	return f.folderID, f.err
}

func (f *fakeUploader) UploadInto(_ context.Context, folderID string, _ []SyncFile) error {
	f.into = append(f.into, folderID)
	return f.err
}

// A session synced before has its Drive folder: syncing it again (after the
// store reopened it to upload a merged session's audio) adds the missing
// files to that folder instead of creating a second one.
func TestOrchestratorResyncsIntoTheSessionsFolder(t *testing.T) {
	started := time.Date(2026, 3, 23, 10, 0, 0, 0, time.UTC)
	store := &fakeOrchestratorStore{session: storage.Session{
		ID: "session-1-merged", Title: "Merged", StartedAt: started, Summary: "summary",
		SyncState: storage.SyncStatePending, GDriveFolderID: "folder-1",
	}}
	uploader := &fakeUploader{folderID: "folder-new"}

	if err := NewOrchestratorWithUploader(uploader, store).SyncSession(context.Background(), "session-1-merged"); err != nil {
		t.Fatalf("sync session: %v", err)
	}
	if uploader.uploads != 0 || len(uploader.into) != 1 || uploader.into[0] != "folder-1" {
		t.Errorf("created %d new folders, uploaded into %v; want none and [folder-1]", uploader.uploads, uploader.into)
	}
	if last := store.updates[len(store.updates)-1]; last.state != storage.SyncStateSynced || last.driveFolderID != "folder-1" {
		t.Errorf("last sync state %q in folder %q, want %q in folder-1", last.state, last.driveFolderID, storage.SyncStateSynced)
	}
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

// MarkSynced records a synced update while unchanged reports true for the
// session as it is, as the real store does.
func (f *fakeOrchestratorStore) MarkSynced(_, driveFolderID string, unchanged func(storage.Session, []transcribe.Segment) bool) (bool, error) {
	if !unchanged(f.session, f.segments) {
		return false, nil
	}
	return true, f.UpdateSyncState("", storage.SyncStateSynced, driveFolderID, 0, nil, "")
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

// Review of #37, N4: a session one of whose recordings is missing from disk
// (its disk not mounted, say) is not marked synced: the files there are
// uploaded, the sync fails and is retried, and once the recording is back the
// retry adds it to the same folder and only then marks the session synced.
// Marked synced without it, GC (gated on sync) could remove that recording
// once it came back, though Drive never had it.
func TestOrchestratorDoesNotMarkSyncedWhileARecordingIsMissing(t *testing.T) {
	dir := t.TempDir()
	present, missing := filepath.Join(dir, "a.mp3"), filepath.Join(dir, "b.mp3")
	if err := os.WriteFile(present, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &fakeOrchestratorStore{session: storage.Session{
		ID: "session-merged", Title: "Merged", StartedAt: time.Date(2026, 3, 23, 10, 0, 0, 0, time.UTC),
		Summary: "summary", SyncState: storage.SyncStatePending, AudioPath: present + "," + missing,
	}}
	uploader := &fakeUploader{folderID: "folder-1"}
	o := NewOrchestratorWithUploader(uploader, store)

	if err := o.SyncSession(context.Background(), "session-merged"); err == nil {
		t.Error("synced a session with a recording missing from disk")
	}
	if last := store.updates[len(store.updates)-1]; last.state != storage.SyncStateRetryScheduled || last.driveFolderID != "folder-1" {
		t.Errorf("after the sync: %q in folder %q, want %q in folder-1", last.state, last.driveFolderID, storage.SyncStateRetryScheduled)
	}

	if err := os.WriteFile(missing, []byte("audio"), 0o644); err != nil { // the disk is back
		t.Fatal(err)
	}
	if err := o.SyncSession(context.Background(), "session-merged"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if uploader.uploads != 1 || len(uploader.into) != 1 || uploader.into[0] != "folder-1" {
		t.Errorf("created %d folders, uploaded into %v; want one and [folder-1]", uploader.uploads, uploader.into)
	}
	if last := store.updates[len(store.updates)-1]; last.state != storage.SyncStateSynced || last.driveFolderID != "folder-1" {
		t.Errorf("after the retry: %q in folder %q, want %q in folder-1", last.state, last.driveFolderID, storage.SyncStateSynced)
	}
}

// Review of #37, N5: a first sync that created its folder and then failed
// keeps that folder's id, so the retry fills the same folder instead of
// creating a second one.
func TestOrchestratorRetryReusesTheFolderAFailedSyncCreated(t *testing.T) {
	store := &fakeOrchestratorStore{session: storage.Session{
		ID: "session-1", Title: "Test", StartedAt: time.Date(2026, 3, 23, 10, 0, 0, 0, time.UTC),
		Summary: "summary", SyncState: storage.SyncStatePending,
	}}
	uploader := &fakeUploader{folderID: "folder-1", err: errors.New("upload transcript.md: connection reset")}
	o := NewOrchestratorWithUploader(uploader, store)

	if err := o.SyncSession(context.Background(), "session-1"); err == nil {
		t.Fatal("expected the sync to fail")
	}
	if last := store.updates[len(store.updates)-1]; last.state != storage.SyncStateRetryScheduled || last.driveFolderID != "folder-1" {
		t.Errorf("after the failed sync: %q in folder %q, want %q in folder-1", last.state, last.driveFolderID, storage.SyncStateRetryScheduled)
	}

	uploader.err = nil
	if err := o.SyncSession(context.Background(), "session-1"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if uploader.uploads != 1 || len(uploader.into) != 1 || uploader.into[0] != "folder-1" {
		t.Errorf("created %d folders, uploaded into %v; want one and [folder-1]", uploader.uploads, uploader.into)
	}
}

// Review of #37, round 2, note 1: once its automatic retries ran out, a
// session sat FAILED for good: a manual retry (the UI's retry-sync button)
// tried FAILED -> SYNCING, which was refused, while the API answered 200. A
// manual retry now starts from FAILED, and CanSync says up front whether a
// sync can start, so the API can tell the truth.
func TestOrchestratorManualRetryStartsFromFailed(t *testing.T) {
	store := &fakeOrchestratorStore{session: storage.Session{
		ID: "session-1", Title: "Test", StartedAt: time.Date(2026, 3, 23, 10, 0, 0, 0, time.UTC),
		Summary: "summary", SyncState: storage.SyncStateFailed, RetryCount: MaxSyncRetries, GDriveFolderID: "folder-1",
	}}
	uploader := &fakeUploader{folderID: "folder-new"}
	o := NewOrchestratorWithUploader(uploader, store)

	if err := o.CanSync("session-1"); err != nil {
		t.Errorf("CanSync of a FAILED session: %v", err)
	}
	if err := o.SyncSession(context.Background(), "session-1"); err != nil {
		t.Fatalf("manual retry of a FAILED session: %v", err)
	}
	if last := store.updates[len(store.updates)-1]; last.state != storage.SyncStateSynced || last.driveFolderID != "folder-1" {
		t.Errorf("after the manual retry: %q in folder %q, want %q in folder-1", last.state, last.driveFolderID, storage.SyncStateSynced)
	}

	for _, state := range []string{storage.SyncStateSynced, storage.SyncStateRemoteDeleted} {
		store.session.SyncState = state
		if err := o.CanSync("session-1"); err == nil {
			t.Errorf("CanSync of a %s session: no error", state)
		}
	}
	if err := o.CanSync("no-such-session"); err == nil {
		t.Error("CanSync of a missing session: no error")
	}
}

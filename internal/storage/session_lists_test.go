package storage

import (
	"testing"
	"time"
)

func TestSessionListsCarryRefinedRevision(t *testing.T) {
	store := newTestSQLiteStore(t)
	_, _ = segmentTestSession(t, store, "list-session")
	revision, _ := publishRefinedSegmentTestSession(t, store, "list-session", "recording.mp3", "recording-sha")
	syncTo(t, store, "list-session", SyncStateSynced, "folder-1", 0, "")

	for name, list := range map[string]func() ([]Session, error){
		"by date": func() ([]Session, error) {
			return store.GetSessionsByDate("2026-11-11", false)
		},
		"with empty titles": store.GetSessionsWithEmptyTitles,
		"by sync state": func() ([]Session, error) {
			return store.GetSessionsBySyncState(SyncStateSynced)
		},
	} {
		t.Run(name, func(t *testing.T) {
			sessions, err := list()
			if err != nil {
				t.Fatal(err)
			}
			if len(sessions) != 1 {
				t.Fatalf("listed sessions = %d, want 1", len(sessions))
			}
			if got := sessions[0].RefinedRevision; got != revision {
				t.Errorf("listed refined revision = %d, want %d", got, revision)
			}
		})
	}
}

func TestSessionListsCarryRestoredAt(t *testing.T) {
	store := newTestSQLiteStore(t)
	started := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	ended := started.Add(time.Minute)
	if err := store.ImportSession(&Session{
		ID:             "restored-list-session",
		StartedAt:      started,
		EndedAt:        &ended,
		Status:         SessionEnded,
		SummaryStatus:  SummaryCompleted,
		SyncStatus:     SyncSynced,
		GDriveFolderID: "folder-1",
	}, nil); err != nil {
		t.Fatal(err)
	}
	for name, list := range map[string]func() ([]Session, error){
		"by date": func() ([]Session, error) {
			return store.GetSessionsByDate("2026-11-11", false)
		},
		"with empty titles": store.GetSessionsWithEmptyTitles,
		"by sync state": func() ([]Session, error) {
			return store.GetSessionsBySyncState(SyncStateSynced)
		},
	} {
		t.Run(name, func(t *testing.T) {
			sessions, err := list()
			if err != nil {
				t.Fatal(err)
			}
			if len(sessions) != 1 || sessions[0].RestoredAt == "" {
				t.Fatalf("listed restored sessions = %+v, want one with restored_at", sessions)
			}
		})
	}
}

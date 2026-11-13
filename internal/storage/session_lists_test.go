package storage

import "testing"

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

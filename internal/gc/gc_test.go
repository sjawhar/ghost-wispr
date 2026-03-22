package gc

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

type mockStore struct {
	sessions   map[string]storage.Session
	gcEligible map[int][]string
	deletedIDs []string
}

func (m *mockStore) GetGCEligibleSessions(maxAgeDays int, syncGated bool) ([]string, error) {
	if ids, ok := m.gcEligible[maxAgeDays]; ok {
		return ids, nil
	}
	return nil, nil
}

func (m *mockStore) GetSession(id string) (storage.Session, error) {
	if s, ok := m.sessions[id]; ok {
		return s, nil
	}
	return storage.Session{}, os.ErrNotExist
}

func (m *mockStore) DeleteSession(id string) error {
	m.deletedIDs = append(m.deletedIDs, id)
	return nil
}

func TestGCDeletesSyncedOldSessions(t *testing.T) {
	dir := t.TempDir()
	audioPath := filepath.Join(dir, "test.mp3")
	if err := os.WriteFile(audioPath, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := &mockStore{
		sessions: map[string]storage.Session{
			"s1": {ID: "s1", AudioPath: audioPath},
		},
		gcEligible: map[int][]string{
			30: {"s1"},
		},
	}

	collector := New(store, Config{
		MaxAgeDays:     30,
		MaxAudioSizeMB: 1024,
		SyncGated:      true,
	})

	deleted, err := collector.Run()
	if err != nil {
		t.Fatalf("gc run: %v", err)
	}
	if len(deleted) != 1 || deleted[0] != "s1" {
		t.Errorf("expected [s1] deleted, got %v", deleted)
	}
	if _, err := os.Stat(audioPath); !os.IsNotExist(err) {
		t.Error("expected audio file to be deleted")
	}
	if len(store.deletedIDs) != 1 {
		t.Error("expected store.DeleteSession called")
	}
}

func TestGCSkipsSessionWithNoAudio(t *testing.T) {
	store := &mockStore{
		sessions: map[string]storage.Session{
			"s1": {ID: "s1", AudioPath: ""},
		},
		gcEligible: map[int][]string{
			30: {"s1"},
		},
	}

	collector := New(store, Config{
		MaxAgeDays:     30,
		MaxAudioSizeMB: 1024,
		SyncGated:      true,
	})

	deleted, err := collector.Run()
	if err != nil {
		t.Fatalf("gc run: %v", err)
	}
	if len(deleted) != 1 {
		t.Fatalf("expected 1 deleted (even without audio), got %d", len(deleted))
	}
}

func TestGCResolvesRelativePath(t *testing.T) {
	dir := t.TempDir()
	audioPath := filepath.Join(dir, "session.mp3")
	if err := os.WriteFile(audioPath, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := &mockStore{
		sessions: map[string]storage.Session{
			"s1": {ID: "s1", AudioPath: "session.mp3"},
		},
		gcEligible: map[int][]string{
			30: {"s1"},
		},
	}

	collector := New(store, Config{
		MaxAgeDays:     30,
		MaxAudioSizeMB: 1024,
		SyncGated:      true,
		AudioDir:       dir,
	})

	deleted, err := collector.Run()
	if err != nil {
		t.Fatalf("gc run: %v", err)
	}
	if len(deleted) != 1 {
		t.Fatalf("expected 1 deleted, got %d", len(deleted))
	}
	if _, err := os.Stat(audioPath); !os.IsNotExist(err) {
		t.Error("expected audio file at resolved path to be deleted")
	}
}

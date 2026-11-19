package concierge

import (
	"path/filepath"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

func TestTheEnvoySessionKeepsItsIDAndResumePointAcrossStarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unit.db")
	store, err := storage.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := LoadEnvoyState(store)
	if err != nil {
		t.Fatal(err)
	}
	if envoy.ValidSessionID(first.SessionID) != nil || first.Resume != nil {
		t.Fatalf("first start = %+v", first)
	}
	if err := first.SaveResume(envoy.ResumePoint{StreamID: "stream-A", Seq: 41}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = storage.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	second, err := LoadEnvoyState(store)
	if err != nil {
		t.Fatal(err)
	}
	if second.SessionID != first.SessionID {
		t.Errorf("session id changed across starts: %q then %q", first.SessionID, second.SessionID)
	}
	if second.Resume == nil || *second.Resume != (envoy.ResumePoint{StreamID: "stream-A", Seq: 41}) {
		t.Errorf("resume point = %+v", second.Resume)
	}
}

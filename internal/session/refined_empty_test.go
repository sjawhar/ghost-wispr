package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

type emptyBatchTranscriber struct{}

func (emptyBatchTranscriber) Transcribe(context.Context, string) (transcribe.BatchResult, error) {
	return transcribe.BatchResult{}, nil
}

func TestEmptyBatchRefinementKeepsLiveSegments(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	if err := os.WriteFile(recording, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession("s", started); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSegment("s", transcribe.Segment{Text: "Live text stays.", StartTime: 0, EndTime: 1, Timestamp: started}); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("s", started.Add(time.Minute), recording); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store, nil, nil, nil, NewDetector(time.Hour), 0)
	manager.SetBatchTranscriber(emptyBatchTranscriber{})
	if err := manager.RefineSession(context.Background(), "s", recording, started); err != nil {
		t.Fatal(err)
	}
	session, err := store.GetSession("s")
	if err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("s")
	if err != nil {
		t.Fatal(err)
	}
	if session.RefinementStatus != storage.RefinementFailed || len(segments) != 1 || segments[0].Text != "Live text stays." {
		t.Fatalf("empty refinement = status %q segments %#v, want failed and live segment retained", session.RefinementStatus, segments)
	}
}

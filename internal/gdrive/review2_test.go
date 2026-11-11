package gdrive

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

func round2Session(t *testing.T, store *storage.SQLiteStore, id string, texts []string) (recording string, segments []transcribe.Segment) {
	t.Helper()
	started := time.Date(2026, 11, 6, 15, 0, 0, 0, time.UTC)
	recording = filepath.Join(t.TempDir(), id+".mp3")
	if err := os.WriteFile(recording, []byte("UNSPLICED audio with the secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(id, started); err != nil {
		t.Fatal(err)
	}
	for i, text := range texts {
		if err := store.AppendSegment(id, transcribe.Segment{Text: text, StartTime: float64(i), EndTime: float64(i + 1), Timestamp: started}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.EndSession(id, started.Add(time.Minute), recording); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Canonicalize(id); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSummary(id, "Standup", "summary", storage.SummaryCompleted, "default", "{}"); err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments(id)
	if err != nil || len(segments) != len(texts) {
		t.Fatalf("segments %v %v", segments, err)
	}
	return recording, segments
}

func TestReview2NoFolderSyncBeforeSpliceRename(t *testing.T) {
	store := openStore(t)
	const id = "20261106150000"
	recording, segments := round2Session(t, store, id, []string{"alpha stays.", "bravo secret."})
	wire, syncer := newWireDrive(t)
	o := NewOrchestrator(syncer, store)
	span := storage.DeletedSpan{SessionID: id, AudioPath: recording, Start: 1, End: 2, SHA256Before: "b", SHA256After: "a"}
	if err := store.DeleteSegment(id, segments[1].ID, span); err != nil {
		t.Fatalf("DeleteSegment: %v", err)
	}
	_ = o.SyncSession(context.Background(), id)
	if err := os.WriteFile(recording, []byte("spliced clean audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSpliceComplete(&span); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET refinement_status = ?, summary_status = ?, summary = 'clean', title = 'Clean', sync_state = ? WHERE id = ?`,
		storage.RefinementCompleted, storage.SummaryCompleted, storage.SyncStatePending, id); err != nil {
		t.Fatal(err)
	}
	wire.allowDeletes = true
	_ = o.SyncSession(context.Background(), id)
	sess, _ := store.GetSession(id)
	var audio string
	for name, copies := range wire.in(sess.GDriveFolderID) {
		if strings.HasPrefix(name, "audio-") {
			for _, c := range copies {
				audio += string(c.Content) + "|"
			}
		}
	}
	if strings.Contains(audio, "UNSPLICED") {
		t.Errorf("the unspliced recording is on Drive and drive_replace_pending=%v", sess.DriveReplacePending)
	}
}

type generationRaceStore struct {
	*storage.SQLiteStore
	hook func()
}

func (s *generationRaceStore) GetSession(id string) (storage.Session, error) {
	sess, err := s.SQLiteStore.GetSession(id)
	if s.hook != nil {
		hook := s.hook
		s.hook = nil
		hook()
	}
	return sess, err
}

func TestReview2DeletionBetweenSessionAndGenerationReads(t *testing.T) {
	store := openStore(t)
	const id = "20261106160000"
	recording, segments := round2Session(t, store, id, []string{"alpha stays here.", "bravo first secret.", "charlie second secret."})
	wire, syncer := newWireDrive(t)
	o := NewOrchestrator(syncer, store)
	folder := syncedSession(t, store, o, id)
	span := storage.DeletedSpan{SessionID: id, AudioPath: recording, Start: 0, End: 1, SHA256Before: "b", SHA256After: "a"}
	if err := store.DeleteSegment(id, segments[1].ID, span); err != nil {
		t.Fatalf("delete bravo: %v", err)
	}
	if err := store.MarkSpliceComplete(&span); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET refinement_status = ?, summary_status = ?, summary = 'about charlie', title = 'Charlie' WHERE id = ?`,
		storage.RefinementCompleted, storage.SummaryCompleted, id); err != nil {
		t.Fatal(err)
	}
	race := &generationRaceStore{SQLiteStore: store}
	race.hook = func() {
		if err := store.DeleteSegment(id, segments[2].ID, span); err != nil {
			t.Errorf("delete charlie: %v", err)
		}
		if err := store.MarkSpliceComplete(&span); err != nil {
			t.Errorf("mark charlie spliced: %v", err)
		}
	}
	syncer.setReplacementStore(race)
	wire.allowDeletes = true
	_ = o.SyncSession(context.Background(), id)
	sess, _ := store.GetSession(id)
	var transcript string
	for _, f := range wire.in(folder)["transcript.md"] {
		transcript += string(f.Content)
	}
	if strings.Contains(transcript, "charlie second secret") && !sess.DriveReplacePending {
		t.Errorf("Drive transcript holds the second deleted segment and drive_replace_pending was cleared")
	}
}

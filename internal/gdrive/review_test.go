package gdrive

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

func TestReviewTwoRecordingsDeletedBeforeReplacement(t *testing.T) {
	fixture := newReplacementFixture(t)
	other := filepath.Join(filepath.Dir(fixture.recording), "20261106110000.mp3")
	if err := os.WriteFile(other, []byte("other recording"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.DB().Exec(`UPDATE sessions SET audio_path = ?, sync_state = ? WHERE id = ?`,
		fixture.recording+", "+other, storage.SyncStatePending, fixture.id); err != nil {
		t.Fatal(err)
	}
	if err := fixture.orchestrator.SyncSession(context.Background(), fixture.id); err != nil {
		t.Fatalf("sync merged: %v", err)
	}
	firstOld := fixture.oldIDs[2]
	fixture.queue(t, storage.RefinementCompleted, storage.SummaryCompleted)
	if err := os.WriteFile(other, []byte("other recording spliced"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := fixture.store.DB().Exec(`INSERT INTO deleted_segments(session_id, audio_path, start, end, sha256_before, sha256_after, deleted_at, splice_complete)
		VALUES(?, ?, 1, 2, 'before', 'after', ?, 1)`, fixture.id, other, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	fixture.wire.allowDeletes = true
	err := fixture.orchestrator.SyncSession(context.Background(), fixture.id)
	sess, _ := fixture.store.GetSession(fixture.id)
	if !slices.Contains(fixture.wire.deletedIDs, firstOld) && !sess.DriveReplacePending {
		t.Errorf("first spliced recording's old Drive file %s was never deleted, and drive_replace_pending is cleared: %v", firstOld, err)
	}
}

func TestReviewReplaceWithoutAnOldSummary(t *testing.T) {
	store := openStore(t)
	const id = "20261106120000"
	started := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	audioDir := t.TempDir()
	summarizedSession(t, store, audioDir, id, started, "")
	if _, err := store.DB().Exec(`UPDATE sessions SET canonical_transcript = ? WHERE id = ?`, "purple elephant seventeen", id); err != nil {
		t.Fatal(err)
	}
	wire, syncer := newWireDrive(t)
	o := NewOrchestrator(syncer, store)
	folder := syncedSession(t, store, o, id)
	if n := len(wire.in(folder)["summary.md"]); n != 0 {
		t.Fatalf("precondition: %d summary.md on Drive, want none", n)
	}
	oldTranscript := wire.in(folder)["transcript.md"][0].ID
	recording := filepath.Join(audioDir, id+".mp3")
	if err := os.WriteFile(recording, []byte("spliced"), 0o644); err != nil {
		t.Fatal(err)
	}
	setDriveReplacementPending(t, store, id, recording)
	if _, err := store.DB().Exec(`UPDATE sessions SET canonical_transcript = 'clean', refinement_status = ?, summary_status = ?, sync_state = ? WHERE id = ?`,
		storage.RefinementCompleted, storage.SummaryFailed, storage.SyncStatePending, id); err != nil {
		t.Fatal(err)
	}
	wire.allowDeletes = true
	for range 3 {
		_ = o.SyncSession(context.Background(), id)
	}
	if !slices.Contains(wire.deletedIDs, oldTranscript) {
		t.Errorf("old transcript %s holding the deleted words is still on Drive after three attempts", oldTranscript)
	}
}

func TestReviewDeletionBeforeFirstSync(t *testing.T) {
	store := openStore(t)
	const id = "20261106130000"
	started := time.Date(2026, 11, 6, 13, 0, 0, 0, time.UTC)
	audioDir := t.TempDir()
	summarizedSession(t, store, audioDir, id, started, "summary")
	_, syncer := newWireDrive(t)
	o := NewOrchestrator(syncer, store)
	setDriveReplacementPending(t, store, id, filepath.Join(audioDir, id+".mp3"))
	if _, err := store.DB().Exec(`UPDATE sessions SET refinement_status = ?, summary_status = ?, sync_state = ? WHERE id = ?`,
		storage.RefinementCompleted, storage.SummaryCompleted, storage.SyncStatePending, id); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		_ = o.SyncSession(context.Background(), id)
	}
	sess, _ := store.GetSession(id)
	if sess.GDriveFolderID == "" {
		t.Errorf("session never reached Drive: sync_state=%q pending=%v", sess.SyncState, sess.DriveReplacePending)
	}
}

func TestReviewStaleSnapshotClearsNewerDeletion(t *testing.T) {
	store := openStore(t)
	const id = "20261106140000"
	started := time.Date(2026, 11, 6, 14, 0, 0, 0, time.UTC)
	audioDir := t.TempDir()
	recording := filepath.Join(audioDir, id+".mp3")
	if err := os.WriteFile(recording, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(id, started); err != nil {
		t.Fatal(err)
	}
	for i, text := range []string{"alpha stays here.", "bravo first secret.", "charlie second secret."} {
		if err := store.AppendSegment(id, transcribe.Segment{Text: text, StartTime: float64(i), EndTime: float64(i + 1), Timestamp: started}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.EndSession(id, started.Add(time.Minute), recording); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Canonicalize(id); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSummary(id, "Standup", "summary", storage.SummaryCompleted, "default", "{}"); err != nil {
		t.Fatal(err)
	}
	wire, syncer := newWireDrive(t)
	o := NewOrchestrator(syncer, store)
	folder := syncedSession(t, store, o, id)
	segments, err := store.GetSegments(id)
	if err != nil || len(segments) != 3 {
		t.Fatalf("segments %v %v", segments, err)
	}
	span := func() storage.DeletedSpan {
		return storage.DeletedSpan{SessionID: id, AudioPath: recording, Start: 0, End: 1, SHA256Before: "b", SHA256After: "a"}
	}
	if err := store.DeleteSegment(id, segments[1].ID, span()); err != nil {
		t.Fatalf("delete bravo: %v", err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET refinement_status = ?, summary_status = ?, summary = 'about charlie', title = 'Charlie' WHERE id = ?`,
		storage.RefinementCompleted, storage.SummaryCompleted, id); err != nil {
		t.Fatal(err)
	}
	wire.allowDeletes = true

	syncer.mu.Lock()
	done := make(chan error, 1)
	go func() { done <- o.SyncSession(context.Background(), id) }()
	time.Sleep(300 * time.Millisecond)
	if err := store.DeleteSegment(id, segments[2].ID, span()); err != nil {
		t.Fatalf("delete charlie: %v", err)
	}
	syncer.mu.Unlock()
	syncErr := <-done

	sess, _ := store.GetSession(id)
	var transcript string
	for _, f := range wire.in(folder)["transcript.md"] {
		transcript += string(f.Content)
	}
	if strings.Contains(transcript, "charlie second secret") && !sess.DriveReplacePending {
		t.Errorf("Drive transcript still holds the second deleted segment and drive_replace_pending was cleared: %v", syncErr)
	}
}

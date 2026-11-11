package gdrive

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/drive/v3"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// A recording's content hash lets a replacement verify the exact file it
// created before it removes its predecessor.
func TestRecordingUploadCarriesContentHash(t *testing.T) {
	_, syncer := newWireDrive(t)
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	if err := os.WriteFile(recording, []byte("recording"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, _, err := BuildSyncFiles(&SyncSession{ID: "session", Title: "Session"}, nil, recording)
	if err != nil {
		t.Fatalf("BuildSyncFiles: %v", err)
	}
	folder, err := syncer.Upload(context.Background(), "session", files)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	for _, file := range syncerFiles(t, syncer, folder) {
		if file.Name == "audio-recording.mp3" {
			if got := file.AppProperties[contentHashProperty]; got != "3ebb153fb24e4411400e94a9a92b0ec458c3a8473e51e03cd37d4a34c99dfda6" {
				t.Errorf("recording hash = %q, want the SHA-256 of recording", got)
			}
			return
		}
	}
	t.Fatal("recording was not uploaded")
}

func syncerFiles(t *testing.T, syncer *Syncer, folder string) []*drive.File {
	t.Helper()
	files, err := syncer.ListFolderFiles(context.Background(), folder)
	if err != nil {
		t.Fatalf("ListFolderFiles: %v", err)
	}
	return files
}

// TestReplaceCreatesRecordsThenDeletes fails if a deletion updates the three
// old Drive files in place, or if it deletes anything before its replacements
// are uploaded.
func TestReplaceCreatesRecordsThenDeletes(t *testing.T) {
	fixture := newReplacementFixture(t)
	fixture.queue(t, storage.RefinementCompleted, storage.SummaryCompleted)
	fixture.wire.allowDeletes = true
	if err := fixture.orchestrator.SyncSession(context.Background(), fixture.id); err != nil {
		t.Fatalf("replace session files: %v", err)
	}

	if !sameIDs(fixture.wire.deletedIDs, fixture.oldIDs) {
		t.Errorf("deleted %v, want exactly old ids %v", fixture.wire.deletedIDs, fixture.oldIDs)
	}
	rows, err := fixture.store.DriveReplacements(fixture.id)
	if err != nil {
		t.Fatalf("DriveReplacements: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("replacement rows = %d, want three", len(rows))
	}
	for _, row := range rows {
		if row.State != storage.DriveReplacementDeleted {
			t.Errorf("replacement row %+v has state %q, want deleted", row, row.State)
		}
		if slices.Contains(fixture.oldIDs, row.NewID) {
			t.Errorf("replacement %s records old id %s as its new id", row.Name, row.NewID)
		}
	}
	files := fixture.wire.in(fixture.folder)
	for _, name := range []string{"summary.md", "transcript.md", "audio-" + fixture.id + ".mp3"} {
		copies := files[name]
		if len(copies) != 1 {
			t.Errorf("new folder has %d %s files, want one", len(copies), name)
			continue
		}
		if strings.Contains(string(copies[0].Content), "purple elephant seventeen") {
			t.Errorf("new %s retains deleted text", name)
		}
	}
	if got := fixture.wire.find(fixture.folder).Name; got != "2026-11-06-regenerated-title" {
		t.Errorf("folder name = %q, want regenerated title", got)
	}
}

func oldReplacementIDs(t *testing.T, wire *wireDrive, folder, recording string) []string {
	t.Helper()
	files := wire.in(folder)
	var ids []string
	for _, name := range []string{"summary.md", "transcript.md", recording} {
		if copies := files[name]; len(copies) == 1 {
			ids = append(ids, copies[0].ID)
		} else {
			t.Fatalf("before replacement %s has %d copies, want one", name, len(copies))
		}
	}
	return ids
}

func setDriveReplacementPending(t *testing.T, store *storage.SQLiteStore, sessionID, audioPath string) {
	t.Helper()
	if _, err := store.DB().Exec(`DELETE FROM drive_replacements WHERE session_id = ?`, sessionID); err != nil {
		t.Fatalf("clear completed drive replacements: %v", err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET drive_replace_pending = 1 WHERE id = ?`, sessionID); err != nil {
		t.Fatalf("mark replacement pending: %v", err)
	}
	if _, err := store.DB().Exec(`INSERT INTO deleted_segments(session_id, audio_path, start, end, sha256_before, sha256_after, deleted_at)
		VALUES(?, ?, 1, 2, 'before', 'after', ?)`, sessionID, audioPath, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("record deleted segment: %v", err)
	}
}

func sameIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]bool, len(got))
	for _, id := range got {
		seen[id] = true
	}
	for _, id := range want {
		if !seen[id] {
			return false
		}
	}
	return true
}

// TestReplaceResumesWithoutDeletingANewFile fails if a retry ignores the
// uploaded rows and treats its new file ids as old files.
func TestReplaceResumesWithoutDeletingANewFile(t *testing.T) {
	fixture := newReplacementFixture(t)
	fixture.queue(t, storage.RefinementCompleted, storage.SummaryCompleted)
	fixture.wire.allowDeletes = true
	fixture.wire.failUploadAt = fixture.wire.uploads + 3

	if err := fixture.orchestrator.SyncSession(context.Background(), fixture.id); err == nil {
		t.Fatal("replacement succeeded despite its third upload failing")
	}
	if len(fixture.wire.deletedIDs) != 0 {
		t.Fatalf("failed replacement deleted %v before every new file was recorded", fixture.wire.deletedIDs)
	}
	rows, err := fixture.store.DriveReplacements(fixture.id)
	if err != nil {
		t.Fatalf("DriveReplacements after failed upload: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("replacement rows after failed upload = %d, want three", len(rows))
	}
	uploaded, uploading := 0, 0
	for _, row := range rows {
		switch row.State {
		case storage.DriveReplacementUploaded:
			uploaded++
		case storage.DriveReplacementUploading:
			uploading++
		default:
			t.Errorf("row %+v has state %q, want uploaded or uploading", row, row.State)
		}
	}
	if uploaded != 2 || uploading != 1 {
		t.Errorf("failed replacement has %d uploaded and %d uploading rows, want two and one", uploaded, uploading)
	}

	fixture.wire.failUploadAt = 0
	if err := fixture.orchestrator.SyncSession(context.Background(), fixture.id); err != nil {
		t.Fatalf("resume replacement: %v", err)
	}
	if !sameIDs(fixture.wire.deletedIDs, fixture.oldIDs) {
		t.Errorf("resume deleted %v, want old ids %v", fixture.wire.deletedIDs, fixture.oldIDs)
	}
	rows, err = fixture.store.DriveReplacements(fixture.id)
	if err != nil {
		t.Fatalf("DriveReplacements after resume: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("replacement rows after resume = %d, want three", len(rows))
	}
	for _, row := range rows {
		if row.State != storage.DriveReplacementDeleted {
			t.Errorf("row %+v has state %q, want deleted", row, row.State)
		}
		if slices.Contains(fixture.wire.deletedIDs, row.NewID) {
			t.Errorf("retry deleted new file %s for %s", row.NewID, row.Name)
		}
	}
}

type replacementFixture struct {
	store        *storage.SQLiteStore
	wire         *wireDrive
	syncer       *Syncer
	orchestrator *Orchestrator
	id           string
	recording    string
	folder       string
	oldIDs       []string
}

func newReplacementFixture(t *testing.T) *replacementFixture {
	t.Helper()
	store := openStore(t)
	const id = "20261106100000"
	started := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	audioDir := t.TempDir()
	summarizedSession(t, store, audioDir, id, started, "purple elephant seventeen")
	if _, err := store.DB().Exec(`UPDATE sessions SET title = ?, canonical_transcript = ? WHERE id = ?`, "Purple elephant seventeen", "purple elephant seventeen", id); err != nil {
		t.Fatalf("seed deleted text: %v", err)
	}
	wire, syncer := newWireDrive(t)
	orchestrator := NewOrchestrator(syncer, store)
	folder := syncedSession(t, store, orchestrator, id)
	return &replacementFixture{
		store:        store,
		wire:         wire,
		syncer:       syncer,
		orchestrator: orchestrator,
		id:           id,
		recording:    filepath.Join(audioDir, id+".mp3"),
		folder:       folder,
		oldIDs:       oldReplacementIDs(t, wire, folder, "audio-"+id+".mp3"),
	}
}

func (f *replacementFixture) queue(t *testing.T, refinement, summary string) {
	t.Helper()
	if err := os.WriteFile(f.recording, []byte("recording without the deleted phrase"), 0o644); err != nil {
		t.Fatal(err)
	}
	setDriveReplacementPending(t, f.store, f.id, f.recording)
	if _, err := f.store.DB().Exec(`UPDATE sessions
		SET title = ?, summary = ?, canonical_transcript = ?, refinement_status = ?, summary_status = ?, sync_state = ?
		WHERE id = ?`,
		"Regenerated title", "clean summary", "clean transcript", refinement, summary, storage.SyncStatePending, f.id,
	); err != nil {
		t.Fatalf("prepare regenerated session: %v", err)
	}
}

// TestReplaceWaitsForRefinementAndSummary fails if an ordinary sync updates
// Drive before its new transcript and summary are final.
func TestReplaceWaitsForRefinementAndSummary(t *testing.T) {
	fixture := newReplacementFixture(t)
	fixture.queue(t, storage.RefinementCompleted, storage.SummaryRunning)
	fixture.wire.allowDeletes = true

	if err := fixture.orchestrator.SyncSession(context.Background(), fixture.id); err != nil {
		t.Fatalf("sync while summary is pending: %v", err)
	}
	if len(fixture.wire.deletedIDs) != 0 {
		t.Errorf("pending summary deleted %v", fixture.wire.deletedIDs)
	}
	rows, err := fixture.store.DriveReplacements(fixture.id)
	if err != nil {
		t.Fatalf("DriveReplacements while summary is pending: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("pending summary created replacement rows %v", rows)
	}
	if _, err := fixture.store.DB().Exec(`UPDATE sessions SET summary_status = ? WHERE id = ?`, storage.SummaryCompleted, fixture.id); err != nil {
		t.Fatalf("finish summary: %v", err)
	}
	if err := fixture.orchestrator.SyncSession(context.Background(), fixture.id); err != nil {
		t.Fatalf("replace after summary: %v", err)
	}
	if !sameIDs(fixture.wire.deletedIDs, fixture.oldIDs) {
		t.Errorf("completed replacement deleted %v, want old ids %v", fixture.wire.deletedIDs, fixture.oldIDs)
	}
}

// TestReplaceLeavesOtherRecordingsAlone fails if replacement uses a merged
// session's complete audio path instead of the one recording that was spliced.
func TestReplaceLeavesOtherRecordingsAlone(t *testing.T) {
	fixture := newReplacementFixture(t)
	other := filepath.Join(filepath.Dir(fixture.recording), "20261106110000.mp3")
	if err := os.WriteFile(other, []byte("other recording"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.DB().Exec(`UPDATE sessions SET audio_path = ?, sync_state = ? WHERE id = ?`,
		fixture.recording+", "+other, storage.SyncStatePending, fixture.id); err != nil {
		t.Fatalf("add merged recording: %v", err)
	}
	if err := fixture.orchestrator.SyncSession(context.Background(), fixture.id); err != nil {
		t.Fatalf("sync merged recording: %v", err)
	}
	folder, err := fixture.store.GetSession(fixture.id)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	otherName := "audio-" + filepath.Base(other)
	otherCopies := fixture.wire.in(folder.GDriveFolderID)[otherName]
	if len(otherCopies) != 1 {
		t.Fatalf("before replacement other recording has %d copies, want one", len(otherCopies))
	}
	otherBefore := otherCopies[0].ID

	fixture.queue(t, storage.RefinementCompleted, storage.SummaryCompleted)
	fixture.wire.allowDeletes = true
	if err := fixture.orchestrator.SyncSession(context.Background(), fixture.id); err != nil {
		t.Fatalf("replace spliced recording: %v", err)
	}
	if !sameIDs(fixture.wire.deletedIDs, fixture.oldIDs) {
		t.Errorf("replacement deleted %v, want only spliced recording and documents %v", fixture.wire.deletedIDs, fixture.oldIDs)
	}
	otherCopies = fixture.wire.in(folder.GDriveFolderID)[otherName]
	if len(otherCopies) != 1 || otherCopies[0].ID != otherBefore || string(otherCopies[0].Content) != "other recording" {
		t.Errorf("other recording after replacement = %+v, want id %s and unchanged content", otherCopies, otherBefore)
	}
}

// TestTheNewFilesHoldNoDeletedText fails if replacement reuses an old Drive
// file or leaves a new file with more than its one initial revision.
func TestTheNewFilesHoldNoDeletedText(t *testing.T) {
	fixture := newReplacementFixture(t)
	fixture.queue(t, storage.RefinementCompleted, storage.SummaryCompleted)
	fixture.wire.allowDeletes = true
	if err := fixture.orchestrator.SyncSession(context.Background(), fixture.id); err != nil {
		t.Fatalf("replace session files: %v", err)
	}
	rows, err := fixture.store.DriveReplacements(fixture.id)
	if err != nil {
		t.Fatalf("DriveReplacements: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("replacement rows = %d, want three", len(rows))
	}
	for _, row := range rows {
		file := fixture.wire.find(row.NewID)
		if file == nil {
			t.Errorf("new %s id %s is not on Drive", row.Name, row.NewID)
			continue
		}
		if strings.Contains(string(file.Content), "purple elephant seventeen") {
			t.Errorf("new %s contains deleted text", row.Name)
		}
		revisions, err := fixture.syncer.service.Revisions.List(row.NewID).Do()
		if err != nil {
			t.Errorf("list revisions for new %s: %v", row.Name, err)
			continue
		}
		if len(revisions.Revisions) != 1 {
			t.Errorf("new %s has %d revisions, want one", row.Name, len(revisions.Revisions))
		}
	}
}

// TestReplaceWritesEmptySummaryAfterFinalFailure fails if a failed summary
// revives the old title or summary while the deleted words are being removed.
func TestReplaceWritesEmptySummaryAfterFinalFailure(t *testing.T) {
	fixture := newReplacementFixture(t)
	fixture.queue(t, storage.RefinementFailed, storage.SummaryFailed)
	if _, err := fixture.store.DB().Exec(`UPDATE sessions SET title = '', summary = 'stale summary' WHERE id = ?`, fixture.id); err != nil {
		t.Fatalf("prepare failed summary: %v", err)
	}
	fixture.wire.allowDeletes = true
	if err := fixture.orchestrator.SyncSession(context.Background(), fixture.id); err != nil {
		t.Fatalf("replace after summary failure: %v", err)
	}
	sess, err := fixture.store.GetSession(fixture.id)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.Title != "" {
		t.Errorf("failed summary leaves title %q, want it empty", sess.Title)
	}
	files := fixture.wire.in(sess.GDriveFolderID)
	if copies := files["summary.md"]; len(copies) != 1 || len(copies[0].Content) != 0 {
		t.Errorf("failed summary replacement = %+v, want one empty document", copies)
	}
	if got := fixture.wire.find(sess.GDriveFolderID).Name; got != "2026-11-06-untitled" {
		t.Errorf("folder name after summary failure = %q, want untitled", got)
	}
}

// TestReplaceRecoversAnUploadedFileBeforeItsRowIsUpdated fails if a retry
// uploads a second file after Drive accepted the first one but verification
// could not read it back.
func TestReplaceRecoversAnUploadedFileBeforeItsRowIsUpdated(t *testing.T) {
	fixture := newReplacementFixture(t)
	fixture.queue(t, storage.RefinementCompleted, storage.SummaryCompleted)
	fixture.wire.allowDeletes = true
	fixture.wire.failGetAt = fixture.wire.gets + 1

	if err := fixture.orchestrator.SyncSession(context.Background(), fixture.id); err == nil {
		t.Fatal("replacement succeeded though Drive did not verify the uploaded file")
	}
	rows, err := fixture.store.DriveReplacements(fixture.id)
	if err != nil {
		t.Fatalf("DriveReplacements after failed verification: %v", err)
	}
	if len(rows) != 1 || rows[0].State != storage.DriveReplacementUploading {
		t.Fatalf("rows after failed verification = %+v, want one uploading row", rows)
	}
	uploads := fixture.wire.uploads

	fixture.wire.failGetAt = 0
	if err := fixture.orchestrator.SyncSession(context.Background(), fixture.id); err != nil {
		t.Fatalf("resume after failed verification: %v", err)
	}
	if got := fixture.wire.uploads - uploads; got != 2 {
		t.Errorf("resume made %d uploads, want transcript and recording only", got)
	}
	if !sameIDs(fixture.wire.deletedIDs, fixture.oldIDs) {
		t.Errorf("resume deleted %v, want old ids %v", fixture.wire.deletedIDs, fixture.oldIDs)
	}
}

// TestReplaceStartsANewCycleAfterAnotherDeletion fails if the deleted rows
// from the first replacement stop a later segment deletion from replacing the
// then-current three files.
func TestReplaceStartsANewCycleAfterAnotherDeletion(t *testing.T) {
	fixture := newReplacementFixture(t)
	fixture.queue(t, storage.RefinementCompleted, storage.SummaryCompleted)
	fixture.wire.allowDeletes = true
	if err := fixture.orchestrator.SyncSession(context.Background(), fixture.id); err != nil {
		t.Fatalf("first replacement: %v", err)
	}
	firstRows, err := fixture.store.DriveReplacements(fixture.id)
	if err != nil {
		t.Fatalf("first DriveReplacements: %v", err)
	}
	firstNewIDs := make([]string, 0, len(firstRows))
	for _, row := range firstRows {
		firstNewIDs = append(firstNewIDs, row.NewID)
	}

	fixture.queue(t, storage.RefinementCompleted, storage.SummaryCompleted)
	if err := fixture.orchestrator.SyncSession(context.Background(), fixture.id); err != nil {
		t.Fatalf("second replacement: %v", err)
	}
	if len(fixture.wire.deletedIDs) != 6 {
		t.Fatalf("two replacements deleted %d files, want six", len(fixture.wire.deletedIDs))
	}
	if !sameIDs(fixture.wire.deletedIDs[3:], firstNewIDs) {
		t.Errorf("second replacement deleted %v, want first new ids %v", fixture.wire.deletedIDs[3:], firstNewIDs)
	}
	rows, err := fixture.store.DriveReplacements(fixture.id)
	if err != nil {
		t.Fatalf("second DriveReplacements: %v", err)
	}
	for _, row := range rows {
		if row.State != storage.DriveReplacementDeleted {
			t.Errorf("second row %+v has state %q, want deleted", row, row.State)
		}
	}
}

// TestReplaceFinishesAfterDeletesBeforePendingClears fails if a retry after
// every old id is deleted treats the new files as a fresh deletion's old ids.
func TestReplaceFinishesAfterDeletesBeforePendingClears(t *testing.T) {
	fixture := newReplacementFixture(t)
	fixture.queue(t, storage.RefinementCompleted, storage.SummaryCompleted)
	fixture.wire.allowDeletes = true
	if err := fixture.orchestrator.SyncSession(context.Background(), fixture.id); err != nil {
		t.Fatalf("replacement: %v", err)
	}
	if _, err := fixture.store.DB().Exec(`UPDATE sessions SET drive_replace_pending = 1 WHERE id = ?`, fixture.id); err != nil {
		t.Fatalf("restore pending marker: %v", err)
	}
	if err := fixture.orchestrator.SyncSession(context.Background(), fixture.id); err != nil {
		t.Fatalf("finish interrupted replacement: %v", err)
	}
	if !sameIDs(fixture.wire.deletedIDs, fixture.oldIDs) {
		t.Errorf("completion retry deleted %v, want only original old ids %v", fixture.wire.deletedIDs, fixture.oldIDs)
	}
	var pending int
	if err := fixture.store.DB().QueryRow(`SELECT drive_replace_pending FROM sessions WHERE id = ?`, fixture.id).Scan(&pending); err != nil {
		t.Fatalf("read replacement pending: %v", err)
	}
	if pending != 0 {
		t.Errorf("completion retry left drive_replace_pending = %d, want 0", pending)
	}
}

// TestReplaceMarksAnAlreadyGoneOldFileDeleted fails if a stop after Drive
// removed an old file but before SQLite recorded it leaves the replacement
// unable to resume.
func TestReplaceMarksAnAlreadyGoneOldFileDeleted(t *testing.T) {
	fixture := newReplacementFixture(t)
	fixture.queue(t, storage.RefinementCompleted, storage.SummaryCompleted)
	fixture.wire.allowDeletes = true
	fixture.wire.failDeleteAt = 1
	if err := fixture.orchestrator.SyncSession(context.Background(), fixture.id); err == nil {
		t.Fatal("replacement succeeded though its first delete failed")
	}
	rows, err := fixture.store.DriveReplacements(fixture.id)
	if err != nil {
		t.Fatalf("DriveReplacements after delete failure: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows after delete failure = %d, want three", len(rows))
	}
	fixture.wire.deleteFileByHand(rows[0].OldID)
	fixture.wire.failDeleteAt = 0
	if err := fixture.orchestrator.SyncSession(context.Background(), fixture.id); err != nil {
		t.Fatalf("resume after old file vanished: %v", err)
	}
	rows, err = fixture.store.DriveReplacements(fixture.id)
	if err != nil {
		t.Fatalf("DriveReplacements after resume: %v", err)
	}
	for _, row := range rows {
		if row.State != storage.DriveReplacementDeleted {
			t.Errorf("row %+v has state %q, want deleted", row, row.State)
		}
	}
}

// TestFinishDriveReplacementKeepsANewerDeletionPending fails if an older
// replacement clears the marker a later segment deletion set.
func TestFinishDriveReplacementKeepsANewerDeletionPending(t *testing.T) {
	store := openStore(t)
	const id = "replacement-generation"
	if err := store.CreateSession(id, time.Now().UTC()); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET drive_replace_pending = 1, deletion_generation = 2 WHERE id = ?`, id); err != nil {
		t.Fatalf("prepare newer deletion: %v", err)
	}
	for _, name := range []string{"summary.md", "transcript.md", "audio-recording.mp3"} {
		if _, err := store.DB().Exec(`INSERT INTO drive_replacements(session_id, name, old_id, new_id, state) VALUES(?, ?, ?, ?, ?)`,
			id, name, "old-"+name, "new-"+name, storage.DriveReplacementDeleted); err != nil {
			t.Fatalf("seed replacement row %s: %v", name, err)
		}
	}
	if err := store.FinishDriveReplacement(id, 1); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("FinishDriveReplacement = %v, want generation refusal", err)
	}
	var pending int
	if err := store.DB().QueryRow(`SELECT drive_replace_pending FROM sessions WHERE id = ?`, id).Scan(&pending); err != nil {
		t.Fatalf("read pending marker: %v", err)
	}
	if pending != 1 {
		t.Errorf("newer deletion pending marker = %d, want 1", pending)
	}
}

func TestReplaceRefusesARecordingWithWrongDriveChecksum(t *testing.T) {
	fixture := newReplacementFixture(t)
	fixture.queue(t, storage.RefinementCompleted, storage.SummaryCompleted)
	fixture.wire.allowDeletes = true
	fixture.wire.corruptChecksum = true
	if err := fixture.orchestrator.SyncSession(context.Background(), fixture.id); err == nil {
		t.Fatal("replacement succeeded with a wrong Drive checksum")
	}
	if len(fixture.wire.deletedIDs) != 0 {
		t.Fatalf("wrong checksum deleted old files %v", fixture.wire.deletedIDs)
	}
}

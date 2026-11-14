package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/drive/v3"

	"github.com/sjawhar/ghost-wispr/internal/gdrive"
	recordingname "github.com/sjawhar/ghost-wispr/internal/recording"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

var reviewR19RestoredAt = time.Date(2026, 11, 13, 15, 0, 0, 0, time.UTC)

// reviewR19RestoredThenMergedDelete restores a plain session R from Drive,
// merges it with T, lets the merge hook restore and refine both recordings,
// then deletes R's first segment. It returns R's canonical transcript after
// the delete. throughRestore imports R with the real Drive restore callback;
// without it, R is imported with no record of its file names (the control).
func reviewR19RestoredThenMergedDelete(t *testing.T, throughRestore bool) string {
	t.Helper()
	dir := t.TempDir()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "rs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	start := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	rID, tID := "20261113090000", "20261113100000"
	rPath, tPath := filepath.Join(dir, rID+".mp3"), filepath.Join(dir, tID+".mp3")
	ended := start.Add(time.Minute)
	local, ok := recordingname.LocalName(recordingname.FileName(rPath))
	if !ok {
		t.Fatalf("no local name for %s", rPath)
	}
	rs := gdrive.RestoredSession{ID: rID, StartedAt: start, EndedAt: &ended, DriveFolderID: "folder-r", HasAudio: true, RecordingPaths: []string{local},
		Segments: []transcribe.Segment{{Speaker: 0, Text: "romeo restored words", StartTime: 0, EndTime: 1, Timestamp: start}}}
	if throughRestore {
		if err := importRestoredDriveSession(store, func() time.Time { return reviewR19RestoredAt })(rs); err != nil {
			t.Fatal(err)
		}
	} else {
		sess := restoredDriveSession(&rs, reviewR19RestoredAt)
		if err := store.ImportSession(&sess, rs.Segments, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(tPath, []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(tID, start.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession(tID, start.Add(61*time.Minute), tPath); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("merged", []string{rID, tID}, start, start.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	source := &r11FolderDrive{
		files:     map[string][]*drive.File{"folder-r": {{Name: recordingname.FileName(rPath), MimeType: "audio/mpeg"}}},
		downloads: map[string][]byte{"folder-r/" + recordingname.FileName(rPath): []byte("r")},
	}
	refine := suiteTwoWordRefine(store)
	retry := newRetryRefinementHook(store, func() string { return dir }, func(ctx context.Context, s storage.Session) (deletionRecordingSet, error) {
		return restoreDeletionRecordings(ctx, store, &s, dir, source, "")
	}, newWarningsRegistry(), refine)
	onSessionMerged(context.Background(), store, refine, func(context.Context, string, time.Time) error { return nil }, retry,
		func(_ context.Context, _ string, cause error) { t.Fatalf("hook failed: %v", cause) }, "merged")
	merged, err := store.GetSession("merged")
	if err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("merged")
	if err != nil {
		t.Fatal(err)
	}
	deleted := false
	for i := range segments {
		if strings.HasPrefix(segments[i].Text, "first-"+rID) {
			if err := store.DeleteRefinedSegmentText("merged", segments[i].ID, merged.RefinedRevision); err != nil {
				t.Fatal(err)
			}
			deleted = true
		}
	}
	if !deleted {
		t.Fatalf("control: R's refined segment is missing from the merge: %+v", segments)
	}
	restored, err := store.GetSession(rID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(restored.CanonicalTranscript, "first-"+rID) {
		t.Fatalf("R kept its deleted words: %q", restored.CanonicalTranscript)
	}
	return restored.CanonicalTranscript
}

func TestReviewR19RestoredSourceDeleteControl(t *testing.T) {
	if got := reviewR19RestoredThenMergedDelete(t, false); !strings.Contains(got, "second-20261113090000") {
		t.Errorf("control: R lost its other words without recorded file names: %q", got)
	}
}

func TestReviewR19RestoredSourceDeleteThroughRestore(t *testing.T) {
	if got := reviewR19RestoredThenMergedDelete(t, true); !strings.Contains(got, "second-20261113090000") {
		t.Errorf("a restored source lost all its words on one delete: %q", got)
	}
}

// A merged session restored from Drive onto a new unit, then merged with T:
// retry must restore the restored merge's recordings, whose names only the
// Drive restore saw.
func TestReviewR19RestoredMergeThenMergedAgain(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	start := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	a, b, tID := "20261113090000", "20261113100000", "20261113110000"
	path := func(id string) string { return filepath.Join(dir, id+".mp3") }
	ended := start.Add(2 * time.Hour)
	var recordings []string
	for _, id := range []string{a, b} {
		local, ok := recordingname.LocalName(recordingname.FileName(path(id)))
		if !ok {
			t.Fatalf("no local name for %s", path(id))
		}
		recordings = append(recordings, local)
	}
	rs := gdrive.RestoredSession{ID: a + "-merged", StartedAt: start, EndedAt: &ended, DriveFolderID: "folder-r", HasAudio: true, RecordingPaths: recordings}
	if err := importRestoredDriveSession(store, func() time.Time { return reviewR19RestoredAt })(rs); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path(tID), []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(tID, start.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession(tID, start.Add(181*time.Minute), path(tID)); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("top", []string{rs.ID, tID}, start, start.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET gdrive_folder_id = 'folder-top', sync_status = ?, sync_state = ?, refinement_status = 'failed' WHERE id = 'top'`, storage.SyncSynced, storage.SyncStateSynced); err != nil {
		t.Fatal(err)
	}
	audio := func(id string) *drive.File {
		return &drive.File{Name: recordingname.FileName(path(id)), MimeType: "audio/mpeg"}
	}
	source := &r11FolderDrive{
		files: map[string][]*drive.File{
			"folder-r":   {audio(a), audio(b)},
			"folder-top": {audio(tID)},
		},
		downloads: map[string][]byte{
			"folder-r/" + recordingname.FileName(path(a)): []byte("a"),
			"folder-r/" + recordingname.FileName(path(b)): []byte("b"),
		},
	}
	session, err := store.GetSession("top")
	if err != nil {
		t.Fatal(err)
	}
	if err := restoreRefinementRecording(context.Background(), store, &session, dir, func(ctx context.Context, current storage.Session) (deletionRecordingSet, error) {
		return restoreDeletionRecordings(ctx, store, &current, dir, source, "")
	}); err != nil {
		t.Fatalf("retry fails for a restored merge merged again: %v (downloads %v)", err, source.downloaded)
	}
	if got := strings.Join(source.downloaded, ","); !strings.Contains(got, "folder-r/"+recordingname.FileName(path(a))) || !strings.Contains(got, "folder-r/"+recordingname.FileName(path(b))) {
		t.Fatalf("retry downloads = %q, want both restored recordings from folder-r", got)
	}
}

// A merged session restored from Drive and retried on its own: its folder
// lost one of the two recordings the restore saw, so the retry must refuse
// and name it rather than refine half the conversation.
func TestReviewR19RestoredMergeRetryNamesAMissingRecording(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "missing.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	start := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	a, b := "20261113090000", "20261113100000"
	path := func(id string) string { return filepath.Join(dir, id+".mp3") }
	ended := start.Add(2 * time.Hour)
	var recordings []string
	for _, id := range []string{a, b} {
		local, ok := recordingname.LocalName(recordingname.FileName(path(id)))
		if !ok {
			t.Fatalf("no local name for %s", path(id))
		}
		recordings = append(recordings, local)
	}
	rs := gdrive.RestoredSession{ID: a + "-merged", StartedAt: start, EndedAt: &ended, DriveFolderID: "folder-r", HasAudio: true, RecordingPaths: recordings}
	if err := importRestoredDriveSession(store, func() time.Time { return reviewR19RestoredAt })(rs); err != nil {
		t.Fatal(err)
	}
	source := &r11FolderDrive{
		files:     map[string][]*drive.File{"folder-r": {{Name: recordingname.FileName(path(a)), MimeType: "audio/mpeg"}}},
		downloads: map[string][]byte{"folder-r/" + recordingname.FileName(path(a)): []byte("a")},
	}
	session, err := store.GetSession(rs.ID)
	if err != nil {
		t.Fatal(err)
	}
	err = restoreRefinementRecording(context.Background(), store, &session, dir, func(ctx context.Context, current storage.Session) (deletionRecordingSet, error) {
		return restoreDeletionRecordings(ctx, store, &current, dir, source, "")
	})
	if err == nil || !strings.Contains(err.Error(), b+".mp3") {
		t.Fatalf("retry with a recording missing from Drive = %v, want a refusal naming %s.mp3", err, b)
	}
}

// The Drive restore imports a session and records its recording names in one
// transaction: when the names cannot be written, the session is not imported
// either, so the next restore imports it with its names rather than skip it.
func TestImportRestoredDriveSessionIsAllOrNothing(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "atomic.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	start := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	ended := start.Add(time.Minute)
	id := "20261113090000"
	local, ok := recordingname.LocalName(recordingname.FileName(filepath.Join(t.TempDir(), id+".mp3")))
	if !ok {
		t.Fatal("no local name")
	}
	rs := gdrive.RestoredSession{ID: id, StartedAt: start, EndedAt: &ended, DriveFolderID: "folder-r", HasAudio: true, RecordingPaths: []string{local},
		Segments: []transcribe.Segment{{Speaker: 0, Text: "restored words", StartTime: 0, EndTime: 1, Timestamp: start}}}
	restore := importRestoredDriveSession(store, func() time.Time { return reviewR19RestoredAt })
	if _, err := store.DB().Exec(`CREATE TRIGGER refuse_restored_names BEFORE INSERT ON restored_recordings BEGIN SELECT RAISE(ABORT, 'names refused'); END`); err != nil {
		t.Fatal(err)
	}
	if err := restore(rs); err == nil {
		t.Fatal("control: the restore succeeded although its recording names could not be written")
	}
	if session, err := store.GetSession(id); err == nil {
		t.Fatalf("the restore imported session %s (%+v) without its recording names", id, session)
	}
	if _, err := store.DB().Exec(`DROP TRIGGER refuse_restored_names`); err != nil {
		t.Fatal(err)
	}
	if err := restore(rs); err != nil {
		t.Fatalf("the next restore: %v", err)
	}
	paths, err := store.RefinementRecordingPaths(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != local {
		t.Fatalf("restored recording names = %q, want [%q]", paths, local)
	}
}

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/api/drive/v3"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/gdrive"
	recordingname "github.com/sjawhar/ghost-wispr/internal/recording"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

type deletionWordsFake struct {
	before []audio.Word
	after  []audio.Word
}

func (f deletionWordsFake) Words(_ context.Context, path string) ([]audio.Word, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(contents, []byte("spliced")) {
		return f.after, nil
	}
	return f.before, nil
}

func newDeletionTestStore(t *testing.T, recording string) (*storage.SQLiteStore, int64, time.Time) {
	t.Helper()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "delete.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	if err := store.CreateSession("session", started); err != nil {
		t.Fatalf("create session: %v", err)
	}
	for _, segment := range []transcribe.Segment{
		{Speaker: 0, Text: "before bridge", StartTime: 0, EndTime: 1, Timestamp: started},
		{Speaker: 1, Text: "private deletion token", StartTime: 1, EndTime: 2, Timestamp: started.Add(time.Second)},
		{Speaker: 0, Text: "after bridge", StartTime: 2, EndTime: 3, Timestamp: started.Add(2 * time.Second)},
	} {
		if err := store.AppendSegment("session", segment); err != nil {
			t.Fatalf("append segment: %v", err)
		}
	}
	if err := store.EndSession("session", started.Add(3*time.Second), recording); err != nil {
		t.Fatalf("end session: %v", err)
	}
	segments, err := store.GetSegments("session")
	if err != nil {
		t.Fatalf("get segments: %v", err)
	}
	return store, segments[1].ID, started
}

func initialWords() []audio.Word {
	return []audio.Word{
		{Text: "before", Start: 0.1, End: 0.2},
		{Text: "bridge", Start: 0.3, End: 0.4},
		{Text: "private", Start: 1.1, End: 1.2},
		{Text: "deletion", Start: 1.3, End: 1.4},
		{Text: "token", Start: 1.5, End: 1.6},
		{Text: "after", Start: 2.1, End: 2.2},
		{Text: "bridge", Start: 2.3, End: 2.4},
	}
}

func cutWords() []audio.Word {
	return []audio.Word{
		{Text: "before", Start: 0.1, End: 0.2},
		{Text: "bridge", Start: 0.3, End: 0.4},
		{Text: "after", Start: 1.0, End: 1.1},
		{Text: "bridge", Start: 1.2, End: 1.3},
	}
}

func fakeSplice(_ context.Context, path string, _, _ float64) (string, error) {
	if err := os.WriteFile(path, []byte("spliced"), 0o600); err != nil {
		return "", err
	}
	hash := sha256.Sum256([]byte("spliced"))
	return hex.EncodeToString(hash[:]), nil
}

func TestSegmentDeleteRunnerSplicesAndRefinesTheRecording(t *testing.T) {
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	if err := os.WriteFile(recording, []byte("original"), 0o600); err != nil {
		t.Fatalf("write recording: %v", err)
	}
	store, segmentID, started := newDeletionTestStore(t, recording)
	held := holdRecording(t, recording)
	var refinedPath string
	runner := segmentDeleteRunner{
		store:       store,
		transcriber: deletionWordsFake{before: initialWords(), after: cutWords()},
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			return deletionRecordingSet{Paths: []string{recording}}, nil
		},
		splice: fakeSplice,
		refine: func(_ context.Context, sessionID, audioPath string, gotStarted time.Time) error {
			if sessionID != "session" || gotStarted != started {
				t.Fatalf("refine called with session=%q started=%s", sessionID, gotStarted)
			}
			refinedPath = audioPath
			return nil
		},
	}

	if err := runner.Delete(context.Background(), "session", segmentID); err != nil {
		t.Fatalf("delete segment: %v", err)
	}
	contents, err := os.ReadFile(recording)
	if err != nil {
		t.Fatalf("read spliced recording: %v", err)
	}
	if !bytes.Equal(contents, []byte("spliced")) {
		t.Fatalf("recording = %q, want spliced bytes", contents)
	}
	if refinedPath != recording {
		t.Fatalf("refinement read %q, want spliced %q", refinedPath, recording)
	}
	assertRecordingUnwritten(t, held, "original")
}

// holdRecording opens the recording at path, so the test still reads its
// inode after a splice has replaced the name.
func holdRecording(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// assertRecordingUnwritten fails unless the recording held still reads want:
// the one a splice replaced is freed by a plain rename, not overwritten
// first. A segment deletion zeroes no whole audio file, because removing
// filler words must not set off a write of the whole recording (Sami,
// 2026-11-12, dispatch://INFRA-2961/ask/172349fa-0019-488c-9d3b-9d6b7af26a81).
func assertRecordingUnwritten(t *testing.T, held *os.File, want string) {
	t.Helper()
	got := make([]byte, len(want))
	if _, err := held.ReadAt(got, 0); err != nil {
		t.Fatalf("read the replaced recording: %v", err)
	}
	if string(got) != want {
		t.Errorf("a segment deletion rewrote the recording it replaced: it holds %q, want %q", got, want)
	}
}

func TestSegmentDeleteRunnerUsesProbedRecordingDuration(t *testing.T) {
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	if err := os.WriteFile(recording, []byte("original"), 0o600); err != nil {
		t.Fatalf("write recording: %v", err)
	}
	store, segmentID, _ := newDeletionTestStore(t, recording)
	probed := false
	runner := segmentDeleteRunner{
		store:       store,
		transcriber: deletionWordsFake{before: initialWords(), after: cutWords()},
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			return deletionRecordingSet{Paths: []string{recording}}, nil
		},
		duration: func(_ context.Context, path string) (float64, error) {
			if path != recording {
				t.Fatalf("probed %q, want %q", path, recording)
			}
			probed = true
			return 3, nil
		},
		splice: fakeSplice,
	}
	if err := runner.Delete(context.Background(), "session", segmentID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !probed {
		t.Fatal("runner did not probe recording duration")
	}
}

// TestCleanupSpliceTempCopies fails if a copy of a recording a stopped app
// left (a splice's copy or output, a Drive download) or a whole-file erase it
// cut off is still there after the app starts, if a copy is rewritten before
// it is removed (a segment deletion zeroes no whole audio file), if the
// cut-off erase is not finished with zeros, or if a recording is touched.
func TestCleanupSpliceTempCopies(t *testing.T) {
	dir := t.TempDir()
	held := map[string]*os.File{}
	for _, name := range []string{".delete-segment-before.mp3", ".splice-before.mp3", ".download-before.mp3", ".erasing-cut-off", "recording.mp3"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("audio of "+name), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		held[name] = f
	}
	if err := cleanupSpliceTempCopies(dir); err != nil {
		t.Fatalf("cleanup splice temp copies: %v", err)
	}
	for _, name := range []string{".delete-segment-before.mp3", ".splice-before.mp3", ".download-before.mp3", ".erasing-cut-off"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("temp copy %s remains: %v", name, err)
		}
		got := make([]byte, len("audio of "+name))
		if _, err := held[name].ReadAt(got, 0); err != nil {
			t.Fatalf("read the removed %s: %v", name, err)
		}
		zeroed := bytes.Equal(got, make([]byte, len(got)))
		if name == ".erasing-cut-off" && !zeroed {
			t.Errorf("the cut-off erase %s was removed still holding %q", name, got)
		}
		if name != ".erasing-cut-off" && zeroed {
			t.Errorf("the splice copy %s was rewritten before it was removed", name)
		}
	}
	if got, err := os.ReadFile(filepath.Join(dir, "recording.mp3")); err != nil || string(got) != "audio of recording.mp3" {
		t.Fatalf("recording after cleanup = %q (%v), want it whole", got, err)
	}
}

// TestStartupCleanupReachesRecordingsOutsideTheAudioDirectory fails if a
// copy a stopped splice left, or a whole-file erase it cut off, beside a
// recording kept outside the audio directory (a session's absolute
// audio_path) is not removed as the app starts, or the cut-off erase is not
// finished with zeros.
func TestStartupCleanupReachesRecordingsOutsideTheAudioDirectory(t *testing.T) {
	audioDir, elsewhere := t.TempDir(), t.TempDir()
	recording := filepath.Join(elsewhere, "recording.mp3")
	if err := os.WriteFile(recording, []byte("recording"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, _, _ := newDeletionTestStore(t, recording)
	held := map[string]*os.File{}
	for _, name := range []string{".erasing-cut-off", ".splice-cut-off.mp3"} {
		path := filepath.Join(elsewhere, name)
		if err := os.WriteFile(path, []byte("audio of "+name), 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		held[name] = f
	}

	if err := cleanupRecordingDirectories(store, audioDir); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	for name, f := range held {
		if _, err := os.Stat(filepath.Join(elsewhere, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s beside a recording outside the audio directory remains: %v", name, err)
		}
		got := make([]byte, len("audio of "+name))
		if _, err := f.ReadAt(got, 0); err != nil {
			t.Fatal(err)
		}
		if name == ".erasing-cut-off" && !bytes.Equal(got, make([]byte, len(got))) {
			t.Errorf("%s was removed still holding %q", name, got)
		}
	}
	if got, err := os.ReadFile(recording); err != nil || string(got) != "recording" {
		t.Errorf("the recording after cleanup = %q (%v), want it whole", got, err)
	}
}

// TestStartupFinishesAnEraseCutOffAfterItsSessionWasDeleted fails if the
// erase of a recording kept outside the audio directory, cut off by a stop
// after DELETE /api/sessions/{id} removed its row, is not finished with zeros
// as the app starts (no row names the directory any more), or if the
// directory is still listed once the cleanup has finished it.
func TestStartupFinishesAnEraseCutOffAfterItsSessionWasDeleted(t *testing.T) {
	audioDir, elsewhere := t.TempDir(), t.TempDir()
	recording := filepath.Join(elsewhere, "recording.mp3")
	audio := []byte("the deleted session's audio")
	if err := os.WriteFile(recording, audio, 0o600); err != nil {
		t.Fatal(err)
	}
	store, _, _ := newDeletionTestStore(t, recording)
	if _, err := store.DeleteSession("session", audioDir); err != nil {
		t.Fatal(err)
	}
	// The route recorded the recording's directory, moved the recording to
	// its hidden name, and the app stopped before it overwrote it.
	if err := store.RecordEraseDirectory(elsewhere); err != nil {
		t.Fatal(err)
	}
	hidden := filepath.Join(elsewhere, ".erasing-cut-off")
	if err := os.Rename(recording, hidden); err != nil {
		t.Fatal(err)
	}
	held := holdRecording(t, hidden)

	if err := cleanupRecordingDirectories(store, audioDir); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := os.Stat(hidden); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the cut-off erase of the deleted session's recording remains: %v", err)
	}
	got := make([]byte, len(audio))
	if _, err := held.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, make([]byte, len(got))) {
		t.Errorf("the cut-off erase was removed still holding %q", got)
	}
	dirs, err := store.RecordingDirectories()
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(dirs, elsewhere) {
		t.Error("the directory is still listed after the cleanup finished it")
	}
}

// eraseDirectoryLister lists dirs as recording directories and records the
// ones the cleanup forgets and records.
type eraseDirectoryLister struct {
	dirs      []string
	reserved  map[string]bool
	forgotten []string
	recorded  []string
}

func (l *eraseDirectoryLister) RecordingDirectories() ([]string, error) { return l.dirs, nil }

func (l *eraseDirectoryLister) ReservedRawErasePaths() (map[string]bool, error) {
	return l.reserved, nil
}

func (l *eraseDirectoryLister) ForgetEraseDirectories(dirs []string) error {
	l.forgotten = append(l.forgotten, dirs...)
	return nil
}

func (l *eraseDirectoryLister) RecordEraseDirectory(dir string) error {
	l.recorded = append(l.recorded, dir)
	return nil
}

// TestStartupCleanupForgetsNoDirectoryItFailedToClean fails if the startup
// cleanup forgets a directory whose cleanup failed: a cut-off erase left
// there would never be finished. A path that is a file, not a directory,
// makes the cleanup fail; the directory beside it is cleaned and forgotten.
func TestStartupCleanupForgetsNoDirectoryItFailedToClean(t *testing.T) {
	audioDir, good := t.TempDir(), t.TempDir()
	failing := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(failing, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	lister := &eraseDirectoryLister{dirs: []string{failing, good}}
	if err := cleanupRecordingDirectories(lister, audioDir); err == nil {
		t.Fatal("control: cleaning a path that is not a directory did not fail")
	}
	if slices.Contains(lister.forgotten, failing) {
		t.Errorf("the directory whose cleanup failed was forgotten: %v", lister.forgotten)
	}
	if !slices.Contains(lister.forgotten, good) {
		t.Errorf("the directory cleaned beside it was not forgotten: %v", lister.forgotten)
	}
}

// TestStartupCleanupNamesTheAudioDirectoryAsAnAbsolutePath fails if the
// startup cleanup hands on a relative audio_dir as named, beside the absolute
// spelling the store lists for the same directory: it cleans that directory
// twice and forgets it under two names.
func TestStartupCleanupNamesTheAudioDirectoryAsAnAbsolutePath(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.Mkdir("audio", 0o700); err != nil {
		t.Fatal(err)
	}
	absolute := filepath.Join(root, "audio")
	lister := &eraseDirectoryLister{dirs: []string{absolute}}
	if err := cleanupRecordingDirectories(lister, "audio"); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if !slices.Equal(lister.forgotten, []string{absolute}) {
		t.Errorf("forgotten %v, want the audio directory once, as %s", lister.forgotten, absolute)
	}
}

// TestStartupCleanupKeepsAnEraseDirectoryThatIsNotThere fails if an erase
// directory that is not there as the app starts (a disk not mounted) is
// forgotten: a cut-off erase in it is finished at a later start, once it is
// back.
func TestStartupCleanupKeepsAnEraseDirectoryThatIsNotThere(t *testing.T) {
	audioDir := t.TempDir()
	unplugged := filepath.Join(t.TempDir(), "usb")
	if err := os.Mkdir(unplugged, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "cleanup.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.RecordEraseDirectory(unplugged); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(unplugged); err != nil {
		t.Fatal(err)
	}
	if err := cleanupRecordingDirectories(store, audioDir); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	dirs, err := store.RecordingDirectories()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(dirs, unplugged) {
		t.Error("the erase directory that was not there was forgotten")
	}
}

// TestStartupRecordsTheAudioDirectoryForALaterStart fails if the audio
// directory is not recorded as an erase directory as the app starts, after
// the cleanup: the recorder erases a saved session's raw recording there
// without recording it, so an erase of it a stop cut off would not be
// finished at a start with another audio_dir.
func TestStartupRecordsTheAudioDirectoryForALaterStart(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "cleanup.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	oldAudioDir, newAudioDir := t.TempDir(), t.TempDir()
	if err := cleanupRecordingDirectories(store, oldAudioDir); err != nil {
		t.Fatalf("cleanup at the first start: %v", err)
	}
	// The recorder's erase of a raw recording is cut off by a stop.
	audio := []byte("a saved session's raw recording")
	cutOff := filepath.Join(oldAudioDir, ".erasing-cut-off")
	if err := os.WriteFile(cutOff, audio, 0o600); err != nil {
		t.Fatal(err)
	}
	held := holdRecording(t, cutOff)

	if err := cleanupRecordingDirectories(store, newAudioDir); err != nil {
		t.Fatalf("cleanup at a start with another audio_dir: %v", err)
	}
	if _, err := os.Stat(cutOff); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the cut-off erase in the earlier audio directory remains: %v", err)
	}
	got := make([]byte, len(audio))
	if _, err := held.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, make([]byte, len(got))) {
		t.Errorf("the cut-off erase was left holding %q", got)
	}
}

func TestStartupRecoveryContinuesPastBadSplice(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "recovery.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	audioDir := t.TempDir()
	bad := filepath.Join(audioDir, "bad.mp3")
	good := filepath.Join(audioDir, "good.mp3")
	if err := os.WriteFile(bad, []byte("unexpected"), 0o600); err != nil {
		t.Fatalf("write bad recording: %v", err)
	}
	if err := os.WriteFile(good, []byte("original"), 0o600); err != nil {
		t.Fatalf("write good recording: %v", err)
	}
	now := time.Now().UTC()
	for _, entry := range []struct{ id, path string }{{"bad", bad}, {"good", good}} {
		if err := store.CreateSession(entry.id, now); err != nil {
			t.Fatalf("create %s: %v", entry.id, err)
		}
		if err := store.EndSession(entry.id, now.Add(time.Second), entry.path); err != nil {
			t.Fatalf("end %s: %v", entry.id, err)
		}
	}
	goodAfter := hashBytesForDeleteTest([]byte("spliced"))
	if _, err := store.DB().Exec(`
		INSERT INTO deleted_segments(session_id, audio_path, start, end, sha256_before, sha256_after, deleted_at)
		VALUES (?, ?, 1, 2, ?, ?, ?), (?, ?, 1, 2, ?, ?, ?)`,
		"bad", bad, "not-the-current-hash", "ignored", now.Format(time.RFC3339Nano),
		"good", good, hashBytesForDeleteTest([]byte("original")), goodAfter, now.Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("insert pending rows: %v", err)
	}
	outcomes, err := recoverPendingSplices(context.Background(), store, audioDir, nil, fakeSplice, nil)
	if err != nil {
		t.Fatalf("recover splices: %v", err)
	}
	recovered, failed := 0, 0
	for i := range outcomes {
		if outcomes[i].Recovered {
			recovered++
		}
		if outcomes[i].Err != nil {
			failed++
		}
	}
	if recovered != 1 || failed != 1 {
		t.Fatalf("outcomes=%v, want one recovered and one failed row", outcomes)
	}
	if contents, err := os.ReadFile(good); err != nil || !bytes.Equal(contents, []byte("spliced")) {
		t.Fatalf("good recovery bytes=%q err=%v", contents, err)
	}
	if contents, err := os.ReadFile(bad); err != nil || !bytes.Equal(contents, []byte("unexpected")) {
		t.Fatalf("bad recovery bytes=%q err=%v, want unchanged recording", contents, err)
	}
}

type mergedDeletionWordsFake struct{}

func (mergedDeletionWordsFake) Words(_ context.Context, path string) ([]audio.Word, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(contents, []byte("spliced")) {
		return cutWords(), nil
	}
	if strings.Contains(filepath.Base(path), "120000") {
		return []audio.Word{{Text: "unrelated", Start: 0, End: 3}}, nil
	}
	return initialWords(), nil
}

func TestDeleteSegmentWhenGCRemovedTheLocalAudio(t *testing.T) {
	for _, tc := range []struct {
		name          string
		audioPath     string
		wantDownloads []string
	}{
		{name: "one recording removed", audioPath: "first", wantDownloads: []string{"audio-20261111130000.mp3"}},
		{name: "all recordings removed", wantDownloads: []string{"audio-20261111120000.mp3", "audio-20261111130000.mp3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			audioDir := t.TempDir()
			first := filepath.Join(audioDir, "20261111120000.mp3")
			second := filepath.Join(audioDir, "20261111130000.mp3")
			if tc.audioPath != "" {
				if err := os.WriteFile(first, []byte("first"), 0o600); err != nil {
					t.Fatalf("write first recording: %v", err)
				}
			}
			store, segmentID, _ := newDeletionTestStore(t, second)
			path := ""
			if tc.audioPath != "" {
				path = first
			}
			if _, err := store.DB().Exec(`UPDATE sessions SET audio_path = ?, audio_removed_at = ?, gdrive_folder_id = ? WHERE id = ?`, path, time.Now().UTC().Format(time.RFC3339Nano), "folder", "session"); err != nil {
				t.Fatalf("mark local audio removed: %v", err)
			}
			drive := &deletionDriveFake{
				files: []*drive.File{
					{Name: "audio-20261111120000.mp3", MimeType: "audio/mpeg"},
					{Name: "audio-20261111130000.mp3", MimeType: "audio/mpeg"},
				},
				downloads: map[string][]byte{
					"audio-20261111120000.mp3": []byte("first"),
					"audio-20261111130000.mp3": []byte("second"),
				},
			}
			var refinedPath string
			runner := segmentDeleteRunner{
				store:       store,
				transcriber: mergedDeletionWordsFake{},
				ensureRecordings: func(ctx context.Context, session storage.Session) (deletionRecordingSet, error) {
					return restoreDeletionRecordings(ctx, &session, audioDir, drive)
				},
				splice: fakeSplice,
				refine: func(_ context.Context, _ string, audioPath string, _ time.Time) error {
					refinedPath = audioPath
					return nil
				},
			}
			if err := runner.Delete(context.Background(), "session", segmentID); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			want := first + "," + second
			session := storeSessionForDelete(t, store, "session")
			if session.AudioPath != want || session.AudioRemovedAt != "" {
				t.Fatalf("session audio_path=%q removed_at=%q, want %q and empty marker", session.AudioPath, session.AudioRemovedAt, want)
			}
			if refinedPath != want {
				t.Fatalf("refinement read %q, want complete audio path %q", refinedPath, want)
			}
			if contents, err := os.ReadFile(second); err != nil || !bytes.Equal(contents, []byte("spliced")) {
				t.Fatalf("spliced downloaded recording: bytes=%q err=%v", contents, err)
			}
			if !slices.Equal(drive.downloaded, tc.wantDownloads) {
				t.Fatalf("Drive downloads = %v, want %v", drive.downloaded, tc.wantDownloads)
			}
		})
	}
}

// TestDeleteSegmentRemovesDownloadedAudioOnRefusal fails if a refused
// deletion leaves a recording it downloaded from Drive, or rewrites it before
// removing it: a segment deletion zeroes no whole audio file (Sami,
// 2026-11-12, dispatch://INFRA-2961/ask/172349fa-0019-488c-9d3b-9d6b7af26a81).
func TestDeleteSegmentRemovesDownloadedAudioOnRefusal(t *testing.T) {
	downloaded := filepath.Join(t.TempDir(), "downloaded.mp3")
	if err := os.WriteFile(downloaded, []byte("downloaded"), 0o600); err != nil {
		t.Fatalf("write downloaded recording: %v", err)
	}
	store, segmentID, _ := newDeletionTestStore(t, downloaded)
	held := holdRecording(t, downloaded)
	runner := segmentDeleteRunner{
		store:       store,
		transcriber: deletionWordsFake{before: initialWords(), after: append(cutWords(), audio.Word{Text: "private", Start: 0.9, End: 1})},
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			return deletionRecordingSet{Paths: []string{downloaded}, Downloaded: []string{downloaded}}, nil
		},
		splice: fakeSplice,
	}
	if err := runner.Delete(context.Background(), "session", segmentID); !errors.Is(err, server.ErrSegmentNotRemovedCleanly) {
		t.Fatalf("Delete error = %v, want clean-cut refusal", err)
	}
	if _, err := os.Stat(downloaded); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("downloaded recording still exists after refusal: %v", err)
	}
	assertRecordingUnwritten(t, held, "downloaded")
}

// TestTheRecordingDirectoryIsFlushedBeforeTheSpliceIsMarkedComplete fails
// if the runner or the startup recovery marks a splice complete before the
// directory holding the spliced recording is flushed: after a power loss the
// mark could stand beside the unspliced recording, which the Drive
// replacement would then upload as the clean copy.
func TestTheRecordingDirectoryIsFlushedBeforeTheSpliceIsMarkedComplete(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, store *storage.SQLiteStore, recording string, segmentID int64)
	}{
		{"deletion runner", func(t *testing.T, store *storage.SQLiteStore, recording string, segmentID int64) {
			runner := r4Runner(store, recording)
			if err := runner.Delete(context.Background(), "session", segmentID); err != nil {
				t.Fatalf("delete segment: %v", err)
			}
		}},
		{"recovery that splices again", func(t *testing.T, store *storage.SQLiteStore, recording string, segmentID int64) {
			recoverInterruptedDeletion(t, store, recording, segmentID, nil)
		}},
		{"recovery of a spliced recording from Drive", func(t *testing.T, store *storage.SQLiteStore, recording string, segmentID int64) {
			if _, err := store.DB().Exec(`UPDATE sessions SET gdrive_folder_id = 'folder' WHERE id = 'session'`); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(recording); err != nil {
				t.Fatal(err)
			}
			name := "audio-" + filepath.Base(recording)
			recoverInterruptedDeletion(t, store, recording, segmentID, &deletionDriveFake{
				files:     []*drive.File{{Name: name, MimeType: "audio/mpeg"}},
				downloads: map[string][]byte{name: []byte("spliced")},
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recording := filepath.Join(t.TempDir(), "recording.mp3")
			if err := os.WriteFile(recording, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			store, segmentID, _ := newDeletionTestStore(t, recording)
			original := syncRecordingDirectory
			t.Cleanup(func() { syncRecordingDirectory = original })
			flushed := 0
			syncRecordingDirectory = func(path string) error {
				flushed++
				if contents, err := os.ReadFile(path); err != nil || string(contents) != "spliced" {
					t.Errorf("flushed the directory of %s holding %q (%v), want the spliced recording", path, contents, err)
				}
				if pending, err := store.HasPendingSplice("session"); err != nil || !pending {
					t.Errorf("flushed the directory after the splice was marked complete (pending %v, %v)", pending, err)
				}
				return original(path)
			}
			tc.run(t, store, recording, segmentID)
			if flushed != 1 {
				t.Errorf("the recording directory was flushed %d times before the splice was marked complete, want 1", flushed)
			}
			if pending, err := store.HasPendingSplice("session"); err != nil || pending {
				t.Fatalf("control: splice still pending (%v, %v)", pending, err)
			}
		})
	}
}

// recoverInterruptedDeletion records a deletion of segmentID whose splice did
// not reach recording, then runs the startup recovery with Drive as source.
func recoverInterruptedDeletion(t *testing.T, store *storage.SQLiteStore, recording string, segmentID int64, source deletionRecordingSource) {
	t.Helper()
	if err := store.DeleteSegment("session", segmentID, storage.DeletedSpan{
		SessionID: "session", AudioPath: recording, Start: 1, End: 2,
		SHA256Before: hashBytesForDeleteTest([]byte("original")), SHA256After: hashBytesForDeleteTest([]byte("spliced")),
	}); err != nil {
		t.Fatalf("record interrupted delete: %v", err)
	}
	outcomes, err := recoverPendingSplices(context.Background(), store, filepath.Dir(recording), source, fakeSplice, nil)
	if err != nil || len(outcomes) != 1 || outcomes[0].Err != nil {
		t.Fatalf("recover pending splice: outcomes=%v err=%v", outcomes, err)
	}
}

func TestSegmentDeleteRunnerFindsUnknownSegmentBeforeNeedingTranscriber(t *testing.T) {
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	if err := os.WriteFile(recording, []byte("recording"), 0o600); err != nil {
		t.Fatalf("write recording: %v", err)
	}
	store, _, _ := newDeletionTestStore(t, recording)
	runner := segmentDeleteRunner{store: store}
	if err := runner.Delete(context.Background(), "session", 999999); !errors.Is(err, storage.ErrSegmentNotFound) {
		t.Fatalf("Delete error = %v, want segment not found", err)
	}
}

func TestSegmentDeleteRunnerRefusesSecondDeleteWhileSplicePending(t *testing.T) {
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	if err := os.WriteFile(recording, []byte("original"), 0o600); err != nil {
		t.Fatalf("write recording: %v", err)
	}
	store, firstID, _ := newDeletionTestStore(t, recording)
	segments, err := store.GetSegments("session")
	if err != nil {
		t.Fatalf("get segments: %v", err)
	}
	if err := store.DeleteSegment("session", firstID, storage.DeletedSpan{AudioPath: recording, Start: 1, End: 2, SHA256Before: "before", SHA256After: "after"}); err != nil {
		t.Fatalf("record pending splice: %v", err)
	}
	runner := segmentDeleteRunner{store: store}
	if err := runner.Delete(context.Background(), "session", segments[2].ID); !errors.Is(err, storage.ErrSegmentSplicePending) {
		t.Fatalf("second delete error = %v, want pending splice conflict", err)
	}
}

func storeSessionForDelete(t *testing.T, store *storage.SQLiteStore, id string) storage.Session {
	t.Helper()
	session, err := store.GetSession(id)
	if err != nil {
		t.Fatalf("get session %s: %v", id, err)
	}
	return session
}

type sequenceDeletionWordsFake struct{}

func (sequenceDeletionWordsFake) Words(_ context.Context, path string) ([]audio.Word, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	switch string(contents) {
	case "original":
		return []audio.Word{
			{Text: "before", Start: 0.1, End: 0.2},
			{Text: "bridge", Start: 0.3, End: 0.4},
			{Text: "private", Start: 1.1, End: 1.2},
			{Text: "deletion", Start: 1.3, End: 1.4},
			{Text: "token", Start: 1.5, End: 1.6},
			{Text: "after", Start: 2.1, End: 2.2},
			{Text: "bridge", Start: 2.3, End: 2.4},
		}, nil
	case "spliced-one":
		return []audio.Word{
			{Text: "before", Start: 0.1, End: 0.2},
			{Text: "bridge", Start: 0.3, End: 0.4},
			{Text: "after", Start: 1.1, End: 1.2},
			{Text: "bridge", Start: 1.3, End: 1.4},
		}, nil
	case "spliced-two":
		return []audio.Word{{Text: "before", Start: 0.1, End: 0.2}, {Text: "bridge", Start: 0.3, End: 0.4}}, nil
	default:
		return nil, fmt.Errorf("unknown recording state %q", contents)
	}
}

func TestSegmentDeleteRunnerSerializesConcurrentDeletes(t *testing.T) {
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	if err := os.WriteFile(recording, []byte("original"), 0o600); err != nil {
		t.Fatalf("write recording: %v", err)
	}
	store, firstID, _ := newDeletionTestStore(t, recording)
	segments, err := store.GetSegments("session")
	if err != nil {
		t.Fatalf("get segments: %v", err)
	}
	secondID := segments[2].ID
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var spliceCalls atomic.Int32
	controlledSplice := func(_ context.Context, path string, _, _ float64) (string, error) {
		contents, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		if spliceCalls.Add(1) == 1 {
			close(firstEntered)
			<-releaseFirst
		}
		next := []byte("spliced-one")
		if bytes.Equal(contents, []byte("spliced-one")) {
			next = []byte("spliced-two")
		}
		if err := os.WriteFile(path, next, 0o600); err != nil {
			return "", err
		}
		return hashBytesForDeleteTest(next), nil
	}
	runner := segmentDeleteRunner{
		store:       store,
		transcriber: sequenceDeletionWordsFake{},
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			return deletionRecordingSet{Paths: []string{recording}}, nil
		},
		splice: controlledSplice,
	}
	errs := make(chan error, 2)
	go func() { errs <- runner.Delete(context.Background(), "session", firstID) }()
	<-firstEntered
	go func() { errs <- runner.Delete(context.Background(), "session", secondID) }()
	close(releaseFirst)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent delete: %v", err)
		}
	}
	remaining, err := store.GetSegments("session")
	if err != nil {
		t.Fatalf("get remaining segments: %v", err)
	}
	if len(remaining) != 1 || remaining[0].Text != "before bridge" {
		t.Fatalf("remaining segments = %+v, want only the first", remaining)
	}
	contents, err := os.ReadFile(recording)
	if err != nil {
		t.Fatalf("read recording: %v", err)
	}
	if !bytes.Equal(contents, []byte("spliced-two")) {
		t.Fatalf("recording = %q, want both cuts", contents)
	}
}

func TestNeighbourCheckUsesWordsNearestTheCut(t *testing.T) {
	var beforeTokens []string
	var words []audio.Word
	for i := range 30 {
		token := fmt.Sprintf("word%d", i)
		beforeTokens = append(beforeTokens, token)
		words = append(words, audio.Word{Text: token, Start: float64(i), End: float64(i) + 0.5})
	}
	if missing := missingNeighbourTokens(words, 30, strings.Join(beforeTokens, " "), true); len(missing) != 0 {
		t.Fatalf("missing words = %v, want none from the six words nearest the cut", missing)
	}
}

func TestNeighbourCheckCountsRepeatedLostWords(t *testing.T) {
	words := []audio.Word{{Text: "the", Start: 1, End: 1.2}}
	if missing := missingNeighbourTokens(words, 0, "so the so", false); len(missing) != 2 {
		t.Fatalf("missing words = %v, want both lost occurrences of %q", missing, "so")
	}
}

func TestBoundaryCutSkipsOnlyCrossRecordingNeighbour(t *testing.T) {
	secondRecording := []audio.Word{{Text: "deleted", Start: 0.1, End: 0.2}, {Text: "after", Start: 1.2, End: 1.3}}
	if recordingHasWordsOnSide(secondRecording, 0, 1, true) {
		t.Fatal("selected recording unexpectedly has a before-side neighbour")
	}
	if !recordingHasWordsOnSide(secondRecording, 0, 1, false) {
		t.Fatal("selected recording should validate its after-side neighbour")
	}
}

func TestSegmentDeleteRunnerRefusesSurvivingWordsWithoutChangingRecording(t *testing.T) {
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	original := []byte("original")
	if err := os.WriteFile(recording, original, 0o600); err != nil {
		t.Fatalf("write recording: %v", err)
	}
	store, segmentID, _ := newDeletionTestStore(t, recording)
	runner := segmentDeleteRunner{
		store:       store,
		transcriber: deletionWordsFake{before: initialWords(), after: append(cutWords(), audio.Word{Text: "private", Start: 0.9, End: 1.0})},
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			return deletionRecordingSet{Paths: []string{recording}}, nil
		},
		splice: fakeSplice,
	}

	err := runner.Delete(context.Background(), "session", segmentID)
	if !errors.Is(err, server.ErrSegmentNotRemovedCleanly) {
		t.Fatalf("delete error = %v, want clean-cut refusal", err)
	}
	contents, readErr := os.ReadFile(recording)
	if readErr != nil {
		t.Fatalf("read original recording: %v", readErr)
	}
	if !bytes.Equal(contents, original) {
		t.Fatalf("refused deletion changed recording: got %q, want %q", contents, original)
	}
}

func TestSegmentDeleteRunnerRefusesKeptRawRecordingFromMergedSource(t *testing.T) {
	audioDir := t.TempDir()
	recording := filepath.Join(audioDir, "merged.mp3")
	if err := os.WriteFile(recording, []byte("original"), 0o600); err != nil {
		t.Fatalf("write recording: %v", err)
	}
	store, segmentID, _ := newDeletionTestStore(t, recording)
	if err := store.CreateSession("source-session", time.Now().UTC()); err != nil {
		t.Fatalf("create source session: %v", err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET status = 'merged', merged_into = ? WHERE id = ?`, "session", "source-session"); err != nil {
		t.Fatalf("mark source merged: %v", err)
	}
	if err := os.WriteFile(filepath.Join(audioDir, "source-session.pcm"), []byte("raw audio"), 0o600); err != nil {
		t.Fatalf("write source raw audio: %v", err)
	}
	runner := segmentDeleteRunner{
		store:       store,
		audioDir:    audioDir,
		transcriber: deletionWordsFake{before: initialWords(), after: cutWords()},
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			t.Fatal("recordings checked after kept raw source audio")
			return deletionRecordingSet{}, nil
		},
		splice: fakeSplice,
	}
	err := runner.Delete(context.Background(), "session", segmentID)
	if !errors.Is(err, server.ErrSegmentRawAudioKept) || !strings.Contains(err.Error(), "source-session.pcm") {
		t.Fatalf("delete error = %v, want named source raw recording", err)
	}
}

// TestSegmentDeleteRunnerRefusesKeptRawRecordingThroughAChainedMerge fails if
// a raw recording kept beside a session merged into the deleting one through
// another merge (A into M1, M1 into this session) lets the deletion go ahead:
// the raw file would keep the deleted segment's audio as a live file.
func TestSegmentDeleteRunnerRefusesKeptRawRecordingThroughAChainedMerge(t *testing.T) {
	audioDir := t.TempDir()
	recording := filepath.Join(audioDir, "merged.mp3")
	if err := os.WriteFile(recording, []byte("original"), 0o600); err != nil {
		t.Fatalf("write recording: %v", err)
	}
	store, segmentID, _ := newDeletionTestStore(t, recording)
	for _, id := range []string{"middle-merge", "recorded-source"} {
		if err := store.CreateSession(id, time.Now().UTC()); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET status = 'merged', merged_into = CASE id WHEN 'middle-merge' THEN 'session' ELSE 'middle-merge' END
		WHERE id IN ('middle-merge', 'recorded-source')`); err != nil {
		t.Fatalf("chain the merges: %v", err)
	}
	if err := os.WriteFile(filepath.Join(audioDir, "recorded-source.pcm"), []byte("raw audio"), 0o600); err != nil {
		t.Fatalf("write source raw audio: %v", err)
	}
	runner := segmentDeleteRunner{
		store:       store,
		audioDir:    audioDir,
		transcriber: deletionWordsFake{before: initialWords(), after: cutWords()},
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			t.Fatal("recordings checked after kept raw audio of a chained source")
			return deletionRecordingSet{}, nil
		},
		splice: fakeSplice,
	}
	err := runner.Delete(context.Background(), "session", segmentID)
	if !errors.Is(err, server.ErrSegmentRawAudioKept) || !strings.Contains(err.Error(), "recorded-source.pcm") {
		t.Fatalf("delete error = %v, want the chained source's raw recording named", err)
	}
}

func TestSegmentDeleteRunnerRefusesOverCutWithoutChangingRecording(t *testing.T) {
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	original := []byte("original")
	if err := os.WriteFile(recording, original, 0o600); err != nil {
		t.Fatalf("write recording: %v", err)
	}
	store, segmentID, _ := newDeletionTestStore(t, recording)
	runner := segmentDeleteRunner{
		store:       store,
		transcriber: deletionWordsFake{before: initialWords(), after: cutWords()[:2]},
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			return deletionRecordingSet{Paths: []string{recording}}, nil
		},
		splice: fakeSplice,
	}

	err := runner.Delete(context.Background(), "session", segmentID)
	if !errors.Is(err, server.ErrSegmentNotRemovedCleanly) {
		t.Fatalf("delete error = %v, want clean-cut refusal", err)
	}
	contents, readErr := os.ReadFile(recording)
	if readErr != nil {
		t.Fatalf("read original recording: %v", readErr)
	}
	if !bytes.Equal(contents, original) {
		t.Fatalf("over-cut refusal changed recording: got %q, want %q", contents, original)
	}
}

func TestSegmentDeleteRunnerRefusesKeptRawRecording(t *testing.T) {
	audioDir := t.TempDir()
	recording := filepath.Join(audioDir, "recording.mp3")
	if err := os.WriteFile(recording, []byte("original"), 0o600); err != nil {
		t.Fatalf("write recording: %v", err)
	}
	if err := os.WriteFile(filepath.Join(audioDir, "session.pcm"), []byte("raw audio"), 0o600); err != nil {
		t.Fatalf("write raw recording: %v", err)
	}
	store, segmentID, _ := newDeletionTestStore(t, recording)
	runner := segmentDeleteRunner{
		store:       store,
		audioDir:    audioDir,
		transcriber: deletionWordsFake{before: initialWords(), after: cutWords()},
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			t.Fatal("recordings checked after kept raw audio")
			return deletionRecordingSet{}, nil
		},
		splice: fakeSplice,
	}

	err := runner.Delete(context.Background(), "session", segmentID)
	if !errors.Is(err, server.ErrSegmentRawAudioKept) || !strings.Contains(err.Error(), "session.pcm") {
		t.Fatalf("delete error = %v, want named kept raw recording", err)
	}
}

type deletionDriveFake struct {
	files      []*drive.File
	downloads  map[string][]byte
	downloaded []string
}

func (f *deletionDriveFake) ListFolderFiles(context.Context, string) ([]*drive.File, error) {
	return f.files, nil
}

func (f *deletionDriveFake) DownloadRecording(_ context.Context, _ string, name, destination string) error {
	f.downloaded = append(f.downloaded, name)
	return os.WriteFile(destination, f.downloads[name], 0o600)
}

func TestRestoreDeletionRecordingsDownloadsGCMissingAudioInTimeOrder(t *testing.T) {
	audioDir := t.TempDir()
	first := filepath.Join(audioDir, "20261111120000.mp3")
	second := filepath.Join(audioDir, "20261111130000.mp3")
	if err := os.WriteFile(first, []byte("local audio"), 0o600); err != nil {
		t.Fatalf("write local recording: %v", err)
	}
	drive := &deletionDriveFake{
		files: []*drive.File{
			{Name: "audio-20261111130000.mp3", MimeType: "audio/mpeg"},
			{Name: "transcript.md", MimeType: "application/vnd.google-apps.document"},
		},
		downloads: map[string][]byte{"audio-20261111130000.mp3": []byte("restored audio")},
	}
	recordingSet, err := restoreDeletionRecordings(context.Background(), &storage.Session{
		AudioPath:      first,
		AudioRemovedAt: time.Now().UTC().Format(time.RFC3339),
		GDriveFolderID: "folder",
	}, audioDir, drive)
	if err != nil {
		t.Fatalf("restore deletion recordings: %v", err)
	}
	if want := []string{first, second}; !slices.Equal(recordingSet.Paths, want) {
		t.Fatalf("recordings = %v, want %v", recordingSet.Paths, want)
	}
	if !slices.Equal(recordingSet.Downloaded, []string{second}) {
		t.Fatalf("downloaded paths = %v, want %v", recordingSet.Downloaded, []string{second})
	}
	if !slices.Equal(drive.downloaded, []string{"audio-20261111130000.mp3"}) {
		t.Fatalf("downloaded = %v, want missing Drive recording", drive.downloaded)
	}
	contents, err := os.ReadFile(second)
	if err != nil {
		t.Fatalf("read restored recording: %v", err)
	}
	if !bytes.Equal(contents, []byte("restored audio")) {
		t.Fatalf("restored recording = %q", contents)
	}
}

func TestRestoreDeletionRecordingsHandlesTypedNilDriveSource(t *testing.T) {
	var source deletionRecordingSource = (*gdrive.Syncer)(nil)
	set, err := restoreDeletionRecordings(context.Background(), &storage.Session{GDriveFolderID: "folder"}, t.TempDir(), source)
	if err != nil {
		t.Fatalf("restore with sync off: %v", err)
	}
	if len(set.Paths) != 0 {
		t.Fatalf("restore paths = %v, want no Drive download while sync is off", set.Paths)
	}
}

// TestRestoreDeletionRecordingsRestoresOnlyRecordingNames fails if a segment
// deletion restores, from the session's Drive folder, a file whose name is
// not one recording.FileName gives a recording: one named to reach outside
// the audio directory ("audio-../...") would be written there. A recording
// Drive names as FileName does is restored into the audio directory.
func TestRestoreDeletionRecordingsRestoresOnlyRecordingNames(t *testing.T) {
	root := t.TempDir()
	audioDir := filepath.Join(root, "audio")
	if err := os.Mkdir(audioDir, 0o700); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(audioDir, "20261111130000.mp3")
	drive := &deletionDriveFake{
		files: []*drive.File{
			{Name: recordingname.FileName(missing), MimeType: "audio/mpeg"},
			{Name: "audio-../escape.mp3", MimeType: "audio/mpeg"},
		},
		downloads: map[string][]byte{recordingname.FileName(missing): []byte("restored audio"), "audio-../escape.mp3": []byte("escaped")},
	}
	set, err := restoreDeletionRecordings(context.Background(), &storage.Session{GDriveFolderID: "folder"}, audioDir, drive)
	if err != nil {
		t.Fatalf("restore deletion recordings: %v", err)
	}
	if !slices.Equal(set.Paths, []string{missing}) || !slices.Equal(drive.downloaded, []string{recordingname.FileName(missing)}) {
		t.Errorf("restored %v (downloaded %v), want only %s", set.Paths, drive.downloaded, missing)
	}
	if _, err := os.Stat(filepath.Join(root, "escape.mp3")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a Drive file named to reach outside the audio directory was written there: %v", err)
	}
}

// TestSegmentDeleteRunnerSplicesTheRecordingLeftInPlaceAfterAnAudioDirChange
// fails if, after audio_dir changes with the old recordings left in place, a
// segment deletion splices a copy of the recording downloaded from Drive into
// the new audio directory (sjawhar/ghost-wispr#50's runner): the recording
// where the session's entry points, which the app plays and syncs, would
// keep the deleted audio as a live file. The recorder writes
// filepath.Join(audio_dir, name), relative to the working directory.
func TestSegmentDeleteRunnerSplicesTheRecordingLeftInPlaceAfterAnAudioDirChange(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	newAudioDir := filepath.Join(root, "new-audio")
	for _, dir := range []string{"old-audio", newAudioDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	recording := filepath.Join("old-audio", "20261111120000.mp3")
	if err := os.WriteFile(recording, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, segmentID, _ := newDeletionTestStore(t, recording)
	if _, err := store.DB().Exec(`UPDATE sessions SET gdrive_folder_id = 'folder' WHERE id = 'session'`); err != nil {
		t.Fatal(err)
	}
	drive := &deletionDriveFake{
		files:     []*drive.File{{Name: recordingname.FileName(recording), MimeType: "audio/mpeg"}},
		downloads: map[string][]byte{recordingname.FileName(recording): []byte("original")},
	}
	runner := segmentDeleteRunner{
		store:       store,
		transcriber: deletionWordsFake{before: initialWords(), after: cutWords()},
		audioDir:    newAudioDir,
		ensureRecordings: func(ctx context.Context, session storage.Session) (deletionRecordingSet, error) {
			return restoreDeletionRecordings(ctx, &session, newAudioDir, drive)
		},
		splice: fakeSplice,
	}
	if err := runner.Delete(context.Background(), "session", segmentID); err != nil {
		t.Fatalf("delete segment: %v", err)
	}
	if got, err := os.ReadFile(recording); err != nil || string(got) != "spliced" {
		t.Errorf("the recording where the entry points = %q (%v), want it spliced", got, err)
	}
	if len(drive.downloaded) != 0 {
		t.Errorf("downloaded %v from Drive, want none: the recording is on disk where its entry points", drive.downloaded)
	}
	if _, err := os.Stat(filepath.Join(newAudioDir, filepath.Base(recording))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a copy of the recording is in the new audio directory: %v", err)
	}
}

func TestStartupResplicesAnInterruptedDeletion(t *testing.T) {
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	original := []byte("original")
	if err := os.WriteFile(recording, original, 0o600); err != nil {
		t.Fatalf("write recording: %v", err)
	}
	store, segmentID, started := newDeletionTestStore(t, recording)
	held := holdRecording(t, recording)
	segments, err := store.GetSegments("session")
	if err != nil {
		t.Fatalf("get segments: %v", err)
	}
	if segmentID != segments[1].ID {
		t.Fatalf("segment ID = %d, want %d", segmentID, segments[1].ID)
	}
	afterHash := sha256.Sum256([]byte("spliced"))
	if err := store.DeleteSegment("session", segmentID, storage.DeletedSpan{
		SessionID:    "session",
		AudioPath:    recording,
		Start:        1,
		End:          2,
		SHA256Before: hashBytesForDeleteTest(original),
		SHA256After:  hex.EncodeToString(afterHash[:]),
	}); err != nil {
		t.Fatalf("record interrupted delete: %v", err)
	}
	var refined bool
	outcomes, err := recoverPendingSplices(context.Background(), store, filepath.Dir(recording), nil, fakeSplice, func(_ context.Context, sessionID, audioPath string, gotStarted time.Time) error {
		if sessionID != "session" || audioPath != recording || gotStarted != started {
			t.Fatalf("refine recovered splice session=%q audio=%q started=%s", sessionID, audioPath, gotStarted)
		}
		refined = true
		return nil
	})
	if err != nil {
		t.Fatalf("recover pending splice: %v", err)
	}
	if len(outcomes) != 1 || !outcomes[0].Recovered || outcomes[0].Err != nil || !refined {
		t.Fatalf("outcomes=%v refined=%v; want one recovered span and true", outcomes, refined)
	}
	contents, err := os.ReadFile(recording)
	if err != nil {
		t.Fatalf("read re-spliced recording: %v", err)
	}
	if !bytes.Equal(contents, []byte("spliced")) {
		t.Fatalf("re-spliced recording = %q", contents)
	}
	pending, err := store.PendingSplices(filepath.Dir(recording))
	if err != nil {
		t.Fatalf("check completed splice: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending splices after recovery = %+v", pending)
	}
	assertRecordingUnwritten(t, held, string(original))
}

// TestStartupRefinesADeletionCutOffAfterItsSplice fails if a segment
// deletion that the app stopped after its splice, but before its refinement
// and summary ran, is not refined and summarized as the app starts. The
// deletion left both pending, nothing else starts them, and the Drive
// replacement waits for both, so Drive would keep the deleted words. The
// cases are a stop after the spliced recording replaced the original but
// before the splice was marked complete, a stop after the mark, and a stop
// during the refinement (failed at the start, its summary never run). A
// deletion whose refinement and summary finished is not refined again.
func TestStartupRefinesADeletionCutOffAfterItsSplice(t *testing.T) {
	for _, tc := range []struct {
		name        string
		stop        func(t *testing.T, store *storage.SQLiteStore, span *storage.DeletedSpan)
		wantRefined bool
	}{
		{"stopped after the rename", func(*testing.T, *storage.SQLiteStore, *storage.DeletedSpan) {}, true},
		{"stopped after the splice was marked complete", func(t *testing.T, store *storage.SQLiteStore, span *storage.DeletedSpan) {
			if err := store.MarkSpliceComplete(span); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"stopped during the refinement", func(t *testing.T, store *storage.SQLiteStore, span *storage.DeletedSpan) {
			if err := store.MarkSpliceComplete(span); err != nil {
				t.Fatal(err)
			}
			if _, claimed, err := store.ClaimRefinement("session", span.AudioPath); err != nil || !claimed {
				t.Fatalf("claim refinement: %v, %v", claimed, err)
			}
			if err := store.RecoverStaleRefinements(); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"control: refinement and summary finished", func(t *testing.T, store *storage.SQLiteStore, span *storage.DeletedSpan) {
			if err := store.MarkSpliceComplete(span); err != nil {
				t.Fatal(err)
			}
			generation, claimed, err := store.ClaimRefinement("session", span.AudioPath)
			if err != nil || !claimed {
				t.Fatalf("claim refinement: %v, %v", claimed, err)
			}
			if err := store.UpdateRefinementAtGeneration("session", "before bridge after bridge", storage.RefinementCompleted, "", generation); err != nil {
				t.Fatal(err)
			}
			summaryGeneration, err := store.DeletionGeneration("session")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.StartSummary("session"); err != nil {
				t.Fatal(err)
			}
			if err := store.UpdateSummaryAtGeneration("session", "Title", "Summary", storage.SummaryCompleted, "default", "{}", "", summaryGeneration); err != nil {
				t.Fatal(err)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recording := filepath.Join(t.TempDir(), "recording.mp3")
			if err := os.WriteFile(recording, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			store, segmentID, started := newDeletionTestStore(t, recording)
			span := storage.DeletedSpan{
				SessionID: "session", AudioPath: recording, Start: 1, End: 2,
				SHA256Before: hashBytesForDeleteTest([]byte("original")), SHA256After: hashBytesForDeleteTest([]byte("spliced")),
			}
			if err := store.DeleteSegment("session", segmentID, span); err != nil {
				t.Fatalf("delete segment: %v", err)
			}
			// The spliced recording replaced the original, as the runner's
			// rename does, and then the app stopped.
			if err := os.WriteFile(recording, []byte("spliced"), 0o600); err != nil {
				t.Fatal(err)
			}
			tc.stop(t, store, &span)

			var refined []string
			outcomes, err := recoverPendingSplices(context.Background(), store, filepath.Dir(recording), nil, fakeSplice, func(_ context.Context, sessionID, audioPath string, gotStarted time.Time) error {
				if audioPath != recording || !gotStarted.Equal(started) {
					t.Errorf("refined session %s with audio %q started %s, want %q started %s", sessionID, audioPath, gotStarted, recording, started)
				}
				refined = append(refined, sessionID)
				return nil
			})
			if err != nil {
				t.Fatalf("startup recovery: %v", err)
			}
			for i := range outcomes {
				if outcomes[i].Err != nil {
					t.Errorf("startup recovery outcome for %s: %v", outcomes[i].Span.SessionID, outcomes[i].Err)
				}
			}
			if tc.wantRefined && !slices.Equal(refined, []string{"session"}) {
				t.Fatalf("refinements run at startup = %v, want the deletion's session refined once", refined)
			}
			if !tc.wantRefined && len(refined) != 0 {
				t.Fatalf("refinements run at startup = %v, want none for a deletion whose refinement and summary finished", refined)
			}
			if pending, err := store.HasPendingSplice("session"); err != nil || pending {
				t.Errorf("splice still pending after startup (%v, %v)", pending, err)
			}
		})
	}
}

func hashBytesForDeleteTest(value []byte) string {
	hash := sha256.Sum256(value)
	return hex.EncodeToString(hash[:])
}

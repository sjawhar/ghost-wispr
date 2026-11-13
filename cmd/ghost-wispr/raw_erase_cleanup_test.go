package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/erase"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

func TestStartupCleanupLeavesReservedRawEraseOutsideTheAudioDirectory(t *testing.T) {
	audioDir, rawDir := t.TempDir(), t.TempDir()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("session", started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("session", started.Add(time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	raw := filepath.Join(rawDir, "session.pcm")
	contents := []byte("raw recording protected after a failed restore")
	if err := os.WriteFile(raw, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	reservation, err := erase.Reserve(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReserveRawSessionErase("session", reservation.OriginalPath(), reservation.HiddenPath()); err != nil {
		t.Fatal(err)
	}
	if _, err := reservation.Begin(); err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(reservation.HiddenPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })

	if err := cleanupRecordingDirectories(store, audioDir); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(reservation.HiddenPath()); err != nil || !bytes.Equal(got, contents) {
		t.Fatalf("startup cleanup changed reserved raw to %q (%v)", got, err)
	}
	got := make([]byte, len(contents))
	if _, err := held.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, contents) {
		t.Fatalf("startup cleanup zeroed a reserved raw recording: %q", got)
	}
}

func holdRecording(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func assertRecordingUnwritten(t *testing.T, held *os.File, want string) {
	t.Helper()
	got := make([]byte, len(want))
	if _, err := held.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("recording bytes = %q, want %q", got, want)
	}
}

func TestCleanupSpliceTempCopies(t *testing.T) {
	dir := t.TempDir()
	held := map[string]*os.File{}
	for _, name := range []string{".delete-segment-before.mp3", ".splice-before.mp3", ".download-before.mp3", ".erasing-cut-off", "recording.mp3"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("audio of "+name), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		held[name] = holdRecording(t, filepath.Join(dir, name))
	}
	if err := cleanupSpliceTempCopiesExcept(dir, nil); err != nil {
		t.Fatalf("cleanup splice temp copies: %v", err)
	}
	for _, name := range []string{".delete-segment-before.mp3", ".splice-before.mp3", ".download-before.mp3", ".erasing-cut-off"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("temp copy %s remains: %v", name, err)
		}
		got := make([]byte, len("audio of "+name))
		if _, err := held[name].ReadAt(got, 0); err != nil {
			t.Fatalf("read removed %s: %v", name, err)
		}
		zeroed := bytes.Equal(got, make([]byte, len(got)))
		if name == ".erasing-cut-off" && !zeroed {
			t.Errorf("cut-off erase %s was removed still holding %q", name, got)
		}
		if name != ".erasing-cut-off" && zeroed {
			t.Errorf("splice copy %s was rewritten before it was removed", name)
		}
	}
	if got, err := os.ReadFile(filepath.Join(dir, "recording.mp3")); err != nil || string(got) != "audio of recording.mp3" {
		t.Fatalf("recording after cleanup = %q (%v), want it whole", got, err)
	}
}

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
		held[name] = holdRecording(t, path)
	}

	if err := cleanupRecordingDirectories(store, audioDir); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	for name, f := range held {
		if _, err := os.Stat(filepath.Join(elsewhere, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s beside a recording outside audio_dir remains: %v", name, err)
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
		t.Errorf("recording after cleanup = %q (%v), want it whole", got, err)
	}
}

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
		t.Errorf("cut-off erase of the deleted session's recording remains: %v", err)
	}
	got := make([]byte, len(audio))
	if _, err := held.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, make([]byte, len(got))) {
		t.Errorf("cut-off erase was removed still holding %q", got)
	}
	dirs, err := store.RecordingDirectories()
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(dirs, elsewhere) {
		t.Error("directory is still listed after cleanup finished it")
	}
}

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
		t.Errorf("directory whose cleanup failed was forgotten: %v", lister.forgotten)
	}
	if !slices.Contains(lister.forgotten, good) {
		t.Errorf("directory cleaned beside it was not forgotten: %v", lister.forgotten)
	}
}

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
		t.Errorf("forgotten %v, want audio directory once as %s", lister.forgotten, absolute)
	}
}

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
		t.Error("erase directory that was not there was forgotten")
	}
}

func TestStartupRecordsTheAudioDirectoryForALaterStart(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "cleanup.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	oldAudioDir, newAudioDir := t.TempDir(), t.TempDir()
	if err := cleanupRecordingDirectories(store, oldAudioDir); err != nil {
		t.Fatalf("cleanup at first start: %v", err)
	}
	audio := []byte("a saved session's raw recording")
	cutOff := filepath.Join(oldAudioDir, ".erasing-cut-off")
	if err := os.WriteFile(cutOff, audio, 0o600); err != nil {
		t.Fatal(err)
	}
	held := holdRecording(t, cutOff)

	if err := cleanupRecordingDirectories(store, newAudioDir); err != nil {
		t.Fatalf("cleanup at start with another audio_dir: %v", err)
	}
	if _, err := os.Stat(cutOff); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("cut-off erase in earlier audio directory remains: %v", err)
	}
	got := make([]byte, len(audio))
	if _, err := held.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, make([]byte, len(got))) {
		t.Errorf("cut-off erase was left holding %q", got)
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
	if err := deleteRefinedAudioForTest(t, store, "session", segmentID, &storage.DeletedSpan{
		SessionID:    "session",
		AudioPath:    recording,
		Start:        1,
		End:          2,
		SHA256Before: hashBytesForDeleteTest(original),
		SHA256After:  hashBytesForDeleteTest([]byte("spliced")),
	}); err != nil {
		t.Fatalf("record interrupted refined delete: %v", err)
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
	if got, err := os.ReadFile(recording); err != nil || !bytes.Equal(got, []byte("spliced")) {
		t.Fatalf("re-spliced recording = %q (%v)", got, err)
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

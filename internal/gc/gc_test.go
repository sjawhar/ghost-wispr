package gc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

type mockStore struct {
	sessions      map[string]storage.Session
	gcEligible    map[int][]string
	deletedIDs    []string
	remoteDeleted []string
}

// The mock serves age-based retention only; disk pressure runs against the
// real store.
func (m *mockStore) Recordings(bool) ([]storage.SessionRecording, error) { return nil, nil }

func (m *mockStore) MarkAudioRemoved(string, string, string, bool, bool, time.Time) (storage.AudioRemoval, bool, error) {
	return storage.AudioRemoval{}, false, nil
}

func (m *mockStore) RestoreAudio(*storage.AudioRemoval, string) error { return nil }

func (m *mockStore) GetGCEligibleSessions(maxAgeDays int, _ bool) ([]string, error) {
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

func (m *mockStore) DeleteEndedSession(id, _ string, _ bool) (bool, error) {
	m.deletedIDs = append(m.deletedIDs, id)
	return true, nil
}

func (m *mockStore) PurgeOldEvents(maxAge time.Duration) error {
	return nil
}

func (m *mockStore) MarkRemoteDeleted(id, _, _ string) (bool, error) {
	m.remoteDeleted = append(m.remoteDeleted, id)
	return true, nil
}

// driveFolders is Drive as GC asks it: every folder is on it, out of the
// trash, except those in gone; err, when set, is Drive not answering.
type driveFolders struct {
	gone  map[string]bool
	err   error
	asked []string
}

func (d *driveFolders) FolderOnDrive(_ context.Context, id string) (bool, error) {
	d.asked = append(d.asked, id)
	if d.err != nil {
		return false, d.err
	}
	return !d.gone[id], nil
}

func TestGCDeletesSyncedOldSessions(t *testing.T) {
	dir := t.TempDir()
	audioPath := filepath.Join(dir, "test.mp3")
	if err := os.WriteFile(audioPath, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := &mockStore{
		sessions: map[string]storage.Session{
			"s1": {ID: "s1", AudioPath: audioPath, GDriveFolderID: "folder-s1"},
		},
		gcEligible: map[int][]string{
			30: {"s1"},
		},
	}

	collector := New(store, Config{
		MaxAgeDays:     30,
		MaxAudioSizeMB: 1024,
		SyncGated:      true,
		Drive:          &driveFolders{},
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
			"s1": {ID: "s1", AudioPath: "", GDriveFolderID: "folder-s1"},
		},
		gcEligible: map[int][]string{
			30: {"s1"},
		},
	}

	collector := New(store, Config{
		MaxAgeDays:     30,
		MaxAudioSizeMB: 1024,
		SyncGated:      true,
		Drive:          &driveFolders{},
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
			"s1": {ID: "s1", AudioPath: "session.mp3", GDriveFolderID: "folder-s1"},
		},
		gcEligible: map[int][]string{
			30: {"s1"},
		},
	}

	collector := New(store, Config{
		MaxAgeDays:     30,
		MaxAudioSizeMB: 1024,
		SyncGated:      true,
		Drive:          &driveFolders{},
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

// writeRaw writes a raw recording of size bytes, last written at modTime, and
// returns its path.
func writeRaw(t *testing.T, dir, sessionID string, size int, modTime time.Time) string {
	t.Helper()
	path := filepath.Join(dir, sessionID+".pcm")
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatal(err)
	}
	return path
}

func expectKept(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("raw recording %s removed: %v", path, err)
	}
}

func expectRemoved(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("raw recording %s still on disk (stat: %v)", path, err)
	}
}

// daysAgo is the time n days before now.
func daysAgo(n int) time.Time {
	return time.Now().Add(-time.Duration(n) * 24 * time.Hour)
}

// A raw .pcm left beside a session's saved recording (its removal after the
// save failed) is a redundant copy: it goes when GC deletes the session.
func TestGCRemovesALeftoverRawRecordingOfASavedSession(t *testing.T) {
	dir := t.TempDir()
	saved := filepath.Join(dir, "20261106140930.wav")
	if err := os.WriteFile(saved, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw := writeRaw(t, dir, "20261106140930", 1024, time.Now())
	store := &mockStore{
		sessions:   map[string]storage.Session{"20261106140930": {ID: "20261106140930", Status: storage.SessionEnded, AudioPath: saved}},
		gcEligible: map[int][]string{30: {"20261106140930"}},
	}

	if _, err := New(store, Config{MaxAgeDays: 30, AudioDir: dir}).Run(); err != nil {
		t.Fatalf("gc run: %v", err)
	}
	expectRemoved(t, raw)
}

// A raw .pcm whose session's saved recording is gone from disk is not a
// redundant copy any more: GC deletes the session by its usual policy but
// keeps the raw recording.
func TestGCKeepsTheRawRecordingWhenTheSavedRecordingIsGone(t *testing.T) {
	dir := t.TempDir()
	raw := writeRaw(t, dir, "20261106140930", 1024, time.Now())
	store := &mockStore{
		sessions:   map[string]storage.Session{"20261106140930": {ID: "20261106140930", Status: storage.SessionEnded, AudioPath: filepath.Join(dir, "20261106140930.wav")}},
		gcEligible: map[int][]string{30: {"20261106140930"}},
	}

	deleted, err := New(store, Config{MaxAgeDays: 30, AudioDir: dir}).Run()
	if err != nil {
		t.Fatalf("gc run: %v", err)
	}
	if len(deleted) != 1 {
		t.Fatalf("deleted %v, want the session deleted by age", deleted)
	}
	expectKept(t, raw)
}

// newStore opens a real SQLite store.
func newStore(t *testing.T) *storage.SQLiteStore {
	t.Helper()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// transcriptSegments is how many transcript segments createSession gives a
// session.
const transcriptSegments = 2

// createSession creates a session with a transcript.
func createSession(t *testing.T, store *storage.SQLiteStore, id string, startedAt time.Time) {
	t.Helper()
	if err := store.CreateSession(id, startedAt); err != nil {
		t.Fatalf("create session %s: %v", id, err)
	}
	for i := range transcriptSegments {
		if err := store.AppendSegment(id, transcribe.Segment{Text: fmt.Sprintf("said in %s, part %d", id, i), StartTime: float64(i), EndTime: float64(i + 1), Timestamp: startedAt}); err != nil {
			t.Fatalf("append segment to %s: %v", id, err)
		}
	}
}

// endWithRecording creates an ended session whose saved recording of size
// bytes is in dir.
func endWithRecording(t *testing.T, store *storage.SQLiteStore, dir, id string, startedAt time.Time, size int) string {
	t.Helper()
	createSession(t, store, id, startedAt)
	saved := filepath.Join(dir, id+".mp3")
	if err := os.WriteFile(saved, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession(id, startedAt.Add(time.Minute), saved); err != nil {
		t.Fatalf("end session %s: %v", id, err)
	}
	return saved
}

// crashRecovered creates a session the app crashed inside: startup recovery
// ends it with no recording, and its raw .pcm of size bytes is the only copy.
func crashRecovered(t *testing.T, store *storage.SQLiteStore, dir, id string, startedAt time.Time, size int) string {
	t.Helper()
	createSession(t, store, id, startedAt)
	raw := writeRaw(t, dir, id, size, startedAt)
	if _, err := store.RecoverStaleSessions(); err != nil {
		t.Fatalf("recover stale sessions: %v", err)
	}
	return raw
}

// runGC runs GC passes times, as its hourly schedule does, and returns the
// sessions the passes deleted. A sync-gated config with no Drive gets one
// that has every folder: what a test checks is then kept or removed for its
// own reason, not because GC could not ask Drive.
func runGC(t *testing.T, store *storage.SQLiteStore, config Config, passes int) []string {
	t.Helper()
	if config.SyncGated && config.Drive == nil {
		config.Drive = &driveFolders{}
	}
	var deleted []string
	for range passes {
		ids, err := New(store, config).Run()
		if err != nil {
			t.Fatalf("gc run: %v", err)
		}
		deleted = append(deleted, ids...)
	}
	return deleted
}

// expectEveryRowAndTranscript fails the test unless each session is still in
// the store with its whole transcript.
func expectEveryRowAndTranscript(t *testing.T, store *storage.SQLiteStore, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := store.GetSession(id); err != nil {
			t.Errorf("session %s deleted: %v", id, err)
			continue
		}
		if n, err := store.CountSegments(id); err != nil || n != transcriptSegments {
			t.Errorf("session %s has %d transcript segments (%v), want %d", id, n, err, transcriptSegments)
		}
	}
}

// expectAudioRemoved fails the test unless GC removed session id's saved
// recording to free space: the file is gone and the session, kept, has no
// audio path, the marker the UI shows, and nothing left to refine.
func expectAudioRemoved(t *testing.T, store *storage.SQLiteStore, id, saved string) {
	t.Helper()
	expectRemoved(t, saved)
	sess, err := store.GetSession(id)
	if err != nil {
		t.Fatalf("read session %s: %v", id, err)
	}
	if sess.AudioPath != "" || sess.AudioRemovedAt == "" || sess.RefinementStatus != storage.RefinementNoAudio {
		t.Errorf("session %s after its audio was removed: audio_path %q, audio_removed_at %q, refinement_status %q; want none, set and %q",
			id, sess.AudioPath, sess.AudioRemovedAt, sess.RefinementStatus, storage.RefinementNoAudio)
	}
}

// expectAudioKept fails the test unless session id still has its saved
// recording, on disk and in its row.
func expectAudioKept(t *testing.T, store *storage.SQLiteStore, id, saved string) {
	t.Helper()
	if _, err := os.Stat(saved); err != nil {
		t.Errorf("saved recording %s removed: %v", saved, err)
	}
	sess, err := store.GetSession(id)
	if err != nil {
		t.Fatalf("read session %s: %v", id, err)
	}
	if sess.AudioPath != saved || sess.AudioRemovedAt != "" {
		t.Errorf("session %s: audio_path %q, audio_removed_at %q; want %q and none", id, sess.AudioPath, sess.AudioRemovedAt, saved)
	}
}

// readOnly makes dir refuse removals until the test ends.
func readOnly(t *testing.T, dir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root removes files from a read-only directory")
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
}

// Round-1 review: disk pressure ignores age and summary, and it removes only
// audio files, never a session. The raw .pcm of a session the app crashed
// inside, and the file of a start rolled back while still being written, are
// the only copies and stay. A healthy session's saved recording goes to make
// room; its row and transcript stay.
func TestGCKeepsTheOnlyCopyOfARecordingUnderDiskPressure(t *testing.T) {
	dir := t.TempDir()
	store := newStore(t)
	started := time.Now().UTC().Add(-time.Hour)

	crashed := crashRecovered(t, store, dir, "20261106100000", started, 2*1024*1024)

	createSession(t, store, "20261106101000", started.Add(10*time.Minute))
	if err := store.EndSession("20261106101000", started.Add(11*time.Minute), ""); err != nil {
		t.Fatalf("end rolled-back session: %v", err)
	}
	rolledBack := filepath.Join(dir, "20261106101000.pcm")
	open, err := os.OpenFile(rolledBack, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = open.Close() })
	if _, err := open.Write(make([]byte, 1024*1024)); err != nil {
		t.Fatal(err)
	}

	// Its saved recording alone is over the budget.
	healthy := endWithRecording(t, store, dir, "20261106102000", started.Add(20*time.Minute), 2*1024*1024)

	if deleted := runGC(t, store, Config{MaxAgeDays: 30, MaxAudioSizeMB: 1, AudioDir: dir}, 1); len(deleted) != 0 {
		t.Errorf("disk pressure deleted sessions %v; it may remove only audio files", deleted)
	}
	expectEveryRowAndTranscript(t, store, "20261106100000", "20261106101000", "20261106102000")
	expectKept(t, crashed)
	expectKept(t, rolledBack)
	expectAudioRemoved(t, store, "20261106102000", healthy)
}

// Round-2 review: raw recordings kept as the only copy are not counted
// against the budget, so 8 MB of them cannot make GC remove the healthy
// sessions' 1.5 MB of audio under a 4 MB budget.
func TestGCDiskPressureCountsOnlyRemovableAudio(t *testing.T) {
	dir := t.TempDir()
	store := newStore(t)
	started := time.Now().UTC().Add(-time.Hour)

	crashed := crashRecovered(t, store, dir, "20261106100000", started, 8*1024*1024)
	healthy := map[string]string{}
	for i, id := range []string{"20261106101000", "20261106102000", "20261106103000"} {
		healthy[id] = endWithRecording(t, store, dir, id, started.Add(time.Duration(i+1)*10*time.Minute), 512*1024)
	}

	if deleted := runGC(t, store, Config{MaxAgeDays: 30, MaxAudioSizeMB: 4, AudioDir: dir}, 1); len(deleted) != 0 {
		t.Errorf("disk pressure deleted sessions %v", deleted)
	}
	expectEveryRowAndTranscript(t, store, "20261106100000", "20261106101000", "20261106102000", "20261106103000")
	for id, saved := range healthy {
		expectAudioKept(t, store, id, saved)
	}
	expectKept(t, crashed)
}

// Round-3 review, blocking finding 1: audio disk pressure may not remove
// (a recording whose Drive sync failed for good, while sync gates GC, or a
// discarded session's) does not count against the budget, so it cannot make
// GC remove the healthy sessions' audio, let alone their rows.
func TestGCDiskPressureNeverCountsAudioItMayNotRemove(t *testing.T) {
	for _, tc := range []struct {
		name      string
		syncGated bool
		hold      func(t *testing.T, store *storage.SQLiteStore, id string)
	}{
		{name: "Drive sync failed for good", syncGated: true, hold: func(t *testing.T, store *storage.SQLiteStore, id string) {
			syncTo(t, store, id, storage.SyncStateFailed, "", 0, "")
		}},
		{name: "discarded sessions", hold: func(t *testing.T, store *storage.SQLiteStore, id string) {
			if err := store.DiscardSession(id); err != nil {
				t.Fatalf("discard %s: %v", id, err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store := newStore(t)
			started := time.Now().UTC().Add(-time.Hour)
			var held []string
			for i, id := range []string{"20261106100000", "20261106100500"} {
				held = append(held, endWithRecording(t, store, dir, id, started.Add(time.Duration(i)*time.Minute), 2*1024*1024))
				tc.hold(t, store, id)
			}
			healthy := map[string]string{}
			for i, id := range []string{"20261106101000", "20261106102000", "20261106103000"} {
				healthy[id] = endWithRecording(t, store, dir, id, started.Add(time.Duration(i+1)*10*time.Minute), 128*1024)
				syncTo(t, store, id, storage.SyncStateSynced, "folder", 0, "")
			}

			if deleted := runGC(t, store, Config{MaxAgeDays: 30, MaxAudioSizeMB: 1, SyncGated: tc.syncGated, AudioDir: dir}, 3); len(deleted) != 0 {
				t.Errorf("disk pressure deleted sessions %v", deleted)
			}
			expectEveryRowAndTranscript(t, store, "20261106100000", "20261106100500", "20261106101000", "20261106102000", "20261106103000")
			for id, saved := range healthy {
				expectAudioKept(t, store, id, saved)
			}
			for _, saved := range held {
				expectKept(t, saved)
			}
		})
	}
}

// Round-3 review, blocking finding 2: when the audio directory refuses
// removals, disk pressure frees nothing, so it changes nothing: every pass,
// as GC runs hourly, keeps every session, transcript and recording.
func TestGCDiskPressureKeepsEverySessionWhenRemovalsFail(t *testing.T) {
	dir := t.TempDir()
	store := newStore(t)
	started := time.Now().UTC().Add(-time.Hour)
	ids := []string{"20261106101000", "20261106102000", "20261106103000", "20261106104000"}
	saved := map[string]string{}
	for i, id := range ids {
		saved[id] = endWithRecording(t, store, dir, id, started.Add(time.Duration(i)*10*time.Minute), 2*1024*1024)
	}
	readOnly(t, dir)

	if deleted := runGC(t, store, Config{MaxAgeDays: 30, MaxAudioSizeMB: 1, AudioDir: dir}, 3); len(deleted) != 0 {
		t.Errorf("three passes deleted sessions %v without freeing a byte", deleted)
	}
	expectEveryRowAndTranscript(t, store, ids...)
	for id, path := range saved {
		expectAudioKept(t, store, id, path)
	}
}

// Round-3 review, blocking finding 2: a removable 1-byte raw copy beside each
// recording that cannot be removed does not make the pass look productive.
// The raw copies go, the recordings and every session stay.
func TestGCDiskPressureIsNotFooledByARemovableRawCopy(t *testing.T) {
	dir := t.TempDir()
	savedDir := t.TempDir()
	store := newStore(t)
	started := time.Now().UTC().Add(-time.Hour)
	ids := []string{"20261106101000", "20261106102000", "20261106103000", "20261106104000"}
	saved := map[string]string{}
	for i, id := range ids {
		saved[id] = endWithRecording(t, store, savedDir, id, started.Add(time.Duration(i)*10*time.Minute), 2*1024*1024)
		writeRaw(t, dir, id, 1, started)
	}
	readOnly(t, savedDir)

	if deleted := runGC(t, store, Config{MaxAgeDays: 30, MaxAudioSizeMB: 1, AudioDir: dir}, 3); len(deleted) != 0 {
		t.Errorf("disk pressure deleted sessions %v while freeing no saved audio", deleted)
	}
	expectEveryRowAndTranscript(t, store, ids...)
	for id, path := range saved {
		expectAudioKept(t, store, id, path)
		expectRemoved(t, filepath.Join(dir, id+".pcm"))
	}
}

// Round-3 review, red-team finding B4: an empty saved recording (a killed
// encoder on a full card) frees nothing, so it is skipped: its session stays,
// and so does a .pcm beside it, which is then the only real copy of the
// audio. The next session's recording goes instead.
func TestGCDiskPressureSkipsAnEmptyRecording(t *testing.T) {
	dir := t.TempDir()
	store := newStore(t)
	started := time.Now().UTC().Add(-time.Hour)
	empty := endWithRecording(t, store, dir, "20261106100000", started, 0)
	raw := writeRaw(t, dir, "20261106100000", 512*1024, started)
	healthy := endWithRecording(t, store, dir, "20261106101000", started.Add(10*time.Minute), 2*1024*1024)

	if deleted := runGC(t, store, Config{MaxAgeDays: 30, MaxAudioSizeMB: 1, AudioDir: dir}, 1); len(deleted) != 0 {
		t.Errorf("disk pressure deleted sessions %v", deleted)
	}
	expectEveryRowAndTranscript(t, store, "20261106100000", "20261106101000")
	expectAudioKept(t, store, "20261106100000", empty)
	expectKept(t, raw)
	expectAudioRemoved(t, store, "20261106101000", healthy)
}

// GC never removes the raw .pcm of a session that ended without a saved
// recording, ever, even once its row is gone: a row goes missing outside GC
// only when the database was lost or restored, which is when the raw files
// are the only copy.
func TestGCNeverRemovesARawRecordingThatIsTheOnlyCopy(t *testing.T) {
	t.Run("database restored from an older backup", func(t *testing.T) {
		dir := t.TempDir()
		store := newStore(t)
		createSession(t, store, "20251001120000", daysAgo(400))
		var raws []string
		for _, id := range []string{"20261001120000", "20261002120000", "20261003120000"} {
			raws = append(raws, writeRaw(t, dir, id, 1024, daysAgo(40)))
		}

		if _, err := New(store, Config{MaxAgeDays: 30, MaxAudioSizeMB: 1024, AudioDir: dir}).Run(); err != nil {
			t.Fatalf("gc run: %v", err)
		}
		for _, raw := range raws {
			expectKept(t, raw)
		}
	})

	t.Run("session deleted by age", func(t *testing.T) {
		dir := t.TempDir()
		store := newStore(t)
		createSession(t, store, "20251001120000", daysAgo(400))
		createSession(t, store, "20261001120000", daysAgo(40))
		if err := store.EndSession("20261001120000", daysAgo(40).Add(time.Hour), ""); err != nil {
			t.Fatalf("end session: %v", err)
		}
		if _, err := store.DB().Exec(`UPDATE sessions SET summary_status = 'completed' WHERE id = ?`, "20261001120000"); err != nil {
			t.Fatalf("mark summarized: %v", err)
		}
		raw := writeRaw(t, dir, "20261001120000", 1024, daysAgo(40))

		deleted, err := New(store, Config{MaxAgeDays: 30, MaxAudioSizeMB: 1024, AudioDir: dir}).Run()
		if err != nil {
			t.Fatalf("gc run: %v", err)
		}
		if len(deleted) != 1 || deleted[0] != "20261001120000" {
			t.Fatalf("deleted %v, want the session deleted by age", deleted)
		}
		expectKept(t, raw)
	})
}

// summarized ends session id, started at startedAt, with no recording and a
// completed summary, synced to Drive: the age policy deletes it once it is
// old enough.
func summarized(t *testing.T, store *storage.SQLiteStore, id string, startedAt time.Time) {
	t.Helper()
	createSession(t, store, id, startedAt)
	if err := store.EndSession(id, startedAt.Add(time.Minute), ""); err != nil {
		t.Fatalf("end session %s: %v", id, err)
	}
	if err := store.UpdateSummary(id, "", "A summary.", storage.SummaryCompleted, "default", "{}"); err != nil {
		t.Fatalf("summarize %s: %v", id, err)
	}
	syncTo(t, store, id, storage.SyncStateSynced, "folder", 0, "")
}

// expectNoSessionNamesAMissingFile fails the test unless every file the
// sessions' audio paths name is on disk: the UI offers to play what a
// session names.
func expectNoSessionNamesAMissingFile(t *testing.T, store *storage.SQLiteStore, ids ...string) {
	t.Helper()
	for _, id := range ids {
		sess, err := store.GetSession(id)
		if err != nil {
			t.Errorf("session %s deleted: %v", id, err)
			continue
		}
		for _, entry := range splitAudioPath(sess.AudioPath) {
			if _, err := os.Stat(entry); err != nil {
				t.Errorf("session %s names %s, which is gone (%v)", id, entry, err)
			}
		}
	}
}

// Review note 1: an age limit above 106,751 days wrapped the cutoff round
// into the future, so the age policy deleted every summarized session, an
// hour-old one too. No limit may wrap: one longer than any session has
// existed deletes nothing.
func TestGCAgeLimitNeverWraps(t *testing.T) {
	const recent, old = "20261106090000", "20161106090000"
	setup := func(t *testing.T) *storage.SQLiteStore {
		store := newStore(t)
		summarized(t, store, recent, time.Now().UTC().Add(-time.Hour))
		summarized(t, store, old, daysAgo(3650))
		return store
	}
	// The age policy runs: a 3,000-day limit deletes the ten-year-old session.
	if deleted := runGC(t, setup(t), Config{MaxAgeDays: 3000, SyncGated: true}, 1); !slices.Equal(deleted, []string{old}) {
		t.Fatalf("a 3000-day limit deleted %v, want [%s]", deleted, old)
	}
	for _, days := range []int{106751, 106752, 999999, 1000000, math.MaxInt} {
		store := setup(t)
		if deleted := runGC(t, store, Config{MaxAgeDays: days, SyncGated: true}, 1); len(deleted) != 0 {
			t.Errorf("gc_max_age_days %d deleted %v; no session is that old", days, deleted)
		}
		expectEveryRowAndTranscript(t, store, recent, old)
	}
}

// Review note 1: no age limit meant "never". 0 does: the age policy deletes
// nothing, however old.
func TestGCNoAgeLimitDeletesNothing(t *testing.T) {
	store := newStore(t)
	summarized(t, store, "20161106090000", daysAgo(3650))

	if deleted := runGC(t, store, Config{MaxAgeDays: 0, SyncGated: true}, 1); len(deleted) != 0 {
		t.Errorf("no age limit deleted %v", deleted)
	}
	expectEveryRowAndTranscript(t, store, "20161106090000")
}

// Review note 1: an audio budget at or above 2^43 MB wrapped negative in
// bytes, so every removable recording went. No budget may wrap: one larger
// than the audio removes nothing.
func TestGCAudioBudgetNeverWraps(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("budgets past 2^31 MB need a 64-bit int")
	}
	const id = "20261106100000"
	started := time.Now().UTC().Add(-time.Hour)
	// Disk pressure runs: a 1 MB budget removes the 4 MB recording.
	dir := t.TempDir()
	store := newStore(t)
	saved := endWithRecording(t, store, dir, id, started, 4*1024*1024)
	runGC(t, store, Config{MaxAgeDays: 30, MaxAudioSizeMB: 1, AudioDir: dir}, 1)
	expectAudioRemoved(t, store, id, saved)

	for _, mb := range []int64{1 << 40, 1 << 43, 9_000_000_000_000, math.MaxInt64} {
		dir := t.TempDir()
		store := newStore(t)
		saved := endWithRecording(t, store, dir, id, started, 4*1024*1024)
		runGC(t, store, Config{MaxAgeDays: 30, MaxAudioSizeMB: int(mb), AudioDir: dir}, 1)
		if _, err := os.Stat(saved); err != nil {
			t.Errorf("a %d MB budget removed a 4 MB recording: %v", mb, err)
		}
		expectAudioKept(t, store, id, saved)
	}
}

// Review note 1: an audio budget of 0 is no budget: disk pressure removes
// nothing.
func TestGCNoAudioBudgetRemovesNothing(t *testing.T) {
	dir := t.TempDir()
	store := newStore(t)
	saved := endWithRecording(t, store, dir, "20261106100000", time.Now().UTC().Add(-time.Hour), 4*1024*1024)

	runGC(t, store, Config{MaxAgeDays: 30, MaxAudioSizeMB: 0, AudioDir: dir}, 1)
	expectAudioKept(t, store, "20261106100000", saved)
}

// markFailsStore is the real store whose writes of the audio-removed marker
// fail, as a locked or full database's do.
type markFailsStore struct{ *storage.SQLiteStore }

func (markFailsStore) MarkAudioRemoved(string, string, string, bool, bool, time.Time) (storage.AudioRemoval, bool, error) {
	return storage.AudioRemoval{}, false, errors.New("database is locked")
}

// Review note 2: when the marker write failed, GC had already removed the
// file, so the session went on naming a file that was gone (its player
// offered to play it) and no later pass put it right. GC records a removal
// before it removes: what it cannot record, it does not remove.
func TestGCRemovesNoFileItCannotRecord(t *testing.T) {
	dir := t.TempDir()
	store := newStore(t)
	saved := endWithRecording(t, store, dir, "20261106100000", time.Now().UTC().Add(-time.Hour), 2*1024*1024)

	for range 2 {
		if _, err := New(markFailsStore{store}, Config{MaxAgeDays: 30, MaxAudioSizeMB: 1, AudioDir: dir}).Run(); err != nil {
			t.Fatalf("gc run: %v", err)
		}
	}
	expectAudioKept(t, store, "20261106100000", saved)
	expectNoSessionNamesAMissingFile(t, store, "20261106100000")
}

// Review note 2: removing a merged session's audio left its source sessions
// naming the files it removed.
func TestGCLeavesNoMergedSourceNamingARemovedFile(t *testing.T) {
	dir := t.TempDir()
	store := newStore(t)
	started := time.Now().UTC().Add(-time.Hour)
	a := endWithRecording(t, store, dir, "20261106100000", started, 1024*1024)
	b := endWithRecording(t, store, dir, "20261106101000", started.Add(10*time.Minute), 1024*1024)
	if err := store.MergeSessions("20261106100000-merged", []string{"20261106100000", "20261106101000"}, started, started.Add(11*time.Minute)); err != nil {
		t.Fatalf("merge: %v", err)
	}

	if deleted := runGC(t, store, Config{MaxAgeDays: 30, MaxAudioSizeMB: 1, AudioDir: dir}, 1); len(deleted) != 0 {
		t.Errorf("disk pressure deleted sessions %v", deleted)
	}
	expectRemoved(t, a)
	expectRemoved(t, b)
	expectNoSessionNamesAMissingFile(t, store, "20261106100000", "20261106101000", "20261106100000-merged")
}

// Review note 2: with two sessions naming one file (an imported or
// hand-edited row; nothing in the app makes one), GC removed the file for
// the first and left the second naming it, or, when the second's sync had
// failed, removed its only synced-or-not copy. A file another session names
// is not GC's to remove: it stays, for both.
func TestGCNeverRemovesAFileAnotherSessionNames(t *testing.T) {
	for _, tc := range []struct {
		name        string
		otherSynced bool
	}{
		{name: "both synced", otherSynced: true},
		{name: "the other's sync failed", otherSynced: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store := newStore(t)
			started := time.Now().UTC().Add(-time.Hour)
			saved := endWithRecording(t, store, dir, "20261106100000", started, 2*1024*1024)
			createSession(t, store, "20261106101000", started.Add(10*time.Minute))
			if err := store.EndSession("20261106101000", started.Add(11*time.Minute), saved); err != nil {
				t.Fatalf("end session: %v", err)
			}
			syncTo(t, store, "20261106100000", storage.SyncStateSynced, "folder", 0, "")
			other := storage.SyncStateFailed
			if tc.otherSynced {
				other = storage.SyncStateSynced
			}
			syncTo(t, store, "20261106101000", other, "folder", 0, "")

			runGC(t, store, Config{MaxAgeDays: 30, MaxAudioSizeMB: 1, SyncGated: true, AudioDir: dir}, 2)
			expectAudioKept(t, store, "20261106100000", saved)
			expectAudioKept(t, store, "20261106101000", saved)
		})
	}
}

// Review note 3: a session whose audio path also names a file that is gone
// (deleted by hand) stayed "refine pending" for ever once GC removed its
// other file. When GC removes the last file that is there, the session has no
// audio left to refine, and says so. The entry that names nothing stays in
// the row: GC removes from a row only what it removed itself, because a file
// it cannot see may be on a disk that is not mounted for the moment.
func TestGCMarksNoAudioWhenOnlyADeadEntryIsLeft(t *testing.T) {
	dir := t.TempDir()
	store := newStore(t)
	const id = "20261106100000-merged"
	createSession(t, store, id, time.Now().UTC().Add(-time.Hour))
	live := filepath.Join(dir, "20261106100000.mp3")
	if err := os.WriteFile(live, make([]byte, 2*1024*1024), 0o644); err != nil {
		t.Fatal(err)
	}
	dead := filepath.Join(dir, "20261106101000.mp3")
	if err := store.EndSession(id, time.Now().UTC(), live+","+dead); err != nil {
		t.Fatalf("end session: %v", err)
	}

	runGC(t, store, Config{MaxAgeDays: 30, MaxAudioSizeMB: 1, AudioDir: dir}, 1)
	expectRemoved(t, live)
	sess, err := store.GetSession(id)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	if sess.AudioPath != dead || sess.AudioRemovedAt == "" || sess.RefinementStatus != storage.RefinementNoAudio {
		t.Errorf("audio_path %q, audio_removed_at %q, refinement_status %q; want %q, set and %q",
			sess.AudioPath, sess.AudioRemovedAt, sess.RefinementStatus, dead, storage.RefinementNoAudio)
	}
}

// Review of #37, round 2, blocker 2: a merged session names a recording on
// another disk, which is not mounted during the pass that removes its other
// recording. GC removed that entry from the row with the file it removed, so
// once the disk was back no row named the recording. It keeps the entry.
func TestGCKeepsAnEntryOnADiskUnmountedDuringThePass(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "audio")
	mnt := filepath.Join(root, "mnt")
	for _, d := range []string{dir, mnt} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	store := newStore(t)
	const id = "20261106090000-merged"
	createSession(t, store, id, time.Now().UTC().Add(-time.Hour))
	other := filepath.Join(mnt, "20261106090000.mp3")
	here := filepath.Join(dir, "20261106100000.mp3")
	for _, path := range []string{other, here} {
		if err := os.WriteFile(path, make([]byte, 2*1024*1024), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.EndSession(id, time.Now().UTC(), other+","+here); err != nil {
		t.Fatalf("end session: %v", err)
	}

	unmounted := filepath.Join(root, "mnt-away")
	if err := os.Rename(mnt, unmounted); err != nil {
		t.Fatal(err)
	}
	runGC(t, store, Config{MaxAgeDays: 30, MaxAudioSizeMB: 1, AudioDir: dir}, 1)
	if err := os.Rename(unmounted, mnt); err != nil {
		t.Fatal(err)
	}

	expectRemoved(t, here)
	expectKept(t, other)
	sess, err := store.GetSession(id)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	if sess.AudioPath != other {
		t.Errorf("audio_path %q, want %q: the recording on the other disk is back and must stay named", sess.AudioPath, other)
	}
}

// logLines returns the JSON log lines in buf.
func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, raw := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		var line map[string]any
		if err := json.Unmarshal(raw, &line); err != nil {
			t.Fatalf("log line %q: %v", raw, err)
		}
		lines = append(lines, line)
	}
	return lines
}

// Review note 4: when part of a session's audio could not be removed, GC and
// the session said all of it was gone. The session keeps exactly what is
// left, and GC's log says what it removed and what is left.
func TestGCSaysWhatIsLeftOfAPartlyRemovedRecording(t *testing.T) {
	dir := t.TempDir()
	stuckDir := t.TempDir()
	store := newStore(t)
	const id = "20261106100000-merged"
	createSession(t, store, id, time.Now().UTC().Add(-time.Hour))
	gone := filepath.Join(dir, "20261106100000.mp3")
	stuck := filepath.Join(stuckDir, "20261106101000.mp3")
	for _, path := range []string{gone, stuck} {
		if err := os.WriteFile(path, make([]byte, 1024*1024), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.EndSession(id, time.Now().UTC(), gone+","+stuck); err != nil {
		t.Fatalf("end session: %v", err)
	}
	readOnly(t, stuckDir)

	var logs bytes.Buffer
	if _, err := New(store, Config{MaxAgeDays: 30, MaxAudioSizeMB: 1, AudioDir: dir}, slog.New(slog.NewJSONHandler(&logs, nil))).Run(); err != nil {
		t.Fatalf("gc run: %v", err)
	}

	expectRemoved(t, gone)
	sess, err := store.GetSession(id)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	if sess.AudioPath != stuck || sess.AudioRemovedAt == "" || sess.RefinementStatus != storage.RefinementPending {
		t.Errorf("partly removed session: audio_path %q, audio_removed_at %q, refinement_status %q; want %q, set and %q",
			sess.AudioPath, sess.AudioRemovedAt, sess.RefinementStatus, stuck, storage.RefinementPending)
	}
	said := false
	for _, line := range logLines(t, &logs) {
		if line["session_id"] != id || line["level"] != "INFO" {
			continue
		}
		removed, _ := json.Marshal(line["removed"])
		left, _ := json.Marshal(line["left"])
		if string(removed) == fmt.Sprintf("[%q]", gone) && string(left) == fmt.Sprintf("[%q]", stuck) {
			said = true
		}
	}
	if !said {
		t.Errorf("no log line says GC removed [%s] and left [%s]:\n%s", gone, stuck, logs.String())
	}
}

// refinementStartsStore is the real store with a refinement of session id
// starting right after GC lists the audio it may remove: the window between
// a session's end, which saves its audio path, and its refinement marking
// itself running.
type refinementStartsStore struct {
	*storage.SQLiteStore
	id string
}

func (s refinementStartsStore) Recordings(syncGated bool) ([]storage.SessionRecording, error) {
	recordings, err := s.SQLiteStore.Recordings(syncGated)
	if err == nil {
		err = s.UpdateRefinement(s.id, "", storage.RefinementRunning)
	}
	return recordings, err
}

// Review note 5: GC lists the audio it may remove before it removes it, so
// a refinement that started in between had its file taken mid-read; the
// session then ended marked removed with its refinement failed. GC removes
// nothing a refinement is reading.
func TestGCRemovesNoFileARefinementIsReading(t *testing.T) {
	dir := t.TempDir()
	store := newStore(t)
	const id = "20261106100000"
	saved := endWithRecording(t, store, dir, id, time.Now().UTC().Add(-time.Hour), 2*1024*1024)

	if _, err := New(refinementStartsStore{store, id}, Config{MaxAgeDays: 30, MaxAudioSizeMB: 1, AudioDir: dir}).Run(); err != nil {
		t.Fatalf("gc run: %v", err)
	}
	expectAudioKept(t, store, id, saved)
	if _, status, err := store.GetRefinement(id); err != nil || status != storage.RefinementRunning {
		t.Errorf("refinement_status %q (%v), want it still running", status, err)
	}
}

// Review note 6: a session whose saved recording is its .pcm (an imported
// or hand-edited row; the recorder saves .mp3 or .wav) had that file
// counted twice, as the recording and as "the redundant raw copy", and
// removed as the raw copy, without the marker. It is the recording: it
// counts once, and goes only as the recording, with the marker.
func TestGCTreatsAPcmSavedRecordingAsTheRecording(t *testing.T) {
	const id = "20261106100000"
	setup := func(t *testing.T) (*storage.SQLiteStore, string, string) {
		dir := t.TempDir()
		store := newStore(t)
		started := time.Now().UTC().Add(-time.Hour)
		createSession(t, store, id, started)
		pcm := writeRaw(t, dir, id, 3*1024*1024, started)
		if err := store.EndSession(id, started.Add(time.Minute), pcm); err != nil {
			t.Fatalf("end session: %v", err)
		}
		return store, dir, pcm
	}
	t.Run("counted once", func(t *testing.T) {
		store, dir, pcm := setup(t)
		runGC(t, store, Config{MaxAgeDays: 30, MaxAudioSizeMB: 4, AudioDir: dir}, 1)
		expectAudioKept(t, store, id, pcm)
	})
	t.Run("removed as the recording", func(t *testing.T) {
		store, dir, pcm := setup(t)
		runGC(t, store, Config{MaxAgeDays: 30, MaxAudioSizeMB: 1, AudioDir: dir}, 1)
		expectAudioRemoved(t, store, id, pcm)
	})
	t.Run("named by its absolute path, under a relative audio directory", func(t *testing.T) {
		store, dir, pcm := setup(t)
		t.Chdir(filepath.Dir(dir))
		runGC(t, store, Config{MaxAgeDays: 30, MaxAudioSizeMB: 4, AudioDir: filepath.Base(dir)}, 1)
		expectAudioKept(t, store, id, pcm)
	})
}

// Review note 7: sessions restored from Drive keep their original start
// times, so the next hourly pass deleted them again by age and a restore
// came to nothing. A restored session's age runs from its restore.
func TestGCKeepsSessionsJustRestoredFromDrive(t *testing.T) {
	store := newStore(t)
	const id = "20251001120000"
	started := daysAgo(400)
	ended := started.Add(time.Hour)
	// As the restore from Drive builds it.
	restored := storage.Session{
		ID: id, StartedAt: started, EndedAt: &ended, Status: storage.SessionEnded,
		Summary: "A summary.", SummaryStatus: storage.SummaryCompleted, SyncStatus: storage.SyncSynced,
	}
	segments := make([]transcribe.Segment, transcriptSegments)
	for i := range segments {
		segments[i] = transcribe.Segment{Text: fmt.Sprintf("said in %s, part %d", id, i), StartTime: float64(i), EndTime: float64(i + 1), Timestamp: started}
	}
	if err := store.ImportSession(&restored, segments); err != nil {
		t.Fatalf("restore: %v", err)
	}

	if deleted := runGC(t, store, Config{MaxAgeDays: 30, SyncGated: true}, 1); len(deleted) != 0 {
		t.Errorf("the pass after a restore deleted %v", deleted)
	}
	expectEveryRowAndTranscript(t, store, id)
}

// Review note 8: the age policy removed a session's raw .pcm because its
// saved recording was on disk before the pass, even when removing that
// recording then failed. It removes the raw copy only once the recording is
// gone.
func TestGCAgeKeepsTheRawCopyWhenTheRecordingStays(t *testing.T) {
	dir := t.TempDir()
	savedDir := t.TempDir()
	store := newStore(t)
	const id = "20261001120000"
	saved := endWithRecording(t, store, savedDir, id, daysAgo(40), 1024)
	if err := store.UpdateSummary(id, "", "A summary.", storage.SummaryCompleted, "default", "{}"); err != nil {
		t.Fatalf("summarize: %v", err)
	}
	raw := writeRaw(t, dir, id, 1024, daysAgo(40))
	readOnly(t, savedDir)

	if deleted := runGC(t, store, Config{MaxAgeDays: 30, AudioDir: dir}, 1); !slices.Equal(deleted, []string{id}) {
		t.Fatalf("deleted %v, want [%s] by age", deleted, id)
	}
	expectKept(t, saved)
	expectKept(t, raw)
}

// mergedMeanwhileStore is the real store with session id merged into another
// right after GC reads it for the age policy: the window between reading a
// session and deleting it.
type mergedMeanwhileStore struct {
	*storage.SQLiteStore
	id, into string
}

func (s mergedMeanwhileStore) GetSession(id string) (storage.Session, error) {
	sess, err := s.SQLiteStore.GetSession(id)
	if err == nil && id == s.id {
		err = s.MergeSessions(s.into, []string{id}, sess.StartedAt, sess.StartedAt.Add(time.Minute))
	}
	return sess, err
}

// A session merged into another as the age policy deletes it: GC read it
// naming its recording, then the merge gave that recording to the merged
// session. GC deletes a session only while it is as GC read it, so the
// merged session keeps its recording.
func TestGCAgeKeepsARecordingMergedAwayMeanwhile(t *testing.T) {
	dir := t.TempDir()
	store := newStore(t)
	const id, merged = "20261001120000", "20261001120000-merged"
	saved := endWithRecording(t, store, dir, id, daysAgo(40), 1024)
	if err := store.UpdateSummary(id, "", "A summary.", storage.SummaryCompleted, "default", "{}"); err != nil {
		t.Fatalf("summarize: %v", err)
	}

	if _, err := New(mergedMeanwhileStore{store, id, merged}, Config{MaxAgeDays: 30, AudioDir: dir}).Run(); err != nil {
		t.Fatalf("gc run: %v", err)
	}
	expectKept(t, saved)
	expectNoSessionNamesAMissingFile(t, store, merged)
}

// Review note 8: a pass that removed only a raw copy and then fitted the
// budget logged nothing, and an empty recording was logged as a warning with
// error=<nil>. Each removal is logged, and a skip that is not an error
// carries none.
func TestGCLogsARawCopyItRemovesAndNoNilError(t *testing.T) {
	dir := t.TempDir()
	store := newStore(t)
	started := time.Now().UTC().Add(-time.Hour)
	endWithRecording(t, store, dir, "20261106100000", started, 0)
	writeRaw(t, dir, "20261106100000", 1024*1024, started)
	endWithRecording(t, store, dir, "20261106101000", started.Add(10*time.Minute), 1024*1024)
	raw := writeRaw(t, dir, "20261106101000", 2*1024*1024, started)

	var logs bytes.Buffer
	if _, err := New(store, Config{MaxAgeDays: 30, MaxAudioSizeMB: 2, AudioDir: dir}, slog.New(slog.NewJSONHandler(&logs, nil))).Run(); err != nil {
		t.Fatalf("gc run: %v", err)
	}
	expectRemoved(t, raw)
	saidRaw := false
	for _, line := range logLines(t, &logs) {
		if line["level"] == "INFO" && line["session_id"] == "20261106101000" && line["raw_audio"] == raw {
			saidRaw = true
		}
		if errValue, ok := line["error"]; ok && errValue == nil {
			t.Errorf("log line with error=<nil>: %v", line)
		}
	}
	if !saidRaw {
		t.Errorf("no log line says GC removed %s:\n%s", raw, logs.String())
	}
}

// recordingsFailStore is the real store whose listing of recordings fails.
type recordingsFailStore struct{ *storage.SQLiteStore }

func (recordingsFailStore) Recordings(bool) ([]storage.SessionRecording, error) {
	return nil, errors.New("database is locked")
}

// Round-4 review: when the store cannot list the recordings, GC removes and
// deletes nothing, the age policy included (it cannot tell which files
// another session names).
func TestGCChangesNothingWhenItCannotListRecordings(t *testing.T) {
	dir := t.TempDir()
	store := newStore(t)
	old := endWithRecording(t, store, dir, "20261001120000", daysAgo(40), 1024)
	if err := store.UpdateSummary("20261001120000", "", "A summary.", storage.SummaryCompleted, "default", "{}"); err != nil {
		t.Fatalf("summarize: %v", err)
	}
	healthy := endWithRecording(t, store, dir, "20261106100000", time.Now().UTC().Add(-time.Hour), 2*1024*1024)

	deleted, err := New(recordingsFailStore{store}, Config{MaxAgeDays: 30, MaxAudioSizeMB: 1, AudioDir: dir}).Run()
	if err == nil || len(deleted) != 0 {
		t.Errorf("Run = %v, %v; want an error and nothing deleted", deleted, err)
	}
	expectEveryRowAndTranscript(t, store, "20261001120000", "20261106100000")
	expectAudioKept(t, store, "20261001120000", old)
	expectAudioKept(t, store, "20261106100000", healthy)
}

// Round-4 review: against a replaced (empty) database, no audio file in the
// audio directory is named by a session, so a pass at a 1 MB budget removes
// none of them, raw or saved.
func TestGCRemovesNothingAgainstAReplacedDatabase(t *testing.T) {
	dir := t.TempDir()
	store := newStore(t)
	var files []string
	for _, id := range []string{"20261001120000", "20261002120000"} {
		files = append(files, writeRaw(t, dir, id, 2*1024*1024, daysAgo(40)))
		saved := filepath.Join(dir, id+".mp3")
		if err := os.WriteFile(saved, make([]byte, 2*1024*1024), 0o644); err != nil {
			t.Fatal(err)
		}
		files = append(files, saved)
	}

	if deleted := runGC(t, store, Config{MaxAgeDays: 30, MaxAudioSizeMB: 1, AudioDir: dir}, 2); len(deleted) != 0 {
		t.Errorf("deleted %v from an empty database", deleted)
	}
	for _, f := range files {
		expectKept(t, f)
	}
}

// The age policy deletes no session while its refinement reads the
// recording: GC removes nothing a refinement is reading.
func TestGCAgeKeepsASessionWhileItsRefinementRuns(t *testing.T) {
	dir := t.TempDir()
	store := newStore(t)
	const id = "20261001120000"
	saved := endWithRecording(t, store, dir, id, daysAgo(40), 1024)
	if err := store.UpdateSummary(id, "", "A summary.", storage.SummaryCompleted, "default", "{}"); err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if err := store.UpdateRefinement(id, "", storage.RefinementRunning); err != nil {
		t.Fatalf("start refinement: %v", err)
	}

	if deleted := runGC(t, store, Config{MaxAgeDays: 30, AudioDir: dir}, 1); len(deleted) != 0 {
		t.Errorf("deleted %v while its refinement runs", deleted)
	}
	expectAudioKept(t, store, id, saved)
}

// A session whose recording is missing from disk is left as it is: GC
// changes a session's audio path only for a file it removes itself, because
// a missing file may be on an audio directory that is not mounted. (Rows that
// 5df1e1e left naming removed files were merged sources, which no longer
// name audio.)
func TestGCLeavesASessionWhoseRecordingIsMissingAlone(t *testing.T) {
	dir := t.TempDir()
	store := newStore(t)
	const id = "20261106100000"
	createSession(t, store, id, time.Now().UTC().Add(-time.Hour))
	missing := filepath.Join(dir, id+".mp3")
	if err := store.EndSession(id, time.Now().UTC(), missing); err != nil {
		t.Fatalf("end session: %v", err)
	}
	healthy := endWithRecording(t, store, dir, "20261106101000", time.Now().UTC().Add(-30*time.Minute), 2*1024*1024)

	runGC(t, store, Config{MaxAgeDays: 30, MaxAudioSizeMB: 1, AudioDir: dir}, 2)
	sess, err := store.GetSession(id)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	if sess.AudioPath != missing || sess.AudioRemovedAt != "" || sess.RefinementStatus != storage.RefinementPending {
		t.Errorf("session with a missing recording: audio_path %q, audio_removed_at %q, refinement_status %q; want it unchanged", sess.AudioPath, sess.AudioRemovedAt, sess.RefinementStatus)
	}
	expectAudioRemoved(t, store, "20261106101000", healthy)
}

// Review of #37, note 1: after audio_dir changes, a session's relative entry
// resolves into the new directory, where its file is not, while the file is
// still where the entry itself points (where the app plays and syncs it). GC
// drops an entry as naming nothing only when neither path has a file, so the
// row keeps naming that recording.
func TestGCKeepsAnEntryWhoseFileIsWhereTheEntryPoints(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	for _, dir := range []string{"old", "new"} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	store := newStore(t)
	const id = "20261106100000-merged"
	createSession(t, store, id, time.Now().UTC().Add(-time.Hour))
	moved, current := filepath.Join("old", "20261106100000.mp3"), filepath.Join("new", "20261106101000.mp3")
	for _, path := range []string{moved, current} {
		if err := os.WriteFile(path, make([]byte, 2*1024*1024), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.EndSession(id, time.Now().UTC(), moved+","+current); err != nil {
		t.Fatalf("end session: %v", err)
	}

	runGC(t, store, Config{MaxAgeDays: 30, MaxAudioSizeMB: 1, AudioDir: "new"}, 1)
	expectRemoved(t, current)
	expectKept(t, moved)
	sess, err := store.GetSession(id)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	if sess.AudioPath != moved {
		t.Errorf("audio_path %q, want %q: the recording at old/ is on disk and must stay named", sess.AudioPath, moved)
	}
}

package gc

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

type mockStore struct {
	sessions   map[string]storage.Session
	gcEligible map[int][]string
	deletedIDs []string
}

// The mock serves age-based retention only; disk pressure runs against the
// real store.
func (m *mockStore) RemovableRecordings(bool) ([]storage.SessionRecording, error) { return nil, nil }

func (m *mockStore) MarkAudioRemoved(string, string, time.Time) error { return nil }

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

func (m *mockStore) DeleteSession(id string) error {
	m.deletedIDs = append(m.deletedIDs, id)
	return nil
}

func (m *mockStore) PurgeOldEvents(maxAge time.Duration) error {
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
// sessions the passes deleted.
func runGC(t *testing.T, store *storage.SQLiteStore, config Config, passes int) []string {
	t.Helper()
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
			if err := store.UpdateSyncStatus(id, storage.SyncFailed, ""); err != nil {
				t.Fatalf("fail sync of %s: %v", id, err)
			}
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
				if err := store.UpdateSyncStatus(id, storage.SyncSynced, "folder"); err != nil {
					t.Fatalf("sync %s: %v", id, err)
				}
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

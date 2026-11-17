package main

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hajimehoshi/go-mp3"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/gdrive"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// keptDrive is a Drive that has the session's folder and takes every upload
// into it. For each recording uploaded it records whether the session's raw
// recording was still on disk at that moment.
type keptDrive struct {
	folderBackUploader
	raw string

	mu      sync.Mutex
	uploads map[string]bool // Drive name of a recording -> raw recording on disk at its upload
}

func (d *keptDrive) UploadInto(_ context.Context, _ string, files []gdrive.SyncFile) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := range files {
		if strings.HasPrefix(files[i].Name, "audio-") {
			_, err := os.Stat(d.raw)
			d.uploads[files[i].Name] = err == nil
		}
	}
	return nil
}

func (d *keptDrive) uploaded(name string) (uploaded, rawOnDisk bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rawOnDisk, uploaded = d.uploads[name]
	return uploaded, rawOnDisk
}

// requireMP3Encoder skips the test where ffmpeg, the encoder the recorder
// tries first, is not installed. CI installs it.
func requireMP3Encoder(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skipf("ffmpeg is not installed: %v", err)
	}
}

// tone is seconds of a 440 Hz tone as the recorder writes the microphone:
// 16 kHz, mono, signed 16-bit little-endian.
func tone(seconds int) []byte {
	const rate = 16000
	pcm := make([]byte, 2*rate*seconds)
	for i := range rate * seconds {
		sample := int16(8000 * math.Sin(2*math.Pi*440*float64(i)/rate))
		binary.LittleEndian.PutUint16(pcm[2*i:], uint16(sample))
	}
	return pcm
}

// mp3Seconds decodes the MP3 at path and returns how long it plays.
func mp3Seconds(t *testing.T, path string) float64 {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = file.Close() }()
	decoder, err := mp3.NewDecoder(file)
	if err != nil {
		t.Fatalf("%s is not an MP3: %v", path, err)
	}
	// go-mp3 decodes to 16-bit stereo: 4 bytes a sample.
	return float64(decoder.Length()) / float64(4*decoder.SampleRate())
}

// keptSession is a session as a power cut left one on the office Pi on
// 2026-11-17: the run that recorded it stopped inside it, so the next start
// ended it with no recording (storage.RecoverStaleSessions), and Drive sync
// had synced it without its audio. Its raw recording holds seconds of audio.
// It returns the raw recording's path.
func keptSession(t *testing.T, store *storage.SQLiteStore, audioDir, id string, seconds int) string {
	t.Helper()
	started := time.Date(2026, 11, 6, 14, 9, 30, 0, time.UTC)
	if err := store.CreateSession(id, started); err != nil {
		t.Fatalf("create session %s: %v", id, err)
	}
	stopped := audio.NewRecorder(audioDir)
	if err := stopped.StartSession(id); err != nil {
		t.Fatalf("start recording %s: %v", id, err)
	}
	if _, err := stopped.Writer(io.Discard).Write(tone(seconds)); err != nil {
		t.Fatalf("record %s: %v", id, err)
	}
	// The power cut: the session never ends in that run.
	if _, err := store.RecoverStaleSessions(); err != nil {
		t.Fatalf("recover stale sessions: %v", err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET sync_state = ?, sync_status = ?, gdrive_folder_id = 'folder-1' WHERE id = ?`,
		storage.SyncStateSynced, storage.SyncSynced, id); err != nil {
		t.Fatalf("sync %s without its audio: %v", id, err)
	}
	sess, err := store.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Status != storage.SessionEnded || sess.AudioPath != "" || sess.RefinementStatus != storage.RefinementNoAudio {
		t.Fatalf("control: the kept session is %q with audio %q and refinement %q, want ended with none and %q",
			sess.Status, sess.AudioPath, sess.RefinementStatus, storage.RefinementNoAudio)
	}
	return filepath.Join(audioDir, id+".pcm")
}

func openKeptStore(t *testing.T) *storage.SQLiteStore {
	t.Helper()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// A recording the app stopped inside (a power cut on the office Pi) is not
// left as a raw .pcm that nothing saves: the app saves it as the session's
// MP3, Drive sync uploads it, and the raw recording is erased only once Drive
// has it.
func TestARecordingTheAppStoppedInsideIsSavedSyncedThenErased(t *testing.T) {
	requireMP3Encoder(t)
	store := openKeptStore(t)
	audioDir := t.TempDir()
	const id = "20261106140930"
	raw := keptSession(t, store, audioDir, id, 2)

	recorder := audio.NewRecorder(audioDir)
	drive := &keptDrive{raw: raw, uploads: map[string]bool{}}
	orchestrator := gdrive.NewOrchestrator(drive, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	orchestrator.SetAudioDir(audioDir)
	syncer := &driveSync{}
	syncer.Set(orchestrator)
	manager := session.NewManager(store, recorder, nil, nil, session.NewDetector(time.Hour), 0)
	kept := newKeptRecordings(store, recorder, audioDir, refineRecoveredRecording(manager))

	// As the app starts: the recording is saved and becomes the session's,
	// which goes back to sync in its folder. Drive does not have it yet, so
	// its raw recording stays.
	kept.run(context.Background())
	saved := filepath.Join(audioDir, id+".mp3")
	sess, err := store.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	if sess.AudioPath != saved {
		t.Fatalf("after the start the session names recording %q, want %q", sess.AudioPath, saved)
	}
	if sess.SyncState != storage.SyncStatePending || sess.GDriveFolderID != "folder-1" {
		t.Errorf("after the start the session is %q in folder %q, want %q in folder-1 so Drive gets its recording",
			sess.SyncState, sess.GDriveFolderID, storage.SyncStatePending)
	}
	if seconds := mp3Seconds(t, saved); seconds < 1.9 || seconds > 2.3 {
		t.Errorf("the saved MP3 plays %.2f s, want the 2 s recorded", seconds)
	}
	if _, err := os.Stat(raw); err != nil {
		t.Fatalf("the raw recording is gone before Drive has the saved one: %v", err)
	}

	// The sync sweep uploads it; the raw recording goes only after that.
	ctx, cancel := context.WithCancel(context.Background())
	swept := goDone(func() { sweepDriveSync(ctx, store, syncer, newWarningsRegistry(), time.Millisecond, kept) })
	defer func() {
		cancel()
		if !returnedWithin(swept, testWait) {
			t.Error("the sync sweep did not stop")
		}
	}()
	for deadline := time.Now().Add(testWait); ; time.Sleep(time.Millisecond) {
		if _, err := os.Stat(raw); errors.Is(err, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the raw recording is still on disk after the sync sweep ran")
		}
	}
	uploaded, rawOnDisk := drive.uploaded("audio-" + id + ".mp3")
	if !uploaded {
		t.Fatal("the raw recording was erased, but Drive never got the saved recording")
	}
	if !rawOnDisk {
		t.Error("the raw recording was erased before Drive got the saved recording")
	}
	if sess, err = store.GetSession(id); err != nil {
		t.Fatal(err)
	}
	if sess.SyncState != storage.SyncStateSynced || sess.AudioPath != saved {
		t.Errorf("at the end the session is %q naming %q, want synced naming %q", sess.SyncState, sess.AudioPath, saved)
	}
	if _, err := os.Stat(saved); err != nil {
		t.Errorf("the saved recording is gone: %v", err)
	}
}

// While Drive does not have the saved recording (sync off, or each upload
// failing), the raw recording stays beside it.
func TestAKeptRecordingStaysRawUntilDriveHasIt(t *testing.T) {
	requireMP3Encoder(t)
	store := openKeptStore(t)
	audioDir := t.TempDir()
	const id = "20261106140930"
	raw := keptSession(t, store, audioDir, id, 1)
	kept := newKeptRecordings(store, audio.NewRecorder(audioDir), audioDir, nil)

	kept.run(context.Background())
	syncSweep(context.Background(), store, &driveSync{}) // sync off
	kept.run(context.Background())

	if _, err := os.Stat(filepath.Join(audioDir, id+".mp3")); err != nil {
		t.Fatalf("the recording was not saved: %v", err)
	}
	if _, err := os.Stat(raw); err != nil {
		t.Errorf("the raw recording is gone though Drive never got the saved one: %v", err)
	}
}

// refusingKeptStore refuses to record the kept recording, as the store does
// for a session deleted, or given a recording, while it was being saved.
type refusingKeptStore struct{ *storage.SQLiteStore }

func (refusingKeptStore) RecordKeptRecording(string, string) (bool, error) { return false, nil }

// A recording saved for a session that changed meanwhile (deleted while its
// raw recording was being saved) belongs to no session: it is erased, and the
// raw recording is left as it is.
func TestARecordingSavedForASessionThatChangedIsErased(t *testing.T) {
	requireMP3Encoder(t)
	store := openKeptStore(t)
	audioDir := t.TempDir()
	const id = "20261106140930"
	raw := keptSession(t, store, audioDir, id, 1)
	finished := false
	kept := newKeptRecordings(refusingKeptStore{store}, audio.NewRecorder(audioDir), audioDir,
		func(context.Context, string, string, time.Time) { finished = true })

	kept.run(context.Background())

	if _, err := os.Stat(filepath.Join(audioDir, id+".mp3")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the recording saved for a session that refused it is still on disk (%v)", err)
	}
	if _, err := os.Stat(raw); err != nil {
		t.Errorf("the raw recording is gone: %v", err)
	}
	if finished {
		t.Error("the refinement of a recording the session refused started")
	}
}

// The recording under way, and a raw recording no session row names, are
// not saved.
func TestOnlyTheRecordingsOfEndedSessionsAreSaved(t *testing.T) {
	requireMP3Encoder(t)
	store := openKeptStore(t)
	audioDir := t.TempDir()
	recorder := audio.NewRecorder(audioDir)
	const live, orphan = "20261106150000", "20261106120000"
	if err := store.CreateSession(live, time.Date(2026, 11, 6, 15, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if err := recorder.StartSession(live); err != nil {
		t.Fatal(err)
	}
	if _, err := recorder.Writer(io.Discard).Write(tone(1)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(audioDir, orphan+".pcm"), tone(1), 0o644); err != nil {
		t.Fatal(err)
	}

	newKeptRecordings(store, recorder, audioDir, nil).run(context.Background())

	for _, id := range []string{live, orphan} {
		if _, err := os.Stat(filepath.Join(audioDir, id+".mp3")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a recording was saved for %s (%v)", id, err)
		}
		if _, err := os.Stat(filepath.Join(audioDir, id+".pcm")); err != nil {
			t.Errorf("the raw recording of %s is gone: %v", id, err)
		}
	}
}

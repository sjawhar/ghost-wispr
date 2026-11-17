package main

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hajimehoshi/go-mp3"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/erase"
	"github.com/sjawhar/ghost-wispr/internal/gdrive"
	"github.com/sjawhar/ghost-wispr/internal/server"
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
	if _, err := store.RecoverStaleSessions(audioDir); err != nil {
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
	kept := newKeptRecordings(store, recorder, audioDir, syncer, refineRecoveredRecording(manager))

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

// syncingDrive returns a Drive sync that is on, with a Drive that takes every
// upload.
func syncingDrive(t *testing.T, store *storage.SQLiteStore, audioDir, raw string) *driveSync {
	t.Helper()
	orchestrator := gdrive.NewOrchestrator(&keptDrive{raw: raw, uploads: map[string]bool{}}, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	orchestrator.SetAudioDir(audioDir)
	drive := &driveSync{}
	drive.Set(orchestrator)
	return drive
}

// While Drive will take the saved recording (sync on, the session waiting
// for its sync or its retry), the raw recording stays beside it until Drive
// has it.
func TestAKeptRecordingStaysRawWhileDriveWillTakeIt(t *testing.T) {
	requireMP3Encoder(t)
	for _, state := range []string{storage.SyncStateSynced, storage.SyncStatePending, storage.SyncStateRetryScheduled} {
		t.Run(state, func(t *testing.T) {
			store := openKeptStore(t)
			audioDir := t.TempDir()
			const id = "20261106140930"
			raw := keptSession(t, store, audioDir, id, 1)
			if _, err := store.DB().Exec(`UPDATE sessions SET sync_state = ? WHERE id = ?`, state, id); err != nil {
				t.Fatal(err)
			}
			kept := newKeptRecordings(store, audio.NewRecorder(audioDir), audioDir, syncingDrive(t, store, audioDir, raw), nil)

			kept.run(context.Background())
			kept.run(context.Background())

			if sess, err := store.GetSession(id); err != nil || sess.AudioPath != filepath.Join(audioDir, id+".mp3") {
				t.Fatalf("the session names %q (%v), want the saved recording", sess.AudioPath, err)
			}
			if _, err := os.Stat(raw); err != nil {
				t.Errorf("the raw recording is gone though Drive will take the saved one and does not have it: %v", err)
			}
		})
	}
}

// Review of #76, P2: where Drive cannot take the saved recording (sync off,
// or the session FAILED or REMOTE_DELETED), the raw recording is erased as
// soon as the saved recording is the session's, as a session end erases it:
// the saved recording is on disk, so the raw one is never the last copy, and
// a raw recording kept beside it would refuse "Delete text and audio" for
// good. Drive can still get the saved recording later.
func TestAKeptRecordingIsErasedOnceSavedWhenDriveCannotTakeIt(t *testing.T) {
	requireMP3Encoder(t)
	for _, tc := range []struct {
		name   string
		state  string
		syncOn bool
	}{
		{name: "sync off", state: storage.SyncStateSynced},
		{name: "failed", state: storage.SyncStateFailed, syncOn: true},
		{name: "remote deleted", state: storage.SyncStateRemoteDeleted, syncOn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openKeptStore(t)
			audioDir := t.TempDir()
			const id = "20261106140930"
			raw := keptSession(t, store, audioDir, id, 1)
			if _, err := store.DB().Exec(`UPDATE sessions SET sync_state = ? WHERE id = ?`, tc.state, id); err != nil {
				t.Fatal(err)
			}
			drive := &driveSync{}
			if tc.syncOn {
				drive = syncingDrive(t, store, audioDir, raw)
			}
			kept := newKeptRecordings(store, audio.NewRecorder(audioDir), audioDir, drive, nil)

			kept.run(context.Background())

			saved := filepath.Join(audioDir, id+".mp3")
			if sess, err := store.GetSession(id); err != nil || sess.AudioPath != saved {
				t.Fatalf("the session names %q (%v), want %s", sess.AudioPath, err, saved)
			}
			if _, err := os.Stat(saved); err != nil {
				t.Fatalf("the saved recording is not on disk: %v", err)
			}
			if _, err := os.Stat(raw); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the raw recording is still on disk after the pass saved it, and Drive cannot take it (%v)", err)
			}
		})
	}
}

// refusingKeptStore refuses to record the kept recording, as the store does
// for a session deleted, or given a recording, while it was being saved.
type refusingKeptStore struct{ *storage.SQLiteStore }

func (refusingKeptStore) RecordKeptRecording(string, string, func() error) (bool, error) {
	return false, nil
}

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
	kept := newKeptRecordings(refusingKeptStore{store}, audio.NewRecorder(audioDir), audioDir, nil,
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

// The recording under way is neither saved nor erased. A raw recording no
// session row names, and no delete recorded, is neither saved nor erased: it
// waits for a person (a restored database backup leaves such captures).
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

	newKeptRecordings(store, recorder, audioDir, nil, nil).run(context.Background())

	for _, id := range []string{live, orphan} {
		if _, err := os.Stat(filepath.Join(audioDir, id+".mp3")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a recording was saved for %s (%v)", id, err)
		}
		if _, err := os.Stat(filepath.Join(audioDir, id+".pcm")); err != nil {
			t.Errorf("the raw recording of %s is gone: %v", id, err)
		}
	}
}

// A stop after a session end saved its recording and erased its raw
// recording, but before the session named the saved one, leaves that
// recording named by no session, and the next start ends the session with
// none. The start-up pass gives the session that recording, and starts its
// refinement; where the save left a WAV beside a failed MP3, the WAV is the
// recording. A saved recording of a session that has one, or of a session
// still recording, is left as it is.
func TestASavedRecordingNoSessionNamesIsGivenToItsSession(t *testing.T) {
	store := openKeptStore(t)
	audioDir := t.TempDir()
	started := time.Date(2026, 11, 6, 14, 9, 30, 0, time.UTC)
	const mp3Only, wavBeside, named, live = "20261106140930", "20261106141000", "20261106142000", "20261106143000"
	files := map[string][]string{mp3Only: {".mp3"}, wavBeside: {".mp3", ".wav"}, named: {".mp3"}, live: {".mp3"}}
	for id, exts := range files {
		if err := store.CreateSession(id, started); err != nil {
			t.Fatal(err)
		}
		for _, ext := range exts {
			if err := os.WriteFile(filepath.Join(audioDir, id+ext), []byte(id+ext), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := store.EndSession(named, started.Add(time.Minute), filepath.Join(audioDir, "elsewhere.mp3")); err != nil {
		t.Fatal(err)
	}
	// The stop: the next start ends every session still recording with no
	// recording; one more session has started since.
	if _, err := store.RecoverStaleSessions(audioDir); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET status = ? WHERE id = ?`, storage.SessionActive, live); err != nil {
		t.Fatal(err)
	}
	finished := map[string]string{}
	var mu sync.Mutex
	kept := newKeptRecordings(store, audio.NewRecorder(audioDir), audioDir, nil, func(_ context.Context, id, audioPath string, _ time.Time) {
		mu.Lock()
		defer mu.Unlock()
		finished[id] = audioPath
	})

	kept.run(context.Background())

	for id, want := range map[string]string{
		mp3Only:   filepath.Join(audioDir, mp3Only+".mp3"),
		wavBeside: filepath.Join(audioDir, wavBeside+".wav"),
		named:     filepath.Join(audioDir, "elsewhere.mp3"),
		live:      "",
	} {
		sess, err := store.GetSession(id)
		if err != nil {
			t.Fatal(err)
		}
		if sess.AudioPath != want {
			t.Errorf("session %s names recording %q, want %q", id, sess.AudioPath, want)
		}
	}
	for deadline := time.Now().Add(testWait); ; time.Sleep(time.Millisecond) {
		mu.Lock()
		n := len(finished)
		mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if finished[mp3Only] != filepath.Join(audioDir, mp3Only+".mp3") || finished[wavBeside] != filepath.Join(audioDir, wavBeside+".wav") || len(finished) != 2 {
		t.Errorf("refinements started = %v, want one for each recording given to its session", finished)
	}
	for id, exts := range files {
		for _, ext := range exts {
			if got, err := os.ReadFile(filepath.Join(audioDir, id+ext)); err != nil || string(got) != id+ext {
				t.Errorf("%s%s after the pass = %q (%v), want it as it was", id, ext, got, err)
			}
		}
	}
}

// restoreFromDrive imports session id as a Restore from Drive does, from a
// Drive folder that holds no audio, and fails the test if it cannot.
func restoreFromDrive(t *testing.T, store *storage.SQLiteStore, id string) {
	t.Helper()
	started := time.Date(2026, 11, 6, 14, 9, 30, 0, time.UTC)
	ended := started.Add(time.Hour)
	rs := gdrive.RestoredSession{ID: id, Title: "Restored", StartedAt: started, EndedAt: &ended, DriveFolderID: "folder-1"}
	restored := restoredDriveSession(&rs, ended)
	if err := store.ImportSession(&restored, nil, nil); err != nil {
		t.Fatalf("restore %s from Drive: %v", id, err)
	}
}

// Review of #76, P1 (the reviewer's repro): a person deletes a session while
// the kept recordings pass saves its raw recording, and the app stops before
// the pass finds the session gone. The deleted conversation's saved
// recording must not survive on the unit: else a later Restore from Drive,
// which brings the session back with no recording, gives it that recording
// and queues it for Drive. A session deleted the same way with no save under
// way is the control.
func TestADeletedSessionLeavesNoSavedRecordingToRestore(t *testing.T) {
	requireMP3Encoder(t)
	store := openKeptStore(t)
	audioDir := t.TempDir()
	const deleted, control = "20261106140930", "20261106150000"
	raws := map[string]string{
		deleted: keptSession(t, store, audioDir, deleted, 1),
		control: keptSession(t, store, audioDir, control, 1),
	}
	recorder := audio.NewRecorder(audioDir)
	// The save under way: the encoder has written the recording.
	if _, err := recorder.SaveKept(context.Background(), deleted); err != nil {
		t.Fatalf("save %s: %v", deleted, err)
	}
	// The delete, as the store and the raw erase do it; then the app stops
	// before the save records anything.
	for id, raw := range raws {
		if _, err := store.DeleteSession(id, audioDir); err != nil {
			t.Fatalf("delete %s: %v", id, err)
		}
		if err := erase.Remove(raw); err != nil {
			t.Fatal(err)
		}
	}

	// The next start.
	newKeptRecordings(store, audio.NewRecorder(audioDir), audioDir, nil, nil).run(context.Background())
	entries, err := os.ReadDir(audioDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), deleted) {
			t.Errorf("after the next start: %s of the deleted session is still on disk", entry.Name())
		}
	}

	for id := range raws {
		restoreFromDrive(t, store, id)
	}
	newKeptRecordings(store, audio.NewRecorder(audioDir), audioDir, nil, nil).run(context.Background())
	for id := range raws {
		sess, err := store.GetSession(id)
		if err != nil {
			t.Fatal(err)
		}
		if sess.AudioPath != "" {
			t.Errorf("after the restore and a pass: session %s audio_path=%q sync_state=%s refinement=%s, want no recording", id, sess.AudioPath, sess.SyncState, sess.RefinementStatus)
		}
	}
}

// A session delete holds the session's audio first (keptRecordings.Hold): a
// save of its raw recording under way is stopped, its encoder killed, before
// the hold returns, so no encoder writes a file of the session after the
// delete erased it, and the save records nothing. The encoder here writes
// its output, then hangs, as a long recording's encode does on a Pi.
func TestAHoldStopsASaveOfTheSessionAndNothingIsRecorded(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\nfor out; do :; done\nprintf 'partial' > \"$out\"\nexec /bin/sleep 3600\n"
	if err := os.WriteFile(filepath.Join(bin, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	store := openKeptStore(t)
	audioDir := t.TempDir()
	const id = "20261106140930"
	keptSession(t, store, audioDir, id, 1)
	kept := newKeptRecordings(store, audio.NewRecorder(audioDir), audioDir, nil, nil)

	passed := goDone(func() { kept.run(context.Background()) })
	temp := filepath.Join(audioDir, ".kept-"+id+".mp3")
	for deadline := time.Now().Add(testWait); ; time.Sleep(time.Millisecond) {
		if _, err := os.Stat(temp); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the save never started encoding")
		}
	}
	release := kept.Hold([]string{id})
	// What the hold leaves, before the delete would erase anything: the
	// save has ended, its encoder is gone and it wrote nothing that stays.
	entries, err := os.ReadDir(audioDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if name := entry.Name(); name != id+".pcm" {
			t.Errorf("when the hold returned, the save of the held session had left %s", name)
		}
	}
	if sess, err := store.GetSession(id); err != nil || sess.AudioPath != "" {
		t.Errorf("the held session names recording %q (%v), want none", sess.AudioPath, err)
	}
	if !returnedWithin(passed, testWait) {
		t.Fatal("the pass did not end after the hold stopped its save")
	}
	release()
}

// Review of #76, P1: when a stop cuts a delete's own erase short, the audio
// files of the deleted session stay under their names with no row, for a
// later Restore from Drive to bring back. The delete records the session in
// its transaction (storage.DeletedSessionAudio), and the pass erases, with
// zeros, every audio file of a recorded session: its raw capture, MP3 and
// WAV, and the recordings its row named, unless another session names one.
// It then clears the record. A file of a session with no row that no delete
// recorded is left, and so is every file of a session that has a row.
func TestThePassErasesTheAudioOfARecordedDelete(t *testing.T) {
	store := openKeptStore(t)
	audioDir := t.TempDir()
	const deleted, namedElsewhere, other, unrecorded, kept = "20261106140930", "20261106150000", "20261106160000", "20261106170000", "20261106180000"
	write := func(name string) string {
		t.Helper()
		path := filepath.Join(audioDir, name)
		if err := os.WriteFile(path, []byte("conversation "+name), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	gone := []string{write(deleted + ".pcm"), write(deleted + ".mp3"), write(deleted + ".wav"), write("imported-" + deleted + ".mp3")}
	held, err := os.Open(gone[1])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	shared := write(namedElsewhere + ".mp3")
	loose := write(unrecorded + ".mp3")
	own := write(kept + ".mp3")
	started := time.Date(2026, 11, 6, 14, 9, 30, 0, time.UTC)
	for id, recording := range map[string]string{
		deleted:        gone[1] + "," + gone[3],
		namedElsewhere: shared,
		other:          shared,
		kept:           filepath.Join(audioDir, "elsewhere.mp3"),
	} {
		if err := store.CreateSession(id, started); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, started.Add(time.Minute), recording); err != nil {
			t.Fatal(err)
		}
	}
	// The deletes commit, then a stop cuts their own erase short.
	for _, id := range []string{deleted, namedElsewhere} {
		if _, err := store.DeleteSession(id, audioDir); err != nil {
			t.Fatalf("delete %s: %v", id, err)
		}
	}

	newKeptRecordings(store, audio.NewRecorder(audioDir), audioDir, nil, nil).run(context.Background())

	for _, path := range gone {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s of a deleted session is still on disk (%v)", filepath.Base(path), err)
		}
	}
	got := make([]byte, len("conversation "+deleted+".mp3"))
	if _, err := held.ReadAt(got, 0); err == nil && strings.Contains(string(got), "conversation") {
		t.Errorf("the erased %s.mp3 still holds %q", deleted, got)
	}
	for _, path := range []string{shared, loose, own} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s is gone: %v", filepath.Base(path), err)
		}
	}
	if records, err := store.DeletedSessionAudio(); err != nil || len(records) != 0 {
		t.Errorf("deleted session records after the pass = %+v (%v), want none", records, err)
	}
}

// copyFile copies the file at from over to, and fails the test if it cannot.
func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Re-review of #76, P1 (the reviewer's repro): a person puts the
// pre-migration backup back over the database, the documented recovery
// (docs/solutions/schema-migration-safety.md). The sessions recorded since
// the backup have no row in it, but nobody deleted them: their audio stays,
// a crash session's only raw capture included, and the status names that
// capture as one for a person.
func TestARestoredBackupKeepsTheAudioRecordedSinceIt(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ghost-wispr.db")
	audioDir := t.TempDir()
	first, err := storage.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = first.Close()
	store, err := storage.NewSQLiteStore(dbPath) // takes the backup
	if err != nil {
		t.Fatal(err)
	}
	const saved, crashed = "20261116100000", "20261116110000"
	started := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	mp3 := filepath.Join(audioDir, saved+".mp3")
	pcm := filepath.Join(audioDir, crashed+".pcm")
	for id, path := range map[string]string{saved: mp3, crashed: pcm} {
		if err := store.CreateSession(id, started); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("conversation "+id), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.EndSession(saved, started.Add(time.Hour), mp3); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecoverStaleSessions(audioDir); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	copyFile(t, dbPath+".pre-migrate.bak", dbPath)
	for _, side := range []string{"-wal", "-shm"} {
		if err := os.Remove(dbPath + side); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	restored, err := storage.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restored.Close() })
	if _, err := restored.GetSession(crashed); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("control: the restored database has session %s (%v), want no row", crashed, err)
	}
	kept := newKeptRecordings(restored, audio.NewRecorder(audioDir), audioDir, nil, nil)

	kept.run(context.Background())

	for id, path := range map[string]string{saved: mp3, crashed: pcm} {
		if got, err := os.ReadFile(path); err != nil || string(got) != "conversation "+id {
			t.Errorf("after the pass %s holds %q (%v), want it as it was", filepath.Base(path), got, err)
		}
	}
	if census := kept.census(); !slices.Equal(census.ByHand, []string{crashed}) {
		t.Errorf("census names %v for a person, want [%s]", census.ByHand, crashed)
	}
}

// Review of #76, P2 and P3: the status counts every raw recording the app
// keeps, those beside a saved recording waiting for Drive too, and tells them
// apart: the ones the app tries again to save, the ones it erases once Drive
// has the saved recording, and, by session, the ones it neither saves nor
// erases, which wait for a person. A raw recording the next pass erases, and
// the one under way, are not counted.
func TestKeptRawCensusTellsTheKindsApart(t *testing.T) {
	store := openKeptStore(t)
	audioDir := t.TempDir()
	recorder := audio.NewRecorder(audioDir)
	started := time.Date(2026, 11, 6, 14, 9, 30, 0, time.UTC)
	const unsaved, awaiting, merged, unrecorded, deleted, live = "20261106100000", "20261106110000", "20261106120000", "20261106130000", "20261106140000", "20261106150000"
	raw := func(id string, size int) string {
		t.Helper()
		path := filepath.Join(audioDir, id+".pcm")
		if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for _, id := range []string{unsaved, awaiting, merged, deleted} {
		if err := store.CreateSession(id, started); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.RecoverStaleSessions(audioDir); err != nil {
		t.Fatal(err)
	}
	raw(unsaved, 1000)
	awaitingRaw := raw(awaiting, 2000)
	if err := os.WriteFile(filepath.Join(audioDir, awaiting+".mp3"), []byte("saved"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET audio_path = ?, sync_state = ? WHERE id = ?`, filepath.Join(audioDir, awaiting+".mp3"), storage.SyncStatePending, awaiting); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET status = ?, merged_into = 'elsewhere' WHERE id = ?`, storage.SessionMerged, merged); err != nil {
		t.Fatal(err)
	}
	raw(merged, 4000)
	raw(unrecorded, 8000) // no row, and no delete recorded it: a person's
	raw(deleted, 16000)   // a recorded delete: the next pass erases it
	if _, err := store.DeleteSession(deleted, audioDir); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(live, started); err != nil {
		t.Fatal(err)
	}
	if err := recorder.StartSession(live); err != nil {
		t.Fatal(err)
	}
	if _, err := recorder.Writer(io.Discard).Write(make([]byte, 32000)); err != nil {
		t.Fatal(err)
	}
	kept := newKeptRecordings(store, recorder, audioDir, syncingDrive(t, store, audioDir, awaitingRaw), nil)

	got := kept.census()
	slices.Sort(got.ByHand)
	want := server.KeptRawRecordings{Count: 4, Bytes: 1000 + 2000 + 4000 + 8000, Unsaved: 1, AwaitingDrive: 1, ByHand: []string{merged, unrecorded}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("census = %+v, want %+v", got, want)
	}
}

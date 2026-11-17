package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/erase"
	recordingname "github.com/sjawhar/ghost-wispr/internal/recording"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// keptRecordingStore reads a session and gives it the recording saved from
// its kept raw recording (storage.SQLiteStore).
type keptRecordingStore interface {
	GetSession(id string) (storage.Session, error)
	RecordKeptRecording(id, audioPath string) (bool, error)
}

// keptRecordingSaver lists the raw recordings the recorder is not using and
// saves one, keeping it (audio.Recorder).
type keptRecordingSaver interface {
	KeptRawSessions() []string
	SaveKept(sessionID string) (string, error)
}

// keptRecordings saves the raw recordings kept for sessions that ended
// without a recording: the app stopped inside the session (a power cut, a
// crash), so the next start ended it with none (storage.RecoverStaleSessions),
// or every save at its end failed. Nothing else saves such a raw recording,
// so the conversation's audio never reached Drive, as happened on the office
// Pi when it lost power inside a conversation on 2026-11-17.
//
// A pass (run) saves each such raw recording as a session end saves it
// (MP3, else WAV), makes it the session's recording, which sends a synced
// session back to sync, and starts what a session end starts after saving
// (finish: the refinement, then the summary and the Drive sync). It erases a
// raw recording only once its session is synced with the recording saved
// beside it, when Drive has that recording. The app runs a pass as it starts
// and after every Drive sync sweep, so a save that failed is tried again and
// a raw recording goes in the pass after the sweep that synced its recording.
// A raw recording no session names is left for recovery or deletion by hand.
//
// A pass also gives such a session the recording a session end saved, if no
// session names it and no raw recording is beside it: a stop after the end
// saved the recording and erased its raw recording, but before the session
// named the saved one, leaves exactly that. Where the end left a WAV beside
// an MP3, the WAV is the recording: the end saves one only once its MP3
// encoders failed.
type keptRecordings struct {
	store    keptRecordingStore
	recorder keptRecordingSaver
	audioDir string
	finish   func(ctx context.Context, sessionID, audioPath string, startedAt time.Time)

	// running keeps two passes (the start's and a sweep's) from saving one
	// raw recording at once.
	running sync.Mutex
}

func newKeptRecordings(store keptRecordingStore, recorder keptRecordingSaver, audioDir string, finish func(context.Context, string, string, time.Time)) *keptRecordings {
	return &keptRecordings{store: store, recorder: recorder, audioDir: audioDir, finish: finish}
}

// refineRecoveredRecording starts, for a recording saved from a kept raw
// recording, what a session end starts after saving its recording: the batch
// refinement, then the summary and the Drive sync. Without batch
// transcription it starts nothing; the Drive sync sweep uploads the
// recording.
func refineRecoveredRecording(manager *session.Manager) func(context.Context, string, string, time.Time) {
	return func(ctx context.Context, sessionID, audioPath string, startedAt time.Time) {
		if err := manager.RefineSession(ctx, sessionID, audioPath, startedAt); err != nil {
			log.Printf("the recording saved for session %s is not refined: %v", sessionID, err)
		}
	}
}

// run is one pass. It returns at once while another pass runs, and stops
// between raw recordings once ctx ends.
func (k *keptRecordings) run(ctx context.Context) {
	if k == nil || !k.running.TryLock() {
		return
	}
	defer k.running.Unlock()
	for _, id := range k.recorder.KeptRawSessions() {
		if ctx.Err() != nil {
			return
		}
		sess, ok := k.endedSession(id)
		if !ok {
			continue
		}
		switch {
		case sess.AudioPath == "" && sess.AudioRemovedAt == "":
			k.save(&sess)
		case sess.SyncState == storage.SyncStateSynced && k.savedBeside(sess.AudioPath, id):
			k.eraseRaw(id)
		}
	}
	for id, saved := range k.savedWithoutRaw() {
		if ctx.Err() != nil {
			return
		}
		if sess, ok := k.endedSession(id); ok && sess.AudioPath == "" && sess.AudioRemovedAt == "" {
			k.adopt(&sess, saved)
		}
	}
}

// endedSession returns session id, and whether it is ended. A session no row
// has is not, silently: its files are for recovery or deletion by hand.
func (k *keptRecordings) endedSession(id string) (storage.Session, bool) {
	sess, err := k.store.GetSession(id)
	if errors.Is(err, sql.ErrNoRows) {
		return storage.Session{}, false
	}
	if err != nil {
		log.Printf("warning: the recording kept for session %s: read the session: %v", id, err)
		return storage.Session{}, false
	}
	return sess, sess.Status == storage.SessionEnded
}

// savedWithoutRaw returns, by session, the recordings a session end saved
// in the audio directory (<id>.mp3 or <id>.wav, the WAV where there are
// both) with no raw recording beside them.
func (k *keptRecordings) savedWithoutRaw() map[string]string {
	entries, err := os.ReadDir(k.audioDir)
	if err != nil {
		return nil
	}
	saved := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		ext := filepath.Ext(name)
		if !entry.Type().IsRegular() || strings.HasPrefix(name, ".") || (ext != ".mp3" && ext != ".wav") {
			continue
		}
		id := strings.TrimSuffix(name, ext)
		if _, err := os.Lstat(recordingname.RawPath(k.audioDir, id)); !errors.Is(err, os.ErrNotExist) {
			continue
		}
		if _, found := saved[id]; !found || ext == ".wav" {
			saved[id] = filepath.Join(k.audioDir, name)
		}
	}
	return saved
}

// adopt makes saved, the recording a session end saved for sess, the
// session's recording, and starts what a session end starts after saving.
func (k *keptRecordings) adopt(sess *storage.Session, saved string) {
	recorded, err := k.store.RecordKeptRecording(sess.ID, saved)
	if err != nil {
		log.Printf("warning: session %s ended without naming its saved recording %s, and it could not be given it; the next pass tries again: %v", sess.ID, saved, err)
		return
	}
	if !recorded {
		return
	}
	log.Printf("session %s ended without naming its saved recording %s; it is the session's recording now", sess.ID, saved)
	if k.finish != nil {
		go k.finish(context.Background(), sess.ID, saved, sess.StartedAt)
	}
}

// save saves the raw recording kept for sess and makes it the session's
// recording. A recording the session no longer takes (it was deleted, merged
// or given a recording while this one was saved) is no session's, so it is
// erased; the raw recording is left for the next pass.
func (k *keptRecordings) save(sess *storage.Session) {
	saved, err := k.recorder.SaveKept(sess.ID)
	if err != nil {
		log.Printf("warning: the raw recording kept for session %s could not be saved; the next pass tries again: %v", sess.ID, err)
		return
	}
	recorded, err := k.store.RecordKeptRecording(sess.ID, saved)
	if err != nil || !recorded {
		k.eraseUnnamed(sess.ID, saved, err)
		return
	}
	log.Printf("saved the raw recording kept for session %s as %s; the raw recording is erased once Drive has it", sess.ID, saved)
	if k.finish != nil {
		go k.finish(context.Background(), sess.ID, saved, sess.StartedAt)
	}
}

// eraseUnnamed erases saved, a recording saved for session sessionID that the
// session did not take (refused, the reason recordErr if it failed), unless
// the session names it now.
func (k *keptRecordings) eraseUnnamed(sessionID, saved string, recordErr error) {
	if sess, err := k.store.GetSession(sessionID); err == nil && k.names(sess.AudioPath, saved) {
		return
	}
	if err := erase.Remove(saved); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("warning: session %s did not take the recording saved from its raw recording (%v), and %s could not be erased: %v", sessionID, recordErr, saved, err)
		return
	}
	log.Printf("session %s did not take the recording saved from its raw recording (%v); it is erased", sessionID, recordErr)
}

// eraseRaw erases session sessionID's raw recording, which Drive has saved.
func (k *keptRecordings) eraseRaw(sessionID string) {
	raw := recordingname.RawPath(k.audioDir, sessionID)
	if err := erase.Remove(raw); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("warning: the raw recording of session %s, whose saved recording Drive has, could not be erased: %v", sessionID, err)
		return
	}
	log.Printf("erased the raw recording of session %s: Drive has its saved recording", sessionID)
}

// savedBeside reports whether audioPath names the saved recording of session
// sessionID's raw recording (<sessionID>.mp3 or .wav), on disk.
func (k *keptRecordings) savedBeside(audioPath, sessionID string) bool {
	for _, entry := range recordingname.Entries(audioPath) {
		if recordingname.Key(entry) != sessionID {
			continue
		}
		if info, err := os.Stat(recordingname.Resolve(entry, k.audioDir)); err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	return false
}

// names reports whether audioPath names the file at path.
func (k *keptRecordings) names(audioPath, path string) bool {
	want, err := os.Stat(path)
	if err != nil {
		return false
	}
	for _, entry := range recordingname.Entries(audioPath) {
		if got, err := os.Stat(recordingname.Resolve(entry, k.audioDir)); err == nil && os.SameFile(got, want) {
			return true
		}
	}
	return false
}

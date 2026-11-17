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
// its kept raw recording, and says what an audio file with no session row
// may still belong to (storage.SQLiteStore).
type keptRecordingStore interface {
	GetSession(id string) (storage.Session, error)
	RecordKeptRecording(id, audioPath string, place func() error) (bool, error)
	// Recordings lists every session that names audio, so a file another
	// session names is never taken for a deleted session's.
	Recordings() ([]storage.SessionRecording, error)
	// RawEraseReservations lists the raw captures a session delete under way
	// holds.
	RawEraseReservations() ([]storage.RawEraseReservation, error)
	// AudioOwnedSince is from when the audio directory's files belong to
	// this database.
	AudioOwnedSince() (time.Time, error)
}

// keptRecordingSaver lists the raw recordings the recorder is not using and
// saves one, keeping it (audio.Recorder).
type keptRecordingSaver interface {
	KeptRawSessions() []string
	SaveKept(ctx context.Context, sessionID string) (string, error)
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
//
// A save writes under a temporary name (recording.KeptTempPaths) and renames
// the file into place only in the transaction that gives it to the session,
// once the session is found to take it (storage.RecordKeptRecording). A
// session delete holds the session (Hold), which stops a save of it under
// way. A temporary file found as a pass starts is a save a stop cut off, and
// is erased.
type keptRecordings struct {
	store    keptRecordingStore
	recorder keptRecordingSaver
	audioDir string
	finish   func(ctx context.Context, sessionID, audioPath string, startedAt time.Time)

	// running keeps two passes (the start's and a sweep's) from saving one
	// raw recording at once.
	running sync.Mutex

	// mu guards held, the sessions a delete holds, and saving, the saves
	// under way.
	mu     sync.Mutex
	held   map[string]int
	saving map[string]*keptSave
}

// keptSave is a save under way: cancel stops it, and done is closed once it
// has ended.
type keptSave struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func newKeptRecordings(store keptRecordingStore, recorder keptRecordingSaver, audioDir string, finish func(context.Context, string, string, time.Time)) *keptRecordings {
	return &keptRecordings{store: store, recorder: recorder, audioDir: audioDir, finish: finish, held: map[string]int{}, saving: map[string]*keptSave{}}
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
// between sessions once ctx ends.
func (k *keptRecordings) run(ctx context.Context) {
	if k == nil || !k.running.TryLock() {
		return
	}
	defer k.running.Unlock()
	k.eraseCutOffSaves()
	orphans := k.orphanGuard()
	for _, id := range k.recorder.KeptRawSessions() {
		if ctx.Err() != nil {
			return
		}
		sess, row := k.lookup(id)
		if row == rowUnreadable || k.isHeld(id) {
			continue
		}
		if row == rowMissing {
			orphans.erase(k, id, recordingname.RawPath(k.audioDir, id))
			continue
		}
		if sess.Status != storage.SessionEnded {
			continue
		}
		switch {
		case sess.AudioPath == "" && sess.AudioRemovedAt == "":
			k.save(ctx, &sess)
		case sess.SyncState == storage.SyncStateSynced && k.savedBeside(sess.AudioPath, id):
			k.eraseRaw(id)
		}
	}
	for id, saved := range k.savedWithoutRaw() {
		if ctx.Err() != nil {
			return
		}
		sess, row := k.lookup(id)
		if row == rowUnreadable || k.isHeld(id) {
			continue
		}
		if row == rowMissing {
			orphans.erase(k, id, saved...)
			continue
		}
		if sess.Status == storage.SessionEnded && sess.AudioPath == "" && sess.AudioRemovedAt == "" {
			k.adopt(&sess, saved[len(saved)-1])
		}
	}
}

// orphanGuard is what a pass reads once, as it starts, to tell an audio file
// of a session with no row that is a deleted session's from one it cannot
// know to be: the files every session names (named, absolute), the sessions
// whose raw capture a session delete under way holds (reserved), and from
// when the audio directory's files belong to this database (since). ok is
// false when any of these could not be read: the pass then erases no such
// file.
type orphanGuard struct {
	named    map[string]bool
	reserved map[string]bool
	since    time.Time
	ok       bool
}

func (k *keptRecordings) orphanGuard() orphanGuard {
	g := orphanGuard{named: map[string]bool{}, reserved: map[string]bool{}}
	recordings, err := k.store.Recordings()
	if err != nil {
		log.Printf("warning: the audio of sessions with no row is kept this pass: list recordings: %v", err)
		return g
	}
	for _, rec := range recordings {
		for _, entry := range recordingname.Entries(rec.AudioPath) {
			if abs, err := filepath.Abs(recordingname.Resolve(entry, k.audioDir)); err == nil {
				g.named[abs] = true
			}
		}
	}
	reservations, err := k.store.RawEraseReservations()
	if err != nil {
		log.Printf("warning: the audio of sessions with no row is kept this pass: list raw erase reservations: %v", err)
		return g
	}
	for _, reservation := range reservations {
		g.reserved[reservation.SessionID] = true
	}
	if g.since, err = k.store.AudioOwnedSince(); err != nil {
		log.Printf("warning: the audio of sessions with no row is kept this pass: %v", err)
		return g
	}
	g.ok = true
	return g
}

// erase erases, with zeros, each file in paths of session id, which has no
// row: a session deleted while a stop cut its erase short, or a GC deletion
// cut off after its row went. Left, a Restore from Drive that brings the
// session back would get it. It leaves a file it cannot know to be a deleted
// session's: one another session names, one of a session whose delete is
// under way, and one last written before this database owned the audio
// directory, which a database lost and made again would otherwise erase
// with every other recording on the unit.
func (g orphanGuard) erase(k *keptRecordings, id string, paths ...string) {
	if !g.ok || g.reserved[id] {
		return
	}
	for _, path := range paths {
		abs, err := filepath.Abs(path)
		if err != nil || g.named[abs] {
			continue
		}
		info, err := os.Lstat(abs)
		if err != nil || !info.Mode().IsRegular() || !info.ModTime().After(g.since) {
			continue
		}
		k.eraseFile(abs, "audio of session "+id+", which has no row")
		log.Printf("erased %s: its session %s has no row", filepath.Base(abs), id)
	}
}

// Hold keeps the pass off the files of sessionIDs until release is called:
// it stops a save of one under way (its encoder is killed) and waits for it
// to end, and no save, raw erase or adoption of one starts. A session delete
// holds its sessions while it erases their files, so no encoder writes a file
// of a deleted session after the delete erased it.
func (k *keptRecordings) Hold(sessionIDs []string) (release func()) {
	if k == nil {
		return func() {}
	}
	k.mu.Lock()
	var running []*keptSave
	for _, id := range sessionIDs {
		k.held[id]++
		if s := k.saving[id]; s != nil {
			running = append(running, s)
		}
	}
	k.mu.Unlock()
	for _, s := range running {
		s.cancel()
	}
	for _, s := range running {
		<-s.done
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			k.mu.Lock()
			defer k.mu.Unlock()
			for _, id := range sessionIDs {
				if k.held[id]--; k.held[id] <= 0 {
					delete(k.held, id)
				}
			}
		})
	}
}

func (k *keptRecordings) isHeld(id string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.held[id] > 0
}

// startSave registers a save of session id under way, unless a delete holds
// the session, and returns its context, which Hold cancels.
func (k *keptRecordings) startSave(parent context.Context, id string) (context.Context, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.held[id] > 0 {
		return nil, false
	}
	ctx, cancel := context.WithCancel(parent)
	k.saving[id] = &keptSave{cancel: cancel, done: make(chan struct{})}
	return ctx, true
}

// endSave ends the save of session id that startSave registered.
func (k *keptRecordings) endSave(id string) {
	k.mu.Lock()
	s := k.saving[id]
	delete(k.saving, id)
	k.mu.Unlock()
	s.cancel()
	close(s.done)
}

// eraseCutOffSaves erases every temporary file a save writes
// (recording.IsKeptTemp): as a pass starts no save runs, so one there is a
// save a stop cut off, or one a delete stopped.
func (k *keptRecordings) eraseCutOffSaves() {
	entries, err := os.ReadDir(k.audioDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if recordingname.IsKeptTemp(entry.Name()) {
			k.eraseFile(filepath.Join(k.audioDir, entry.Name()), "a save a stop cut off")
		}
	}
}

// eraseFile erases the file at path, which holds audio, with zeros before
// its blocks go; why says what it is, for the log.
func (k *keptRecordings) eraseFile(path, why string) {
	if err := erase.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("warning: %s, %s, could not be erased: %v", why, path, err)
	}
}

// rowState says what a pass found of a session's row.
type rowState int

const (
	rowFound      rowState = iota
	rowMissing             // the session has no row
	rowUnreadable          // the row could not be read; the pass leaves the session's files alone
)

// lookup reads session id's row.
func (k *keptRecordings) lookup(id string) (storage.Session, rowState) {
	sess, err := k.store.GetSession(id)
	if errors.Is(err, sql.ErrNoRows) {
		return storage.Session{}, rowMissing
	}
	if err != nil {
		log.Printf("warning: the audio kept for session %s is left this pass: read the session: %v", id, err)
		return storage.Session{}, rowUnreadable
	}
	return sess, rowFound
}

// savedWithoutRaw returns, by session, the recordings a session end saved in
// the audio directory (<id>.mp3, <id>.wav) with no raw recording beside
// them, the MP3 first: where there are both, the WAV, last, is the recording.
func (k *keptRecordings) savedWithoutRaw() map[string][]string {
	saved := map[string][]string{}
	for _, ext := range []string{".mp3", ".wav"} {
		matches, err := filepath.Glob(filepath.Join(k.audioDir, "*"+ext))
		if err != nil {
			return nil
		}
		for _, path := range matches {
			name := filepath.Base(path)
			if strings.HasPrefix(name, ".") {
				continue
			}
			if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
				continue
			}
			id := strings.TrimSuffix(name, ext)
			if _, err := os.Lstat(recordingname.RawPath(k.audioDir, id)); !errors.Is(err, os.ErrNotExist) {
				continue
			}
			saved[id] = append(saved[id], path)
		}
	}
	return saved
}

// adopt makes saved, the recording a session end saved for sess, the
// session's recording, and starts what a session end starts after saving.
func (k *keptRecordings) adopt(sess *storage.Session, saved string) {
	recorded, err := k.store.RecordKeptRecording(sess.ID, saved, nil)
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

// save saves the raw recording kept for sess under a temporary name, then,
// in the transaction that finds the session still takes it, renames it to
// <id>.mp3 or <id>.wav and makes it the session's recording. A save a delete
// stops (Hold), or one the session no longer takes (deleted, merged or given
// a recording meanwhile), records nothing and erases what it wrote; the raw
// recording is left for the next pass.
func (k *keptRecordings) save(parent context.Context, sess *storage.Session) {
	ctx, ok := k.startSave(parent, sess.ID)
	if !ok {
		return
	}
	defer k.endSave(sess.ID)
	temp, err := k.recorder.SaveKept(ctx, sess.ID)
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		for _, path := range recordingname.KeptTempPaths(k.audioDir, sess.ID) {
			k.eraseFile(path, "a save that did not finish")
		}
		log.Printf("warning: the raw recording kept for session %s was not saved; the next pass tries again: %v", sess.ID, err)
		return
	}
	saved := filepath.Join(k.audioDir, sess.ID+filepath.Ext(temp))
	placed := false
	recorded, err := k.store.RecordKeptRecording(sess.ID, saved, func() error {
		if err := erase.Replace(temp, saved); err != nil && !errors.Is(err, erase.ErrLeftover) {
			return err
		}
		placed = true
		// The rename is on disk before the row names the file.
		return syncRecordingDirectory(saved)
	})
	if err != nil || !recorded {
		k.eraseFile(temp, "a recording no session took")
		if placed {
			k.eraseUnnamed(sess.ID, saved, err)
		} else {
			log.Printf("session %s did not take the recording saved from its raw recording (%v); it is erased", sess.ID, err)
		}
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

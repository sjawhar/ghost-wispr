package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/erase"
	recordingname "github.com/sjawhar/ghost-wispr/internal/recording"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// keptRecordingStore reads a session, gives it the recording saved from its
// kept raw recording, and lists the files sessions name and the deleted
// sessions whose audio is left to erase (storage.SQLiteStore).
type keptRecordingStore interface {
	GetSession(id string) (storage.Session, error)
	RecordKeptRecording(id, audioPath string, place func() error) (bool, error)
	// NamedAudio lists every audio file a session names, the recordings a
	// restored session's Drive folder holds included, so a file a session
	// names is never erased with a deleted session's nor given to another.
	NamedAudio(audioDir string) ([]string, error)
	// DeletedSessionAudio lists the sessions a delete recorded, whose audio
	// files a stop may have left; ClearDeletedSessionAudio forgets one once
	// none is left.
	DeletedSessionAudio() ([]storage.DeletedSessionAudio, error)
	ClearDeletedSessionAudio(id string) error
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
// raw recording once the recording saved beside it is the session's and
// either Drive has it (the session is synced) or Drive cannot take it (sync
// is off, or the session's sync FAILED or its folder is REMOTE_DELETED), as a
// session end erases its raw recording once the recording is saved: the
// saved recording is on disk, so the raw one is never the last copy, and a
// raw recording kept beside it refuses "Delete text and audio". While Drive
// will take the saved recording (the session waits for its sync or its
// retry) the raw recording stays until Drive has it. The app runs a pass as
// it starts and after every Drive sync sweep, so a save that failed is tried
// again and a raw recording goes in the pass after the sweep that synced its
// recording.
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
//
// A pass first erases, with zeros, the audio files a stop left of each
// session a person's delete or GC's age delete recorded in the transaction
// that deleted its row (storage.DeletedSessionAudio), then clears the
// record (eraseDeleted). It erases no other file of a session with no row,
// and saves or gives none: a restored database backup has no row for the
// sessions recorded since it, and nobody deleted those. The status names
// their raw recordings for a person (rawByHand).
type keptRecordings struct {
	store    keptRecordingStore
	recorder keptRecordingSaver
	audioDir string
	// drive is the app's Drive sync; nil, or one with no syncer, is sync off.
	drive  *driveSync
	finish func(ctx context.Context, sessionID, audioPath string, startedAt time.Time)

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

func newKeptRecordings(store keptRecordingStore, recorder keptRecordingSaver, audioDir string, drive *driveSync, finish func(context.Context, string, string, time.Time)) *keptRecordings {
	return &keptRecordings{store: store, recorder: recorder, audioDir: audioDir, drive: drive, finish: finish, held: map[string]int{}, saving: map[string]*keptSave{}}
}

// rawAction is what a pass does with a session's kept raw recording
// (decide).
type rawAction int

const (
	rawSkip       rawAction = iota // a delete holds it or recorded its session, or its row could not be read: this pass leaves it
	rawSave                        // the session ended with no recording: save it
	rawAwaitDrive                  // saved, and Drive will take the saved recording: keep it until Drive has it
	rawErase                       // saved, and Drive has the saved recording or cannot take it: erase it
	rawByHand                      // the app neither saves nor erases it: it waits for a person
)

// decide says what a pass does with session id's kept raw recording, and
// returns the session as read. deleted is the sessions a delete recorded
// whose audio is still to erase (eraseDeleted); named is every file a session
// names (namedFiles), nil if it could not be read.
func (k *keptRecordings) decide(id string, deleted, named map[string]bool) (storage.Session, rawAction) {
	if k.isHeld(id) || deleted[id] {
		return storage.Session{}, rawSkip
	}
	sess, row := k.lookup(id)
	switch row {
	case rowUnreadable:
		return sess, rawSkip
	case rowMissing:
		return sess, rawByHand
	}
	switch {
	case sess.Status != storage.SessionEnded:
		return sess, rawByHand
	case sess.AudioPath == "" && sess.AudioRemovedAt == "" && named == nil:
		return sess, rawSkip
	case sess.AudioPath == "" && sess.AudioRemovedAt == "" && k.claimedElsewhere(id, named):
		return sess, rawByHand
	case sess.AudioPath == "" && sess.AudioRemovedAt == "":
		return sess, rawSave
	case !k.savedBeside(sess.AudioPath, id):
		return sess, rawByHand
	case sess.SyncState == storage.SyncStateSynced || !k.driveWillTake(&sess):
		return sess, rawErase
	default:
		return sess, rawAwaitDrive
	}
}

// claimedElsewhere reports whether a session names session id's <id>.mp3 or
// <id>.wav in named, when id names neither: on a restored unit a merged
// session names its sources' recordings, and a save must not overwrite one,
// nor an adoption give it to the source, whose Drive folder would then keep
// a copy a later cut on the merged session never replaces.
func (k *keptRecordings) claimedElsewhere(id string, named map[string]bool) bool {
	for _, ext := range []string{".mp3", ".wav"} {
		if abs, err := filepath.Abs(filepath.Join(k.audioDir, id+ext)); err == nil && named[abs] {
			return true
		}
	}
	return false
}

// driveWillTake reports whether Drive sync will upload sess's recording: sync
// is on, and the session's sync neither FAILED nor found its folder
// REMOTE_DELETED.
func (k *keptRecordings) driveWillTake(sess *storage.Session) bool {
	if k.drive == nil || k.drive.Get() == nil {
		return false
	}
	return sess.SyncState != storage.SyncStateFailed && sess.SyncState != storage.SyncStateRemoteDeleted
}

// census counts the raw recordings the app keeps, for GET /api/status, by
// what the next pass does with them (decide): Unsaved it tries again to save,
// AwaitingDrive it erases once Drive has their saved recording, and ByHand,
// by session, it neither saves nor erases. One of a recorded delete, one a
// delete holds, and the recording under way or being saved at a session end
// are not counted. WarnBytes is left to the caller.
func (k *keptRecordings) census() server.KeptRawRecordings {
	var c server.KeptRawRecordings
	deleted := map[string]bool{}
	records, err := k.store.DeletedSessionAudio()
	if err != nil {
		slog.Warn("kept raw recordings: list deleted sessions", "error", err)
	}
	for _, record := range records {
		deleted[record.SessionID] = true
	}
	named, err := k.namedFiles()
	if err != nil {
		slog.Warn("kept raw recordings", "error", err)
	}
	for _, id := range k.recorder.KeptRawSessions() {
		_, action := k.decide(id, deleted, named)
		switch action {
		case rawSave:
			c.Unsaved++
		case rawAwaitDrive:
			c.AwaitingDrive++
		case rawByHand:
			c.ByHand = append(c.ByHand, id)
		default:
			continue
		}
		c.Count++
		if info, err := os.Stat(recordingname.RawPath(k.audioDir, id)); err == nil {
			c.Bytes += info.Size()
		}
	}
	return c
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
	deleted := k.eraseDeleted()
	// A save and an adoption read what sessions name again just before they
	// act (save, adopt); this read sorts the raw recordings.
	named, err := k.namedFiles()
	if err != nil {
		slog.Warn("no kept raw recording is saved or given this pass", "error", err)
	}
	for _, id := range k.recorder.KeptRawSessions() {
		if ctx.Err() != nil {
			return
		}
		sess, action := k.decide(id, deleted, named)
		switch action {
		case rawSave:
			// Once the saved recording is the session's, its raw recording
			// goes at once where Drive cannot take the saved one.
			if k.save(ctx, &sess) {
				if _, after := k.decide(id, deleted, map[string]bool{}); after == rawErase {
					k.eraseRaw(id)
				}
			}
		case rawErase:
			k.eraseRaw(id)
		}
	}
	if named == nil {
		return
	}
	for id, saved := range k.savedWithoutRaw() {
		if ctx.Err() != nil {
			return
		}
		if deleted[id] || k.isHeld(id) || k.claimedElsewhere(id, named) {
			continue
		}
		if sess, row := k.lookup(id); row == rowFound && sess.Status == storage.SessionEnded && sess.AudioPath == "" && sess.AudioRemovedAt == "" {
			k.adopt(&sess, saved[len(saved)-1])
		}
	}
}

// eraseDeleted erases, with zeros, the files a stop left of each session a
// delete recorded (storage.DeletedSessionAudio): exactly the files the
// delete listed, absolute paths fixed at the delete, except a file a session
// names, read just before that session's files go, so a session that starts
// to name one meanwhile (a Retry refinement downloading a restored merged
// session's sources) keeps it. Once none of them is left it clears the
// record. It returns the recorded sessions it leaves for a later pass (a
// delete holds it, a file could not be erased, or the store could not be
// read), whose files the rest of the pass leaves alone.
func (k *keptRecordings) eraseDeleted() map[string]bool {
	left := map[string]bool{}
	records, err := k.store.DeletedSessionAudio()
	if err != nil {
		slog.Warn("the audio of deleted sessions is not erased this pass: list them", "error", err)
		return left
	}
	for _, record := range records {
		id := record.SessionID
		if k.isHeld(id) {
			left[id] = true
			continue
		}
		named, err := k.namedFiles()
		if err != nil {
			slog.Warn("the audio of a deleted session is not erased this pass", "session_id", id, "error", err)
			left[id] = true
			continue
		}
		remaining := false
		for _, path := range record.Files {
			if named[path] {
				continue
			}
			if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err := erase.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				slog.Warn("a file of a deleted session could not be erased; the next pass tries again", "file", path, "session_id", id, "error", err)
				remaining = true
				continue
			}
			log.Printf("erased %s of deleted session %s", filepath.Base(path), id)
		}
		if remaining {
			left[id] = true
			continue
		}
		if err := k.store.ClearDeletedSessionAudio(id); err != nil {
			slog.Warn("could not clear the audio record of a deleted session", "session_id", id, "error", err)
			left[id] = true
		}
	}
	return left
}

// namedFiles returns, absolute, every file a session names
// (storage.SQLiteStore.NamedAudio).
func (k *keptRecordings) namedFiles() (map[string]bool, error) {
	paths, err := k.store.NamedAudio(k.audioDir)
	if err != nil {
		return nil, fmt.Errorf("list the audio sessions name: %w", err)
	}
	named := map[string]bool{}
	for _, path := range paths {
		named[path] = true
	}
	return named, nil
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
		slog.Warn("a file that holds audio could not be erased", "what", why, "file", path, "error", err)
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
		slog.Warn("the audio kept for a session is left this pass: read the session", "session_id", id, "error", err)
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
// session's recording, and starts what a session end starts after saving. It
// reads what sessions name just before, and gives no file another session
// names (claimedElsewhere).
func (k *keptRecordings) adopt(sess *storage.Session, saved string) {
	if named, err := k.namedFiles(); err != nil || k.claimedElsewhere(sess.ID, named) {
		return
	}
	recorded, err := k.store.RecordKeptRecording(sess.ID, saved, nil)
	if err != nil {
		slog.Warn("a session ended without naming its saved recording, and it could not be given it; the next pass tries again", "session_id", sess.ID, "recording", saved, "error", err)
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

// save saves the raw recording kept for sess under a temporary name, flushes
// it to the disk (the raw recording can be zeroed in the same pass), then, in
// the transaction that finds the session still takes it, renames it to
// <id>.mp3 or <id>.wav and makes it the session's recording. Just before, it
// reads what sessions name, and overwrites no file another session names
// (claimedElsewhere). A save a delete stops (Hold), or one the session no
// longer takes (deleted, merged or given a recording meanwhile), records
// nothing and erases what it wrote; the raw recording is left for the next
// pass. It reports whether the session took the saved recording.
func (k *keptRecordings) save(parent context.Context, sess *storage.Session) bool {
	ctx, ok := k.startSave(parent, sess.ID)
	if !ok {
		return false
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
		slog.Warn("the raw recording kept for a session was not saved; the next pass tries again", "session_id", sess.ID, "error", err)
		return false
	}
	if named, err := k.namedFiles(); err != nil || k.claimedElsewhere(sess.ID, named) {
		k.eraseFile(temp, "a recording saved for a session whose file another session names")
		log.Printf("the recording saved for session %s is erased: another session names its file, or what sessions name could not be read (%v)", sess.ID, err)
		return false
	}
	if err := syncKeptFile(temp); err != nil {
		k.eraseFile(temp, "a recording that could not be flushed to the disk")
		slog.Warn("the recording saved for a session could not be flushed to the disk; the next pass tries again", "session_id", sess.ID, "error", err)
		return false
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
		return false
	}
	log.Printf("saved the raw recording kept for session %s as %s", sess.ID, saved)
	if k.finish != nil {
		go k.finish(context.Background(), sess.ID, saved, sess.StartedAt)
	}
	return true
}

// eraseUnnamed erases saved, a recording saved for session sessionID that the
// session did not take (refused, the reason recordErr if it failed), unless
// the session names it now.
func (k *keptRecordings) eraseUnnamed(sessionID, saved string, recordErr error) {
	if sess, err := k.store.GetSession(sessionID); err == nil && k.names(sess.AudioPath, saved) {
		return
	}
	if err := erase.Remove(saved); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("a session did not take the recording saved from its raw recording, and the recording could not be erased", "session_id", sessionID, "record_error", recordErr, "recording", saved, "error", err)
		return
	}
	log.Printf("session %s did not take the recording saved from its raw recording (%v); it is erased", sessionID, recordErr)
}

// eraseRaw erases session sessionID's raw recording, beside the saved
// recording the session names, which Drive has or cannot take (decide).
func (k *keptRecordings) eraseRaw(sessionID string) {
	raw := recordingname.RawPath(k.audioDir, sessionID)
	if err := erase.Remove(raw); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("the raw recording of a session could not be erased beside its saved recording", "session_id", sessionID, "error", err)
		return
	}
	log.Printf("erased the raw recording of session %s beside its saved recording", sessionID)
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

// syncKeptFile flushes the file at path to the disk. A variable, so that a
// test can see when a save flushes its recording.
var syncKeptFile = func(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

package gc

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/erase"
	"github.com/sjawhar/ghost-wispr/internal/logging"
	"github.com/sjawhar/ghost-wispr/internal/recording"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

type Store interface {
	GetGCEligibleSessions(maxAgeDays int) ([]string, error)
	GetSession(id string) (storage.Session, error)
	DeleteEndedSessionAndSources(id, audioPath, audioDir string) (deletedIDs []string, deleted bool, err error)
	SourceSessionIDs(mergedID string) ([]string, error)
	RawEraseReservations() ([]storage.RawEraseReservation, error)
	CancelRawSessionErase(id, hiddenPath string) error
	PurgeOldEvents(maxAge time.Duration) error
	Recordings() ([]storage.SessionRecording, error)
	MarkAudioRemoved(id, before, left string, audioLeft bool, removedAt time.Time) (storage.AudioRemoval, bool, error)
	// RecordEraseDirectory records a directory before a whole-file erase in
	// it begins, so the app finishes one a stop cut off as it starts again.
	RecordEraseDirectory(dir string) error
	RestoreAudio(removal *storage.AudioRemoval, restored string) error
	MarkRemoteDeleted(id, folderID, errorMessage string) (bool, error)
}

// DriveVerifier confirms that Drive retains every recording GC may remove.
type DriveVerifier interface {
	// FolderOnDrive reports whether Drive has folder id out of the trash,
	// and an error when Drive cannot say.
	FolderOnDrive(ctx context.Context, id string) (bool, error)
	// RecordingsMatch reports whether folderID has every audio file named by
	// checksums and with the matching Google Drive md5Checksum.
	RecordingsMatch(ctx context.Context, folderID string, checksums map[string]string) (bool, error)
}

type Config struct {
	// MaxAgeDays is the age policy: sessions older than this many days are
	// deleted. 0 (or less) is no age limit.
	MaxAgeDays int
	// MaxAudioSizeMB is the audio budget: past it, disk pressure removes the
	// oldest sessions' audio files. 0 (or less) is no budget.
	MaxAudioSizeMB int
	AudioDir       string
	// HoldReason explains why a nil Drive intentionally holds a pass. When
	// unset, a nil Drive means its configured syncer did not start.
	HoldReason error
	// Drive confirms that it has each session folder and every local recording
	// under its recording name with matching content. A folder in the trash,
	// a missing recording, or a mismatched checksum leaves the device with the
	// only lasting copy.
	Drive DriveVerifier
	// OnRecordingMismatch is called with true to set a warning and false to
	// clear it: Drive may match again, the session may be deleted, or its
	// folder may be gone.
	OnRecordingMismatch func(sessionID string, mismatch bool)
}

// driveCheckTimeout bounds Drive's confirmation of one session.
const driveCheckTimeout = time.Minute

var (
	// errFolderGone: Drive does not have the session's folder out of the
	// trash. GC keeps the session for good (it is now REMOTE_DELETED).
	errFolderGone = errors.New("its Drive folder is in the trash or gone, so the device holds its only lasting copy")
	// errNoFolder: the session records no Drive folder to ask Drive about.
	errNoFolder = errors.New("it records no Drive folder, so GC cannot check that Drive has it")
	// errRecordingsDoNotMatch: Drive lacks a current local recording, or its
	// MD5 differs, so GC keeps the local audio.
	errRecordingsDoNotMatch = errors.New("drive does not have every recording with matching content, so the device holds its only lasting copy")
	// errCannotReadRecording: a local recording cannot be read to verify it,
	// so GC leaves it untouched.
	errCannotReadRecording = errors.New("cannot read local recording to verify it on Drive")
)

const driveSyncOffReason = "Google Drive sync is off, so recordings are kept on the unit"

// The reasons a GC pass holds back, deleting and removing nothing more
// (Held): the app reports them where the user sees its health, so a broken
// sync is noticed before the disk fills.
var (
	// ErrDriveSyncOff: no Drive copy can be verified while sync is disabled
	// or no shared-drive folder is configured, so GC keeps local recordings.
	//nolint:staticcheck // Google Drive is a proper name shown to the operator.
	ErrDriveSyncOff = errors.New(driveSyncOffReason)
	// ErrDriveNotRunning: Drive sync is on but no syncer runs (it failed to
	// start), so nothing can confirm a session's durable copy.
	ErrDriveNotRunning = errors.New("sync to Google Drive is on but not running, so nothing can confirm a session's durable copy")
	// ErrDriveCannotSay: Drive did not answer whether it has a session's
	// folder or recordings; it cannot say for the other sessions either, so
	// the pass stops.
	ErrDriveCannotSay = errors.New("could not ask Drive whether it has a session's durable copy")
)

// AudioBudgetBytes returns an audio budget of maxAudioSizeMB megabytes in
// bytes, and false when there is no budget (0 or less). It saturates instead
// of wrapping: from 2^43 MB, more than an int64 holds in bytes, the budget is
// the largest one.
func AudioBudgetBytes(maxAudioSizeMB int) (int64, bool) {
	const mb = 1024 * 1024
	if maxAudioSizeMB <= 0 {
		return 0, false
	}
	if int64(maxAudioSizeMB) > math.MaxInt64/mb {
		return math.MaxInt64, true
	}
	return int64(maxAudioSizeMB) * mb, true
}

type Collector struct {
	store  Store
	config Config
	logger *slog.Logger
	held   error
}

// Held returns why the last Run held back, deleting or removing nothing more
// (ErrDriveNotRunning, or ErrDriveCannotSay wrapping Drive's error), or nil.
func (c *Collector) Held() error {
	return c.held
}

func New(store Store, config Config, logger ...*slog.Logger) *Collector {
	l := logging.WithModule(slog.Default(), "gc")
	if len(logger) > 0 && logger[0] != nil {
		l = logging.WithModule(logger[0], "gc")
	}

	return &Collector{store: store, config: config, logger: l}
}

// Run applies the age policy, which deletes old sessions, then frees audio
// space under the audio budget, which removes only audio files. With no Drive
// to ask, it deletes nothing and removes no audio.
func (c *Collector) Run() ([]string, error) {
	var deleted []string
	c.held = nil
	if c.config.Drive == nil {
		c.held = c.config.HoldReason
		if c.held == nil {
			c.held = ErrDriveNotRunning
		}
	} else {
		var err error
		if deleted, err = c.applyAgePolicy(); err != nil {
			return nil, err
		}
		// A Drive that did not answer for the age policy stops the pass:
		// it cannot say for the audio budget's sessions either.
		if c.held == nil {
			c.freeAudioSpace()
		}
	}

	// Purge old events from the durable event queue (7-day default retention).
	if err := c.store.PurgeOldEvents(7 * 24 * time.Hour); err != nil {
		c.logger.Warn("gc failed to purge old events", "operation", "purge_events", "error", err)
	}

	return deleted, nil
}

// applyAgePolicy deletes the sessions older than the age policy allows, and
// returns them. A Drive that cannot confirm a session's folder and recordings
// stops it: it cannot say for the others either.
func (c *Collector) applyAgePolicy() ([]string, error) {
	ids, err := c.store.GetGCEligibleSessions(c.config.MaxAgeDays)
	if err != nil {
		return nil, fmt.Errorf("query gc eligible: %w", err)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	recordings, err := c.store.Recordings()
	if err != nil {
		return nil, fmt.Errorf("list recordings: %w", err)
	}
	names := c.namesOf(recordings)
	var deleted []string
	for _, id := range ids {
		if err := c.deleteSession(id, names); err != nil {
			var block *storage.GCSessionBlockError
			if !errors.As(err, &block) {
				c.logger.Warn("gc skipped session", "operation", "delete_session", "session_id", id, "error", err)
			}
			if errors.Is(err, ErrDriveCannotSay) {
				c.held = err
				break
			}
			continue
		}
		deleted = append(deleted, id)
	}
	return deleted, nil
}

// onDrive returns nil when Drive has session sess's folder out of the trash
// and every local recording with matching content. Otherwise it returns why
// GC keeps the session: errFolderGone (the session is then marked
// REMOTE_DELETED), errNoFolder, errRecordingsDoNotMatch,
// errCannotReadRecording, or ErrDriveCannotSay.
func (c *Collector) onDrive(sess *storage.Session) error {
	if sess.GDriveFolderID == "" {
		return errNoFolder
	}
	ctx, cancel := context.WithTimeout(context.Background(), driveCheckTimeout)
	defer cancel()
	present, err := c.config.Drive.FolderOnDrive(ctx, sess.GDriveFolderID)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrDriveCannotSay, err)
	}
	if !present {
		return c.folderGone(sess)
	}

	checksums, err := c.recordingChecksums(sess.AudioPath)
	if err != nil {
		return err
	}
	if len(checksums) == 0 {
		c.recordingMismatch(sess.ID, false)
		return nil
	}
	matches, err := c.config.Drive.RecordingsMatch(ctx, sess.GDriveFolderID, checksums)
	if err != nil {
		// A folder may disappear after the first check. Ask once more so a
		// definite remote deletion retains its existing REMOTE_DELETED
		// semantics; a still-present folder with a failed list holds the
		// whole pass as Drive unavailable.
		present, folderErr := c.config.Drive.FolderOnDrive(ctx, sess.GDriveFolderID)
		switch {
		case folderErr != nil:
			return fmt.Errorf("%w: %w", ErrDriveCannotSay, errors.Join(err, folderErr))
		case !present:
			return c.folderGone(sess)
		default:
			return fmt.Errorf("%w: %w", ErrDriveCannotSay, err)
		}
	}
	if !matches {
		c.recordingMismatch(sess.ID, true)
		return errRecordingsDoNotMatch
	}
	c.recordingMismatch(sess.ID, false)
	return nil
}

func (c *Collector) recordingMismatch(sessionID string, mismatch bool) {
	if c.config.OnRecordingMismatch != nil {
		c.config.OnRecordingMismatch(sessionID, mismatch)
	}
}

func (c *Collector) folderGone(sess *storage.Session) error {
	marked, err := c.store.MarkRemoteDeleted(sess.ID, sess.GDriveFolderID, "GC found the Drive folder in the trash or gone; the device holds the only lasting copy")
	switch {
	case err != nil:
		c.logger.Warn("gc could not mark a session whose Drive folder is in the trash or gone", "operation", "check_drive", "session_id", sess.ID, "drive_folder_id", sess.GDriveFolderID, "error", err)
	case marked:
		c.logger.Warn("gc keeps a session whose Drive folder is in the trash or gone: marked REMOTE_DELETED", "operation", "check_drive", "session_id", sess.ID, "drive_folder_id", sess.GDriveFolderID)
	default:
		c.logger.Info("gc keeps a session whose Drive folder is in the trash or gone; it changed since GC read it, so it is not marked", "operation", "check_drive", "session_id", sess.ID, "drive_folder_id", sess.GDriveFolderID)
	}
	c.recordingMismatch(sess.ID, false)
	return errFolderGone
}

// recordingChecksums returns every local saved recording's Drive name and
// streaming MD5 checksum. Missing files name no local audio to remove; any
// other failure keeps the session's local audio.
func (c *Collector) recordingChecksums(audioPath string) (map[string]string, error) {
	checksums := map[string]string{}
	for _, entry := range splitAudioPath(audioPath) {
		path := c.resolve(entry)
		info, err := os.Stat(path)
		switch {
		case os.IsNotExist(err):
			continue
		case err != nil:
			return nil, fmt.Errorf("%w: stat %s: %w", errCannotReadRecording, path, err)
		case info.IsDir() || !info.Mode().IsRegular():
			return nil, fmt.Errorf("%w: %s is not a regular file", errCannotReadRecording, path)
		}
		checksum, err := md5File(path)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %w", errCannotReadRecording, path, err)
		}
		name := recording.FileName(path)
		if old, duplicate := checksums[name]; duplicate && old != checksum {
			return nil, fmt.Errorf("%w: recordings named %s have different content", errRecordingsDoNotMatch, name)
		}
		checksums[name] = checksum
	}
	return checksums, nil
}

// md5File streams a file into MD5 so recordings are never loaded into memory.
func md5File(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	sum := md5.New()
	if _, err := io.Copy(sum, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// recordingFile is a file of a session's saved recording that is on disk.
type recordingFile struct {
	entry string // as the session's audio path names it
	path  string // resolved
	size  int64
}

// splitAudioPath returns the entries of an audio path, which may name several
// files (a merged session).
func splitAudioPath(audioPath string) []string {
	var entries []string
	for _, entry := range strings.Split(audioPath, ",") {
		if entry = strings.TrimSpace(entry); entry != "" {
			entries = append(entries, entry)
		}
	}
	return entries
}

// resolve returns the file an audio path entry names, as an absolute path so
// two spellings of one file compare equal: where recording.Resolve finds it,
// the entry as written if a file is there, else its name in the audio
// directory, as every other reader of a recording finds it.
func (c *Collector) resolve(entry string) string {
	return absolute(recording.Resolve(entry, c.config.AudioDir))
}

// rawPath returns, as an absolute path, where the recorder keeps session
// id's raw recording: in the audio directory.
func (c *Collector) rawPath(id string) string {
	return absolute(filepath.Join(c.config.AudioDir, id+".pcm"))
}

func absolute(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return filepath.Clean(path)
}

// audioFile resolves an audio path entry and reports whether it is a file on
// disk.
func (c *Collector) audioFile(entry string) (recordingFile, bool) {
	path := c.resolve(entry)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return recordingFile{}, false
	}
	return recordingFile{entry: entry, path: path, size: info.Size()}, true
}

// names maps each file the sessions' audio paths name, resolved, to the
// sessions naming it.
type names map[string]map[string]bool

func (c *Collector) namesOf(recordings []storage.SessionRecording) names {
	n := names{}
	for _, rec := range recordings {
		for _, entry := range splitAudioPath(rec.AudioPath) {
			path := c.resolve(entry)
			if n[path] == nil {
				n[path] = map[string]bool{}
			}
			n[path][rec.ID] = true
		}
	}
	return n
}

// namedByAnother reports whether a session other than id names path. Such a
// file is that session's recording too, so GC never removes it: nothing in
// the app makes one (a merge gives its sources' recordings to the merged
// session), but an imported or hand-edited row can.
func (n names) namedByAnother(path, id string) bool {
	for other := range n[path] {
		if other != id {
			return true
		}
	}
	return false
}

// forget drops session id from the files it names.
func (n names) forget(id string) {
	for _, ids := range n {
		delete(ids, id)
	}
}

// deleteSession deletes session id under the age policy, with the files of
// its saved recording that no other session names, as long as the session is
// still as GC read it (a session merged meanwhile gave its recording to the
// merged session; one whose summary or transcript changed meanwhile is
// waiting to sync them) and Drive confirms its folder and recordings. It
// removes the session's raw .pcm only once a saved recording with audio in it
// is gone, so the raw file was a redundant copy, and only when no session
// names it; otherwise the raw file may be the only copy of the audio and
// stays: GC never removes one.
func (c *Collector) deleteSession(id string, names names) error {
	sess, err := c.store.GetSession(id)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}
	if err := c.onDrive(&sess); err != nil {
		return err
	}
	reservations, err := c.store.RawEraseReservations()
	if err != nil {
		return fmt.Errorf("list raw erase reservations: %w", err)
	}
	sourceIDs, err := c.store.SourceSessionIDs(id)
	if err != nil {
		return fmt.Errorf("list merged sources: %w", err)
	}
	for _, sourceID := range sourceIDs {
		source, err := c.store.GetSession(sourceID)
		if err != nil {
			return fmt.Errorf("get merged source %s: %w", sourceID, err)
		}
		if source.GDriveFolderID == "" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), driveCheckTimeout)
		present, folderErr := c.config.Drive.FolderOnDrive(ctx, source.GDriveFolderID)
		cancel()
		if folderErr != nil {
			return fmt.Errorf("check Drive folder of merged source %s: %w: %w", sourceID, ErrDriveCannotSay, folderErr)
		}
		if !present {
			return fmt.Errorf("merged source %s has a Drive folder that is in the trash or gone", sourceID)
		}
	}
	var files []recordingFile
	for _, entry := range splitAudioPath(sess.AudioPath) {
		f, ok := c.audioFile(entry)
		if !ok {
			continue
		}
		if names.namedByAnother(f.path, id) {
			c.logger.Warn("gc kept an audio file another session names", "operation", "delete_session", "session_id", id, "audio_path", f.path)
			continue
		}
		files = append(files, f)
	}
	if c.config.AudioDir != "" {
		for _, sourceID := range sourceIDs {
			if sourceHasSavedAudio(sourceID, files) {
				continue
			}
			if kept, err := rawReservationExists(sourceID, reservations); err != nil {
				return err
			} else if kept {
				return fmt.Errorf("merged source %s keeps its only audio as a reserved raw PCM", sourceID)
			}
			if _, err := os.Stat(c.rawPath(sourceID)); err == nil {
				return fmt.Errorf("merged source %s keeps its only audio as raw PCM", sourceID)
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("stat raw PCM of merged source %s: %w", sourceID, err)
			}
		}
	}
	hasSavedAudio := false
	for _, f := range files {
		hasSavedAudio = hasSavedAudio || f.size > 0
	}
	if !hasSavedAudio {
		for _, reservation := range reservations {
			if reservation.SessionID != id {
				continue
			}
			if err := restoreReservedRaw(c.store, reservation); err != nil {
				return fmt.Errorf("keep raw recording of session %s: %w", id, err)
			}
		}
	}

	// Record where the saved files are erased before the row that names them
	// goes, so an erase a stop cuts off is finished as the app starts; what
	// GC cannot record, it does not delete. DeleteEndedSessionAndSources
	// records every raw PCM directory in its row-delete transaction.
	erased := make([]string, 0, len(files))
	for _, f := range files {
		erased = append(erased, f.path)
	}
	if err := c.recordEraseDirectories(erased); err != nil {
		return fmt.Errorf("kept with its audio: %w", err)
	}
	deletedIDs, deleted, err := c.store.DeleteEndedSessionAndSources(id, sess.AudioPath, c.config.AudioDir)
	if err != nil {
		var block *storage.GCSessionBlockError
		if errors.As(err, &block) {
			blockRole := "source"
			if block.BlockingSessionIsTarget {
				blockRole = "target"
			}
			c.logger.Warn("gc session blocks age deletion", "operation", "delete_session", "session_id", id, "blocking_session_id", block.BlockingSessionID, "blocking_session_role", blockRole, "blocking_session_block_reason", block.Reason)
			return err
		}
		return fmt.Errorf("delete from db: %w", err)
	}
	if !deleted {
		return fmt.Errorf("session changed since GC read it (merged, no longer ended, or waiting to sync a change); kept with its audio")
	}
	deletedSet := make(map[string]bool, len(deletedIDs))
	for _, deletedID := range deletedIDs {
		deletedSet[deletedID] = true
		c.recordingMismatch(deletedID, false)
		names.forget(deletedID)
	}

	saved := false
	for _, f := range files {
		if err := c.removeErasing(id, f.path); err != nil {
			if !os.IsNotExist(err) {
				c.logger.Warn("gc failed to remove audio file", "operation", "remove_audio", "session_id", id, "audio_path", f.path, "error", err)
			}
			continue
		}
		saved = saved || f.size > 0
	}
	if saved && c.config.AudioDir != "" {
		for _, deletedID := range deletedIDs {
			rawPath := c.rawPath(deletedID)
			if len(names[rawPath]) > 0 {
				continue
			}
			if err := c.removeErasing(deletedID, rawPath); err != nil && !os.IsNotExist(err) {
				c.logger.Warn("gc failed to remove raw recording", "operation", "remove_raw_audio", "session_id", deletedID, "raw_audio", rawPath, "error", err)
			}
		}
	}
	for _, reservation := range reservations {
		if !deletedSet[reservation.SessionID] {
			continue
		}
		if saved {
			if err := erase.Finish(reservation.OriginalPath, reservation.HiddenPath); err != nil {
				c.logger.Warn("gc could not overwrite a reserved raw recording", "operation", "remove_raw_audio", "session_id", reservation.SessionID, "raw_audio", reservation.OriginalPath, "error", err)
				continue
			}
		} else if err := erase.Restore(reservation.OriginalPath, reservation.HiddenPath); err != nil {
			c.logger.Warn("gc could not restore the only raw recording", "operation", "keep_raw_audio", "session_id", reservation.SessionID, "raw_audio", reservation.OriginalPath, "error", err)
			continue
		}
		if err := c.store.CancelRawSessionErase(reservation.SessionID, reservation.HiddenPath); err != nil {
			c.logger.Warn("gc could not clear a raw erase reservation", "operation", "remove_raw_audio", "session_id", reservation.SessionID, "raw_audio", reservation.OriginalPath, "error", err)
		}
	}
	return nil
}

func rawReservationExists(sessionID string, reservations []storage.RawEraseReservation) (bool, error) {
	for _, reservation := range reservations {
		if reservation.SessionID != sessionID {
			continue
		}
		if _, err := os.Stat(reservation.HiddenPath); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("stat reserved raw PCM of merged source %s: %w", sessionID, err)
		}
	}
	return false, nil
}

func restoreReservedRaw(store Store, reservation storage.RawEraseReservation) error {
	if err := erase.Restore(reservation.OriginalPath, reservation.HiddenPath); err != nil {
		return err
	}
	return store.CancelRawSessionErase(reservation.SessionID, reservation.HiddenPath)
}

func sourceHasSavedAudio(sourceID string, files []recordingFile) bool {
	for _, file := range files {
		base := filepath.Base(file.path)
		if strings.TrimSuffix(base, filepath.Ext(base)) == sourceID && file.size > 0 {
			return true
		}
	}
	return false
}

// recordEraseDirectories records the directory of each of paths before GC
// changes the row that names them or erases them, so an erase a stop cuts off
// is finished as the app starts, also once no row names the file. It reports
// the first failure; GC then changes and erases none of them.
func (c *Collector) recordEraseDirectories(paths []string) error {
	recorded := map[string]bool{}
	for _, path := range paths {
		dir := filepath.Dir(path)
		if recorded[dir] {
			continue
		}
		if err := c.store.RecordEraseDirectory(dir); err != nil {
			return fmt.Errorf("record the erase directory %s: %w", dir, err)
		}
		recorded[dir] = true
	}
	return nil
}

// removeErasing removes the audio file at path, overwriting its bytes with
// zeros before the file system frees them (erase.Remove): a whole-file audio
// removal zeroes (Sami, 2026-11-12, "Zero every audio removal",
// dispatch://INFRA-2961/ask/172349fa-0019-488c-9d3b-9d6b7af26a81). Its caller
// has recorded path's directory (recordEraseDirectories). It reports an error
// only if path still names the file. A file gone from path whose bytes could
// not be overwritten is logged; erase.Leftovers finishes it as the app
// starts.
func (c *Collector) removeErasing(sessionID, path string) error {
	err := erase.Remove(path)
	if errors.Is(err, erase.ErrLeftover) {
		c.logger.Warn("gc removed an audio file but could not overwrite it yet; it is erased as the app starts", "operation", "erase_audio", "session_id", sessionID, "audio_path", path, "error", err)
		return nil
	}
	return err
}

// sessionAudio is a session's audio that disk pressure may remove.
type sessionAudio struct {
	id        string
	audioPath string          // as the session's row has it
	entries   []string        // its entries
	files     []recordingFile // the entries' files on disk that no other session names
	dead      map[string]bool // resolved paths of entries that name no file
	raw       *recordingFile  // a raw .pcm beside a saved recording, which no session names: a redundant copy
}

// size is what a's files take on disk: what the audio budget counts of it.
func (a *sessionAudio) size() int64 {
	var size int64
	for _, f := range a.files {
		size += f.size
	}
	if a.raw != nil {
		size += a.raw.size
	}
	return size
}

// removableAudio returns the audio disk pressure may remove, oldest session
// first, and its total size on disk: the saved recordings of ended synced
// sessions (not while a refinement reads one), and a raw .pcm beside a saved
// recording that has audio in it. It never lists a raw recording that is the
// only copy of a session's audio, the recording under way, or a file another
// session names. The audio budget counts exactly these files, so what is
// counted can be removed.
func (c *Collector) removableAudio() ([]sessionAudio, int64, error) {
	recordings, err := c.store.Recordings()
	if err != nil {
		return nil, 0, err
	}
	names := c.namesOf(recordings)
	var total int64
	var audio []sessionAudio
	for _, rec := range recordings {
		if !rec.Removable {
			continue
		}
		a := sessionAudio{id: rec.ID, audioPath: rec.AudioPath, entries: splitAudioPath(rec.AudioPath), dead: map[string]bool{}}
		seen := map[string]bool{}
		saved := false
		for _, entry := range a.entries {
			path := c.resolve(entry)
			if seen[path] || names.namedByAnother(path, rec.ID) {
				continue
			}
			seen[path] = true
			info, err := os.Stat(path)
			if os.IsNotExist(err) {
				// It names nothing: recording.Resolve found no file where the
				// entry points either.
				a.dead[path] = true
				continue
			}
			if err != nil || info.IsDir() {
				continue
			}
			a.files = append(a.files, recordingFile{entry: entry, path: path, size: info.Size()})
			total += info.Size()
			saved = saved || info.Size() > 0
		}
		if saved && c.config.AudioDir != "" {
			// A .pcm a session names is that session's recording, not a raw copy.
			rawPath := c.rawPath(rec.ID)
			if raw, ok := c.audioFile(rawPath); ok && len(names[rawPath]) == 0 {
				a.raw = &raw
				total += raw.size
			}
		}
		audio = append(audio, a)
	}
	return audio, total, nil
}

// freeAudioSpace removes audio, oldest session first, while the audio disk
// pressure may remove takes more than the audio budget. It deletes no
// session: one whose recording it removed keeps its row, transcript and
// summary, with its audio path naming only what is left and the marker the UI
// shows. It removes a session's audio only after Drive confirms its folder
// and every local recording by content (onDrive); a session it keeps no
// longer counts, and a Drive that cannot say ends the pass. A file whose
// removal frees nothing (missing, empty, or refused) is skipped and logged.
// The pass ends when the list is exhausted or the audio fits.
func (c *Collector) freeAudioSpace() {
	budget, limited := AudioBudgetBytes(c.config.MaxAudioSizeMB)
	if !limited || c.config.AudioDir == "" {
		return
	}
	audio, total, err := c.removableAudio()
	if err != nil {
		c.logger.Warn("gc skipped freeing audio space", "operation", "free_audio_space", "error", err)
		return
	}

	for i := range audio {
		a := &audio[i]
		if total <= budget {
			return
		}
		if err := c.driveKeeps(a.id); err != nil {
			if errors.Is(err, ErrDriveCannotSay) {
				c.held = err
				c.logger.Warn("gc stopped freeing audio space", "operation", "free_audio_space", "session_id", a.id, "error", err)
				return
			}
			c.logger.Info("gc kept a session's audio", "operation", "free_audio_space", "session_id", a.id, "error", err)
			total -= a.size()
			continue
		}
		// The raw copy first: if the saved recording then cannot go, its audio
		// is still on disk. Its directory is recorded before it is erased, and
		// it is kept if that fails.
		if a.raw != nil {
			if err := c.recordEraseDirectories([]string{a.raw.path}); err != nil {
				c.logger.Warn("gc kept a raw recording: it could not record where it erases it", "operation", "free_audio_space", "session_id", a.id, "raw_audio", a.raw.path, "error", err)
			} else if freed := c.removeAudioFile(a.id, *a.raw); freed > 0 {
				total -= freed
				c.logger.Info("gc removed a raw recording beside its saved recording to free space", "operation", "free_audio_space", "session_id", a.id, "raw_audio", a.raw.path)
			}
			if total <= budget {
				return
			}
		}
		total -= c.removeRecording(a)
	}
	if total > budget {
		c.logger.Warn("gc could not bring the audio under its budget", "operation", "free_audio_space",
			"removable_mb", total/(1024*1024), "budget_mb", c.config.MaxAudioSizeMB)
	}
}

// driveKeeps returns why GC keeps session id's audio, or nil when it may
// remove it (see onDrive).
func (c *Collector) driveKeeps(id string) error {
	sess, err := c.store.GetSession(id)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}
	return c.onDrive(&sess)
}

// removeRecording removes the files of session a's saved recording that free
// space, and returns the bytes that freed. It first records the files'
// directories, and then the removal: the session's row stops naming those
// files before any of them goes, so the row never names a file GC removed,
// and an erase a stop cuts off is finished as the app starts. It keeps naming
// every other entry, one whose file it cannot see too: that file may be on a
// disk not mounted for the moment, and a name dropped cannot be recovered. If
// the directories cannot be recorded or the row cannot be changed (the
// database fails, a refinement started reading the file, the session changed
// since it was listed), nothing is removed. A file that does not go is put
// back in the row.
func (c *Collector) removeRecording(a *sessionAudio) int64 {
	var take []recordingFile
	for _, f := range a.files {
		if f.size > 0 {
			take = append(take, f)
		} else {
			c.logger.Warn("gc skipped an empty audio file: removing it would free nothing", "operation", "free_audio_space", "session_id", a.id, "audio_path", f.path)
		}
	}
	if len(take) == 0 {
		return 0
	}
	taking := map[string]bool{}
	for _, f := range take {
		taking[f.path] = true
	}
	left := c.left(a, taking)
	erased := make([]string, 0, len(take))
	for _, f := range take {
		erased = append(erased, f.path)
	}
	if err := c.recordEraseDirectories(erased); err != nil {
		c.logger.Error("gc could not record where it erases a session's audio, so it removed none", "operation", "free_audio_space", "session_id", a.id, "error", err)
		return 0
	}
	removal, ok, err := c.store.MarkAudioRemoved(a.id, a.audioPath, strings.Join(left, ","), c.audioLeft(a, left), time.Now())
	if err != nil {
		c.logger.Error("gc could not record removing a session's audio, so it removed none", "operation", "free_audio_space", "session_id", a.id, "error", err)
		return 0
	}
	if !ok {
		c.logger.Info("gc kept a session's audio: the session changed since it was listed, or a refinement is reading it", "operation", "free_audio_space", "session_id", a.id)
		return 0
	}

	var freed int64
	removed := map[string]bool{}
	removedEntries := []string{}
	for _, f := range take {
		if n := c.removeAudioFile(a.id, f); n > 0 {
			freed += n
			removed[f.path] = true
			removedEntries = append(removedEntries, f.entry)
		}
	}
	if len(removed) < len(take) {
		// Put back what did not go: all of it, as it was, if nothing went.
		restored := a.audioPath
		if len(removed) > 0 {
			left = c.left(a, removed)
			restored = strings.Join(left, ",")
		}
		if err := c.store.RestoreAudio(&removal, restored); err != nil {
			c.logger.Error("gc could not name again in its session the audio files it failed to remove; they are on disk", "operation", "free_audio_space",
				"session_id", a.id, "audio_path", restored, "error", err)
		}
		if len(removed) == 0 {
			return 0
		}
	}
	c.logger.Info("gc removed a session's audio to free space; the session is kept", "operation", "free_audio_space",
		"session_id", a.id, "removed", removedEntries, "left", left)
	return freed
}

// left returns the entries of a's audio path that are left once the files in
// gone are removed: every entry but those naming one of them.
func (c *Collector) left(a *sessionAudio, gone map[string]bool) []string {
	left := []string{}
	for _, entry := range a.entries {
		if !gone[c.resolve(entry)] {
			left = append(left, entry)
		}
	}
	return left
}

// audioLeft reports whether any of the entries left of a's audio path names
// a file that was there when GC looked: a recording left to play or refine.
func (c *Collector) audioLeft(a *sessionAudio, left []string) bool {
	for _, entry := range left {
		if !a.dead[c.resolve(entry)] {
			return true
		}
	}
	return false
}

// removeAudioFile removes one file disk pressure may remove and returns the
// bytes that freed: its size before, if it is gone after. A file that is
// missing, empty or refuses removal frees nothing; it is skipped and logged.
func (c *Collector) removeAudioFile(sessionID string, f recordingFile) int64 {
	before, err := os.Stat(f.path)
	if err != nil {
		c.logger.Warn("gc skipped an audio file it cannot read", "operation", "free_audio_space", "session_id", sessionID, "audio_path", f.path, "error", err)
		return 0
	}
	if before.IsDir() || before.Size() == 0 {
		c.logger.Warn("gc skipped an audio file that would free nothing", "operation", "free_audio_space", "session_id", sessionID, "audio_path", f.path)
		return 0
	}
	if err := c.removeErasing(sessionID, f.path); err != nil {
		c.logger.Warn("gc could not remove an audio file", "operation", "free_audio_space", "session_id", sessionID, "audio_path", f.path, "error", err)
		return 0
	}
	if _, err := os.Stat(f.path); err == nil {
		c.logger.Warn("gc removed an audio file that is still there", "operation", "free_audio_space", "session_id", sessionID, "audio_path", f.path)
		return 0
	}
	return before.Size()
}

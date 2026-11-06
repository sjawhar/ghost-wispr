package gc

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/logging"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

type Store interface {
	GetGCEligibleSessions(maxAgeDays int, syncGated bool) ([]string, error)
	GetSession(id string) (storage.Session, error)
	DeleteSession(id string) error
	PurgeOldEvents(maxAge time.Duration) error
	RemovableRecordings(syncGated bool) ([]storage.SessionRecording, error)
	MarkAudioRemoved(id, audioPath string, removedAt time.Time) error
}

type Config struct {
	MaxAgeDays     int
	MaxAudioSizeMB int
	SyncGated      bool
	AudioDir       string
}

type Collector struct {
	store  Store
	config Config
	logger *slog.Logger
}

func New(store Store, config Config, logger ...*slog.Logger) *Collector {
	l := logging.WithModule(slog.Default(), "gc")
	if len(logger) > 0 && logger[0] != nil {
		l = logging.WithModule(logger[0], "gc")
	}

	return &Collector{store: store, config: config, logger: l}
}

// Run applies the age policy, which deletes old sessions, then frees audio
// space under the audio budget, which removes only audio files. It returns
// the sessions it deleted.
func (c *Collector) Run() ([]string, error) {
	ids, err := c.store.GetGCEligibleSessions(c.config.MaxAgeDays, c.config.SyncGated)
	if err != nil {
		return nil, fmt.Errorf("query gc eligible: %w", err)
	}

	var deleted []string
	for _, id := range ids {
		if err := c.deleteSession(id); err != nil {
			c.logger.Warn("gc skipped session", "operation", "delete_session", "session_id", id, "error", err)
			continue
		}
		deleted = append(deleted, id)
	}

	c.freeAudioSpace()

	// Purge old events from the durable event queue (7-day default retention).
	if err := c.store.PurgeOldEvents(7 * 24 * time.Hour); err != nil {
		c.logger.Warn("gc failed to purge old events", "operation", "purge_events", "error", err)
	}

	return deleted, nil
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

// audioFile resolves an audio path entry, absolute or relative to the audio
// directory, and reports whether it is a file on disk.
func (c *Collector) audioFile(entry string) (recordingFile, bool) {
	path := entry
	if !filepath.IsAbs(path) && c.config.AudioDir != "" {
		path = filepath.Join(c.config.AudioDir, filepath.Base(path))
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return recordingFile{}, false
	}
	return recordingFile{entry: entry, path: path, size: info.Size()}, true
}

// deleteSession deletes session id under the age policy, with its saved
// recording. It removes the session's raw .pcm only when a saved recording
// with audio in it was on disk beside it, so the raw file is a redundant copy;
// otherwise the raw file is the only copy of the audio and stays: GC never
// removes one.
func (c *Collector) deleteSession(id string) error {
	sess, err := c.store.GetSession(id)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}
	var files []recordingFile
	for _, entry := range splitAudioPath(sess.AudioPath) {
		if f, ok := c.audioFile(entry); ok {
			files = append(files, f)
		}
	}

	// Delete DB record first — if this fails, audio files remain (recoverable).
	// Reverse order (delete files first) risks permanent audio loss on DB failure.
	if err := c.store.DeleteSession(id); err != nil {
		return fmt.Errorf("delete from db: %w", err)
	}

	saved := false
	for _, f := range files {
		saved = saved || f.size > 0
		if err := os.Remove(f.path); err != nil && !os.IsNotExist(err) {
			c.logger.Warn("gc failed to remove audio file", "operation", "remove_audio", "session_id", id, "audio_path", f.path, "error", err)
		}
	}
	if saved && c.config.AudioDir != "" {
		rawPath := filepath.Join(c.config.AudioDir, id+".pcm")
		if err := os.Remove(rawPath); err != nil && !os.IsNotExist(err) {
			c.logger.Warn("gc failed to remove raw recording", "operation", "remove_raw_audio", "session_id", id, "raw_audio", rawPath, "error", err)
		}
	}
	return nil
}

// sessionAudio is a session's audio that disk pressure may remove.
type sessionAudio struct {
	id      string
	entries []string        // the session's audio path entries
	files   []recordingFile // the entries' files on disk
	raw     *recordingFile  // a raw .pcm beside a saved recording: a redundant copy
}

// removableAudio returns the audio disk pressure may remove, oldest session
// first, and its total size on disk: the saved recordings of ended sessions
// (synced ones only when Drive sync gates GC, so a sync that failed keeps its
// file; not while a refinement reads one), and a raw .pcm beside a saved
// recording that has audio in it. It never lists a raw recording that is the
// only copy of a session's audio, or the recording under way. The audio
// budget counts exactly these files, so what is counted can be removed.
func (c *Collector) removableAudio() ([]sessionAudio, int64, error) {
	recordings, err := c.store.RemovableRecordings(c.config.SyncGated)
	if err != nil {
		return nil, 0, err
	}
	seen := make(map[string]struct{})
	var total int64
	audio := make([]sessionAudio, 0, len(recordings))
	for _, rec := range recordings {
		a := sessionAudio{id: rec.ID, entries: splitAudioPath(rec.AudioPath)}
		saved := false
		for _, entry := range a.entries {
			f, ok := c.audioFile(entry)
			if !ok {
				continue
			}
			if _, dup := seen[f.path]; dup {
				continue
			}
			seen[f.path] = struct{}{}
			a.files = append(a.files, f)
			total += f.size
			saved = saved || f.size > 0
		}
		if saved && c.config.AudioDir != "" {
			if raw, ok := c.audioFile(filepath.Join(c.config.AudioDir, rec.ID+".pcm")); ok {
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
// summary, with its audio path cleared and the marker the UI shows. A file
// whose removal frees nothing (missing, empty, or refused) is skipped and
// logged. The pass ends when the list is exhausted or the audio fits.
func (c *Collector) freeAudioSpace() {
	if c.config.MaxAudioSizeMB <= 0 || c.config.AudioDir == "" {
		return
	}
	budget := int64(c.config.MaxAudioSizeMB) * 1024 * 1024
	audio, total, err := c.removableAudio()
	if err != nil {
		c.logger.Warn("gc skipped freeing audio space", "operation", "free_audio_space", "error", err)
		return
	}

	for _, a := range audio {
		if total <= budget {
			return
		}
		// The raw copy first: if the saved recording then cannot go, its audio
		// is still on disk.
		if a.raw != nil {
			total -= c.removeAudioFile(a.id, *a.raw)
			if total <= budget {
				return
			}
		}
		removed := make(map[string]bool, len(a.files))
		for _, f := range a.files {
			if freed := c.removeAudioFile(a.id, f); freed > 0 {
				total -= freed
				removed[f.entry] = true
			}
		}
		if len(removed) == 0 {
			continue
		}
		var left []string
		for _, entry := range a.entries {
			if !removed[entry] {
				left = append(left, entry)
			}
		}
		if err := c.store.MarkAudioRemoved(a.id, strings.Join(left, ","), time.Now()); err != nil {
			c.logger.Error("gc removed a session's audio but could not record it", "operation", "free_audio_space", "session_id", a.id, "error", err)
			continue
		}
		c.logger.Info("gc removed a session's audio to free space; the session is kept", "operation", "free_audio_space", "session_id", a.id)
	}
	if total > budget {
		c.logger.Warn("gc could not bring the audio under its budget", "operation", "free_audio_space",
			"removable_mb", total/(1024*1024), "budget_mb", c.config.MaxAudioSizeMB)
	}
}

// removeAudioFile removes one file disk pressure may remove and returns the
// bytes that freed: its size before, if it is gone after. A file that is
// missing, empty or refuses removal frees nothing; it is skipped and logged.
func (c *Collector) removeAudioFile(sessionID string, f recordingFile) int64 {
	before, err := os.Stat(f.path)
	if err != nil || before.IsDir() || before.Size() == 0 {
		c.logger.Warn("gc skipped an audio file that would free nothing", "operation", "free_audio_space", "session_id", sessionID, "audio_path", f.path, "error", err)
		return 0
	}
	if err := os.Remove(f.path); err != nil {
		c.logger.Warn("gc could not remove an audio file", "operation", "free_audio_space", "session_id", sessionID, "audio_path", f.path, "error", err)
		return 0
	}
	if _, err := os.Stat(f.path); err == nil {
		c.logger.Warn("gc removed an audio file that is still there", "operation", "free_audio_space", "session_id", sessionID, "audio_path", f.path)
		return 0
	}
	return before.Size()
}

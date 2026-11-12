package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	gwstatus "github.com/sjawhar/ghost-wispr/internal/status"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

const (
	SummaryPending   = gwstatus.SummaryPending
	SummaryRunning   = gwstatus.SummaryRunning
	SummaryCompleted = gwstatus.SummaryCompleted
	SummaryFailed    = gwstatus.SummaryFailed

	RefinementPending   = gwstatus.RefinementPending
	RefinementRunning   = gwstatus.RefinementRunning
	RefinementCompleted = gwstatus.RefinementCompleted
	RefinementFailed    = gwstatus.RefinementFailed
	RefinementNoAudio   = gwstatus.RefinementNoAudio

	SessionActive    = gwstatus.SessionActive
	SessionEnded     = gwstatus.SessionEnded
	SessionDiscarded = gwstatus.SessionDiscarded
	SessionMerged    = gwstatus.SessionMerged

	SyncPending = gwstatus.SyncPending
	SyncSynced  = gwstatus.SyncSynced
	SyncFailed  = gwstatus.SyncFailed

	EnvoyPublishPending   = gwstatus.EnvoyPublishPending
	EnvoyPublishPublished = gwstatus.EnvoyPublishPublished
	EnvoyPublishFailed    = gwstatus.EnvoyPublishFailed
	EnvoyPublishSkipped   = gwstatus.EnvoyPublishSkipped

	SyncStatePending        = gwstatus.SyncStatePending
	SyncStateSyncing        = gwstatus.SyncStateSyncing
	SyncStateSynced         = gwstatus.SyncStateSynced
	SyncStateFailed         = gwstatus.SyncStateFailed
	SyncStateRetryScheduled = gwstatus.SyncStateRetryScheduled
	SyncStateRemoteDeleted  = gwstatus.SyncStateRemoteDeleted

	TranscriptSourceStreaming = gwstatus.TranscriptSourceStreaming
	TranscriptSourceRefined   = gwstatus.TranscriptSourceRefined

	ComponentStatusConnected    = gwstatus.ComponentStatusConnected
	ComponentStatusDisconnected = gwstatus.ComponentStatusDisconnected
	ComponentStatusReconnecting = gwstatus.ComponentStatusReconnecting
	ComponentStatusError        = gwstatus.ComponentStatusError
	ComponentStatusOK           = gwstatus.ComponentStatusOK
	ComponentStatusOpen         = gwstatus.ComponentStatusOpen
	ComponentStatusClosed       = gwstatus.ComponentStatusClosed
	ComponentStatusPaused       = gwstatus.ComponentStatusPaused
	ComponentStatusResuming     = gwstatus.ComponentStatusResuming
	ComponentStatusStalled      = gwstatus.ComponentStatusStalled
)

type Session struct {
	ID                  string     `json:"id"`
	Title               string     `json:"title"`
	StartedAt           time.Time  `json:"started_at"`
	EndedAt             *time.Time `json:"ended_at,omitempty"`
	Status              string     `json:"status"`
	Summary             string     `json:"summary"`
	SummaryVersion      int        `json:"summary_version"`
	SummaryStatus       string     `json:"summary_status"`
	SummaryPreset       string     `json:"summary_preset"`
	SpeakerNames        string     `json:"speaker_names"`
	RefinedTranscript   string     `json:"refined_transcript"`
	RefinementStatus    string     `json:"refinement_status"`
	AudioPath           string     `json:"audio_path"`
	AudioRemovedAt      string     `json:"audio_removed_at"` // when disk pressure removed audio (RFC 3339), "" if never: all of it, or part when AudioPath names what is left; the session is kept
	SummaryError        string     `json:"summary_error"`    // why the last summary attempt failed, "" once one completes; sync errors are in ErrorMessage
	SyncStatus          string     `json:"sync_status"`
	EnvoyPublishStatus  string     `json:"envoy_publish_status"`
	SyncState           string     `json:"sync_state"`
	RetryCount          int        `json:"retry_count"`
	LastSyncAttempt     *time.Time `json:"last_sync_attempt,omitempty"`
	ErrorMessage        string     `json:"error_message"`
	GDriveFolderID      string     `json:"gdrive_folder_id"`
	MergedInto          string     `json:"merged_into"`
	CanonicalTranscript string     `json:"canonical_transcript"`
	TranscriptSource    string     `json:"transcript_source"`
	DriveReplacePending bool       `json:"drive_replace_pending"`
}

type SearchResult struct {
	SessionID string  `json:"session_id"`
	Title     string  `json:"title"`
	Snippet   string  `json:"snippet"`
	Rank      float64 `json:"rank"`
}

type SearchOptions struct {
	DateFrom string // RFC3339 format, optional
	DateTo   string // RFC3339 format, optional
	Preset   string // summary_preset value, optional
	Speaker  string // speaker name or numeric index, optional
}

// AggregateOptions controls filtering and grouping for cross-session aggregation.
type AggregateOptions struct {
	DateFrom string // RFC3339 format, optional
	DateTo   string // RFC3339 format, optional
	Preset   string // summary_preset value, optional
	GroupBy  string // "date" (default) or "preset"
}

// AggregateResult holds cross-session statistics.
type AggregateResult struct {
	SessionCount int              `json:"session_count"`
	Groups       []AggregateGroup `json:"groups"`
}

// AggregateGroup is a single grouping bucket.
type AggregateGroup struct {
	Key      string           `json:"key"`
	Count    int              `json:"count"`
	Sessions []SessionSummary `json:"sessions"`
}

// SessionSummary is a lightweight session representation for aggregation.
type SessionSummary struct {
	ID            string    `json:"id"`
	Title         string    `json:"title"`
	StartedAt     time.Time `json:"started_at"`
	SummaryPreset string    `json:"summary_preset"`
}

type StoredEmbedding struct {
	SessionID  string
	ChunkIndex int
	Vector     []float32
	TextHash   string
	Model      string
	CreatedAt  time.Time
}

type StoredEvent struct {
	ID        int64     `json:"id"`
	EventType string    `json:"event_type"`
	Payload   string    `json:"payload"`
	CreatedAt time.Time `json:"created_at"`
}

var ftsQueryTokenPattern = regexp.MustCompile(`[-+]?[\p{L}\p{N}_]+`)

type SQLiteStore struct {
	db *sql.DB
}

func syncStatusFromState(syncState string) string {
	if syncState == SyncStateSynced {
		return SyncSynced
	}
	if syncState == SyncStatePending || syncState == SyncStateSyncing {
		return SyncPending
	}
	return SyncFailed
}

func NewSQLiteStore(dbPath string) (*SQLiteStore, error) {
	if strings.TrimSpace(dbPath) == "" {
		dbPath = filepath.Join("data", "ghost-wispr.db")
	}

	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}

	// One connection keeps DeleteSegment out of Canonicalize's transaction.
	// TestR7OpenTransactionExcludesDeleteSegment guards this invariant.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	store := &SQLiteStore{db: db}

	if err := store.init(dbPath); err != nil {
		_ = db.Close()
		return nil, err
	}

	return store, nil
}

// createPreMigrationBackup writes a copy of the database to dbPath +
// ".pre-migrate.bak" before migrations run. It copies through db, the store's
// own connection, with VACUUM INTO, and never opens the database file itself:
// closing a second descriptor of that file drops every POSIX lock this
// process holds on it, the locks SQLite took. Another SQLite process (the
// sqlite3 command, a backup script) that opens and closes the database after
// that takes itself for its last user and deletes the write-ahead log, and
// every later write of the app then goes to a deleted file, lost on a crash.
// The copy is written beside the backup, flushed to disk and renamed over it,
// so a backup cut off part way leaves the last one as it was; a backup that
// fails removes its unfinished copy.
// If the DB file doesn't exist (fresh install), the backup is skipped.
// If backup fails, a warning is logged but startup continues (degraded is better than dead).
func createPreMigrationBackup(db *sql.DB, dbPath string) (err error) {
	// Check if DB file exists (skip backup for fresh installs)
	_, err = os.Stat(dbPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Fresh install, no backup needed
			return nil
		}
		return fmt.Errorf("stat db file: %w", err)
	}

	backupPath := dbPath + ".pre-migrate.bak"
	partPath := backupPath + ".part"
	// VACUUM INTO writes only a file that does not exist yet: remove the
	// copy a backup cut off part way left.
	if err := os.Remove(partPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove unfinished backup: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(partPath)
		}
	}()
	// VACUUM INTO reads one consistent snapshot of the database, the
	// write-ahead log included.
	if _, err := db.Exec(`VACUUM INTO ?`, partPath); err != nil {
		return fmt.Errorf("copy db to backup: %w", err)
	}
	// SQLite does not flush the copy VACUUM INTO writes.
	if err := syncFile(partPath); err != nil {
		return fmt.Errorf("flush backup: %w", err)
	}
	if err := os.Rename(partPath, backupPath); err != nil {
		return fmt.Errorf("replace backup: %w", err)
	}
	// The rename lasts only once the directory naming it is on disk.
	if err := syncFile(filepath.Dir(backupPath)); err != nil {
		return fmt.Errorf("flush backup directory: %w", err)
	}

	slog.Info("pre-migration backup created", "path", backupPath)
	return nil
}

// syncFile flushes the file at path to disk.
func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func (s *SQLiteStore) init(dbPath string) error {
	pragmas := []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA secure_delete = ON",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
	}
	for _, p := range pragmas {
		if _, err := s.db.Exec(p); err != nil {
			return fmt.Errorf("apply pragma %q: %w", p, err)
		}
	}

	// Create pre-migration backup after pragmas are set (which creates the DB file)
	if err := createPreMigrationBackup(s.db, dbPath); err != nil {
		slog.Warn("failed to create pre-migration backup", "path", dbPath, "error", err)
	}

	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			title TEXT NOT NULL DEFAULT '',
			started_at TEXT NOT NULL,
			ended_at TEXT,
			status TEXT NOT NULL,
			summary TEXT NOT NULL DEFAULT '',
			summary_version INTEGER NOT NULL DEFAULT 1,
			summary_status TEXT NOT NULL DEFAULT 'pending',
			summary_preset TEXT NOT NULL DEFAULT '',
			speaker_names TEXT NOT NULL DEFAULT '{}',
			refined_transcript TEXT NOT NULL DEFAULT '',
			refinement_status TEXT NOT NULL DEFAULT 'pending',
			audio_path TEXT NOT NULL DEFAULT '',
			sync_status TEXT NOT NULL DEFAULT 'pending',
			envoy_publish_status TEXT NOT NULL DEFAULT 'pending',
			gdrive_folder_id TEXT NOT NULL DEFAULT '',
			merged_into TEXT NOT NULL DEFAULT '',
			retry_count INTEGER NOT NULL DEFAULT 0,
			last_sync_attempt TEXT,
			error_message TEXT NOT NULL DEFAULT '',
			sync_state TEXT NOT NULL DEFAULT 'PENDING',
			canonical_transcript TEXT NOT NULL DEFAULT '',
			transcript_source TEXT NOT NULL DEFAULT '',
			drive_replace_pending INTEGER NOT NULL DEFAULT 0,
			deletion_generation INTEGER NOT NULL DEFAULT 0,
			refinement_generation INTEGER NOT NULL DEFAULT 0,
			summary_generation INTEGER NOT NULL DEFAULT 0,
			drive_replace_since TEXT NOT NULL DEFAULT '',
			drive_replace_failures INTEGER NOT NULL DEFAULT 0,
			drive_replace_error TEXT NOT NULL DEFAULT ''
		);
	`); err != nil {
		return fmt.Errorf("create sessions table: %w", err)
	}

	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS app_state (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
	`); err != nil {
		return fmt.Errorf("create app_state table: %w", err)
	}

	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS drive_replacements (
			session_id TEXT NOT NULL,
			generation INTEGER NOT NULL DEFAULT 0,
			name TEXT NOT NULL,
			old_id TEXT NOT NULL,
			new_id TEXT NOT NULL,
			state TEXT NOT NULL,
			PRIMARY KEY (session_id, generation, name, old_id),
			FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
		);
	`); err != nil {
		return fmt.Errorf("create drive replacements table: %w", err)
	}

	// drive_replacement_folders is the ledger of the Drive folders a segment
	// deletion (session_id, generation) owes a replacement: the session's own
	// and each merged source's that had one when the deletion committed.
	// folder_session_id names the session whose folder it is. A folder stays
	// owed until it is replaced or found gone, so the deletion is finished
	// only when none is owed.
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS drive_replacement_folders (
			session_id TEXT NOT NULL,
			generation INTEGER NOT NULL,
			folder_session_id TEXT NOT NULL,
			state TEXT NOT NULL DEFAULT 'owed',
			PRIMARY KEY (session_id, generation, folder_session_id),
			FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
		);
	`); err != nil {
		return fmt.Errorf("create drive replacement folders table: %w", err)
	}

	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS envoy_lifecycle_events (
			event_type TEXT NOT NULL,
			session_id TEXT NOT NULL,
			PRIMARY KEY (event_type, session_id),
			FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
		);
	`); err != nil {
		return fmt.Errorf("create envoy lifecycle events table: %w", err)
	}

	if _, err := s.db.Exec(`
		CREATE VIRTUAL TABLE IF NOT EXISTS sessions_fts USING fts5(
			title, summary, canonical_transcript,
			content='sessions', content_rowid='rowid'
		);
	`); err != nil {
		return fmt.Errorf("create sessions_fts table: %w", err)
	}

	// Migrate: add columns if they don't exist (for pre-existing DBs).
	// Only ignore "duplicate column" errors; propagate other failures.
	for _, stmt := range []string{
		`ALTER TABLE sessions ADD COLUMN summary_preset TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN speaker_names TEXT NOT NULL DEFAULT '{}'`,
		`ALTER TABLE sessions ADD COLUMN summary_version INTEGER NOT NULL DEFAULT 1`,
		`ALTER TABLE sessions ADD COLUMN title TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN sync_status TEXT NOT NULL DEFAULT 'pending'`,
		`ALTER TABLE sessions ADD COLUMN envoy_publish_status TEXT NOT NULL DEFAULT 'pending'`,
		`ALTER TABLE sessions ADD COLUMN gdrive_folder_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN refined_transcript TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN refinement_status TEXT NOT NULL DEFAULT 'pending'`,
		`ALTER TABLE sessions ADD COLUMN merged_into TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN retry_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE sessions ADD COLUMN last_sync_attempt TEXT`,
		`ALTER TABLE sessions ADD COLUMN error_message TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN sync_state TEXT NOT NULL DEFAULT 'PENDING'`,
		`ALTER TABLE sessions ADD COLUMN canonical_transcript TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN transcript_source TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN audio_removed_at TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN restored_at TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN sync_claim TEXT NOT NULL DEFAULT ''`,
		// 1 while Drive may lack the session's current summary and
		// transcript though it is synced (mark-synced-documents-unverified,
		// below); a sync of it that completes sets 0 (MarkSynced). The age
		// policy never deletes a session marked 1.
		`ALTER TABLE sessions ADD COLUMN documents_unverified INTEGER NOT NULL DEFAULT 0`,
		// Why the last summary attempt failed, apart from error_message, which
		// says why the last sync did.
		`ALTER TABLE sessions ADD COLUMN summary_error TEXT NOT NULL DEFAULT ''`,
		// The sync state a sync's claim came from, while it holds the session:
		// startup recovery puts a sync claimed from REMOTE_DELETED back.
		`ALTER TABLE sessions ADD COLUMN sync_claimed_from TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN drive_replace_pending INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE sessions ADD COLUMN deletion_generation INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE sessions ADD COLUMN refinement_generation INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE sessions ADD COLUMN summary_generation INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE sessions ADD COLUMN drive_replace_since TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN drive_replace_failures INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE sessions ADD COLUMN drive_replace_error TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := s.db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("migration failed: %w", err)
		}
	}

	if _, err := s.db.Exec(`ALTER TABLE drive_replacements ADD COLUMN generation INTEGER NOT NULL DEFAULT 0`); err != nil && !strings.Contains(err.Error(), "duplicate column") {
		return fmt.Errorf("migrate drive replacements generation: %w", err)
	}

	if _, err := s.db.Exec(
		`UPDATE sessions SET envoy_publish_status = ? WHERE envoy_publish_status = ?`,
		EnvoyPublishPending,
		"rejected",
	); err != nil {
		return fmt.Errorf("recover rejected envoy publishes: %w", err)
	}

	// Sessions that ended without a saved recording before no_audio existed
	// can never be refined either: move them off pending.
	if _, err := s.db.Exec(
		`UPDATE sessions SET refinement_status = ? WHERE status = 'ended' AND audio_path = '' AND refinement_status = ?`,
		RefinementNoAudio, RefinementPending,
	); err != nil {
		return fmt.Errorf("mark sessions without a recording no_audio: %w", err)
	}

	// A merge used to leave its sources naming the recordings it gave the
	// merged session; the merged session alone names them now.
	if _, err := s.db.Exec(`UPDATE sessions SET audio_path = '' WHERE status = 'merged' AND audio_path != ''`); err != nil {
		return fmt.Errorf("move recordings off merged sources: %w", err)
	}

	if err := s.runOnce("resync-merged-session-audio",
		// Drive sync uploaded no audio for a merged session (it stat'ed the
		// joined path of its recordings), yet marked it synced. Sync those
		// sessions again: every session naming several recordings, and every
		// merge target, including one disk pressure had cut down to a single
		// recording. They keep their Drive folder; the sync adds what it lacks.
		`UPDATE sessions SET sync_status = 'pending', sync_state = 'PENDING', retry_count = 0, error_message = ''
		 WHERE status = 'ended' AND sync_status = 'synced' AND audio_path != ''
		   AND (audio_path LIKE '%,%' OR id IN (SELECT merged_into FROM sessions WHERE status = 'merged'))`,
	); err != nil {
		return err
	}

	if err := s.runOnce("mark-synced-documents-unverified",
		// Earlier versions left a session synced when its summary or
		// transcript changed after its sync (it synced while its summary was
		// running, or was summarized again, refined again or retitled), and
		// nothing records what they uploaded. So every session synced when
		// this version first opens the database is marked as having
		// documents not verified on Drive, which keeps it from the age
		// policy until a sync of it completes. Its sync state and folder stay
		// as they are: nothing is uploaded for it until its documents change.
		`UPDATE sessions SET documents_unverified = 1 WHERE sync_status = 'synced'`,
	); err != nil {
		return err
	}

	// An earlier build of this change ran a first form of this move as
	// "move-summary-failures-out-of-error-message"; its row in migrations
	// stays, and nothing reads it.
	if err := s.runOnce("move-summary-failures-to-summary-error",
		// Earlier versions wrote why a summary failed into error_message,
		// which says why the last sync failed. Those messages, which no sync
		// writes, move to summary_error unless a summary has since completed
		// (then they go) or summary_error already says why a later attempt
		// failed (then that stays). A summary left running or pending keeps
		// its old reason until its next attempt.
		`UPDATE sessions
		 SET summary_error = CASE WHEN summary_status != 'completed' AND summary_error = '' THEN error_message ELSE summary_error END,
		     error_message = ''
		 WHERE error_message LIKE 'summarization failed: %'
		    OR error_message LIKE 'failed to get canonical transcript: %'
		    OR error_message LIKE 'failed to store summary: %'`,
	); err != nil {
		return err
	}

	if err := s.runOnce("restored-sessions-synced",
		// Earlier versions stored a session restored from Drive synced with no
		// sync state; it is SYNCED, as ImportSession now stores it.
		`UPDATE sessions SET sync_state = 'SYNCED' WHERE sync_state = '' AND sync_status = 'synced'`,
	); err != nil {
		return err
	}

	// The triggers are made in one transaction, so another connection to the
	// database sees the old sessions_au or the new one, never neither.
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin creating sessions triggers: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`CREATE TRIGGER IF NOT EXISTS sessions_ai AFTER INSERT ON sessions BEGIN
			INSERT INTO sessions_fts(rowid, title, summary, canonical_transcript)
			VALUES (new.rowid, new.title, new.summary, new.canonical_transcript);
		END;`,
		`CREATE TRIGGER IF NOT EXISTS sessions_ad AFTER DELETE ON sessions BEGIN
			INSERT INTO sessions_fts(sessions_fts, rowid, title, summary, canonical_transcript)
			VALUES('delete', old.rowid, old.title, old.summary, old.canonical_transcript);
		END;`,
		// The index follows only the columns it holds, so the update of other
		// columns that sessions_documents_changed (below) makes inside its
		// own run never reaches it. Earlier versions fired on every update:
		// that nested update could then run this trigger before its run for
		// the outer update of the row (the order SQLite runs a table's
		// triggers in is not defined), and delete from the index text it did
		// not hold yet (SQLITE_CORRUPT_VTAB).
		`DROP TRIGGER IF EXISTS sessions_au`,
		`CREATE TRIGGER sessions_au AFTER UPDATE OF title, summary, canonical_transcript ON sessions BEGIN
			INSERT INTO sessions_fts(sessions_fts, rowid, title, summary, canonical_transcript)
			VALUES('delete', old.rowid, old.title, old.summary, old.canonical_transcript);
			INSERT INTO sessions_fts(rowid, title, summary, canonical_transcript)
			VALUES (new.rowid, new.title, new.summary, new.canonical_transcript);
		END;`,
		// A synced session's documents on Drive are its summary and
		// transcript as a sync renders them, from its title, times, summary,
		// preset and canonical transcript. A write that changes one of these
		// (a summary made again, a title edited, a refinement retried) sends
		// the session back to pending sync in the same statement, keeping its
		// Drive folder, so the sweep updates its documents there in place and
		// GC, gated on sync, does not delete it until they are. Its segments,
		// from which a transcript is rendered when it has no canonical one,
		// change only while it is recorded, when a restore imports them, and
		// when a merge moves them off it, in the transaction that marks it
		// merged: the sweep and GC take only ended sessions, and the merged
		// session that now holds them is new, so not synced.
		`DROP TRIGGER IF EXISTS sessions_documents_changed`,
		`CREATE TRIGGER sessions_documents_changed
		AFTER UPDATE OF title, started_at, ended_at, summary, summary_preset, canonical_transcript ON sessions
		WHEN new.sync_status = 'synced' AND (
			new.title IS NOT old.title OR new.started_at IS NOT old.started_at OR new.ended_at IS NOT old.ended_at OR
			new.summary IS NOT old.summary OR new.summary_preset IS NOT old.summary_preset OR
			new.canonical_transcript IS NOT old.canonical_transcript)
		BEGIN
			UPDATE sessions SET sync_status = 'pending', sync_state = 'PENDING' WHERE id = new.id;
		END;`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("create sessions trigger: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit sessions triggers: %w", err)
	}

	if _, err := s.db.Exec(`INSERT INTO sessions_fts(sessions_fts) VALUES('rebuild')`); err != nil {
		return fmt.Errorf("rebuild sessions_fts index: %w", err)
	}

	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS segments (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			speaker INTEGER NOT NULL,
			text TEXT NOT NULL,
			start_time REAL NOT NULL,
			end_time REAL NOT NULL,
			timestamp TEXT NOT NULL,
			FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE
		);
	`); err != nil {
		return fmt.Errorf("create segments table: %w", err)
	}

	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS deleted_segments (
			session_id TEXT NOT NULL,
			audio_path TEXT NOT NULL,
			start REAL NOT NULL,
			end REAL NOT NULL,
			sha256_before TEXT NOT NULL,
			sha256_after TEXT NOT NULL,
			splice_complete INTEGER NOT NULL DEFAULT 0,
			deleted_at TEXT NOT NULL,
			FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE
		);
	`); err != nil {
		return fmt.Errorf("create deleted_segments table: %w", err)
	}
	if _, err := s.db.Exec(`ALTER TABLE deleted_segments ADD COLUMN splice_complete INTEGER NOT NULL DEFAULT 0`); err != nil && !strings.Contains(err.Error(), "duplicate column") {
		return fmt.Errorf("migrate deleted_segments splice completion: %w", err)
	}

	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS summary_requests (
			session_id TEXT NOT NULL,
			prompt_hash TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(session_id, prompt_hash)
		);
	`); err != nil {
		return fmt.Errorf("create summary_requests table: %w", err)
	}

	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS summary_batches (
			batch_name TEXT NOT NULL,
			session_id TEXT NOT NULL,
			generation INTEGER NOT NULL,
			state TEXT NOT NULL DEFAULT 'pending',
			PRIMARY KEY (batch_name, session_id),
			FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE
		);
	`); err != nil {
		return fmt.Errorf("create summary_batches table: %w", err)
	}

	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS embeddings (
			session_id TEXT NOT NULL,
			chunk_index INTEGER NOT NULL,
			embedding BLOB NOT NULL,
			text_hash TEXT NOT NULL,
			model TEXT NOT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (session_id, chunk_index),
			FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE
		);
	`); err != nil {
		return fmt.Errorf("create embeddings table: %w", err)
	}

	if _, err := s.db.Exec("CREATE INDEX IF NOT EXISTS idx_sessions_started_at ON sessions(started_at)"); err != nil {
		return fmt.Errorf("create sessions index: %w", err)
	}
	if _, err := s.db.Exec("CREATE INDEX IF NOT EXISTS idx_segments_session_id ON segments(session_id, timestamp)"); err != nil {
		return fmt.Errorf("create segments index: %w", err)
	}
	if _, err := s.db.Exec("CREATE INDEX IF NOT EXISTS idx_deleted_segments_session_audio ON deleted_segments(session_id, audio_path)"); err != nil {
		return fmt.Errorf("create deleted segments index: %w", err)
	}

	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS mcp_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			event_type TEXT NOT NULL,
			payload TEXT NOT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);
	`); err != nil {
		return fmt.Errorf("create mcp_events table: %w", err)
	}

	if err := s.runOnce("discard-unattributable-live-events", `
		DELETE FROM mcp_events
		WHERE event_type = 'live_transcript'
		  AND CASE
			WHEN json_valid(payload) THEN COALESCE(json_extract(payload, '$.session_id'), '') = ''
			ELSE 1
		  END
	`); err != nil {
		return fmt.Errorf("discard unattributable live events: %w", err)
	}

	if _, err := s.db.Exec("CREATE INDEX IF NOT EXISTS idx_mcp_events_created_at ON mcp_events(created_at)"); err != nil {
		return fmt.Errorf("create mcp_events index: %w", err)
	}

	// A session that owed Drive a replacement before the folder ledger
	// existed owes it for its own folder and each merged source's that has
	// one, as DeleteSegment records it now.
	if err := s.runOnce("record-owed-drive-replacement-folders", `
		WITH RECURSIVE owner(session_id, generation, folder_session_id) AS (
			SELECT id, deletion_generation, id FROM sessions WHERE drive_replace_pending = 1
			UNION
			SELECT owner.session_id, owner.generation, sessions.id FROM sessions JOIN owner ON sessions.merged_into = owner.folder_session_id
		)
		INSERT OR IGNORE INTO drive_replacement_folders(session_id, generation, folder_session_id, state)
		SELECT owner.session_id, owner.generation, owner.folder_session_id, 'owed'
		FROM owner JOIN sessions ON sessions.id = owner.folder_session_id
		WHERE sessions.gdrive_folder_id != ''
	`); err != nil {
		return fmt.Errorf("record owed drive replacement folders: %w", err)
	}

	return nil
}

func (s *SQLiteStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *SQLiteStore) DB() *sql.DB {
	return s.db
}

// runOnce runs a data migration the first time a database is opened by a
// version that has it, and records it in the migrations table, in one
// transaction: a migration that must not repeat (one that sends sessions to
// sync again) runs once per database.
func (s *SQLiteStore) runOnce(name, stmt string) error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("create migrations table: %w", err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin migration %s: %w", name, err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`INSERT OR IGNORE INTO migrations(name, applied_at) VALUES(?, ?)`, name, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("record migration %s: %w", name, err)
	}
	if added, err := res.RowsAffected(); err != nil || added == 0 {
		return err
	}
	if _, err := tx.Exec(stmt); err != nil {
		return fmt.Errorf("migration %s: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %s: %w", name, err)
	}
	return nil
}

func (s *SQLiteStore) CreateSession(id string, startedAt time.Time) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("session id is required")
	}

	_, err := s.db.Exec(
		`INSERT INTO sessions(id, started_at, status, summary_status, envoy_publish_status) VALUES(?, ?, 'active', ?, ?)`,
		id,
		startedAt.UTC().Format(time.RFC3339Nano),
		SummaryPending,
		EnvoyPublishPending,
	)
	if err != nil {
		return fmt.Errorf("create session %s: %w", id, err)
	}
	return nil
}

// EndSession ends a session with its saved recording. A session that ends
// without one (it could not be saved, or none was made) can never be refined,
// so its refinement status becomes the terminal no_audio in the same write.
func (s *SQLiteStore) EndSession(id string, endedAt time.Time, audioPath string) error {
	res, err := s.db.Exec(
		`UPDATE sessions SET ended_at = ?, status = 'ended', audio_path = ?,
			refinement_status = CASE WHEN ? = '' THEN ? ELSE refinement_status END
		 WHERE id = ?`,
		endedAt.UTC().Format(time.RFC3339Nano),
		audioPath,
		audioPath,
		RefinementNoAudio,
		id,
	)
	if err != nil {
		return fmt.Errorf("end session %s: %w", id, err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("end session rows affected: %w", err)
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *SQLiteStore) DiscardSession(id string) error {
	res, err := s.db.Exec(`UPDATE sessions SET status = 'discarded' WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("discard session %s: %w", id, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("discard session rows affected: %w", err)
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *SQLiteStore) CountSegments(sessionID string) (int, error) {
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM segments WHERE session_id = ?`, sessionID).Scan(&count); err != nil {
		return 0, fmt.Errorf("count segments for session %s: %w", sessionID, err)
	}
	return count, nil
}

func (s *SQLiteStore) AppendSegment(sessionID string, seg transcribe.Segment) error {
	_, err := s.db.Exec(
		`INSERT INTO segments(session_id, speaker, text, start_time, end_time, timestamp) VALUES(?, ?, ?, ?, ?, ?)`,
		sessionID,
		seg.Speaker,
		strings.TrimSpace(seg.Text),
		seg.StartTime,
		seg.EndTime,
		seg.Timestamp.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("append segment for session %s: %w", sessionID, err)
	}
	return nil
}

// ErrMergeReplacementPending is the error MergeSessions returns, changing
// nothing, when a source session still owes Google Drive a replacement after
// a segment deletion: merged, it would leave the sync sweep before Drive lost
// the deleted words.
var ErrMergeReplacementPending = errors.New("a session to merge still has a deleted segment on Google Drive; merge it after its Drive files are replaced")

func (s *SQLiteStore) MergeSessions(newID string, sourceIDs []string, startedAt, endedAt time.Time) error {
	if strings.TrimSpace(newID) == "" {
		return errors.New("new session id is required")
	}
	if len(sourceIDs) == 0 {
		return errors.New("at least one source session id is required")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin merge tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	placeholders := make([]string, len(sourceIDs))
	args := make([]any, 0, len(sourceIDs)+1)
	args = append(args, newID)
	for i, id := range sourceIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}

	var owing string
	err = tx.QueryRow(fmt.Sprintf(`SELECT id FROM sessions WHERE id IN (%s) AND drive_replace_pending = 1 LIMIT 1`, strings.Join(placeholders, ",")), args[1:]...).Scan(&owing)
	switch {
	case err == nil:
		return fmt.Errorf("merge session %s: %w", owing, ErrMergeReplacementPending)
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("check sources for owed Drive replacements: %w", err)
	}

	_, err = tx.Exec(
		`INSERT INTO sessions(id, started_at, ended_at, status, summary_status) VALUES(?, ?, ?, 'ended', 'pending')`,
		newID,
		startedAt.UTC().Format(time.RFC3339Nano),
		endedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("create merged session: %w", err)
	}
	segQuery := fmt.Sprintf(
		`UPDATE segments SET session_id = ? WHERE session_id IN (%s)`,
		strings.Join(placeholders, ","),
	)
	if _, err := tx.Exec(segQuery, args...); err != nil {
		return fmt.Errorf("move segments: %w", err)
	}

	audioQuery := fmt.Sprintf(
		`SELECT audio_path FROM sessions WHERE id IN (%s) AND audio_path != '' ORDER BY started_at ASC`,
		strings.Join(placeholders, ","),
	)
	rows, err := tx.Query(audioQuery, args[1:]...)
	if err != nil {
		return fmt.Errorf("query audio paths: %w", err)
	}

	var audioPaths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan audio path: %w", err)
		}
		audioPaths = append(audioPaths, p)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate audio paths: %w", err)
	}
	_ = rows.Close()

	if len(audioPaths) > 0 {
		_, err = tx.Exec(`UPDATE sessions SET audio_path = ? WHERE id = ?`, strings.Join(audioPaths, ","), newID)
		if err != nil {
			return fmt.Errorf("set merged audio paths: %w", err)
		}
	}

	// The merged session now has the sources' recordings: one session names
	// each file, so removing the merged session's audio leaves no source
	// naming a file that is gone.
	markQuery := fmt.Sprintf(
		`UPDATE sessions SET status = 'merged', merged_into = ?, audio_path = '' WHERE id IN (%s)`,
		strings.Join(placeholders, ","),
	)
	markArgs := make([]any, 0, len(sourceIDs)+1)
	markArgs = append(markArgs, newID)
	markArgs = append(markArgs, args[1:]...)
	if _, err := tx.Exec(markQuery, markArgs...); err != nil {
		return fmt.Errorf("mark sources merged: %w", err)
	}

	return tx.Commit()
}

func (s *SQLiteStore) GetSessionsByDate(date string, includeDiscarded bool) ([]Session, error) {
	query := `SELECT id, title, started_at, ended_at, status, summary, summary_version, summary_status, summary_preset, speaker_names, refined_transcript, refinement_status, audio_path, sync_status, envoy_publish_status, sync_state, retry_count, last_sync_attempt, error_message, gdrive_folder_id, merged_into, canonical_transcript, transcript_source, audio_removed_at, summary_error, drive_replace_pending
		 FROM sessions
		 WHERE substr(started_at, 1, 10) = ?`
	if !includeDiscarded {
		query += ` AND status NOT IN ('discarded', 'merged')`
	}
	query += ` ORDER BY started_at DESC`

	rows, err := s.db.Query(query, date)
	if err != nil {
		return nil, fmt.Errorf("query sessions by date %s: %w", date, err)
	}
	defer func() { _ = rows.Close() }()

	return scanSessions(rows)
}

func (s *SQLiteStore) GetDates() ([]string, error) {
	rows, err := s.db.Query(
		`SELECT DISTINCT substr(started_at, 1, 10) AS date FROM sessions WHERE status != 'discarded' ORDER BY date DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("query dates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var dates []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, fmt.Errorf("scan date: %w", err)
		}
		dates = append(dates, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate dates rows: %w", err)
	}

	return dates, nil
}

// GetSessionsWithEmptyTitles returns sessions where the title is empty or NULL.
func (s *SQLiteStore) GetSessionsWithEmptyTitles() ([]Session, error) {
	rows, err := s.db.Query(
		`SELECT id, title, started_at, ended_at, status, summary, summary_version, summary_status, summary_preset, speaker_names, refined_transcript, refinement_status, audio_path, sync_status, envoy_publish_status, sync_state, retry_count, last_sync_attempt, error_message, gdrive_folder_id, merged_into, canonical_transcript, transcript_source, audio_removed_at, summary_error, drive_replace_pending
		 FROM sessions WHERE (title = '' OR title IS NULL) AND status NOT IN ('discarded', 'merged') ORDER BY started_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("query sessions with empty titles: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return scanSessions(rows)
}

// querier is what reading a session needs: the database, or a transaction.
type querier interface {
	QueryRow(query string, args ...any) *sql.Row
	Query(query string, args ...any) (*sql.Rows, error)
}

func (s *SQLiteStore) GetSession(id string) (Session, error) {
	return getSession(s.db, id)
}

func getSession(q querier, id string) (Session, error) {
	row := q.QueryRow(
		`SELECT id, title, started_at, ended_at, status, summary, summary_version, summary_status, summary_preset, speaker_names, refined_transcript, refinement_status, audio_path, sync_status, envoy_publish_status, sync_state, retry_count, last_sync_attempt, error_message, gdrive_folder_id, merged_into, canonical_transcript, transcript_source, audio_removed_at, summary_error, drive_replace_pending FROM sessions WHERE id = ?`,
		id,
	)

	var sess Session
	var startedAt string
	var endedAt sql.NullString
	var lastSyncAttempt sql.NullString
	if err := row.Scan(&sess.ID, &sess.Title, &startedAt, &endedAt, &sess.Status, &sess.Summary, &sess.SummaryVersion, &sess.SummaryStatus, &sess.SummaryPreset, &sess.SpeakerNames, &sess.RefinedTranscript, &sess.RefinementStatus, &sess.AudioPath, &sess.SyncStatus, &sess.EnvoyPublishStatus, &sess.SyncState, &sess.RetryCount, &lastSyncAttempt, &sess.ErrorMessage, &sess.GDriveFolderID, &sess.MergedInto, &sess.CanonicalTranscript, &sess.TranscriptSource, &sess.AudioRemovedAt, &sess.SummaryError, &sess.DriveReplacePending); err != nil {
		return Session{}, fmt.Errorf("query session %s: %w", id, err)
	}

	parsedStart, err := time.Parse(time.RFC3339Nano, startedAt)
	if err != nil {
		return Session{}, fmt.Errorf("parse session %s started_at: %w", id, err)
	}
	sess.StartedAt = parsedStart

	if endedAt.Valid {
		parsedEnd, err := time.Parse(time.RFC3339Nano, endedAt.String)
		if err != nil {
			return Session{}, fmt.Errorf("parse session %s ended_at: %w", id, err)
		}
		sess.EndedAt = &parsedEnd
	}

	if lastSyncAttempt.Valid {
		parsedAttempt, err := time.Parse(time.RFC3339Nano, lastSyncAttempt.String)
		if err != nil {
			return Session{}, fmt.Errorf("parse session %s last_sync_attempt: %w", id, err)
		}
		sess.LastSyncAttempt = &parsedAttempt
	}

	return sess, nil
}

func (s *SQLiteStore) GetSegments(sessionID string) ([]transcribe.Segment, error) {
	return getSegments(s.db, sessionID)
}

func getSegments(q querier, sessionID string) ([]transcribe.Segment, error) {
	rows, err := q.Query(
		`SELECT id, speaker, text, start_time, end_time, timestamp
		 FROM segments
		 WHERE session_id = ?
		 ORDER BY id ASC`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("query segments for session %s: %w", sessionID, err)
	}
	defer func() { _ = rows.Close() }()

	segments := make([]transcribe.Segment, 0, 32)
	for rows.Next() {
		var seg transcribe.Segment
		var ts string
		if err := rows.Scan(&seg.ID, &seg.Speaker, &seg.Text, &seg.StartTime, &seg.EndTime, &ts); err != nil {
			return nil, fmt.Errorf("scan segment for session %s: %w", sessionID, err)
		}

		parsedTS, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			return nil, fmt.Errorf("parse segment timestamp for session %s: %w", sessionID, err)
		}
		seg.Timestamp = parsedTS

		segments = append(segments, seg)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate segment rows for session %s: %w", sessionID, err)
	}

	return segments, nil
}

func (s *SQLiteStore) GetSegmentsInTimeRange(sessionID string, startTime, endTime float64) ([]transcribe.Segment, error) {
	rows, err := s.db.Query(
		`SELECT id, speaker, text, start_time, end_time, timestamp
		 FROM segments
		 WHERE session_id = ? AND start_time >= ? AND end_time <= ?
		 ORDER BY start_time`,
		sessionID, startTime, endTime,
	)
	if err != nil {
		return nil, fmt.Errorf("query segments in time range for session %s: %w", sessionID, err)
	}
	defer func() { _ = rows.Close() }()

	segments := make([]transcribe.Segment, 0, 32)
	for rows.Next() {
		var seg transcribe.Segment
		var ts string
		if err := rows.Scan(&seg.ID, &seg.Speaker, &seg.Text, &seg.StartTime, &seg.EndTime, &ts); err != nil {
			return nil, fmt.Errorf("scan segment in time range for session %s: %w", sessionID, err)
		}

		parsedTS, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			return nil, fmt.Errorf("parse segment timestamp in time range for session %s: %w", sessionID, err)
		}
		seg.Timestamp = parsedTS

		segments = append(segments, seg)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate segment rows in time range for session %s: %w", sessionID, err)
	}

	return segments, nil
}

// UpdateSummary stores session id's summary, its status, preset and speaker
// names, and its title unless title is "". An empty summary never replaces
// the one the session has: its summary, preset and speaker names stay (a
// session with none keeps none). A completed summary clears the error of the
// attempt before. Starting a summary and recording one that failed are
// StartSummary and FailSummary, which keep the last good summary.
//
// A changed completed summary not started by StartSummary (a batch result)
// gets a new summary version when the session's current version may already
// have been sent to Envoy, so Envoy does not drop it as a duplicate delivery.
func (s *SQLiteStore) UpdateSummary(sessionID, title, summary, status, preset, speakerNames string) error {
	return s.updateSummary(sessionID, title, summary, status, preset, speakerNames, -1)
}

// UpdateSummaryAtGeneration stores a summary only if the transcript generation
// the writer read remains current.
func (s *SQLiteStore) UpdateSummaryAtGeneration(sessionID, title, summary, status, preset, speakerNames string, generation int) error {
	return s.updateSummary(sessionID, title, summary, status, preset, speakerNames, generation)
}

func (s *SQLiteStore) updateSummary(sessionID, title, summary, status, preset, speakerNames string, generation int) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin summary update for session %s: %w", sessionID, err)
	}
	defer func() { _ = tx.Rollback() }()

	current, err := getSession(tx, sessionID)
	if err != nil {
		return err
	}
	if generation < 0 {
		if err := tx.QueryRow(`SELECT deletion_generation FROM sessions WHERE id = ?`, sessionID).Scan(&generation); err != nil {
			return fmt.Errorf("read summary generation for session %s: %w", sessionID, err)
		}
	}
	bumpVersion := summaryVersionChanges(&current, title, summary, status, preset, speakerNames)
	res, err := tx.Exec(
		`UPDATE sessions
		 SET title = CASE WHEN ? != '' THEN ? ELSE title END,
		     summary_preset = CASE WHEN ? != '' THEN ? ELSE summary_preset END,
		     speaker_names = CASE WHEN ? != '' THEN ? ELSE speaker_names END,
		     summary = CASE WHEN ? != '' THEN ? ELSE summary END,
		     summary_version = CASE WHEN ? THEN summary_version + 1 ELSE summary_version END,
		     envoy_publish_status = CASE WHEN ? THEN ? ELSE envoy_publish_status END,
		     summary_status = ?,
		     summary_error = CASE WHEN ? = ? THEN '' ELSE summary_error END
		 WHERE id = ? AND summary_generation = deletion_generation AND deletion_generation = ?`,
		title, title,
		summary, preset,
		summary, speakerNames,
		summary, summary,
		bumpVersion,
		bumpVersion, EnvoyPublishPending,
		status,
		status, SummaryCompleted,
		sessionID,
		generation,
	)
	if err != nil {
		return fmt.Errorf("update summary for session %s: %w", sessionID, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update summary rows affected: %w", err)
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit summary update for session %s: %w", sessionID, err)
	}
	return nil
}

// DeletionGeneration returns the current transcript generation for sessionID.
func (s *SQLiteStore) DeletionGeneration(sessionID string) (int, error) {
	var generation int
	if err := s.db.QueryRow(`SELECT deletion_generation FROM sessions WHERE id = ?`, sessionID).Scan(&generation); err != nil {
		return 0, fmt.Errorf("read deletion generation for session %s: %w", sessionID, err)
	}
	return generation, nil
}

func summaryVersionChanges(current *Session, title, summary, status, preset, speakerNames string) bool {
	if status != SummaryCompleted || current.SummaryStatus == SummaryRunning {
		return false
	}
	if current.SummaryStatus != SummaryCompleted && current.Summary == "" && current.EnvoyPublishStatus == EnvoyPublishPending {
		return false
	}
	if title != "" && title != current.Title {
		return true
	}
	return summary != "" && (summary != current.Summary || preset != current.SummaryPreset || speakerNames != current.SpeakerNames)
}

// StartSummary marks session id's summary running, and not yet published to
// Envoy. Its title and summary stay as they are until a new summary is
// stored (UpdateSummary): a summary that fails, or never ends, takes nothing
// away. The session gets a new summary version when its current one may
// already have been sent to Envoy: it is or was completed, or Envoy publishing
// of it was attempted.
func (s *SQLiteStore) StartSummary(id string) error {
	res, err := s.db.Exec(`UPDATE sessions SET summary_status = ?, envoy_publish_status = ?, summary_generation = deletion_generation, summary_version = CASE WHEN summary_status = ? OR summary != '' OR envoy_publish_status != ? THEN summary_version + 1 ELSE summary_version END WHERE id = ?`,
		SummaryRunning, EnvoyPublishPending, SummaryCompleted, EnvoyPublishPending, id)
	if err != nil {
		return fmt.Errorf("start summary of session %s: %w", id, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("start summary rows affected: %w", err)
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// StartEmptySummary marks session id's summary running for the current
// transcript generation, as StartSummary does, for an app with no summarizer:
// the summary it then completes is empty, which keeps the session's summary
// as it is. So it neither moves the summary version nor queues the session
// for Envoy again, and a summary Envoy already has is not sent at a new
// version.
func (s *SQLiteStore) StartEmptySummary(id string) error {
	res, err := s.db.Exec(`UPDATE sessions SET summary_status = ?, summary_generation = deletion_generation WHERE id = ?`, SummaryRunning, id)
	if err != nil {
		return fmt.Errorf("start empty summary of session %s: %w", id, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("start empty summary rows affected: %w", err)
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// FailSummary records that making session id's summary failed, and why
// (errMsg, in summary_error: error_message keeps the last sync's error), and
// returns the session as it then is. The session keeps its title and its last
// good summary (with its preset and speaker names), the ones Drive holds: a
// failed summary takes nothing away.
func (s *SQLiteStore) FailSummary(id, errMsg string) (Session, error) {
	return s.failSummary(id, errMsg, -1)
}

// FailSummaryAtGeneration records a failed summary only while generation still
// identifies the transcript the batch request used.
func (s *SQLiteStore) FailSummaryAtGeneration(id, errMsg string, generation int) (Session, error) {
	return s.failSummary(id, errMsg, generation)
}

func (s *SQLiteStore) failSummary(id, errMsg string, generation int) (Session, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Session{}, fmt.Errorf("fail summary of session %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	if generation < 0 {
		if err := tx.QueryRow(`SELECT deletion_generation FROM sessions WHERE id = ?`, id).Scan(&generation); err != nil {
			return Session{}, fmt.Errorf("read summary generation for session %s: %w", id, err)
		}
	}
	res, err := tx.Exec(`UPDATE sessions SET summary_status = ?, summary_error = ? WHERE id = ? AND summary_generation = deletion_generation AND deletion_generation = ?`, SummaryFailed, errMsg, id, generation)
	if err != nil {
		return Session{}, fmt.Errorf("fail summary of session %s: %w", id, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return Session{}, fmt.Errorf("fail summary rows affected: %w", err)
	}
	if rows == 0 {
		return Session{}, sql.ErrNoRows
	}
	sess, err := getSession(tx, id)
	if err != nil {
		return Session{}, err
	}
	if err := tx.Commit(); err != nil {
		return Session{}, fmt.Errorf("fail summary of session %s: %w", id, err)
	}
	return sess, nil
}

func (s *SQLiteStore) UpdateRefinement(sessionID, transcript, status string) error {
	res, err := s.db.Exec(
		`UPDATE sessions SET refined_transcript = ?, refinement_status = ? WHERE id = ? AND refinement_generation = deletion_generation`,
		transcript,
		status,
		sessionID,
	)
	if err != nil {
		return fmt.Errorf("update refinement for session %s: %w", sessionID, err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update refinement rows affected: %w", err)
	}
	if rows == 0 {
		return sql.ErrNoRows
	}

	return nil
}

// ClaimRefinement marks session id's refinement running, dropping any earlier
// refined transcript, only while the session still names audioPath, the
// recording the refinement will read. Disk pressure changes the audio path
// before it removes a file (MarkAudioRemoved), so once GC has taken the
// recording this reports false and the refinement must not start; and GC
// takes nothing from a session whose refinement is running.
func (s *SQLiteStore) ClaimRefinement(id, audioPath string) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE sessions SET refined_transcript = '', refinement_status = ?, refinement_generation = deletion_generation WHERE id = ? AND audio_path = ? AND audio_path != ''`,
		RefinementRunning, id, audioPath,
	)
	if err != nil {
		return false, fmt.Errorf("claim refinement of session %s: %w", id, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim refinement rows affected: %w", err)
	}
	return rows == 1, nil
}

func (s *SQLiteStore) GetRefinement(sessionID string) (string, string, error) {
	var transcript, status string
	if err := s.db.QueryRow(`SELECT refined_transcript, refinement_status FROM sessions WHERE id = ?`, sessionID).Scan(&transcript, &status); err != nil {
		return "", "", fmt.Errorf("query refinement for session %s: %w", sessionID, err)
	}

	return transcript, status, nil
}

// Canonicalize sets the canonical transcript and source for sessionID. It
// returns the transcript and deletion generation it read in the transaction
// that writes it. With the store's single connection, DeleteSegment cannot
// interleave, so this does not retry a deletion.
func (s *SQLiteStore) Canonicalize(sessionID string) (string, int, error) {
	return s.canonicalize(sessionID, nil)
}

// canonicalize accepts beforeWrite for deterministic storage tests. It runs
// with the sole database connection held, so the hook must not use the store.
func (s *SQLiteStore) canonicalize(sessionID string, beforeWrite func()) (string, int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", 0, fmt.Errorf("begin canonicalize %s: %w", sessionID, err)
	}

	var refinedTranscript, refinementStatus string
	var generation int
	if err := tx.QueryRow(`SELECT refined_transcript, refinement_status, deletion_generation FROM sessions WHERE id = ?`, sessionID).Scan(&refinedTranscript, &refinementStatus, &generation); err != nil {
		_ = tx.Rollback()
		return "", 0, fmt.Errorf("query refinement for canonicalize %s: %w", sessionID, err)
	}

	var canonical, source string
	if refinementStatus == RefinementCompleted && strings.TrimSpace(refinedTranscript) != "" {
		canonical = strings.TrimSpace(refinedTranscript)
		source = TranscriptSourceRefined
	} else {
		segs, err := getSegments(tx, sessionID)
		if err != nil {
			_ = tx.Rollback()
			return "", 0, fmt.Errorf("get segments for canonicalize %s: %w", sessionID, err)
		}
		canonical = canonicalTranscript(segs)
		source = TranscriptSourceStreaming
	}
	if beforeWrite != nil {
		beforeWrite()
	}

	if _, err := tx.Exec(`UPDATE sessions SET canonical_transcript = ?, transcript_source = ? WHERE id = ?`, canonical, source, sessionID); err != nil {
		_ = tx.Rollback()
		return "", 0, fmt.Errorf("update canonical transcript for session %s: %w", sessionID, err)
	}
	if err := tx.Commit(); err != nil {
		return "", 0, fmt.Errorf("commit canonical transcript for session %s: %w", sessionID, err)
	}
	return canonical, generation, nil
}

// GetCanonicalTranscriptWithGeneration reads the canonical transcript and the
// deletion generation atomically, so asynchronous writers can fence updates
// against the exact text they consumed.
func (s *SQLiteStore) GetCanonicalTranscriptWithGeneration(sessionID string) (string, string, int, error) {
	var transcript, source string
	var generation int
	if err := s.db.QueryRow(`SELECT canonical_transcript, transcript_source, deletion_generation FROM sessions WHERE id = ?`, sessionID).Scan(&transcript, &source, &generation); err != nil {
		return "", "", 0, fmt.Errorf("query canonical transcript generation for session %s: %w", sessionID, err)
	}
	return transcript, source, generation, nil
}

func (s *SQLiteStore) UpdateTitle(sessionID, title string) error {
	res, err := s.db.Exec(`UPDATE sessions SET title = ? WHERE id = ?`, title, sessionID)
	if err != nil {
		return fmt.Errorf("update title for session %s: %w", sessionID, err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update title rows affected: %w", err)
	}
	if rows == 0 {
		return sql.ErrNoRows
	}

	return nil
}

// ClaimSync claims session id for one sync, which claim names: it moves the
// session to SYNCING only while its sync state is still from (the state the
// sync read) and no sync holds it (from is not SYNCING), and returns the
// session as claimed. So two syncs of one session (retry-sync, the sync after
// a summary, the sweep) never run at once: the second finds it claimed and
// starts nothing. The session stays the sync's until MarkSynced or EndSync,
// under the same claim, ends it, or the app restarts
// (RecoverInterruptedSyncs), which reads from (sync_claimed_from).
func (s *SQLiteStore) ClaimSync(id, from, claim string, at time.Time) (Session, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Session{}, false, fmt.Errorf("begin claiming the sync of session %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(
		`UPDATE sessions SET sync_state = ?, sync_status = ?, sync_claim = ?, sync_claimed_from = ?, last_sync_attempt = ?, error_message = ''
		 WHERE id = ? AND sync_state = ? AND sync_state != ?`,
		SyncStateSyncing, syncStatusFromState(SyncStateSyncing), claim, from, at.UTC().Format(time.RFC3339Nano),
		id, from, SyncStateSyncing,
	)
	if err != nil {
		return Session{}, false, fmt.Errorf("claim the sync of session %s: %w", id, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return Session{}, false, fmt.Errorf("claim sync rows affected: %w", err)
	}
	if rows == 0 {
		return Session{}, false, nil
	}
	sess, err := getSession(tx, id)
	if err != nil {
		return Session{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Session{}, false, fmt.Errorf("commit claiming the sync of session %s: %w", id, err)
	}
	return sess, true, nil
}

// MarkSynced marks session id synced to Drive folder driveFolderID, only while
// the sync claim names still holds it (ClaimSync) and unchanged reports true
// for the session and its segments as they are now, read in the same
// transaction as the update, and reports whether it did. A sync passes a
// check that the session is still what it uploaded: its audio path (GC may
// have put back a recording it could not remove) and its documents (a
// summary made again, a refined transcript). A session that changed during
// the upload is not synced, and the next sync uploads what it is now; one
// the sync no longer holds is left as it is. Marked synced, Drive has the
// session's documents as they are, so they are verified (documents_unverified
// is cleared). unchanged runs inside the transaction: it must not use the
// store.
func (s *SQLiteStore) MarkSynced(id, claim, driveFolderID string, unchanged func(Session, []transcribe.Segment) bool) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("begin marking session %s synced: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()

	sess, err := getSession(tx, id)
	if err != nil {
		return false, err
	}
	segments, err := getSegments(tx, id)
	if err != nil {
		return false, err
	}
	if !unchanged(sess, segments) {
		return false, nil
	}
	res, err := tx.Exec(
		`UPDATE sessions
		 SET sync_state = ?, sync_status = ?, gdrive_folder_id = CASE WHEN ? != '' THEN ? ELSE gdrive_folder_id END,
		     retry_count = 0, last_sync_attempt = NULL, error_message = '', sync_claim = '', sync_claimed_from = '', documents_unverified = 0
		 WHERE id = ? AND sync_state = ? AND sync_claim = ?`,
		SyncStateSynced, SyncSynced, driveFolderID, driveFolderID,
		id, SyncStateSyncing, claim,
	)
	if err != nil {
		return false, fmt.Errorf("mark session %s synced: %w", id, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("mark synced rows affected: %w", err)
	}
	if rows == 0 {
		return false, nil
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit marking session %s synced: %w", id, err)
	}
	return true, nil
}

// EndSync records the end of the sync claim names, which did not mark
// session id synced: its sync state becomes state (back to pending when the
// session changed during the upload, a retry, a failure, or its Drive folder
// gone), with its retry count, the time it was tried and why it did not
// finish, and the Drive folder the sync made, if it made one (one the session
// had stays). It does this only while that sync still holds the session, so
// a sync that no longer does never replaces what the session is now, and
// reports whether it did. Any other state is an error and changes nothing:
// SYNCED would skip MarkSynced's check that Drive has the session as it is
// now, and SYNCING would leave the session held by no sync. A sync that held
// the session while one of its segments was deleted (DeleteSegment leaves the
// claim to it) uploaded the words before the deletion; the folder it made is
// recorded as owed to that deletion's replacement in the same transaction.
func (s *SQLiteStore) EndSync(id, claim, state, driveFolderID string, retryCount int, at time.Time, errorMessage string) (bool, error) {
	switch state {
	case SyncStatePending, SyncStateRetryScheduled, SyncStateFailed, SyncStateRemoteDeleted:
	default:
		return false, fmt.Errorf("end the sync of session %s: %q is not a state a sync ends in unsynced", id, state)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("begin ending the sync of session %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(
		`UPDATE sessions
		 SET sync_state = ?, sync_status = ?, gdrive_folder_id = CASE WHEN ? != '' THEN ? ELSE gdrive_folder_id END,
		     retry_count = ?, last_sync_attempt = ?, error_message = ?, sync_claim = '', sync_claimed_from = ''
		 WHERE id = ? AND sync_state = ? AND sync_claim = ?`,
		state, syncStatusFromState(state), driveFolderID, driveFolderID,
		retryCount, at.UTC().Format(time.RFC3339Nano), errorMessage,
		id, SyncStateSyncing, claim,
	)
	if err != nil {
		return false, fmt.Errorf("end the sync of session %s: %w", id, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("end sync rows affected: %w", err)
	}
	if rows != 1 {
		return false, nil
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO drive_replacement_folders(session_id, generation, folder_session_id, state)
		SELECT id, deletion_generation, id, ? FROM sessions WHERE id = ? AND drive_replace_pending = 1 AND gdrive_folder_id != ''`,
		DriveReplacementFolderOwed, id); err != nil {
		return false, fmt.Errorf("owe the Drive folder of session %s its replacement: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit the end of the sync of session %s: %w", id, err)
	}
	return true, nil
}

// RecoverInterruptedSyncs moves every session an earlier run of the app left
// SYNCING (it stopped during the sync, so no sync holds the session now) back
// to a retry, keeping its Drive folder, and returns their ids. The app calls
// it as it starts, before any sync. The cut-off sync counts as a failed
// attempt, as in gdrive.BuildRetryPlan: the session is RETRY_SCHEDULED with
// one retry more, or FAILED once it has had maxRetries, so a sync that stops
// the app every time it runs is not retried for ever. A sync claimed from
// REMOTE_DELETED (a retry that found the session's Drive folder back) goes
// back to REMOTE_DELETED, its retry count as it was: Drive may have lost the
// folder again since, and only the next retry, asking Drive again, takes the
// session out of it. A session that still owes Drive a replacement
// (drive_replace_pending) goes back to the state its claim came from, its
// retry count as it was: a deletion is never left FAILED, where only a manual
// retry would remove the deleted words from Drive.
func (s *SQLiteStore) RecoverInterruptedSyncs(maxRetries int, at time.Time) ([]string, error) {
	// The state a cut-off replacement goes back to: the one its claim came
	// from, PENDING when that was SYNCED or is not recorded.
	const replacementBack = `CASE WHEN sync_claimed_from IN ('', ?) THEN ? ELSE sync_claimed_from END`
	rows, err := s.db.Query(
		`UPDATE sessions
		 SET sync_state = CASE WHEN drive_replace_pending = 1 THEN `+replacementBack+`
		                       WHEN sync_claimed_from = ? THEN ? WHEN retry_count >= ? THEN ? ELSE ? END,
		     sync_status = CASE WHEN drive_replace_pending = 1 AND `+replacementBack+` = ? THEN ? ELSE ? END,
		     retry_count = CASE WHEN drive_replace_pending = 1 OR sync_claimed_from = ? OR retry_count >= ? THEN retry_count ELSE retry_count + 1 END,
		     last_sync_attempt = ?,
		     error_message = CASE WHEN drive_replace_pending = 1 THEN ? WHEN sync_claimed_from = ? THEN ? ELSE ? END,
		     sync_claim = '', sync_claimed_from = ''
		 WHERE sync_state = ?
		 RETURNING id`,
		SyncStateSynced, SyncStatePending,
		SyncStateRemoteDeleted, SyncStateRemoteDeleted, maxRetries, SyncStateFailed, SyncStateRetryScheduled,
		SyncStateSynced, SyncStatePending, SyncStatePending, SyncPending, SyncFailed,
		SyncStateRemoteDeleted, maxRetries,
		at.UTC().Format(time.RFC3339Nano),
		"a Drive replacement after a segment deletion was cut off: the app stopped during it",
		SyncStateRemoteDeleted, "a retry after the Drive folder was deleted was cut off: the app stopped during it", "the sync was cut off: the app stopped during it",
		SyncStateSyncing,
	)
	if err != nil {
		return nil, fmt.Errorf("recover interrupted syncs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan recovered sync: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("recover interrupted syncs: %w", err)
	}
	return ids, nil
}

// UpdateEnvoyPublishStatusAtVersion changes delivery state only while the
// session still has summaryVersion. A late completion of an earlier summary
// must not mark a newer summary as delivered.
func (s *SQLiteStore) UpdateEnvoyPublishStatusAtVersion(sessionID string, summaryVersion int, status string) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE sessions SET envoy_publish_status = ? WHERE id = ? AND summary_version = ?`,
		status,
		sessionID,
		summaryVersion,
	)
	if err != nil {
		return false, fmt.Errorf("update envoy publish status for session %s: %w", sessionID, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("update envoy publish status rows affected: %w", err)
	}
	return rows > 0, nil
}

// EnvoyLifecycleEvent is one session event awaiting a successful webhook
// response. Its delivery ID is reconstructed from the immutable type and ID.
type EnvoyLifecycleEvent struct {
	Type      string
	SessionID string
}

func (s *SQLiteStore) EnqueueEnvoyLifecycleEvent(eventType, sessionID string) error {
	if _, err := s.db.Exec(
		`INSERT OR IGNORE INTO envoy_lifecycle_events(event_type, session_id) VALUES(?, ?)`,
		eventType,
		sessionID,
	); err != nil {
		return fmt.Errorf("enqueue envoy lifecycle event %s for session %s: %w", eventType, sessionID, err)
	}
	return nil
}

func (s *SQLiteStore) PendingEnvoyLifecycleEvents() ([]EnvoyLifecycleEvent, error) {
	rows, err := s.db.Query(`SELECT event_type, session_id FROM envoy_lifecycle_events ORDER BY rowid ASC`)
	if err != nil {
		return nil, fmt.Errorf("query pending envoy lifecycle events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var events []EnvoyLifecycleEvent
	for rows.Next() {
		var event EnvoyLifecycleEvent
		if err := rows.Scan(&event.Type, &event.SessionID); err != nil {
			return nil, fmt.Errorf("scan pending envoy lifecycle event: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending envoy lifecycle events: %w", err)
	}
	return events, nil
}

func (s *SQLiteStore) MarkEnvoyLifecycleEventDelivered(eventType, sessionID string) error {
	if _, err := s.db.Exec(
		`DELETE FROM envoy_lifecycle_events WHERE event_type = ? AND session_id = ?`,
		eventType,
		sessionID,
	); err != nil {
		return fmt.Errorf("mark envoy lifecycle event %s for session %s delivered: %w", eventType, sessionID, err)
	}
	return nil
}

func (s *SQLiteStore) ClaimSummaryRequest(sessionID, promptHash string) (bool, error) {
	res, err := s.db.Exec(
		`INSERT OR IGNORE INTO summary_requests(session_id, prompt_hash) VALUES(?, ?)`,
		sessionID,
		promptHash,
	)
	if err != nil {
		return false, fmt.Errorf("claim summary request for session %s: %w", sessionID, err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim summary rows affected: %w", err)
	}

	return rows > 0, nil
}

func scanSessions(rows *sql.Rows) ([]Session, error) {
	sessions := make([]Session, 0, 16)
	for rows.Next() {
		var sess Session
		var startedAt string
		var endedAt sql.NullString
		var lastSyncAttempt sql.NullString
		if err := rows.Scan(&sess.ID, &sess.Title, &startedAt, &endedAt, &sess.Status, &sess.Summary, &sess.SummaryVersion, &sess.SummaryStatus, &sess.SummaryPreset, &sess.SpeakerNames, &sess.RefinedTranscript, &sess.RefinementStatus, &sess.AudioPath, &sess.SyncStatus, &sess.EnvoyPublishStatus, &sess.SyncState, &sess.RetryCount, &lastSyncAttempt, &sess.ErrorMessage, &sess.GDriveFolderID, &sess.MergedInto, &sess.CanonicalTranscript, &sess.TranscriptSource, &sess.AudioRemovedAt, &sess.SummaryError, &sess.DriveReplacePending); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}

		parsedStart, err := time.Parse(time.RFC3339Nano, startedAt)
		if err != nil {
			return nil, fmt.Errorf("parse started_at: %w", err)
		}
		sess.StartedAt = parsedStart

		if endedAt.Valid {
			parsedEnd, err := time.Parse(time.RFC3339Nano, endedAt.String)
			if err != nil {
				return nil, fmt.Errorf("parse ended_at: %w", err)
			}
			sess.EndedAt = &parsedEnd
		}

		if lastSyncAttempt.Valid {
			parsedAttempt, err := time.Parse(time.RFC3339Nano, lastSyncAttempt.String)
			if err != nil {
				return nil, fmt.Errorf("parse last_sync_attempt: %w", err)
			}
			sess.LastSyncAttempt = &parsedAttempt
		}

		sessions = append(sessions, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sessions rows: %w", err)
	}

	return sessions, nil
}

// RecoverStaleRefinements marks failed every refinement left running by an
// earlier run of the app, which stopped inside it: nothing is running when the
// app starts. Failed offers a retry, and GC, which leaves a running
// refinement's session and audio alone, is not held off for good.
func (s *SQLiteStore) RecoverStaleRefinements() error {
	if _, err := s.db.Exec(`UPDATE sessions SET refinement_status = ? WHERE refinement_status = ?`, RefinementFailed, RefinementRunning); err != nil {
		return fmt.Errorf("recover stale refinements: %w", err)
	}
	return nil
}

// RecoverStaleSessions ends any sessions still marked as 'active' and returns their IDs.
// This handles the case where the app was restarted while a session was in progress.
func (s *SQLiteStore) RecoverStaleSessions() ([]string, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)

	rows, err := s.db.Query(`SELECT id FROM sessions WHERE status = 'active'`)
	if err != nil {
		return nil, fmt.Errorf("query active sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan active session: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate active sessions: %w", err)
	}

	for _, id := range ids {
		_, err := s.db.Exec(
			// Ended with no saved recording, so it can never be refined.
			`UPDATE sessions SET ended_at = ?, status = 'ended', refinement_status = ? WHERE id = ? AND status = 'active'`,
			now, RefinementNoAudio, id,
		)
		if err != nil {
			return nil, fmt.Errorf("end stale session %s: %w", id, err)
		}
	}

	return ids, nil
}

// GetSessionsNeedingSummary returns IDs of ended sessions with pending or empty summaries.
func (s *SQLiteStore) GetSessionsNeedingSummary() ([]string, error) {
	rows, err := s.db.Query(
		`SELECT id FROM sessions WHERE status = 'ended' AND (summary_status IN ('pending', 'running') OR (summary_status = 'completed' AND (summary IS NULL OR summary = '')))`,
	)
	if err != nil {
		return nil, fmt.Errorf("query sessions needing summary: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sessions: %w", err)
	}
	return ids, nil
}

func (s *SQLiteStore) GetSessionsForBackfill() ([]string, error) {
	rows, err := s.db.Query(`
		SELECT id FROM sessions
		WHERE status = 'ended'
		AND (summary_status IN ('failed', 'pending', 'running')
		     OR (summary_status = 'completed' AND (summary IS NULL OR summary = '')))
		ORDER BY started_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query sessions for backfill: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan session id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sessions: %w", err)
	}
	return ids, nil
}

// GetSessionsNeedingSync returns the sessions the sync sweep syncs now: the
// ended sessions waiting for a sync, and every session that still owes Drive
// a replacement after a segment deletion and that no sync holds, whatever its
// status (ended, merged, or discarded: the consent to delete covers them all)
// once it is no longer recording. An ordinary sync leaves a FAILED or
// REMOTE_DELETED session to a manual retry, and syncs no merged or discarded
// one; a replacement asks Drive whether a deleted folder is back each time,
// so a deletion never waits on a click. (A scheduled retry is read
// separately, by GetSessionsBySyncState, when due.)
func (s *SQLiteStore) GetSessionsNeedingSync() ([]string, error) {
	rows, err := s.db.Query(
		`SELECT id FROM sessions WHERE (status = 'ended' AND sync_state = ?)
		 OR (drive_replace_pending = 1 AND status != ? AND sync_state != ?)`,
		SyncStatePending, SessionActive, SyncStateSyncing,
	)
	if err != nil {
		return nil, fmt.Errorf("query sessions needing sync: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sessions: %w", err)
	}
	return ids, nil
}

// GetSessionsNeedingEnvoyPublish returns the ended sessions with a completed
// summary that Envoy has not had: neither published nor skipped (a summary
// with no text, which is not announced).
func (s *SQLiteStore) GetSessionsNeedingEnvoyPublish() ([]string, error) {
	rows, err := s.db.Query(
		`SELECT id FROM sessions WHERE status = 'ended' AND summary_status = ? AND envoy_publish_status NOT IN (?, ?) ORDER BY started_at ASC`,
		SummaryCompleted,
		EnvoyPublishPublished,
		EnvoyPublishSkipped,
	)
	if err != nil {
		return nil, fmt.Errorf("query sessions needing envoy publish: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sessions: %w", err)
	}
	return ids, nil
}

func (s *SQLiteStore) GetSessionsBySyncState(syncState string) ([]Session, error) {
	rows, err := s.db.Query(
		`SELECT id, title, started_at, ended_at, status, summary, summary_version, summary_status, summary_preset, speaker_names, refined_transcript, refinement_status, audio_path, sync_status, envoy_publish_status, sync_state, retry_count, last_sync_attempt, error_message, gdrive_folder_id, merged_into, canonical_transcript, transcript_source, audio_removed_at, summary_error, drive_replace_pending
		 FROM sessions
		 WHERE status = 'ended' AND sync_state = ?
		 ORDER BY started_at ASC`,
		syncState,
	)
	if err != nil {
		return nil, fmt.Errorf("query sessions by sync state %s: %w", syncState, err)
	}
	defer func() { _ = rows.Close() }()

	return scanSessions(rows)
}

// ageCutoff returns the time before which a session started (or was restored)
// is older than maxAgeDays days at now, and false when no session can be:
// with no limit (maxAgeDays <= 0), or a limit reaching back past year 1.
// Counting in days, not in a time.Duration (which overflows past 106,751
// days), keeps any limit from wrapping round into the future.
func ageCutoff(now time.Time, maxAgeDays int) (time.Time, bool) {
	daysSinceYear1 := (now.Unix() - time.Time{}.Unix()) / (24 * 60 * 60)
	if maxAgeDays <= 0 || int64(maxAgeDays) >= daysSinceYear1 {
		return time.Time{}, false
	}
	return now.AddDate(0, 0, -maxAgeDays), true
}

// GetGCEligibleSessions returns the sessions age-based GC deletes, oldest
// first: ended, with a completed summary, synced if syncGated, with documents
// not marked unverified on Drive (documents_unverified), never one whose
// Drive folder is in the trash or gone (REMOTE_DELETED), and older than
// maxAgeDays days. A session's age runs from when it started or, if it was
// restored from Drive, from its restore, so the pass after a restore keeps
// it. maxAgeDays <= 0 is no age limit: it returns none.
func (s *SQLiteStore) GetGCEligibleSessions(maxAgeDays int, syncGated bool) ([]string, error) {
	cutoff, ok := ageCutoff(time.Now().UTC(), maxAgeDays)
	if !ok {
		return nil, nil
	}
	at := cutoff.Format(time.RFC3339Nano)
	// restored_at is '' for a session recorded here, which is before any time.
	cond, args := removable(syncGated)
	// A session whose documents are unverified on Drive is never deleted.
	query := `SELECT id FROM sessions WHERE summary_status = 'completed' AND started_at < ? AND restored_at < ? AND documents_unverified = 0 AND ` + cond +
		` ORDER BY started_at ASC`

	rows, err := s.db.Query(query, append([]any{at, at}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("query gc eligible sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sessions: %w", err)
	}
	return ids, nil
}

// SessionRecording is a session's saved recording: its audio path, which may
// name several files (a merged session), and whether disk pressure may remove
// it.
type SessionRecording struct {
	ID        string
	AudioPath string
	Removable bool
}

// removable is the condition under which GC may remove a session's recording
// (disk pressure) or the session with it (the age policy, which also needs
// documents_unverified = 0), and its arguments: the session ended, no
// refinement is reading the file, it owes Drive no replacement after a
// segment deletion (drive_replace_pending: the replacement needs the row and
// the spliced recording, sync gate or not), its Drive folder is not in the
// trash or gone (REMOTE_DELETED: the device then holds its only lasting copy,
// sync gate or not), and, when syncGated, it is synced. A session whose
// documents are unverified on Drive (documents_unverified: synced by an
// earlier version) has its audio on Drive: under the sync gate GC removes it
// once Drive answers that the session's folder is there; with no sync gate
// there is no Drive to ask, so it is kept. Any other sync state, a failure
// included, keeps the recording.
func removable(syncGated bool) (string, []any) {
	cond := `status = 'ended' AND refinement_status != ? AND sync_state != ? AND drive_replace_pending = 0
		AND NOT EXISTS (SELECT 1 FROM deleted_segments WHERE deleted_segments.session_id = sessions.id AND splice_complete = 0)`
	if syncGated {
		cond += ` AND sync_status = ?`
		return cond, []any{RefinementRunning, SyncStateRemoteDeleted, SyncSynced}
	}
	cond += ` AND documents_unverified = 0`
	return cond, []any{RefinementRunning, SyncStateRemoteDeleted}
}

// CountUnverifiedSessions returns how many sessions GC keeps until a sync
// verifies their documents on Drive (documents_unverified), for the status
// the app reports.
func (s *SQLiteStore) CountUnverifiedSessions() (int, error) {
	var n int
	// A REMOTE_DELETED session is never synced again, so it is not waiting
	// for a re-check.
	if err := s.db.QueryRow(`SELECT count(*) FROM sessions WHERE documents_unverified = 1 AND sync_state != ?`, SyncStateRemoteDeleted).Scan(&n); err != nil {
		return 0, fmt.Errorf("count unverified sessions: %w", err)
	}
	return n, nil
}

// Recordings lists, oldest first, every session that names audio, and whether
// disk pressure may remove its recording (see removable).
func (s *SQLiteStore) Recordings(syncGated bool) ([]SessionRecording, error) {
	cond, args := removable(syncGated)
	rows, err := s.db.Query(`SELECT id, audio_path, (`+cond+`) FROM sessions
		WHERE audio_path != '' ORDER BY started_at ASC`, args...)
	if err != nil {
		return nil, fmt.Errorf("query recordings: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var recordings []SessionRecording
	for rows.Next() {
		var rec SessionRecording
		if err := rows.Scan(&rec.ID, &rec.AudioPath, &rec.Removable); err != nil {
			return nil, fmt.Errorf("scan recording: %w", err)
		}
		recordings = append(recordings, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recordings: %w", err)
	}
	return recordings, nil
}

// AudioRemoval is what MarkAudioRemoved changed in a session's row: its audio
// path before and after, and its marker, refinement status and sync state
// before, so RestoreAudio can put back the part whose files stayed on disk.
type AudioRemoval struct {
	ID               string
	Before, Left     string
	RemovedAtBefore  string
	RefinementBefore string
	SyncStateBefore  string
}

// MarkAudioRemoved records that disk pressure is about to remove part or all
// of session id's audio, before it removes any file, so the session never
// names a file GC has removed. The audio path becomes left, what will be left
// of before; the marker the UI shows is set to removedAt; and a refinement
// that can no longer run (pending or failed) becomes no_audio when no
// recording is left on disk (audioLeft false; left may still name a file GC
// could not see, on a disk not mounted for the moment). It does this only
// while the session's audio path is still before and disk pressure may still
// remove it (see removable), so a refinement that started reading the file
// since GC listed it keeps it. It reports whether it did, and returns the row
// as it was, for RestoreAudio.
func (s *SQLiteStore) MarkAudioRemoved(id, before, left string, audioLeft, syncGated bool, removedAt time.Time) (AudioRemoval, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return AudioRemoval{}, false, fmt.Errorf("begin marking audio removed for session %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()

	removal := AudioRemoval{ID: id, Before: before, Left: left}
	cond, args := removable(syncGated)
	err = tx.QueryRow(`SELECT audio_removed_at, refinement_status, sync_state FROM sessions WHERE id = ? AND audio_path = ? AND `+cond,
		append([]any{id, before}, args...)...).Scan(&removal.RemovedAtBefore, &removal.RefinementBefore, &removal.SyncStateBefore)
	if errors.Is(err, sql.ErrNoRows) {
		return AudioRemoval{}, false, nil
	}
	if err != nil {
		return AudioRemoval{}, false, fmt.Errorf("read session %s to mark its audio removed: %w", id, err)
	}
	if _, err := tx.Exec(
		`UPDATE sessions SET audio_path = ?, audio_removed_at = ?,
			refinement_status = CASE WHEN ? AND refinement_status IN (?, ?) THEN ? ELSE refinement_status END
		 WHERE id = ?`,
		left, removedAt.UTC().Format(time.RFC3339Nano),
		!audioLeft, RefinementPending, RefinementFailed, RefinementNoAudio,
		id,
	); err != nil {
		return AudioRemoval{}, false, fmt.Errorf("mark audio removed for session %s: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return AudioRemoval{}, false, fmt.Errorf("commit marking audio removed for session %s: %w", id, err)
	}
	return removal, true, nil
}

// RestoreAudio puts back the part of removal whose files GC could not remove
// after all: the session's audio path becomes restored, what is still on disk
// of removal.Before. With all of it back, the marker and refinement status
// are as they were before the mark; with part of it, the marker stays and a
// refinement the mark moved to no_audio goes back. A sync that finished
// since the mark uploaded the row as marked, so the recordings put back may
// not be on Drive: the session goes back to pending sync, keeping its folder.
// It refuses a row whose audio path is no longer what the mark left.
func (s *SQLiteStore) RestoreAudio(removal *AudioRemoval, restored string) error {
	res, err := s.db.Exec(
		`UPDATE sessions SET audio_path = ?,
			audio_removed_at = CASE WHEN ? = ? THEN ? ELSE audio_removed_at END,
			refinement_status = CASE WHEN ? != '' AND refinement_status = ? THEN ? ELSE refinement_status END,
			sync_status = CASE WHEN sync_state = ? AND sync_state != ? THEN ? ELSE sync_status END,
			sync_state = CASE WHEN sync_state = ? AND sync_state != ? THEN ? ELSE sync_state END
		 WHERE id = ? AND audio_path = ? AND status = 'ended'`,
		restored,
		restored, removal.Before, removal.RemovedAtBefore,
		restored, RefinementNoAudio, removal.RefinementBefore,
		SyncStateSynced, removal.SyncStateBefore, SyncPending,
		SyncStateSynced, removal.SyncStateBefore, SyncStatePending,
		removal.ID, removal.Left,
	)
	if err != nil {
		return fmt.Errorf("restore audio of session %s: %w", removal.ID, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("restore audio rows affected: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("restore audio of session %s: its audio path is no longer %q", removal.ID, removal.Left)
	}
	return nil
}

// ErrDeleteReplacementPending is the error DeleteSession returns, deleting
// nothing, for a session that still owes Google Drive a replacement after a
// segment deletion, or whose Drive folder one still owes (a source merged
// into the session the segment was deleted from): with its row would go the
// replacement, its warning, or the folder the replacement must reach, and
// the Drive folder would keep the deleted words.
var ErrDeleteReplacementPending = errors.New("this session still has a deleted segment on Google Drive; delete it after its Drive files are replaced")

// noOwedDriveReplacement is the condition, on a sessions row, that it owes
// Google Drive no replacement and its folder is owed none.
const noOwedDriveReplacement = `drive_replace_pending = 0 AND NOT EXISTS (
	SELECT 1 FROM drive_replacement_folders
	WHERE drive_replacement_folders.folder_session_id = sessions.id AND drive_replacement_folders.state = 'owed')`

func (s *SQLiteStore) DeleteSession(id string) error {
	res, err := s.db.Exec(`DELETE FROM sessions WHERE id = ? AND `+noOwedDriveReplacement, id)
	if err != nil {
		return fmt.Errorf("delete session %s: %w", id, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete session rows affected: %w", err)
	}
	if rows == 0 {
		var pending int
		switch err := s.db.QueryRow(`SELECT drive_replace_pending FROM sessions WHERE id = ?`, id).Scan(&pending); {
		case errors.Is(err, sql.ErrNoRows):
			return sql.ErrNoRows
		case err != nil:
			return fmt.Errorf("delete session %s: %w", id, err)
		default:
			return fmt.Errorf("delete session %s: %w", id, ErrDeleteReplacementPending)
		}
	}
	return nil
}

// DeleteEndedSession deletes session id for the age policy only while it is as
// GC read it: ended, naming audioPath, with documents not marked unverified
// on Drive, and, when syncGated, synced. It reports whether it did. A
// session merged since GC read it gave its recording to the merged session,
// so deleting it, and removing that recording with it, would leave the
// merged session naming a file that is gone. One whose summary or transcript
// changed since GC read it is waiting to sync them (see the
// sessions_documents_changed trigger): until it does, the newest text is
// only on the device.
func (s *SQLiteStore) DeleteEndedSession(id, audioPath string, syncGated bool) (bool, error) {
	cond, args := removable(syncGated)
	res, err := s.db.Exec(`DELETE FROM sessions WHERE id = ? AND audio_path = ? AND documents_unverified = 0 AND `+cond, append([]any{id, audioPath}, args...)...)
	if err != nil {
		return false, fmt.Errorf("delete session %s: %w", id, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("delete session rows affected: %w", err)
	}
	return rows == 1, nil
}

// MarkRemoteDeleted records that synced session id's Drive folder, folderID,
// is in the trash or gone, as GC found before deleting it or removing its
// audio: the session becomes REMOTE_DELETED, keeping its folder, so no GC
// path deletes it (the device then holds its only lasting copy) and no sync
// uploads it again. It does this only while the session is still synced in
// that folder (a session restored from Drive is synced with no sync state),
// so a sync that started since is left as it is, and reports whether it did.
func (s *SQLiteStore) MarkRemoteDeleted(id, folderID, errorMessage string) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE sessions SET sync_state = ?, sync_status = ?, error_message = ?
		 WHERE id = ? AND sync_status = ? AND gdrive_folder_id = ?`,
		SyncStateRemoteDeleted, syncStatusFromState(SyncStateRemoteDeleted), errorMessage,
		id, SyncSynced, folderID,
	)
	if err != nil {
		return false, fmt.Errorf("mark session %s remote deleted: %w", id, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("mark remote deleted rows affected: %w", err)
	}
	return rows == 1, nil
}

// ImportSession inserts a fully-formed session restored from a backup, and its
// segments, in a single transaction. The session keeps the time it started,
// but its age for the retention policy runs from now, its restore.
// Returns an error containing "already exists" if the session ID is already present.
func (s *SQLiteStore) ImportSession(sess *Session, segments []transcribe.Segment) error {
	// Check for existing session first.
	var exists int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id = ?`, sess.ID).Scan(&exists); err != nil {
		return fmt.Errorf("check existing session %s: %w", sess.ID, err)
	}
	if exists > 0 {
		return fmt.Errorf("session %s already exists", sess.ID)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin import tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	endedAt := ""
	if sess.EndedAt != nil {
		endedAt = sess.EndedAt.UTC().Format(time.RFC3339Nano)
	}

	_, err = tx.Exec(
		`INSERT INTO sessions(id, title, started_at, ended_at, status, summary, summary_status, summary_preset, speaker_names, refined_transcript, refinement_status, audio_path, sync_status, envoy_publish_status, sync_state, retry_count, last_sync_attempt, error_message, gdrive_folder_id, canonical_transcript, transcript_source, restored_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.ID,
		sess.Title,
		sess.StartedAt.UTC().Format(time.RFC3339Nano),
		endedAt,
		sess.Status,
		sess.Summary,
		sess.SummaryStatus,
		sess.SummaryPreset,
		sess.SpeakerNames,
		sess.RefinedTranscript,
		sess.RefinementStatus,
		sess.AudioPath,
		sess.SyncStatus,
		sess.EnvoyPublishStatus,
		func() string {
			// A session restored synced, with no sync state given, is SYNCED.
			if sess.SyncState == "" && sess.SyncStatus == SyncSynced {
				return SyncStateSynced
			}
			return sess.SyncState
		}(),
		sess.RetryCount,
		func() any {
			if sess.LastSyncAttempt == nil {
				return nil
			}
			return sess.LastSyncAttempt.UTC().Format(time.RFC3339Nano)
		}(),
		sess.ErrorMessage,
		sess.GDriveFolderID,
		sess.CanonicalTranscript,
		sess.TranscriptSource,
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("insert session %s: %w", sess.ID, err)
	}

	for _, seg := range segments {
		_, err = tx.Exec(
			`INSERT INTO segments(session_id, speaker, text, start_time, end_time, timestamp) VALUES(?, ?, ?, ?, ?, ?)`,
			sess.ID,
			seg.Speaker,
			strings.TrimSpace(seg.Text),
			seg.StartTime,
			seg.EndTime,
			seg.Timestamp.UTC().Format(time.RFC3339Nano),
		)
		if err != nil {
			return fmt.Errorf("insert segment for session %s: %w", sess.ID, err)
		}
	}

	return tx.Commit()
}

// Ping checks if the database is accessible.
func (s *SQLiteStore) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func (s *SQLiteStore) Search(query string, opts SearchOptions) ([]SearchResult, error) {
	matchQuery := buildFTS5MatchQuery(query)
	if matchQuery == "" {
		return []SearchResult{}, nil
	}

	// Build WHERE clause with optional filters
	whereConditions := []string{
		"sessions_fts MATCH ?",
		"sessions.status NOT IN ('discarded', 'merged')",
	}
	args := []any{matchQuery}

	// Add date_from filter if provided
	if opts.DateFrom != "" {
		whereConditions = append(whereConditions, "sessions.started_at >= ?")
		args = append(args, opts.DateFrom)
	}

	// Add date_to filter if provided
	if opts.DateTo != "" {
		whereConditions = append(whereConditions, "sessions.started_at <= ?")
		args = append(args, opts.DateTo)
	}

	// Add preset filter if provided
	if opts.Preset != "" {
		whereConditions = append(whereConditions, "sessions.summary_preset = ?")
		args = append(args, opts.Preset)
	}

	whereClause := strings.Join(whereConditions, " AND ")

	sqlQuery := fmt.Sprintf(`SELECT sessions.id,
	        sessions.title,
	        COALESCE(
	          NULLIF(snippet(sessions_fts, 2, '<mark>', '</mark>', ' … ', 24), ''),
	          NULLIF(snippet(sessions_fts, 1, '<mark>', '</mark>', ' … ', 24), ''),
	          sessions.title
	        ) AS snippet,
	        bm25(sessions_fts) AS rank
	 FROM sessions_fts
	 JOIN sessions ON sessions.rowid = sessions_fts.rowid
	 WHERE %s
	 ORDER BY rank ASC
	 LIMIT 50`, whereClause)

	rows, err := s.db.Query(sqlQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("search sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	results := make([]SearchResult, 0, 16)
	for rows.Next() {
		var result SearchResult
		if err := rows.Scan(&result.SessionID, &result.Title, &result.Snippet, &result.Rank); err != nil {
			return nil, fmt.Errorf("scan search result: %w", err)
		}
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate search results: %w", err)
	}

	return results, nil
}

func buildFTS5MatchQuery(query string) string {
	tokens := ftsQueryTokenPattern.FindAllString(query, -1)
	if len(tokens) == 0 {
		return ""
	}

	quoted := make([]string, 0, len(tokens))
	for _, token := range tokens {
		clean := strings.TrimSpace(token)
		if strings.HasPrefix(clean, "-") {
			continue
		}
		clean = strings.TrimPrefix(clean, "+")
		upper := strings.ToUpper(clean)
		if upper == "AND" || upper == "OR" || upper == "NOT" || upper == "NEAR" {
			continue
		}
		if clean == "" {
			continue
		}
		clean = strings.ReplaceAll(clean, `"`, `""`)
		quoted = append(quoted, `"`+clean+`"`)
	}

	return strings.Join(quoted, " AND ")
}

// AggregateSessions returns cross-session statistics grouped by date or preset.
func (s *SQLiteStore) AggregateSessions(opts AggregateOptions) (AggregateResult, error) {
	query := `SELECT id, title, started_at, summary_preset FROM sessions WHERE status NOT IN ('discarded', 'merged')`
	var args []any

	if opts.DateFrom != "" {
		query += ` AND started_at >= ?`
		args = append(args, opts.DateFrom)
	}
	if opts.DateTo != "" {
		query += ` AND started_at <= ?`
		args = append(args, opts.DateTo)
	}
	if opts.Preset != "" {
		query += ` AND summary_preset = ?`
		args = append(args, opts.Preset)
	}
	query += ` ORDER BY started_at DESC`

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return AggregateResult{}, fmt.Errorf("aggregate sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	groupBy := opts.GroupBy
	if groupBy == "" {
		groupBy = "date"
	}

	groupMap := make(map[string]*AggregateGroup)
	var groupOrder []string
	var totalCount int

	for rows.Next() {
		var id, title, startedAtStr, preset string
		if err := rows.Scan(&id, &title, &startedAtStr, &preset); err != nil {
			return AggregateResult{}, fmt.Errorf("scan aggregate session: %w", err)
		}

		parsedStart, err := time.Parse(time.RFC3339Nano, startedAtStr)
		if err != nil {
			return AggregateResult{}, fmt.Errorf("parse aggregate started_at: %w", err)
		}

		var key string
		if groupBy == "preset" {
			key = preset
		} else {
			key = parsedStart.Format("2006-01-02")
		}

		g, ok := groupMap[key]
		if !ok {
			g = &AggregateGroup{Key: key}
			groupMap[key] = g
			groupOrder = append(groupOrder, key)
		}
		g.Count++
		g.Sessions = append(g.Sessions, SessionSummary{
			ID:            id,
			Title:         title,
			StartedAt:     parsedStart,
			SummaryPreset: preset,
		})
		totalCount++
	}
	if err := rows.Err(); err != nil {
		return AggregateResult{}, fmt.Errorf("iterate aggregate rows: %w", err)
	}

	groups := make([]AggregateGroup, 0, len(groupOrder))
	for _, key := range groupOrder {
		groups = append(groups, *groupMap[key])
	}

	return AggregateResult{
		SessionCount: totalCount,
		Groups:       groups,
	}, nil
}

// StoreEmbedding stores an embedding vector for a session chunk.
func (s *SQLiteStore) StoreEmbedding(sessionID string, chunkIndex int, vector []float32, textHash, model string) error {
	// Serialize vector to BLOB
	buf := new(bytes.Buffer)
	if err := binary.Write(buf, binary.LittleEndian, vector); err != nil {
		return fmt.Errorf("serialize embedding vector: %w", err)
	}

	_, err := s.db.Exec(
		`INSERT INTO embeddings(session_id, chunk_index, embedding, text_hash, model)
		 VALUES(?, ?, ?, ?, ?)
		 ON CONFLICT(session_id, chunk_index)
		 DO UPDATE SET embedding = excluded.embedding, text_hash = excluded.text_hash, model = excluded.model, created_at = CURRENT_TIMESTAMP`,
		sessionID, chunkIndex, buf.Bytes(), textHash, model,
	)
	if err != nil {
		return fmt.Errorf("store embedding for session %s chunk %d: %w", sessionID, chunkIndex, err)
	}
	return nil
}

// StoreEmbeddingAtGeneration writes a chunk only while the transcript
// generation that produced it remains current.
func (s *SQLiteStore) StoreEmbeddingAtGeneration(sessionID string, chunkIndex int, vector []float32, textHash, model string, generation int) error {
	buf := new(bytes.Buffer)
	if err := binary.Write(buf, binary.LittleEndian, vector); err != nil {
		return fmt.Errorf("serialize embedding vector: %w", err)
	}
	res, err := s.db.Exec(`
		INSERT INTO embeddings(session_id, chunk_index, embedding, text_hash, model)
		SELECT ?, ?, ?, ?, ?
		WHERE EXISTS (SELECT 1 FROM sessions WHERE id = ? AND deletion_generation = ?)
		ON CONFLICT(session_id, chunk_index)
		DO UPDATE SET embedding = excluded.embedding, text_hash = excluded.text_hash, model = excluded.model, created_at = CURRENT_TIMESTAMP`,
		sessionID, chunkIndex, buf.Bytes(), textHash, model, sessionID, generation)
	if err != nil {
		return fmt.Errorf("store embedding for session %s chunk %d: %w", sessionID, chunkIndex, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("count embedding write for session %s: %w", sessionID, err)
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DeleteEmbeddingChunksAtOrAfterGeneration removes stale trailing chunks only
// while the indexer's transcript generation remains current.
func (s *SQLiteStore) DeleteEmbeddingChunksAtOrAfterGeneration(sessionID string, chunkIndex, generation int) error {
	_, err := s.db.Exec(`
		DELETE FROM embeddings
		WHERE session_id = ? AND chunk_index >= ?
		  AND EXISTS (SELECT 1 FROM sessions WHERE id = ? AND deletion_generation = ?)`,
		sessionID, chunkIndex, sessionID, generation)
	if err != nil {
		return fmt.Errorf("delete trailing embeddings for session %s: %w", sessionID, err)
	}
	return nil
}

// GetEmbeddings retrieves all embeddings for a session.
func (s *SQLiteStore) GetEmbeddings(sessionID string) ([]StoredEmbedding, error) {
	rows, err := s.db.Query(
		`SELECT session_id, chunk_index, embedding, text_hash, model, created_at FROM embeddings WHERE session_id = ? ORDER BY chunk_index ASC`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("query embeddings for session %s: %w", sessionID, err)
	}
	defer func() { _ = rows.Close() }()

	var embeddings []StoredEmbedding
	for rows.Next() {
		var emb StoredEmbedding
		var embeddingBlob []byte
		var createdAtStr string

		if err := rows.Scan(&emb.SessionID, &emb.ChunkIndex, &embeddingBlob, &emb.TextHash, &emb.Model, &createdAtStr); err != nil {
			return nil, fmt.Errorf("scan embedding for session %s: %w", sessionID, err)
		}

		// Deserialize vector from BLOB
		buf := bytes.NewReader(embeddingBlob)
		var vector []float32
		for {
			var f float32
			if err := binary.Read(buf, binary.LittleEndian, &f); err != nil {
				if err == io.EOF {
					break
				}
				return nil, fmt.Errorf("deserialize embedding vector for session %s: %w", sessionID, err)
			}
			vector = append(vector, f)
		}
		emb.Vector = vector

		// Parse timestamp
		parsedTime, err := time.Parse(time.RFC3339Nano, createdAtStr)
		if err != nil {
			// Fallback to parsing as SQLite datetime format
			parsedTime, err = time.Parse("2006-01-02 15:04:05", createdAtStr)
			if err != nil {
				return nil, fmt.Errorf("parse created_at for session %s: %w", sessionID, err)
			}
		}
		emb.CreatedAt = parsedTime

		embeddings = append(embeddings, emb)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate embeddings for session %s: %w", sessionID, err)
	}

	return embeddings, nil
}

// GetAllEmbeddings retrieves all embeddings across all sessions.
func (s *SQLiteStore) GetAllEmbeddings() ([]StoredEmbedding, error) {
	rows, err := s.db.Query(
		`SELECT session_id, chunk_index, embedding, text_hash, model, created_at FROM embeddings ORDER BY session_id ASC, chunk_index ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("query all embeddings: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var embeddings []StoredEmbedding
	for rows.Next() {
		var emb StoredEmbedding
		var embeddingBlob []byte
		var createdAtStr string

		if err := rows.Scan(&emb.SessionID, &emb.ChunkIndex, &embeddingBlob, &emb.TextHash, &emb.Model, &createdAtStr); err != nil {
			return nil, fmt.Errorf("scan embedding: %w", err)
		}

		// Deserialize vector from BLOB
		buf := bytes.NewReader(embeddingBlob)
		var vector []float32
		for {
			var f float32
			if err := binary.Read(buf, binary.LittleEndian, &f); err != nil {
				if err == io.EOF {
					break
				}
				return nil, fmt.Errorf("deserialize embedding vector: %w", err)
			}
			vector = append(vector, f)
		}
		emb.Vector = vector

		// Parse timestamp
		parsedTime, err := time.Parse(time.RFC3339Nano, createdAtStr)
		if err != nil {
			// Fallback to parsing as SQLite datetime format
			parsedTime, err = time.Parse("2006-01-02 15:04:05", createdAtStr)
			if err != nil {
				return nil, fmt.Errorf("parse created_at: %w", err)
			}
		}
		emb.CreatedAt = parsedTime

		embeddings = append(embeddings, emb)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate all embeddings: %w", err)
	}

	return embeddings, nil
}

func (s *SQLiteStore) GetSessionsWithoutEmbeddings() ([]string, error) {
	rows, err := s.db.Query(
		`SELECT s.id
		 FROM sessions s
		 LEFT JOIN embeddings e ON e.session_id = s.id
		 WHERE s.status = 'ended'
		   AND s.canonical_transcript IS NOT NULL
		   AND TRIM(s.canonical_transcript) != ''
		 GROUP BY s.id
		 HAVING COUNT(e.session_id) = 0
		 ORDER BY s.started_at ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("query sessions without embeddings: %w", err)
	}
	defer func() { _ = rows.Close() }()

	ids := make([]string, 0, 16)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan session without embeddings: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sessions without embeddings: %w", err)
	}

	return ids, nil
}

// DeleteEmbeddings deletes all embeddings for a session.
func (s *SQLiteStore) DeleteEmbeddings(sessionID string) error {
	_, err := s.db.Exec(`DELETE FROM embeddings WHERE session_id = ?`, sessionID)
	if err != nil {
		return fmt.Errorf("delete embeddings for session %s: %w", sessionID, err)
	}
	return nil
}

// StoreEvent inserts an event into the mcp_events table.
func (s *SQLiteStore) StoreEvent(eventType, payload string) error {
	_, err := s.db.Exec(
		`INSERT INTO mcp_events(event_type, payload) VALUES(?, ?)`,
		eventType, payload,
	)
	if err != nil {
		return fmt.Errorf("store event %s: %w", eventType, err)
	}
	return nil
}

// GetEventsSince returns events with id > cursor, ordered by id ASC, limited to limit.
func (s *SQLiteStore) GetEventsSince(cursor int64, limit int) ([]StoredEvent, error) {
	rows, err := s.db.Query(
		`SELECT id, event_type, payload, created_at FROM mcp_events WHERE id > ? ORDER BY id ASC LIMIT ?`,
		cursor, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("get events since %d: %w", cursor, err)
	}
	defer func() { _ = rows.Close() }()

	var events []StoredEvent
	for rows.Next() {
		var ev StoredEvent
		var createdAtStr string
		if err := rows.Scan(&ev.ID, &ev.EventType, &ev.Payload, &createdAtStr); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		parsedTime, err := time.Parse("2006-01-02 15:04:05", createdAtStr)
		if err != nil {
			parsedTime, err = time.Parse(time.RFC3339Nano, createdAtStr)
			if err != nil {
				return nil, fmt.Errorf("parse event created_at: %w", err)
			}
		}
		ev.CreatedAt = parsedTime
		events = append(events, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate events: %w", err)
	}
	return events, nil
}

// PurgeOldEvents deletes events older than maxAge.
func (s *SQLiteStore) PurgeOldEvents(maxAge time.Duration) error {
	cutoff := time.Now().UTC().Add(-maxAge).Format("2006-01-02 15:04:05")
	_, err := s.db.Exec(`DELETE FROM mcp_events WHERE created_at < ?`, cutoff)
	if err != nil {
		return fmt.Errorf("purge old events: %w", err)
	}
	return nil
}

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
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/sjawhar/ghost-wispr/internal/erase"
	"github.com/sjawhar/ghost-wispr/internal/recording"
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
	SummaryModel        string     `json:"summary_model"`
	SummaryVersion      int        `json:"summary_version"`
	SummaryStatus       string     `json:"summary_status"`
	SummaryPreset       string     `json:"summary_preset"`
	SpeakerNames        string     `json:"speaker_names"`
	RefinedTranscript   string     `json:"refined_transcript"`
	RefinementStatus    string     `json:"refinement_status"`
	RefinementModel     string     `json:"refinement_model"`
	RefinedRevision     int        `json:"refined_revision"`
	AudioPath           string     `json:"audio_path"`
	AudioRemovedAt      string     `json:"audio_removed_at"` // when disk pressure removed audio (RFC 3339), "" if never: all of it, or part when AudioPath names what is left; the session is kept
	RestoredAt          string     `json:"restored_at"`      // when a Drive import restored the session, "" otherwise
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
	// path is the database file, beside which createPreMigrationBackup keeps
	// its backup.
	path string
	// eraseMu keeps a deletion's erase from the write-ahead log and the
	// backup, and a retry of an owed one (RetryOwedErasure), from running at
	// once: both overwrite the log and take the backup again.
	eraseMu sync.Mutex
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

	// The per-connection settings are part of the name every connection opens,
	// so one that database/sql opens again after dropping the first has them
	// too: secure_delete (a deleted segment's freed pages are zeroed, the
	// freelist included, which is what keeps its words out of the database
	// file; see postDeleteMaintenance), temporary data in memory (a sort or
	// index SQLite spills to a temporary file would leave transcript text in
	// the blocks it frees), foreign keys (the cascades that remove a session's
	// rows) and the busy timeout. Set with PRAGMA once, they held for the first
	// connection only.
	db, err := sql.Open("sqlite", dbPath+"?_pragma=secure_delete(1)&_pragma=temp_store(2)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}

	// One connection keeps refined deletion out of Canonicalize's transaction.
	// TestR7OpenTransactionExcludesRefinedDelete guards this invariant.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	store := &SQLiteStore{db: db, path: dbPath}

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
// The copy is written beside the backup, flushed to disk and put in its
// place, so a backup cut off part way leaves the last one as it was; a backup
// that fails removes its unfinished copy. The backup it replaces, and an
// unfinished copy it removes, are overwritten with zeros before the file
// system frees them (erase): they hold transcripts.
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

	backupPath := preMigrationBackupPath(dbPath)
	partPath := backupPath + ".part"
	// VACUUM INTO writes only a file that does not exist yet: remove the
	// copy a backup cut off part way left.
	if err := erase.Remove(partPath); err != nil && !os.IsNotExist(err) && !errors.Is(err, erase.ErrLeftover) {
		return fmt.Errorf("remove unfinished backup: %w", err)
	}
	defer func() {
		if err != nil {
			_ = erase.Remove(partPath)
		}
	}()
	// VACUUM INTO reads one consistent snapshot of the database, the
	// write-ahead log included.
	if err := vacuumInto(db, partPath); err != nil {
		return fmt.Errorf("copy db to backup: %w", err)
	}
	// SQLite does not flush the copy VACUUM INTO writes.
	if err := syncFile(partPath); err != nil {
		return fmt.Errorf("flush backup: %w", err)
	}
	if err := erase.Replace(partPath, backupPath); err != nil && !errors.Is(err, erase.ErrLeftover) {
		return fmt.Errorf("replace backup: %w", err)
	}
	// The rename lasts only once the directory naming it is on disk.
	if err := syncFile(filepath.Dir(backupPath)); err != nil {
		return fmt.Errorf("flush backup directory: %w", err)
	}

	slog.Info("pre-migration backup created", "path", backupPath)
	return nil
}

// vacuumInto writes a copy of db to the file at path, which must not exist.
// It needs free space about the size of the database. Tests replace it to
// fail as a full disk does.
var vacuumInto = func(db *sql.DB, path string) error {
	_, err := db.Exec(`VACUUM INTO ?`, path)
	return err
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
	}
	for _, p := range pragmas {
		if _, err := s.db.Exec(p); err != nil {
			return fmt.Errorf("apply pragma %q: %w", p, err)
		}
	}
	// A session delete records a raw erase before it moves the raw file to its
	// hidden name. Restore those live files before any Leftovers pass can
	// overwrite them. A failed restore remains reserved and is reported to the
	// caller at startup rather than preventing the app from starting.
	if err := s.restoreRawEraseReservations(); err != nil {
		return err
	}
	reserved, err := s.ReservedRawErasePaths()
	if err != nil {
		return err
	}
	// Finish erasing a backup the app stopped part way through replacing,
	// except any raw file that a live session's reservation still protects.
	if err := erase.LeftoversExcept(filepath.Dir(dbPath), func(path string) bool { return reserved[path] }); err != nil {
		slog.Warn("failed to erase leftover backups", "path", dbPath, "error", err)
	}
	// A scratch table overwriteWriteAheadLog left when the app stopped goes
	// before the backup is taken, or the backup would carry it and never again
	// have the database's schema (sameSchema).
	if _, err := s.db.Exec(`DROP TABLE IF EXISTS ` + walScratchTable); err != nil {
		return fmt.Errorf("drop the write-ahead log scratch table: %w", err)
	}

	// Create pre-migration backup after pragmas are set (which creates the DB file)
	backupTaken := true
	if err := createPreMigrationBackup(s.db, dbPath); err != nil {
		backupTaken = false
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
			summary_model TEXT NOT NULL DEFAULT '',
			summary_version INTEGER NOT NULL DEFAULT 1,
			summary_status TEXT NOT NULL DEFAULT 'pending',
			summary_preset TEXT NOT NULL DEFAULT '',
			speaker_names TEXT NOT NULL DEFAULT '{}',
			refined_transcript TEXT NOT NULL DEFAULT '',
			refinement_status TEXT NOT NULL DEFAULT 'pending',
			refinement_model TEXT NOT NULL DEFAULT '',
			refined_revision INTEGER NOT NULL DEFAULT 0,
			audio_path TEXT NOT NULL DEFAULT '',
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

	// raw_erase_reservations names a raw file's original and hidden paths
	// before the session delete moves it. On the next startup, a reservation
	// whose session still exists restores that raw file before Leftovers
	// overwrites hidden erasures. DeleteSession removes its reservations in
	// the same transaction as their session rows.
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS raw_erase_reservations (
			session_id TEXT NOT NULL,
			original_path TEXT NOT NULL,
			hidden_path TEXT NOT NULL UNIQUE,
			PRIMARY KEY (session_id, original_path)
		);
	`); err != nil {
		return fmt.Errorf("create raw erase reservations table: %w", err)
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
			state TEXT NOT NULL DEFAULT '` + DriveReplacementFolderOwed + `',
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
		`ALTER TABLE sessions ADD COLUMN summary_model TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN refinement_model TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN refined_revision INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE sessions ADD COLUMN drive_replace_pending INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE sessions ADD COLUMN deletion_generation INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE sessions ADD COLUMN refinement_generation INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE sessions ADD COLUMN summary_generation INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE sessions ADD COLUMN drive_replace_since TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN drive_replace_failures INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE sessions ADD COLUMN drive_replace_error TEXT NOT NULL DEFAULT ''`,
		// 1 while a merged source's own Drive folder holds documents that
		// a refined "Delete text" in the session it was merged into changed:
		// the next sync of either updates them in place
		// (DriveDocumentsOwed, ClearDriveDocumentsPending).
		`ALTER TABLE sessions ADD COLUMN drive_documents_pending INTEGER NOT NULL DEFAULT 0`,
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
			transcript_revision INTEGER NOT NULL DEFAULT 0,
			recording_path TEXT NOT NULL DEFAULT '',
			recording_sha256 TEXT NOT NULL DEFAULT '',
			recording_ordinal INTEGER NOT NULL DEFAULT 0,
			first_word_ordinal INTEGER NOT NULL DEFAULT -1,
			last_word_ordinal INTEGER NOT NULL DEFAULT -1,
			untimed INTEGER NOT NULL DEFAULT 0,
			untimed_source_id TEXT NOT NULL DEFAULT '',
			FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE
		);
	`); err != nil {
		return fmt.Errorf("create segments table: %w", err)
	}

	for _, stmt := range []string{
		`ALTER TABLE segments ADD COLUMN transcript_revision INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE segments ADD COLUMN recording_path TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE segments ADD COLUMN recording_sha256 TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE segments ADD COLUMN recording_ordinal INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE segments ADD COLUMN first_word_ordinal INTEGER NOT NULL DEFAULT -1`,
		`ALTER TABLE segments ADD COLUMN last_word_ordinal INTEGER NOT NULL DEFAULT -1`,
		`ALTER TABLE segments ADD COLUMN untimed INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE segments ADD COLUMN untimed_source_id TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := s.db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("migrate segment provenance: %w", err)
		}
	}

	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS refined_words (
			session_id TEXT NOT NULL,
			transcript_revision INTEGER NOT NULL,
			recording_path TEXT NOT NULL,
			recording_sha256 TEXT NOT NULL,
			recording_ordinal INTEGER NOT NULL,
			ordinal INTEGER NOT NULL,
			text TEXT NOT NULL,
			speaker INTEGER NOT NULL,
			start_time REAL NOT NULL,
			end_time REAL NOT NULL,
			PRIMARY KEY (session_id, transcript_revision, recording_path, recording_sha256, ordinal),
			FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE
		);
	`); err != nil {
		return fmt.Errorf("create refined words table: %w", err)
	}

	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS recording_sources (
			merged_session_id TEXT NOT NULL,
			recording_path TEXT NOT NULL,
			source_session_id TEXT NOT NULL,
			PRIMARY KEY (merged_session_id, recording_path),
			FOREIGN KEY(merged_session_id) REFERENCES sessions(id) ON DELETE CASCADE
		);
	`); err != nil {
		return fmt.Errorf("create recording sources table: %w", err)
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
	// deleted_text_spans: where, in a recording that keeps them, the words of
	// a refined "Delete text" are: the recording's file name, its span and
	// SHA-256, never the words. Refinement drops the words it finds in a span.
	// A span belongs to its recording, not to the session the deletion was
	// made in (session_id), which a merge can leave as a row of its own that is
	// deleted while the recording lives on: so no foreign key takes it.
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS deleted_text_spans (
			session_id TEXT NOT NULL,
			recording TEXT NOT NULL,
			start REAL NOT NULL,
			end REAL NOT NULL,
			sha256 TEXT NOT NULL,
			deleted_at TEXT NOT NULL
		);
	`); err != nil {
		return fmt.Errorf("create deleted_text_spans table: %w", err)
	}
	// erase_directories: a directory a whole-file audio erase was started in
	// (RecordEraseDirectory), and the file system it was on (device). An
	// erase cut off by a stop leaves a .erasing-* file there that no row may
	// lead to any more; the startup cleanup finishes it and then forgets the
	// directory, once it was there to clean (RecordingDirectories,
	// ForgetEraseDirectories). A row is kept per directory and file system,
	// so an erase on one file system does not replace the row of an erase
	// owed on another (a disk not mounted, its mount point showing the file
	// system below).
	if err := migrateEraseDirectories(s.db); err != nil {
		return err
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
	// one, as refined audio deletion records it now.
	if err := s.runOnce("record-owed-drive-replacement-folders", `
		WITH RECURSIVE owner(session_id, generation, folder_session_id) AS (
			SELECT id, deletion_generation, id FROM sessions WHERE drive_replace_pending = 1
			UNION
			SELECT owner.session_id, owner.generation, sessions.id FROM sessions JOIN owner ON sessions.merged_into = owner.folder_session_id
		)
		INSERT OR IGNORE INTO drive_replacement_folders(session_id, generation, folder_session_id, state)
		SELECT owner.session_id, owner.generation, owner.folder_session_id, '`+DriveReplacementFolderOwed+`'
		FROM owner JOIN sessions ON sessions.id = owner.folder_session_id
		WHERE sessions.gdrive_folder_id != ''
	`); err != nil {
		return fmt.Errorf("record owed drive replacement folders: %w", err)
	}
	// Finish the erase of deletions it was owed for when the app stopped. The
	// backup taken at this start holds none of their words.
	if err := s.retryOwedErasure(backupTaken); err != nil {
		slog.Warn("failed to finish an owed erase of deleted segments", "path", dbPath, "error", err)
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
		 WHERE id = ? AND status != ?`,
		endedAt.UTC().Format(time.RFC3339Nano),
		audioPath,
		audioPath,
		RefinementNoAudio,
		id,
		SessionMerged,
	)
	if err != nil {
		return fmt.Errorf("end session %s: %w", id, err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("end session rows affected: %w", err)
	}
	if rows == 0 {
		var status string
		if err := s.db.QueryRow(`SELECT status FROM sessions WHERE id = ?`, id).Scan(&status); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return sql.ErrNoRows
			}
			return fmt.Errorf("read session %s after end: %w", id, err)
		}
		if status == SessionMerged {
			return nil
		}
		return sql.ErrNoRows
	}
	return nil
}

// RefinementRecordingSource identifies a source session recorded with a
// merged session's provenance.
type RefinementRecordingSource struct {
	ID                     string
	GDriveFolderID         string
	FallbackGDriveFolderID string
	RecordingPaths         []string
}

// RefinementRecordingSources follows recording provenance through nested
// merges to the original source session that owns each recording and Drive
// folder. Without provenance, a restore must refuse rather than guess.
func (s *SQLiteStore) RefinementRecordingSources(sessionID string) ([]RefinementRecordingSource, error) {
	rows, err := s.db.Query(`WITH RECURSIVE source(recording_path, session_id, folder_session_id) AS (
			SELECT recording_path, source_session_id, source_session_id
			FROM recording_sources WHERE merged_session_id = ?
			UNION ALL
			SELECT source.recording_path, nested.source_session_id,
				CASE WHEN parent.gdrive_folder_id != '' THEN source.session_id ELSE nested.source_session_id END
			FROM source
			JOIN recording_sources AS nested
			  ON nested.merged_session_id = source.session_id
			 AND nested.recording_path = source.recording_path
			JOIN sessions AS parent ON parent.id = source.session_id
		)
		SELECT source.session_id, folders.gdrive_folder_id, leaves.gdrive_folder_id, source.recording_path
		FROM source
		JOIN sessions AS folders ON folders.id = source.folder_session_id
		JOIN sessions AS leaves ON leaves.id = source.session_id
		WHERE NOT EXISTS (
			SELECT 1 FROM recording_sources AS nested
			WHERE nested.merged_session_id = source.session_id
			  AND nested.recording_path = source.recording_path
		)
		ORDER BY folders.started_at, source.recording_path`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list recording sources for session %s: %w", sessionID, err)
	}
	defer func() { _ = rows.Close() }()
	byID := make(map[string]int)
	var sources []RefinementRecordingSource
	for rows.Next() {
		var id, folderID, fallbackFolderID, path string
		if err := rows.Scan(&id, &folderID, &fallbackFolderID, &path); err != nil {
			return nil, fmt.Errorf("scan recording source for session %s: %w", sessionID, err)
		}
		index, found := byID[id]
		if !found {
			index = len(sources)
			byID[id] = index
			sources = append(sources, RefinementRecordingSource{ID: id, GDriveFolderID: folderID, FallbackGDriveFolderID: fallbackFolderID})
		}
		sources[index].RecordingPaths = append(sources[index].RecordingPaths, path)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recording sources for session %s: %w", sessionID, err)
	}
	return sources, nil
}

// RefinementRecordingPaths returns the recordings whose content belongs to a
// session. Merged sessions retain their source paths even when their current
// audio path was cleared by garbage collection; other refined sessions retain
// paths in their current stored word occurrences.
func (s *SQLiteStore) RefinementRecordingPaths(sessionID string) ([]string, error) {
	paths, err := refinementRecordingPaths(s.db, `SELECT recording_path FROM recording_sources
		WHERE merged_session_id = ? ORDER BY recording_path`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list merged recording sources for session %s: %w", sessionID, err)
	}
	if len(paths) != 0 {
		return paths, nil
	}
	paths, err = refinementRecordingPaths(s.db, `SELECT DISTINCT recording_path FROM refined_words
		WHERE session_id = ? AND transcript_revision = (SELECT refined_revision FROM sessions WHERE id = ?)
		ORDER BY recording_path`, sessionID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list refined recording paths for session %s: %w", sessionID, err)
	}
	if len(paths) != 0 {
		return paths, nil
	}
	var merged int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE merged_into = ?`, sessionID).Scan(&merged); err != nil {
		return nil, fmt.Errorf("check merged recording provenance for session %s: %w", sessionID, err)
	}
	if merged != 0 {
		return nil, fmt.Errorf("recording provenance is unavailable for merged session %s", sessionID)
	}
	return nil, nil
}

func refinementRecordingPaths(q querier, query string, args ...any) ([]string, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return paths, nil
}

// RestoreAudioForRefinement records recordings restored from Drive so the
// normal generation-fenced refinement path can claim and process them. It
// writes only when audioPath remains the path read before the restoration.
func (s *SQLiteStore) RestoreAudioForRefinement(id, beforeAudioPath, restoredAudioPath string) (bool, error) {
	if strings.TrimSpace(restoredAudioPath) == "" {
		return false, errors.New("restored audio path is required")
	}
	res, err := s.db.Exec(`UPDATE sessions SET audio_path = ?, audio_removed_at = '', refinement_status = ? WHERE id = ? AND audio_path = ?`,
		restoredAudioPath, RefinementPending, id, beforeAudioPath)
	if err != nil {
		return false, fmt.Errorf("record restored audio for session %s: %w", id, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count restored audio session %s: %w", id, err)
	}
	return rows > 0, nil
}

// DriveRestoreAllowed reports whether restoring this session's old Drive audio
// cannot reintroduce a recording whose replacement is still owed.
func (s *SQLiteStore) DriveRestoreAllowed(id string) (bool, error) {
	var pending bool
	if err := s.db.QueryRow(`SELECT drive_replace_pending FROM sessions WHERE id = ?`, id).Scan(&pending); err != nil {
		return false, fmt.Errorf("read Drive replacement state for session %s: %w", id, err)
	}
	if pending {
		return false, nil
	}
	var owed bool
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM drive_replacement_folders WHERE session_id = ? AND state = ?)`, id, DriveReplacementFolderOwed).Scan(&owed); err != nil {
		return false, fmt.Errorf("read owed Drive replacement folders for session %s: %w", id, err)
	}
	return !owed, nil
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

//nolint:gocritic // Store's established append API passes one immutable segment by value.
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

// ErrMergeSessionActive is returned without changing rows when a source is
// still recording. A recorder must finish before its session can be merged.
var ErrMergeSessionActive = errors.New("a session to merge is still recording; end it before merging")

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
	err = tx.QueryRow(fmt.Sprintf(`SELECT id FROM sessions WHERE id IN (%s) AND NOT (%s) LIMIT 1`, strings.Join(placeholders, ","), noOwedDriveReplacement), args[1:]...).Scan(&owing)
	switch {
	case err == nil:
		return fmt.Errorf("merge session %s: %w", owing, ErrMergeReplacementPending)
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("check sources for owed Drive replacements: %w", err)
	}
	var active string
	err = tx.QueryRow(
		fmt.Sprintf(`SELECT id FROM sessions WHERE id IN (%s) AND status = ? LIMIT 1`, strings.Join(placeholders, ",")),
		append(args[1:], SessionActive)...,
	).Scan(&active)
	switch {
	case err == nil:
		return fmt.Errorf("merge session %s: %w", active, ErrMergeSessionActive)
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("check sources for active session: %w", err)
	}

	_, err = tx.Exec(
		`INSERT INTO sessions(id, started_at, ended_at, status, summary_status, refinement_status) VALUES(?, ?, ?, 'ended', 'pending', 'no_audio')`,
		newID,
		startedAt.UTC().Format(time.RFC3339Nano),
		endedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("create merged session: %w", err)
	}
	noAudioRows, err := tx.Query(
		fmt.Sprintf(`SELECT id FROM sessions WHERE id IN (%s) AND audio_path = '' AND audio_removed_at = ''`, strings.Join(placeholders, ",")),
		args[1:]...,
	)
	if err != nil {
		return fmt.Errorf("list sources without recordings: %w", err)
	}
	for noAudioRows.Next() {
		var sourceID string
		if err := noAudioRows.Scan(&sourceID); err != nil {
			_ = noAudioRows.Close()
			return fmt.Errorf("scan source without recording: %w", err)
		}
		if _, err := tx.Exec(`UPDATE segments
			SET untimed = 1,
				untimed_source_id = CASE WHEN untimed_source_id != '' THEN untimed_source_id ELSE ? END,
				transcript_revision = 0, recording_path = '', recording_sha256 = '', recording_ordinal = 0,
				first_word_ordinal = -1, last_word_ordinal = -1
			WHERE session_id = ?`, sourceID, sourceID); err != nil {
			_ = noAudioRows.Close()
			return fmt.Errorf("mark source segments untimed: %w", err)
		}
	}
	if err := errors.Join(noAudioRows.Err(), noAudioRows.Close()); err != nil {
		return fmt.Errorf("list sources without recordings: %w", err)
	}
	segQuery := fmt.Sprintf(
		`UPDATE segments SET session_id = ? WHERE session_id IN (%s)`,
		strings.Join(placeholders, ","),
	)
	if _, err := tx.Exec(segQuery, args...); err != nil {
		return fmt.Errorf("move segments: %w", err)
	}

	audioQuery := fmt.Sprintf(
		`SELECT id, audio_path FROM sessions WHERE id IN (%s) AND audio_path != '' ORDER BY started_at ASC`,
		strings.Join(placeholders, ","),
	)
	rows, err := tx.Query(audioQuery, args[1:]...)
	if err != nil {
		return fmt.Errorf("query audio paths: %w", err)
	}

	var audioPaths []string
	for rows.Next() {
		var sourceID, path string
		if err := rows.Scan(&sourceID, &path); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan audio paths: %w", err)
		}
		for _, recordingPath := range strings.Split(path, ",") {
			recordingPath = strings.TrimSpace(recordingPath)
			if recordingPath == "" {
				continue
			}
			audioPaths = append(audioPaths, recordingPath)
			if _, err := tx.Exec(`INSERT OR REPLACE INTO recording_sources(merged_session_id, recording_path, source_session_id) VALUES(?, ?, ?)`, newID, recordingPath, sourceID); err != nil {
				_ = rows.Close()
				return fmt.Errorf("record merged recording source: %w", err)
			}
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate audio paths: %w", err)
	}
	_ = rows.Close()
	removedRows, err := tx.Query(
		fmt.Sprintf(`SELECT id FROM sessions WHERE id IN (%s) AND audio_path = '' AND audio_removed_at != ''`, strings.Join(placeholders, ",")),
		args[1:]...,
	)
	if err != nil {
		return fmt.Errorf("list sources with removed recordings: %w", err)
	}
	for removedRows.Next() {
		var sourceID string
		if err := removedRows.Scan(&sourceID); err != nil {
			_ = removedRows.Close()
			return fmt.Errorf("scan source with removed recording: %w", err)
		}
		paths, err := refinementRecordingPaths(tx, `SELECT recording_path FROM recording_sources
			WHERE merged_session_id = ? ORDER BY recording_path`, sourceID)
		if err != nil {
			_ = removedRows.Close()
			return fmt.Errorf("list inherited recordings for source %s: %w", sourceID, err)
		}
		if len(paths) == 0 {
			paths, err = refinementRecordingPaths(tx, `SELECT DISTINCT recording_path FROM refined_words
				WHERE session_id = ? AND transcript_revision = (SELECT refined_revision FROM sessions WHERE id = ?)
				ORDER BY recording_path`, sourceID, sourceID)
			if err != nil {
				_ = removedRows.Close()
				return fmt.Errorf("list removed recordings for source %s: %w", sourceID, err)
			}
		}
		if len(paths) == 0 {
			paths = []string{sourceID + ".mp3"}
		}
		for _, recordingPath := range paths {
			if _, err := tx.Exec(`INSERT OR REPLACE INTO recording_sources(merged_session_id, recording_path, source_session_id) VALUES(?, ?, ?)`, newID, recordingPath, sourceID); err != nil {
				_ = removedRows.Close()
				return fmt.Errorf("record removed recording source: %w", err)
			}
		}
	}
	if err := errors.Join(removedRows.Err(), removedRows.Close()); err != nil {
		return fmt.Errorf("list sources with removed recordings: %w", err)
	}
	var audioRemovedAt string
	if err := tx.QueryRow(
		fmt.Sprintf(`SELECT COALESCE(MAX(audio_removed_at), '') FROM sessions WHERE id IN (%s)`, strings.Join(placeholders, ",")),
		args[1:]...,
	).Scan(&audioRemovedAt); err != nil {
		return fmt.Errorf("read source audio removal: %w", err)
	}
	if audioRemovedAt != "" {
		if _, err := tx.Exec(`UPDATE sessions SET audio_removed_at = ? WHERE id = ?`, audioRemovedAt, newID); err != nil {
			return fmt.Errorf("record removed source audio: %w", err)
		}
	}

	if len(audioPaths) > 0 {
		_, err = tx.Exec(`UPDATE sessions SET audio_path = ?, refinement_status = 'pending' WHERE id = ?`, strings.Join(audioPaths, ","), newID)
		if err != nil {
			return fmt.Errorf("set merged audio paths: %w", err)
		}
	}

	// The merged session now has the sources' recordings: one session names
	// each file, so removing the merged session's audio leaves no source
	// naming a file that is gone.
	markQuery := fmt.Sprintf(
		`UPDATE sessions
		 SET status = 'merged', merged_into = ?, audio_path = '', deletion_generation = deletion_generation + 1
		 WHERE id IN (%s)`,
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
	query := `SELECT id, title, started_at, ended_at, status, summary, summary_model, summary_version, summary_status, summary_preset, speaker_names, refined_transcript, refinement_status, refinement_model, refined_revision, audio_path, sync_status, envoy_publish_status, sync_state, retry_count, last_sync_attempt, error_message, gdrive_folder_id, merged_into, canonical_transcript, transcript_source, audio_removed_at, restored_at, summary_error, drive_replace_pending
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
		`SELECT id, title, started_at, ended_at, status, summary, summary_model, summary_version, summary_status, summary_preset, speaker_names, refined_transcript, refinement_status, refinement_model, refined_revision, audio_path, sync_status, envoy_publish_status, sync_state, retry_count, last_sync_attempt, error_message, gdrive_folder_id, merged_into, canonical_transcript, transcript_source, audio_removed_at, restored_at, summary_error, drive_replace_pending
		 FROM sessions WHERE (title = '' OR title IS NULL) AND status NOT IN ('discarded', 'merged') ORDER BY started_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("query sessions with empty titles: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return scanSessions(rows)
}

// migrateEraseDirectories creates erase_directories keyed on the directory
// and its file system, and rebuilds, keeping its rows, one an earlier build
// keyed on the directory alone. That build recorded a directory as the
// caller named it, so a relative one (a relative audio_dir) is made absolute
// from the working directory, which the service fixes; RecordEraseDirectory
// now records only absolute paths. Every erase_directories table has the
// device column: none was published without it (no release holds the table
// at all, and main does not have it), so no ALTER adds it.
func migrateEraseDirectories(db *sql.DB) error {
	var keyColumns int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('erase_directories') WHERE pk > 0`).Scan(&keyColumns); err != nil {
		return fmt.Errorf("read the erase_directories key: %w", err)
	}
	if keyColumns == 2 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin keying erase_directories on the file system: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	type eraseDirectory struct {
		path   string
		device int64
	}
	var earlier []eraseDirectory
	if keyColumns == 1 {
		rows, err := tx.Query(`SELECT path, device FROM erase_directories`)
		if err != nil {
			return fmt.Errorf("read the earlier erase directories: %w", err)
		}
		for rows.Next() {
			var dir eraseDirectory
			if err := rows.Scan(&dir.path, &dir.device); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan an earlier erase directory: %w", err)
			}
			if dir.path, err = filepath.Abs(dir.path); err != nil {
				_ = rows.Close()
				return fmt.Errorf("make an earlier erase directory absolute: %w", err)
			}
			earlier = append(earlier, dir)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return fmt.Errorf("read the earlier erase directories: %w", err)
		}
		if _, err := tx.Exec(`DROP TABLE erase_directories`); err != nil {
			return fmt.Errorf("drop the earlier erase directories: %w", err)
		}
	}
	if _, err := tx.Exec(`CREATE TABLE erase_directories (path TEXT NOT NULL, device INTEGER NOT NULL, PRIMARY KEY (path, device))`); err != nil {
		return fmt.Errorf("create erase_directories table: %w", err)
	}
	for _, dir := range earlier {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO erase_directories(path, device) VALUES(?, ?)`, dir.path, dir.device); err != nil {
			return fmt.Errorf("keep the earlier erase directory %s: %w", dir.path, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit keying erase_directories on the file system: %w", err)
	}
	return nil
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
		`SELECT id, title, started_at, ended_at, status, summary, summary_model, summary_version, summary_status, summary_preset, speaker_names, refined_transcript, refinement_status, refinement_model, refined_revision, audio_path, sync_status, envoy_publish_status, sync_state, retry_count, last_sync_attempt, error_message, gdrive_folder_id, merged_into, canonical_transcript, transcript_source, audio_removed_at, restored_at, summary_error, drive_replace_pending FROM sessions WHERE id = ?`,
		id,
	)

	var sess Session
	var startedAt string
	var endedAt sql.NullString
	var lastSyncAttempt sql.NullString
	if err := row.Scan(&sess.ID, &sess.Title, &startedAt, &endedAt, &sess.Status, &sess.Summary, &sess.SummaryModel, &sess.SummaryVersion, &sess.SummaryStatus, &sess.SummaryPreset, &sess.SpeakerNames, &sess.RefinedTranscript, &sess.RefinementStatus, &sess.RefinementModel, &sess.RefinedRevision, &sess.AudioPath, &sess.SyncStatus, &sess.EnvoyPublishStatus, &sess.SyncState, &sess.RetryCount, &lastSyncAttempt, &sess.ErrorMessage, &sess.GDriveFolderID, &sess.MergedInto, &sess.CanonicalTranscript, &sess.TranscriptSource, &sess.AudioRemovedAt, &sess.RestoredAt, &sess.SummaryError, &sess.DriveReplacePending); err != nil {
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
	session, err := s.GetSession(sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return []transcribe.Segment{}, nil
	}
	if err != nil {
		return nil, err
	}
	if session.Status == SessionMerged {
		return []transcribe.Segment{}, nil
	}
	return getSegments(s.db, sessionID)
}

func getSegments(q querier, sessionID string) ([]transcribe.Segment, error) {
	rows, err := q.Query(
		`SELECT id, speaker, text, start_time, end_time, timestamp,
		        transcript_revision, recording_path, recording_sha256, recording_ordinal,
		        first_word_ordinal, last_word_ordinal, untimed, untimed_source_id
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
		if err := rows.Scan(&seg.ID, &seg.Speaker, &seg.Text, &seg.StartTime, &seg.EndTime, &ts,
			&seg.TranscriptRevision, &seg.RecordingPath, &seg.RecordingSHA256, &seg.RecordingOrdinal,
			&seg.FirstWordOrdinal, &seg.LastWordOrdinal, &seg.Untimed, &seg.UntimedSourceID); err != nil {
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
	session, err := s.GetSession(sessionID)
	if err != nil {
		return nil, err
	}
	if session.Status == SessionMerged {
		return []transcribe.Segment{}, nil
	}
	rows, err := s.db.Query(
		`SELECT id, speaker, text, start_time, end_time, timestamp,
		        transcript_revision, recording_path, recording_sha256, recording_ordinal,
		        first_word_ordinal, last_word_ordinal, untimed, untimed_source_id
		 FROM segments
		 WHERE session_id = ? AND start_time >= ? AND end_time <= ? AND untimed = 0
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
		if err := rows.Scan(&seg.ID, &seg.Speaker, &seg.Text, &seg.StartTime, &seg.EndTime, &ts,
			&seg.TranscriptRevision, &seg.RecordingPath, &seg.RecordingSHA256, &seg.RecordingOrdinal,
			&seg.FirstWordOrdinal, &seg.LastWordOrdinal, &seg.Untimed, &seg.UntimedSourceID); err != nil {
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

// UpdateSummaryAtGeneration stores session id's summary, its status, preset,
// speaker names and the model its provider reported, and its title unless
// title is "", only while generation, the transcript generation the writer
// read with the text it summarized, is still the session's and the
// session's summary run started at it. Otherwise it stores nothing and
// returns sql.ErrNoRows.
//
// An empty summary never replaces the one the session has: its summary,
// preset, speaker names and model stay (a session with none keeps none). A
// completed summary clears the error of the attempt before. Starting a
// summary and recording one that failed are StartSummary and
// FailSummaryAtGeneration, which keep the last good summary.
//
// A changed completed summary not started by StartSummary (a batch result)
// gets a new summary version when the session's current version may already
// have been sent to Envoy, so Envoy does not drop it as a duplicate delivery.
func (s *SQLiteStore) UpdateSummaryAtGeneration(sessionID, title, summary, status, preset, speakerNames, model string, generation int) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin summary update for session %s: %w", sessionID, err)
	}
	defer func() { _ = tx.Rollback() }()

	current, err := getSession(tx, sessionID)
	if err != nil {
		return err
	}
	if current.Status == SessionMerged {
		return sql.ErrNoRows
	}
	bumpVersion := summaryVersionChanges(&current, title, summary, status, preset, speakerNames, model)
	res, err := tx.Exec(
		`UPDATE sessions
		 SET title = CASE WHEN ? != '' THEN ? ELSE title END,
		     summary_preset = CASE WHEN ? != '' THEN ? ELSE summary_preset END,
		     speaker_names = CASE WHEN ? != '' THEN ? ELSE speaker_names END,
		     summary = CASE WHEN ? != '' THEN ? ELSE summary END,
		     summary_model = CASE WHEN ? != '' AND ? != '' THEN ? ELSE summary_model END,
		     summary_version = CASE WHEN ? THEN summary_version + 1 ELSE summary_version END,
		     envoy_publish_status = CASE WHEN ? THEN ? ELSE envoy_publish_status END,
		     summary_status = ?,
		     summary_error = CASE WHEN ? = ? THEN '' ELSE summary_error END
		 WHERE id = ? AND summary_generation = deletion_generation AND deletion_generation = ? AND status != ?`,
		title, title,
		summary, preset,
		summary, speakerNames,
		summary, summary,
		summary, model, model,
		bumpVersion,
		bumpVersion, EnvoyPublishPending,
		status,
		status, SummaryCompleted,
		sessionID,
		generation,
		SessionMerged,
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

// GetSessionWithGeneration reads session id and its transcript generation in
// one transaction, so a writer that derives a summary from the session can
// store it only at the generation of the text it read
// (UpdateSummaryAtGeneration).
func (s *SQLiteStore) GetSessionWithGeneration(id string) (Session, int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Session{}, 0, fmt.Errorf("begin read of session %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	sess, err := getSession(tx, id)
	if err != nil {
		return Session{}, 0, err
	}
	var generation int
	if err := tx.QueryRow(`SELECT deletion_generation FROM sessions WHERE id = ?`, id).Scan(&generation); err != nil {
		return Session{}, 0, fmt.Errorf("read deletion generation for session %s: %w", id, err)
	}
	return sess, generation, nil
}

func summaryVersionChanges(current *Session, title, summary, status, preset, speakerNames, model string) bool {
	if status != SummaryCompleted || current.SummaryStatus == SummaryRunning {
		return false
	}
	if current.SummaryStatus != SummaryCompleted && current.Summary == "" && current.EnvoyPublishStatus == EnvoyPublishPending {
		return false
	}
	if title != "" && title != current.Title {
		return true
	}
	// An empty summary keeps the summary the session has, and with it the
	// model that made it.
	if summary != "" && model != "" && model != current.SummaryModel {
		return true
	}
	return summary != "" && (summary != current.Summary || preset != current.SummaryPreset || speakerNames != current.SpeakerNames)
}

// StartSummary marks session id's summary running, and not yet published to
// Envoy. Its title and summary stay as they are until a new summary is
// stored (UpdateSummaryAtGeneration): a summary that fails, or never ends,
// takes nothing away. The session gets a new summary version when its
// current one may already have been sent to Envoy: it is or was completed,
// or Envoy publishing
// of it was attempted.
func (s *SQLiteStore) StartSummary(id string) error {
	res, err := s.db.Exec(`UPDATE sessions SET summary_status = ?, envoy_publish_status = ?, summary_generation = deletion_generation, summary_version = CASE WHEN summary_status = ? OR summary != '' OR envoy_publish_status != ? THEN summary_version + 1 ELSE summary_version END WHERE id = ? AND status != ?`,
		SummaryRunning, EnvoyPublishPending, SummaryCompleted, EnvoyPublishPending, id, SessionMerged)
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
	res, err := s.db.Exec(`UPDATE sessions SET summary_status = ?, summary_generation = deletion_generation WHERE id = ? AND status != ?`, SummaryRunning, id, SessionMerged)
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

// StartEmptySummaryAtGeneration marks session id's empty summary running only
// while generation is still current and no completed summary owns it. A stale
// empty writer must leave a newer completion alone.
func (s *SQLiteStore) StartEmptySummaryAtGeneration(id string, generation int) error {
	res, err := s.db.Exec(`UPDATE sessions
		SET summary_status = ?, summary_generation = deletion_generation
		WHERE id = ? AND deletion_generation = ? AND summary_status IN (?, ?, ?) AND status != ?`,
		SummaryRunning, id, generation, SummaryPending, SummaryFailed, SummaryRunning, SessionMerged)
	if err != nil {
		return fmt.Errorf("start empty summary of session %s at generation %d: %w", id, generation, err)
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

// FailSummaryAtGeneration records that making session id's summary failed,
// and why (errMsg, in summary_error: error_message keeps the last sync's
// error), and returns the session as it then is, only while generation, the
// transcript generation the summary run read, is still the session's and the
// run started at it; otherwise it records nothing and returns sql.ErrNoRows.
// The session keeps its title and its last good summary (with its preset and
// speaker names), the ones Drive holds: a failed summary takes nothing away.
func (s *SQLiteStore) FailSummaryAtGeneration(id, errMsg string, generation int) (Session, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Session{}, fmt.Errorf("fail summary of session %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`UPDATE sessions SET summary_status = ?, summary_error = ? WHERE id = ? AND summary_generation = deletion_generation AND deletion_generation = ? AND status != ?`, SummaryFailed, errMsg, id, generation, SessionMerged)
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

// UpdateRefinementAtGeneration atomically stores a refinement and the model
// reported by its provider, only while generation, the transcript generation
// ClaimRefinement returned, is still the session's. A refinement of the
// recording from before a segment deletion is refused (sql.ErrNoRows), also
// once the deletion's own refinement has claimed the session.
func (s *SQLiteStore) UpdateRefinementAtGeneration(sessionID, transcript, status, model string, generation int) error {
	res, err := s.db.Exec(
		`UPDATE sessions
		 SET refined_transcript = ?, refinement_status = ?,
		     refinement_model = CASE WHEN ? != '' THEN ? ELSE refinement_model END
		 WHERE id = ? AND refinement_generation = deletion_generation AND deletion_generation = ? AND status != ?`,
		transcript,
		status,
		model, model,
		sessionID,
		generation,
		SessionMerged,
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
// refined transcript and its model, only while the session still names
// audioPath, the recording the refinement will read. Disk pressure changes
// the audio path before it removes a file (MarkAudioRemoved), so once GC has
// taken the recording this reports false and the refinement must not start;
// and GC takes nothing from a session whose refinement is running. It returns
// the transcript generation it claimed at, the one the refinement stores its
// result at (UpdateRefinementAtGeneration).
func (s *SQLiteStore) ClaimRefinement(id, audioPath string) (generation int, claimed bool, err error) {
	err = s.db.QueryRow(
		`UPDATE sessions SET refined_transcript = '', refinement_status = ?, refinement_model = '', refinement_generation = deletion_generation WHERE id = ? AND audio_path = ? AND audio_path != '' AND status != ? RETURNING refinement_generation`,
		RefinementRunning, id, audioPath, SessionMerged,
	).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("claim refinement of session %s: %w", id, err)
	}
	return generation, true, nil
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
// that writes it. With the store's single connection, refined deletion cannot
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
		     retry_count = 0, last_sync_attempt = NULL, error_message = '', sync_claim = '', sync_claimed_from = '', documents_unverified = 0, drive_documents_pending = 0
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
	if err := oweFolderOfPendingDeletion(tx, id); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit marking session %s synced: %w", id, err)
	}
	return true, nil
}

// RecordSyncFolder records a folder just created for the sync claim that
// names it, before that sync uploads its first file. A stopped process then
// leaves a retry with the folder to reuse. When a segment deletion is pending,
// the same transaction puts the folder in its replacement ledger.
func (s *SQLiteStore) RecordSyncFolder(id, claim, driveFolderID string) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("begin recording Drive folder of session %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`UPDATE sessions SET gdrive_folder_id = ?
		WHERE id = ? AND sync_state = ? AND sync_claim = ?`,
		driveFolderID, id, SyncStateSyncing, claim)
	if err != nil {
		return false, fmt.Errorf("record Drive folder of session %s: %w", id, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count recorded Drive folder of session %s: %w", id, err)
	}
	if rows == 0 {
		return false, nil
	}
	if err := oweFolderOfPendingDeletion(tx, id); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit Drive folder of session %s: %w", id, err)
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
// the session while a refined segment was deleted uploaded the words before
// the deletion; the folder it made is recorded as owed to that deletion's
// replacement in the same transaction.
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
	if err := oweFolderOfPendingDeletion(tx, id); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit the end of the sync of session %s: %w", id, err)
	}
	return true, nil
}

// oweFolderOfPendingDeletion records, in tx, the Drive folder a sync just
// recorded for session id as owed to every pending refined deletion that
// contains that session, directly or through a merge. A sync that held either
// the owner or a merged source while a segment was deleted uploaded the words
// before the deletion. A sync that made no folder leaves nothing to owe, and
// its deletion is settled only once no folder can hold the words.
func oweFolderOfPendingDeletion(tx *sql.Tx, id string) error {
	if _, err := tx.Exec(`WITH RECURSIVE owners(id) AS (
			SELECT ?
			UNION SELECT source.merged_into FROM sessions AS source JOIN owners ON source.id = owners.id WHERE source.merged_into != ''
		) INSERT OR IGNORE INTO drive_replacement_folders(session_id, generation, folder_session_id, state)
		SELECT owner.id, owner.deletion_generation, ?, ?
		FROM sessions AS owner JOIN owners ON owner.id = owners.id
		WHERE owner.drive_replace_pending = 1
		  AND EXISTS (SELECT 1 FROM sessions AS folder WHERE folder.id = ? AND folder.gdrive_folder_id != '')`,
		id, id, DriveReplacementFolderOwed, id); err != nil {
		return fmt.Errorf("owe the Drive folder of session %s its replacement: %w", id, err)
	}
	return settleUnownedDriveReplacement(tx, id)
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

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan recovered sync: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("recover interrupted syncs: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close recovered syncs: %w", err)
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
		if err := rows.Scan(&sess.ID, &sess.Title, &startedAt, &endedAt, &sess.Status, &sess.Summary, &sess.SummaryModel, &sess.SummaryVersion, &sess.SummaryStatus, &sess.SummaryPreset, &sess.SpeakerNames, &sess.RefinedTranscript, &sess.RefinementStatus, &sess.RefinementModel, &sess.RefinedRevision, &sess.AudioPath, &sess.SyncStatus, &sess.EnvoyPublishStatus, &sess.SyncState, &sess.RetryCount, &lastSyncAttempt, &sess.ErrorMessage, &sess.GDriveFolderID, &sess.MergedInto, &sess.CanonicalTranscript, &sess.TranscriptSource, &sess.AudioRemovedAt, &sess.RestoredAt, &sess.SummaryError, &sess.DriveReplacePending); err != nil {
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
// ended sessions waiting for a sync, every session that still owes Drive
// a replacement after a segment deletion and that no sync holds, whatever its
// status (ended, merged, or discarded: the consent to delete covers them all)
// once it is no longer recording, and every merged source whose own Drive
// folder still holds documents "Delete text" changed (DriveDocumentsOwed).
// An ordinary sync leaves a FAILED or REMOTE_DELETED session to a manual
// retry, and syncs no merged or discarded one; a replacement asks Drive
// whether a deleted folder is back each time, so a deletion never waits on a
// click. (A scheduled retry is read separately, by GetSessionsBySyncState,
// when due.)
func (s *SQLiteStore) GetSessionsNeedingSync() ([]string, error) {
	rows, err := s.db.Query(
		`SELECT id FROM sessions WHERE (status = 'ended' AND sync_state = ?)
		 OR (drive_replace_pending = 1 AND status != ? AND sync_state != ?)
		 OR (status = ? AND drive_documents_pending = 1 AND gdrive_folder_id != '')`,
		SyncStatePending, SessionActive, SyncStateSyncing, SessionMerged,
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
		`SELECT id, title, started_at, ended_at, status, summary, summary_model, summary_version, summary_status, summary_preset, speaker_names, refined_transcript, refinement_status, refinement_model, refined_revision, audio_path, sync_status, envoy_publish_status, sync_state, retry_count, last_sync_attempt, error_message, gdrive_folder_id, merged_into, canonical_transcript, transcript_source, audio_removed_at, restored_at, summary_error, drive_replace_pending
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
// first: ended, with a completed summary, synced, with documents not marked
// unverified on Drive (documents_unverified), never one whose Drive folder is
// in the trash or gone (REMOTE_DELETED), and older than maxAgeDays days. A
// session's age runs from when it started or, if it was restored from Drive,
// from its restore, so the pass after a restore keeps it. maxAgeDays <= 0 is
// no age limit: it returns none.
func (s *SQLiteStore) GetGCEligibleSessions(maxAgeDays int) ([]string, error) {
	cutoff, ok := ageCutoff(time.Now().UTC(), maxAgeDays)
	if !ok {
		return nil, nil
	}
	at := cutoff.Format(time.RFC3339Nano)
	// restored_at is '' for a session recorded here, which is before any time.
	// A session whose documents are unverified on Drive is never deleted.
	rows, err := s.db.Query(
		`SELECT id FROM sessions WHERE summary_status = 'completed' AND started_at < ? AND restored_at < ? AND documents_unverified = 0 AND `+removableCondition+` ORDER BY started_at ASC`,
		at, at,
	)
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

// removableCondition identifies a session whose recording GC may remove under
// disk pressure or with the session under the age policy (which also requires
// documents_unverified = 0): it ended, no refinement is reading the file, it
// owes Drive no replacement after a segment deletion and its folder is owed
// none (noOwedDriveReplacement: the replacement needs the row and the spliced
// recording), no segment deletion's splice of its recording is unfinished,
// its Drive folder is not in the trash or gone (REMOTE_DELETED: the device
// then holds its only lasting copy), and it is synced. A session whose
// documents are unverified on Drive (synced by an earlier version) has its
// audio on Drive: GC removes it once Drive answers that the session's folder
// is there. Any other sync state, a failure included, keeps the recording.
const removableCondition = `status = 'ended'` +
	` AND refinement_status != '` + RefinementRunning + `'` +
	` AND sync_state != '` + SyncStateRemoteDeleted + `'` +
	` AND sync_status = '` + SyncSynced + `'` +
	` AND ` + noOwedDriveReplacement +
	` AND NOT EXISTS (SELECT 1 FROM deleted_segments WHERE deleted_segments.session_id = sessions.id AND splice_complete = 0)`

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

// CountUnitOnlySessions returns how many sessions are kept on this unit
// only: REMOTE_DELETED (their Drive folder is in the trash, gone, or cannot
// be read) with their audio still named here, for the status the app
// reports. No GC path deletes such a session or removes its audio.
func (s *SQLiteStore) CountUnitOnlySessions() (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM sessions WHERE sync_state = ? AND audio_path != ''`, SyncStateRemoteDeleted).Scan(&n); err != nil {
		return 0, fmt.Errorf("count sessions kept on this unit only: %w", err)
	}
	return n, nil
}

// Recordings lists, oldest first, every session that names audio, and whether
// disk pressure may remove its recording (see removableCondition).
func (s *SQLiteStore) Recordings() ([]SessionRecording, error) {
	rows, err := s.db.Query(`SELECT id, audio_path, (` + removableCondition + `) FROM sessions WHERE audio_path != '' ORDER BY started_at ASC`)
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
// path before and after, and its marker and refinement status before, so
// RestoreAudio can put back the part whose files stayed on disk.
type AudioRemoval struct {
	ID               string
	Before, Left     string
	RemovedAtBefore  string
	RefinementBefore string
}

// MarkAudioRemoved records that disk pressure is about to remove part or all
// of session id's audio, before it removes any file, so the session never
// names a file GC has removed. The audio path becomes left, what will be left
// of before; the marker the UI shows is set to removedAt; and a pending or
// failed refinement becomes no_audio when no recording is left on disk
// (audioLeft false; left may still name a file GC could not see, on a disk not
// mounted for the moment). It does this only while the session's audio path is
// still before and disk pressure may still remove it (see removableCondition),
// so a refinement that started reading the file since GC listed it keeps it.
// It reports whether it did, and returns the row as it was, for RestoreAudio.
func (s *SQLiteStore) MarkAudioRemoved(id, before, left string, audioLeft bool, removedAt time.Time) (AudioRemoval, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return AudioRemoval{}, false, fmt.Errorf("begin marking audio removed for session %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()

	removal := AudioRemoval{ID: id, Before: before, Left: left}
	err = tx.QueryRow(
		`SELECT audio_removed_at, refinement_status FROM sessions WHERE id = ? AND audio_path = ? AND `+removableCondition,
		id, before,
	).Scan(&removal.RemovedAtBefore, &removal.RefinementBefore)
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
// refinement the mark moved to no_audio goes back. A sync may have finished
// since the mark and uploaded the row as marked, so the recordings put back
// may not be on Drive: a synced session goes back to pending sync, keeping its
// folder. GC marks only synced sessions, so the state before the mark cannot
// tell whether a sync ran since. It refuses a row whose audio path is no
// longer what the mark left.
func (s *SQLiteStore) RestoreAudio(removal *AudioRemoval, restored string) error {
	res, err := s.db.Exec(
		`UPDATE sessions SET audio_path = ?,
			audio_removed_at = CASE WHEN ? = ? THEN ? ELSE audio_removed_at END,
			refinement_status = CASE WHEN ? != '' AND refinement_status = ? THEN ? ELSE refinement_status END,
			sync_status = CASE WHEN sync_state = ? THEN ? ELSE sync_status END,
			sync_state = CASE WHEN sync_state = ? THEN ? ELSE sync_state END
		 WHERE id = ? AND audio_path = ? AND status = 'ended'`,
		restored,
		restored, removal.Before, removal.RemovedAtBefore,
		restored, RefinementNoAudio, removal.RefinementBefore,
		SyncStateSynced, SyncPending,
		SyncStateSynced, SyncStatePending,
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
// nothing, for a session or source that still owes Google Drive a replacement
// after a segment deletion, or whose Drive folder one still owes: with its row
// would go the replacement, its warning, or the folder the replacement must
// reach, and the Drive folder would keep the deleted words.
var ErrDeleteReplacementPending = errors.New("this session still has a deleted segment on Google Drive; delete it after its Drive files are replaced")

// ErrDeleteMergeActive is the error DeleteSession returns when the target is
// already part of a later merge, or a source it would recursively delete is
// active. The later merge or recorder would lose its source.
var ErrDeleteMergeActive = errors.New("this session is part of another merge or has an active merged source")

// GCSessionBlockError identifies the session that prevents an age GC deletion.
type GCSessionBlockError struct {
	BlockingSessionID       string
	BlockingSessionIsTarget bool
	Reason                  string
}

func (e *GCSessionBlockError) Error() string {
	role := "source"
	if e.BlockingSessionIsTarget {
		role = "target"
	}
	return fmt.Sprintf("%s session %s blocks age GC: %s", role, e.BlockingSessionID, e.Reason)
}

// noOwedDriveReplacement is the condition, on a sessions row, that it owes
// Google Drive no replacement and its folder is owed none. GC
// (removableCondition) and MergeSessions check it; DeleteSession applies it
// to its full merged-source set.
const noOwedDriveReplacement = `sessions.drive_replace_pending = 0 AND NOT EXISTS (
	SELECT 1 FROM drive_replacement_folders
	WHERE drive_replacement_folders.folder_session_id = sessions.id AND drive_replacement_folders.state = '` + DriveReplacementFolderOwed + `')`

// RawEraseReservation is a raw microphone capture hidden before a session
// delete commits. It stays present when startup cannot restore the capture.
type RawEraseReservation struct {
	SessionID    string
	OriginalPath string
	HiddenPath   string
}

// ReserveRawSessionErase records the raw microphone capture's two names
// before the API moves it to the hidden one. The record commits before the
// rename, so startup can restore a session whose delete did not commit. A
// retry reuses the earlier hidden name instead of stranding it.
func (s *SQLiteStore) ReserveRawSessionErase(id, originalPath, hiddenPath string) (string, error) {
	originalPath, err := filepath.Abs(originalPath)
	if err != nil {
		return "", fmt.Errorf("find raw erase path for session %s: %w", id, err)
	}
	hiddenPath, err = filepath.Abs(hiddenPath)
	if err != nil {
		return "", fmt.Errorf("find hidden raw erase path for session %s: %w", id, err)
	}
	if filepath.Dir(originalPath) != filepath.Dir(hiddenPath) {
		return "", fmt.Errorf("reserve raw erase for session %s: paths are in different directories", id)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", fmt.Errorf("begin reserving raw erase for session %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	var exists int
	if err := tx.QueryRow(`SELECT 1 FROM sessions WHERE id = ?`, id).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", sql.ErrNoRows
		}
		return "", fmt.Errorf("read session %s for raw erase reservation: %w", id, err)
	}
	if err := recordEraseDirectory(tx, filepath.Dir(originalPath)); err != nil {
		return "", fmt.Errorf("reserve raw erase for session %s: %w", id, err)
	}
	var persistedHiddenPath string
	if err := tx.QueryRow(`INSERT INTO raw_erase_reservations(session_id, original_path, hidden_path)
		VALUES(?, ?, ?)
		ON CONFLICT(session_id, original_path) DO UPDATE SET session_id = excluded.session_id
		RETURNING hidden_path`,
		id, originalPath, hiddenPath).Scan(&persistedHiddenPath); err != nil {
		return "", fmt.Errorf("record raw erase reservation for session %s: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit raw erase reservation for session %s: %w", id, err)
	}
	return persistedHiddenPath, nil
}

// RawEraseReservations lists raw captures still protected from automatic
// zeroing. Older databases have none until this version creates the table.
func (s *SQLiteStore) RawEraseReservations() ([]RawEraseReservation, error) {
	rows, err := s.db.Query(`SELECT session_id, original_path, hidden_path FROM raw_erase_reservations`)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil, nil
		}
		return nil, fmt.Errorf("list raw erase reservations: %w", err)
	}
	var reservations []RawEraseReservation
	for rows.Next() {
		var reservation RawEraseReservation
		if err := rows.Scan(&reservation.SessionID, &reservation.OriginalPath, &reservation.HiddenPath); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan raw erase reservation: %w", err)
		}
		reservations = append(reservations, reservation)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("list raw erase reservations: %w", err)
	}
	return reservations, nil
}

// ReservedRawErasePaths returns hidden raw paths that no automatic zeroing
// pass may touch while their reservation remains.
func (s *SQLiteStore) ReservedRawErasePaths() (map[string]bool, error) {
	reservations, err := s.RawEraseReservations()
	if err != nil {
		return nil, err
	}
	paths := make(map[string]bool, len(reservations))
	for _, reservation := range reservations {
		paths[reservation.HiddenPath] = true
	}
	return paths, nil
}

// CancelRawSessionErase forgets a reservation whose delete did not continue.
// A restore that fails deliberately keeps its reservation for startup to
// recover rather than allowing Leftovers to zero a live session's raw file.
func (s *SQLiteStore) CancelRawSessionErase(id, hiddenPath string) error {
	hiddenPath, err := filepath.Abs(hiddenPath)
	if err != nil {
		return fmt.Errorf("find hidden raw erase path for session %s: %w", id, err)
	}
	if _, err := s.db.Exec(`DELETE FROM raw_erase_reservations WHERE session_id = ? AND hidden_path = ?`, id, hiddenPath); err != nil {
		return fmt.Errorf("cancel raw erase reservation for session %s: %w", id, err)
	}
	return nil
}

// restoreRawEraseReservations makes a raw recording visible again when its
// session survived the delete that hid it. It must run before any
// erase.Leftovers pass. A failed restore stays reserved, so the app starts
// and every later zeroing pass skips that file until a delete retry resolves
// it.
func (s *SQLiteStore) restoreRawEraseReservations() error {
	reservations, err := s.RawEraseReservations()
	if err != nil {
		return err
	}
	for _, reservation := range reservations {
		var exists int
		err := s.db.QueryRow(`SELECT 1 FROM sessions WHERE id = ?`, reservation.SessionID).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read session %s for raw erase recovery: %w", reservation.SessionID, err)
		}
		hidden, err := os.Lstat(reservation.HiddenPath)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			slog.Warn("could not read a hidden raw recording during startup recovery", "session_id", reservation.SessionID, "hidden_path", reservation.HiddenPath, "error", err)
			continue
		case !hidden.Mode().IsRegular():
			slog.Warn("hidden raw recording is not a regular file during startup recovery", "session_id", reservation.SessionID, "hidden_path", reservation.HiddenPath)
			continue
		default:
			_, originalErr := os.Lstat(reservation.OriginalPath)
			switch {
			case errors.Is(originalErr, os.ErrNotExist):
				if err := erase.Restore(reservation.OriginalPath, reservation.HiddenPath); err != nil {
					slog.Warn("could not restore a raw recording during startup recovery", "session_id", reservation.SessionID, "original_path", reservation.OriginalPath, "hidden_path", reservation.HiddenPath, "error", err)
					continue
				}
			case originalErr != nil:
				slog.Warn("could not read a raw recording destination during startup recovery", "session_id", reservation.SessionID, "original_path", reservation.OriginalPath, "error", originalErr)
				continue
			}
		}
		if err := s.CancelRawSessionErase(reservation.SessionID, reservation.HiddenPath); err != nil {
			slog.Warn("could not clear a recovered raw erase reservation", "session_id", reservation.SessionID, "hidden_path", reservation.HiddenPath, "error", err)
		}
	}
	if _, err := s.db.Exec(`DELETE FROM raw_erase_reservations
		WHERE NOT EXISTS (SELECT 1 FROM sessions WHERE sessions.id = raw_erase_reservations.session_id)`); err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil
		}
		return fmt.Errorf("discard deleted raw erase reservations: %w", err)
	}
	return nil
}

// DeleteSession deletes session id and its merged-source set, refusing a target
// that feeds another merge (ErrDeleteMergeActive), any active member, or any
// member that owes Google Drive a replacement
// (ErrDeleteReplacementPending). It returns the target row's audio path: the
// recordings the caller then erases are the ones the row named when it went,
// not ones it read before. In the same transaction, before the rows go, it
// records the directory of each of those recordings, found where
// recording.Resolve finds it in audioDir (the one the caller then erases), so
// an erase a stop cuts off is finished as the app starts; it deletes nothing
// if it cannot record one.
func (s *SQLiteStore) DeleteSession(id, audioDir string) (string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", fmt.Errorf("begin deleting session %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	var audioPath string
	switch err := tx.QueryRow(`WITH RECURSIVE doomed(id) AS (
			SELECT ?
			UNION SELECT source.id FROM sessions AS source JOIN doomed ON source.merged_into = doomed.id
		) SELECT target.audio_path
		FROM sessions AS target
		WHERE target.id = ?
		  AND (target.merged_into = '' OR NOT EXISTS (
			SELECT 1 FROM sessions AS parent WHERE parent.id = target.merged_into
		  ))
		  AND NOT EXISTS (
			SELECT 1 FROM sessions AS member JOIN doomed ON member.id = doomed.id
			WHERE member.status = ?
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM sessions AS member JOIN doomed ON member.id = doomed.id
			WHERE member.drive_replace_pending = 1
			   OR EXISTS (
				SELECT 1 FROM drive_replacement_folders
				WHERE drive_replacement_folders.folder_session_id = member.id
				  AND drive_replacement_folders.state = ?
			   )
		  )`,
		id, id, SessionActive, DriveReplacementFolderOwed).Scan(&audioPath); {
	case errors.Is(err, sql.ErrNoRows):
		var exists int
		switch err := tx.QueryRow(`SELECT 1 FROM sessions WHERE id = ?`, id).Scan(&exists); {
		case errors.Is(err, sql.ErrNoRows):
			return "", sql.ErrNoRows
		case err != nil:
			return "", fmt.Errorf("read session %s to delete it: %w", id, err)
		}
		var unsafe int
		err := tx.QueryRow(`WITH RECURSIVE doomed(id) AS (
				SELECT ?
				UNION SELECT source.id FROM sessions AS source JOIN doomed ON source.merged_into = doomed.id
			) SELECT 1
			WHERE EXISTS (
				SELECT 1 FROM sessions AS target
				JOIN sessions AS parent ON parent.id = target.merged_into
				WHERE target.id = ?
			)
			   OR EXISTS (
				SELECT 1 FROM sessions AS member JOIN doomed ON member.id = doomed.id
				WHERE member.status = ?
			   )`,
			id, id, SessionActive).Scan(&unsafe)
		switch {
		case err == nil:
			return "", fmt.Errorf("delete session %s: %w", id, ErrDeleteMergeActive)
		case errors.Is(err, sql.ErrNoRows):
			return "", fmt.Errorf("delete session %s: %w", id, ErrDeleteReplacementPending)
		default:
			return "", fmt.Errorf("check whether session %s is safe to delete: %w", id, err)
		}
	case err != nil:
		return "", fmt.Errorf("read session %s to delete it: %w", id, err)
	}
	rows, err := tx.Query(`WITH RECURSIVE doomed(id) AS (
			SELECT ?
			UNION SELECT source.id FROM sessions AS source JOIN doomed ON source.merged_into = doomed.id
		) SELECT sessions.id, sessions.audio_path FROM sessions JOIN doomed ON sessions.id = doomed.id`, id)
	if err != nil {
		return "", fmt.Errorf("list merged sources of session %s: %w", id, err)
	}
	paths := []string{audioPath}
	for rows.Next() {
		var sourceID, sourceAudioPath string
		if err := rows.Scan(&sourceID, &sourceAudioPath); err != nil {
			_ = rows.Close()
			return "", fmt.Errorf("scan merged source of session %s: %w", id, err)
		}
		if sourceID == id {
			continue
		}
		paths = append(paths, sourceAudioPath, filepath.Join(audioDir, sourceID+".pcm"))
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return "", fmt.Errorf("list merged sources of session %s: %w", id, err)
	}
	audioPath = strings.Join(paths, ",")
	for _, entry := range append(strings.Split(audioPath, ","), id+".pcm") {
		if entry = strings.TrimSpace(entry); entry == "" {
			continue
		}
		if err := recordEraseDirectory(tx, filepath.Dir(recording.Resolve(entry, audioDir))); err != nil {
			return "", fmt.Errorf("delete session %s: %w", id, err)
		}
	}
	if _, err := tx.Exec(`WITH RECURSIVE doomed(id) AS (
			SELECT ?
			UNION SELECT source.id FROM sessions AS source JOIN doomed ON source.merged_into = doomed.id
		) DELETE FROM raw_erase_reservations WHERE session_id IN (SELECT id FROM doomed)`, id); err != nil {
		return "", fmt.Errorf("clear raw erase reservations for session %s: %w", id, err)
	}
	if _, err := tx.Exec(`WITH RECURSIVE doomed(id) AS (
			SELECT ?
			UNION SELECT source.id FROM sessions AS source JOIN doomed ON source.merged_into = doomed.id
		) DELETE FROM sessions WHERE id IN (SELECT id FROM doomed)`, id); err != nil {
		return "", fmt.Errorf("delete session %s and its merged sources: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit deleting session %s: %w", id, err)
	}
	return audioPath, nil
}

// DeleteEndedSessionAndSources deletes an age-eligible merged session and its
// source rows in one transaction. It records every raw capture directory
// before the rows go. It refuses when a session in that deletion set is
// active, remote-deleted, refining, owes a Drive replacement, has an owed
// Drive folder, or has a folder whose documents still need refresh. It returns
// the deleted session IDs so GC can erase each now-redundant raw capture.
func (s *SQLiteStore) DeleteEndedSessionAndSources(id, audioPath, audioDir string) ([]string, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, fmt.Errorf("begin deleting session %s and its sources: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	var block GCSessionBlockError
	err = tx.QueryRow(`WITH RECURSIVE doomed(id) AS (
			SELECT ?
			UNION SELECT source.id FROM sessions AS source JOIN doomed ON source.merged_into = doomed.id
		) SELECT member.id, CASE
			WHEN member.status = ? THEN 'active'
			WHEN member.sync_state = ? THEN 'remote-deleted'
			WHEN member.refinement_status = ? THEN 'refinement running'
			WHEN member.drive_replace_pending = 1 THEN 'Drive replacement owed'
			WHEN EXISTS (
				SELECT 1 FROM drive_replacement_folders
				WHERE drive_replacement_folders.folder_session_id = member.id
				  AND drive_replacement_folders.state = ?
			) THEN 'Drive folder replacement owed'
			ELSE 'documents need Drive refresh'
		END
		FROM sessions AS member JOIN doomed ON member.id = doomed.id
		WHERE (
			member.status = ?
			OR member.sync_state = ?
			OR member.refinement_status = ?
			OR member.drive_replace_pending = 1
			OR EXISTS (
				SELECT 1 FROM drive_replacement_folders
				WHERE drive_replacement_folders.folder_session_id = member.id
				  AND drive_replacement_folders.state = ?
			)
			OR (member.gdrive_folder_id != '' AND member.drive_documents_pending != 0)
		  )
		LIMIT 1`,
		id, SessionActive, SyncStateRemoteDeleted, RefinementRunning, DriveReplacementFolderOwed,
		SessionActive, SyncStateRemoteDeleted, RefinementRunning, DriveReplacementFolderOwed).Scan(&block.BlockingSessionID, &block.Reason)
	switch {
	case err == nil:
		block.BlockingSessionIsTarget = block.BlockingSessionID == id
		return nil, false, &block
	case !errors.Is(err, sql.ErrNoRows):
		return nil, false, fmt.Errorf("check session set of %s before GC deletion: %w", id, err)
	}
	var eligible int
	err = tx.QueryRow(`SELECT 1 FROM sessions
		WHERE sessions.id = ? AND sessions.audio_path = ? AND sessions.documents_unverified = 0
		  AND `+removableCondition,
		id, audioPath).Scan(&eligible)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("check session %s and its sources before GC deletion: %w", id, err)
	}
	rows, err := tx.Query(`WITH RECURSIVE doomed(id) AS (
			SELECT ?
			UNION SELECT source.id FROM sessions AS source JOIN doomed ON source.merged_into = doomed.id
		) SELECT id FROM doomed`, id)
	if err != nil {
		return nil, false, fmt.Errorf("list source sessions of %s for GC deletion: %w", id, err)
	}
	var deletedIDs []string
	for rows.Next() {
		var deletedID string
		if err := rows.Scan(&deletedID); err != nil {
			_ = rows.Close()
			return nil, false, fmt.Errorf("scan source session of %s for GC deletion: %w", id, err)
		}
		deletedIDs = append(deletedIDs, deletedID)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, false, fmt.Errorf("list source sessions of %s for GC deletion: %w", id, err)
	}
	if audioDir != "" {
		for _, deletedID := range deletedIDs {
			if err := recordEraseDirectory(tx, filepath.Dir(filepath.Join(audioDir, deletedID+".pcm"))); err != nil {
				return nil, false, fmt.Errorf("record raw erase directory for session %s: %w", deletedID, err)
			}
		}
	}
	if _, err := tx.Exec(`WITH RECURSIVE doomed(id) AS (
			SELECT ?
			UNION SELECT source.id FROM sessions AS source JOIN doomed ON source.merged_into = doomed.id
		) DELETE FROM sessions WHERE id IN (SELECT id FROM doomed)`, id); err != nil {
		return nil, false, fmt.Errorf("delete session %s and its sources for GC: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit GC deletion of session %s and its sources: %w", id, err)
	}
	return deletedIDs, true, nil
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
		`INSERT INTO sessions(id, title, started_at, ended_at, status, summary, summary_status, summary_preset, speaker_names, refined_transcript, refinement_status, audio_path, sync_status, envoy_publish_status, sync_state, retry_count, last_sync_attempt, error_message, gdrive_folder_id, canonical_transcript, transcript_source, audio_removed_at, restored_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
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
		sess.AudioRemovedAt,
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("insert session %s: %w", sess.ID, err)
	}

	for i := range segments {
		_, err = tx.Exec(
			`INSERT INTO segments(session_id, speaker, text, start_time, end_time, timestamp) VALUES(?, ?, ?, ?, ?, ?)`,
			sess.ID,
			segments[i].Speaker,
			strings.TrimSpace(segments[i].Text),
			segments[i].StartTime,
			segments[i].EndTime,
			segments[i].Timestamp.UTC().Format(time.RFC3339Nano),
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

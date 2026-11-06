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
	SummaryStatus       string     `json:"summary_status"`
	SummaryPreset       string     `json:"summary_preset"`
	SpeakerNames        string     `json:"speaker_names"`
	RefinedTranscript   string     `json:"refined_transcript"`
	RefinementStatus    string     `json:"refinement_status"`
	AudioPath           string     `json:"audio_path"`
	AudioRemovedAt      string     `json:"audio_removed_at"` // when disk pressure removed audio (RFC 3339), "" if never: all of it, or part when AudioPath names what is left; the session is kept
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

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	store := &SQLiteStore{db: db}

	if err := store.init(dbPath); err != nil {
		_ = db.Close()
		return nil, err
	}

	return store, nil
}

// createPreMigrationBackup copies the database file to a backup before migrations run.
// If the DB file doesn't exist (fresh install), the backup is skipped.
// If backup fails, a warning is logged but startup continues (degraded is better than dead).
func createPreMigrationBackup(db *sql.DB, dbPath string) error {
	// Check if DB file exists (skip backup for fresh installs)
	_, err := os.Stat(dbPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Fresh install, no backup needed
			return nil
		}
		return fmt.Errorf("stat db file: %w", err)
	}

	// Flush WAL to main DB file to ensure consistent backup
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return fmt.Errorf("wal checkpoint: %w", err)
	}

	// Open source DB file
	src, err := os.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open source db: %w", err)
	}
	defer func() { _ = src.Close() }()

	// Create/overwrite backup file
	backupPath := dbPath + ".pre-migrate.bak"
	dst, err := os.Create(backupPath)
	if err != nil {
		return fmt.Errorf("create backup file: %w", err)
	}
	defer func() { _ = dst.Close() }()

	// Copy file content
	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("copy db to backup: %w", err)
	}

	slog.Info("pre-migration backup created", "path", backupPath)
	return nil
}

func (s *SQLiteStore) init(dbPath string) error {
	pragmas := []string{
		"PRAGMA journal_mode = WAL",
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
			transcript_source TEXT NOT NULL DEFAULT ''
		);
	`); err != nil {
		return fmt.Errorf("create sessions table: %w", err)
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
	} {
		if _, err := s.db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("migration failed: %w", err)
		}
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

	for _, stmt := range []string{
		`CREATE TRIGGER IF NOT EXISTS sessions_ai AFTER INSERT ON sessions BEGIN
			INSERT INTO sessions_fts(rowid, title, summary, canonical_transcript)
			VALUES (new.rowid, new.title, new.summary, new.canonical_transcript);
		END;`,
		`CREATE TRIGGER IF NOT EXISTS sessions_ad AFTER DELETE ON sessions BEGIN
			INSERT INTO sessions_fts(sessions_fts, rowid, title, summary, canonical_transcript)
			VALUES('delete', old.rowid, old.title, old.summary, old.canonical_transcript);
		END;`,
		`CREATE TRIGGER IF NOT EXISTS sessions_au AFTER UPDATE ON sessions BEGIN
			INSERT INTO sessions_fts(sessions_fts, rowid, title, summary, canonical_transcript)
			VALUES('delete', old.rowid, old.title, old.summary, old.canonical_transcript);
			INSERT INTO sessions_fts(rowid, title, summary, canonical_transcript)
			VALUES (new.rowid, new.title, new.summary, new.canonical_transcript);
		END;`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("create sessions_fts trigger: %w", err)
		}
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

	if _, err := s.db.Exec("CREATE INDEX IF NOT EXISTS idx_mcp_events_created_at ON mcp_events(created_at)"); err != nil {
		return fmt.Errorf("create mcp_events index: %w", err)
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

	_, err = tx.Exec(
		`INSERT INTO sessions(id, started_at, ended_at, status, summary_status) VALUES(?, ?, ?, 'ended', 'pending')`,
		newID,
		startedAt.UTC().Format(time.RFC3339Nano),
		endedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("create merged session: %w", err)
	}

	placeholders := make([]string, len(sourceIDs))
	args := make([]any, 0, len(sourceIDs)+1)
	args = append(args, newID)
	for i, id := range sourceIDs {
		placeholders[i] = "?"
		args = append(args, id)
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
	query := `SELECT id, title, started_at, ended_at, status, summary, summary_status, summary_preset, speaker_names, refined_transcript, refinement_status, audio_path, sync_status, envoy_publish_status, sync_state, retry_count, last_sync_attempt, error_message, gdrive_folder_id, merged_into, canonical_transcript, transcript_source, audio_removed_at
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
		`SELECT id, title, started_at, ended_at, status, summary, summary_status, summary_preset, speaker_names, refined_transcript, refinement_status, audio_path, sync_status, envoy_publish_status, sync_state, retry_count, last_sync_attempt, error_message, gdrive_folder_id, merged_into, canonical_transcript, transcript_source, audio_removed_at
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
		`SELECT id, title, started_at, ended_at, status, summary, summary_status, summary_preset, speaker_names, refined_transcript, refinement_status, audio_path, sync_status, envoy_publish_status, sync_state, retry_count, last_sync_attempt, error_message, gdrive_folder_id, merged_into, canonical_transcript, transcript_source, audio_removed_at FROM sessions WHERE id = ?`,
		id,
	)

	var sess Session
	var startedAt string
	var endedAt sql.NullString
	var lastSyncAttempt sql.NullString
	if err := row.Scan(&sess.ID, &sess.Title, &startedAt, &endedAt, &sess.Status, &sess.Summary, &sess.SummaryStatus, &sess.SummaryPreset, &sess.SpeakerNames, &sess.RefinedTranscript, &sess.RefinementStatus, &sess.AudioPath, &sess.SyncStatus, &sess.EnvoyPublishStatus, &sess.SyncState, &sess.RetryCount, &lastSyncAttempt, &sess.ErrorMessage, &sess.GDriveFolderID, &sess.MergedInto, &sess.CanonicalTranscript, &sess.TranscriptSource, &sess.AudioRemovedAt); err != nil {
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
		`SELECT speaker, text, start_time, end_time, timestamp
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
		if err := rows.Scan(&seg.Speaker, &seg.Text, &seg.StartTime, &seg.EndTime, &ts); err != nil {
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
		`SELECT speaker, text, start_time, end_time, timestamp
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
		if err := rows.Scan(&seg.Speaker, &seg.Text, &seg.StartTime, &seg.EndTime, &ts); err != nil {
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

func (s *SQLiteStore) UpdateSummary(sessionID, title, summary, status, preset, speakerNames string) error {
	res, err := s.db.Exec(
		`UPDATE sessions
		 SET title = CASE WHEN ? != '' THEN ? ELSE title END,
		     summary = ?,
		     summary_status = ?,
		     summary_preset = ?,
		     speaker_names = ?,
		     envoy_publish_status = CASE WHEN ? = ? THEN ? ELSE envoy_publish_status END
		 WHERE id = ?`,
		title,
		title,
		summary,
		status,
		preset,
		speakerNames,
		status,
		SummaryRunning,
		EnvoyPublishPending,
		sessionID,
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

	return nil
}

func (s *SQLiteStore) UpdateSummaryError(sessionID, errMsg string) error {
	res, err := s.db.Exec(
		`UPDATE sessions SET error_message = ? WHERE id = ?`,
		errMsg,
		sessionID,
	)
	if err != nil {
		return fmt.Errorf("update summary error for session %s: %w", sessionID, err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update summary error rows affected: %w", err)
	}
	if rows == 0 {
		return sql.ErrNoRows
	}

	return nil
}

func (s *SQLiteStore) UpdateRefinement(sessionID, transcript, status string) error {
	res, err := s.db.Exec(
		`UPDATE sessions SET refined_transcript = ?, refinement_status = ? WHERE id = ?`,
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
		`UPDATE sessions SET refined_transcript = '', refinement_status = ? WHERE id = ? AND audio_path = ? AND audio_path != ''`,
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

// Canonicalize sets the canonical_transcript and transcript_source for a session.
// If refinement is completed, uses the refined transcript; otherwise assembles streaming segments.
func (s *SQLiteStore) Canonicalize(sessionID string) error {
	var refinedTranscript, refinementStatus string
	if err := s.db.QueryRow(`SELECT refined_transcript, refinement_status FROM sessions WHERE id = ?`, sessionID).Scan(&refinedTranscript, &refinementStatus); err != nil {
		return fmt.Errorf("query refinement for canonicalize %s: %w", sessionID, err)
	}

	var canonical, source string
	if refinementStatus == RefinementCompleted && strings.TrimSpace(refinedTranscript) != "" {
		canonical = strings.TrimSpace(refinedTranscript)
		source = TranscriptSourceRefined
	} else {
		// Assemble from streaming segments.
		segs, err := s.GetSegments(sessionID)
		if err != nil {
			return fmt.Errorf("get segments for canonicalize %s: %w", sessionID, err)
		}
		var b strings.Builder
		for _, seg := range segs {
			text := strings.TrimSpace(seg.Text)
			if text == "" {
				continue
			}
			b.WriteString(text)
			b.WriteByte('\n')
		}
		canonical = b.String()
		source = TranscriptSourceStreaming
	}

	res, err := s.db.Exec(`UPDATE sessions SET canonical_transcript = ?, transcript_source = ? WHERE id = ?`, canonical, source, sessionID)
	if err != nil {
		return fmt.Errorf("update canonical transcript for session %s: %w", sessionID, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("canonicalize rows affected: %w", err)
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// GetCanonicalTranscript returns the canonical transcript and its source for a session.
func (s *SQLiteStore) GetCanonicalTranscript(sessionID string) (string, string, error) {
	var transcript, source string
	if err := s.db.QueryRow(`SELECT canonical_transcript, transcript_source FROM sessions WHERE id = ?`, sessionID).Scan(&transcript, &source); err != nil {
		return "", "", fmt.Errorf("query canonical transcript for session %s: %w", sessionID, err)
	}
	return transcript, source, nil
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

func (s *SQLiteStore) UpdateSyncStatus(sessionID, status, driveFolderID string) error {
	syncState := SyncStatePending
	switch status {
	case SyncSynced:
		syncState = SyncStateSynced
	case SyncFailed:
		syncState = SyncStateFailed
	}

	res, err := s.db.Exec(
		`UPDATE sessions SET sync_status = ?, sync_state = ?, gdrive_folder_id = ? WHERE id = ?`,
		status, syncState, driveFolderID, sessionID,
	)
	if err != nil {
		return fmt.Errorf("update sync status for session %s: %w", sessionID, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update sync status rows affected: %w", err)
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *SQLiteStore) UpdateSyncState(sessionID, state, driveFolderID string, retryCount int, lastSyncAttempt *time.Time, errorMessage string) error {
	var lastAttempt any
	if lastSyncAttempt != nil {
		lastAttempt = lastSyncAttempt.UTC().Format(time.RFC3339Nano)
	}

	res, err := s.db.Exec(
		`UPDATE sessions
		 SET sync_state = ?, sync_status = ?, gdrive_folder_id = CASE WHEN ? != '' THEN ? ELSE gdrive_folder_id END,
		     retry_count = ?, last_sync_attempt = ?, error_message = ?
		 WHERE id = ?`,
		state,
		syncStatusFromState(state),
		driveFolderID,
		driveFolderID,
		retryCount,
		lastAttempt,
		errorMessage,
		sessionID,
	)
	if err != nil {
		return fmt.Errorf("update sync state for session %s: %w", sessionID, err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update sync state rows affected: %w", err)
	}
	if rows == 0 {
		return sql.ErrNoRows
	}

	return nil
}

// MarkSynced marks session id synced to Drive folder driveFolderID, only while
// unchanged reports true for the session and its segments as they are now,
// read in the same transaction as the update, and reports whether it did. A
// sync passes a check that the session is still what it uploaded: its audio
// path (GC may have put back a recording it could not remove) and its
// documents (a summary made again, a refined transcript). A session that
// changed during the upload is not synced, and the next sync uploads what it
// is now. unchanged runs inside the transaction: it must not use the store.
func (s *SQLiteStore) MarkSynced(id, driveFolderID string, unchanged func(Session, []transcribe.Segment) bool) (bool, error) {
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
	if _, err := tx.Exec(
		`UPDATE sessions
		 SET sync_state = ?, sync_status = ?, gdrive_folder_id = CASE WHEN ? != '' THEN ? ELSE gdrive_folder_id END,
		     retry_count = 0, last_sync_attempt = NULL, error_message = ''
		 WHERE id = ?`,
		SyncStateSynced, SyncSynced, driveFolderID, driveFolderID, id,
	); err != nil {
		return false, fmt.Errorf("mark session %s synced: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit marking session %s synced: %w", id, err)
	}
	return true, nil
}

func (s *SQLiteStore) UpdateEnvoyPublishStatus(sessionID, status string) error {
	res, err := s.db.Exec(`UPDATE sessions SET envoy_publish_status = ? WHERE id = ?`, status, sessionID)
	if err != nil {
		return fmt.Errorf("update envoy publish status for session %s: %w", sessionID, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update envoy publish status rows affected: %w", err)
	}
	if rows == 0 {
		return sql.ErrNoRows
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
		if err := rows.Scan(&sess.ID, &sess.Title, &startedAt, &endedAt, &sess.Status, &sess.Summary, &sess.SummaryStatus, &sess.SummaryPreset, &sess.SpeakerNames, &sess.RefinedTranscript, &sess.RefinementStatus, &sess.AudioPath, &sess.SyncStatus, &sess.EnvoyPublishStatus, &sess.SyncState, &sess.RetryCount, &lastSyncAttempt, &sess.ErrorMessage, &sess.GDriveFolderID, &sess.MergedInto, &sess.CanonicalTranscript, &sess.TranscriptSource, &sess.AudioRemovedAt); err != nil {
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

func (s *SQLiteStore) GetSessionsNeedingSync() ([]string, error) {
	rows, err := s.db.Query(
		`SELECT id FROM sessions WHERE status = 'ended' AND sync_state = ?`,
		SyncStatePending,
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

func (s *SQLiteStore) GetSessionsNeedingEnvoyPublish() ([]string, error) {
	rows, err := s.db.Query(
		`SELECT id FROM sessions WHERE status = 'ended' AND summary_status = ? AND envoy_publish_status != ? ORDER BY started_at ASC`,
		SummaryCompleted,
		EnvoyPublishPublished,
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
		`SELECT id, title, started_at, ended_at, status, summary, summary_status, summary_preset, speaker_names, refined_transcript, refinement_status, audio_path, sync_status, envoy_publish_status, sync_state, retry_count, last_sync_attempt, error_message, gdrive_folder_id, merged_into, canonical_transcript, transcript_source, audio_removed_at
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
// first: ended, with a completed summary, synced if syncGated, and older than
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
	query := `SELECT id FROM sessions WHERE status = 'ended' AND summary_status = 'completed'
		AND started_at < ? AND restored_at < ? AND refinement_status != 'running'`
	if syncGated {
		query += ` AND sync_status = 'synced'`
	}
	query += ` ORDER BY started_at ASC`

	rows, err := s.db.Query(query, at, at)
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

// removable is the condition under which disk pressure may remove a session's
// recording, and its arguments: the session ended, no refinement is reading
// the file, and, when syncGated, it is synced. Any other sync state, a
// failure included, keeps the recording.
func removable(syncGated bool) (string, []any) {
	cond := `status = 'ended' AND refinement_status != ?`
	if syncGated {
		cond += ` AND sync_status = ?`
		return cond, []any{RefinementRunning, SyncSynced}
	}
	return cond, []any{RefinementRunning}
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

func (s *SQLiteStore) DeleteSession(id string) error {
	res, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete session %s: %w", id, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete session rows affected: %w", err)
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DeleteEndedSession deletes session id for the age policy only while it is as
// GC read it: ended, and naming audioPath. It reports whether it did. A
// session merged since GC read it gave its recording to the merged session,
// so deleting it, and removing that recording with it, would leave the merged
// session naming a file that is gone.
func (s *SQLiteStore) DeleteEndedSession(id, audioPath string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM sessions WHERE id = ? AND status = 'ended' AND audio_path = ? AND refinement_status != ?`, id, audioPath, RefinementRunning)
	if err != nil {
		return false, fmt.Errorf("delete session %s: %w", id, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("delete session rows affected: %w", err)
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
		sess.SyncState,
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

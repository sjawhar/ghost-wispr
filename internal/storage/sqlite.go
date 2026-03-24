package storage

import (
	"context"
	"database/sql"
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

	SessionActive    = gwstatus.SessionActive
	SessionEnded     = gwstatus.SessionEnded
	SessionDiscarded = gwstatus.SessionDiscarded
	SessionMerged    = gwstatus.SessionMerged

	SyncPending = gwstatus.SyncPending
	SyncSynced  = gwstatus.SyncSynced
	SyncFailed  = gwstatus.SyncFailed

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
	RefinedTranscript   string     `json:"refined_transcript"`
	RefinementStatus    string     `json:"refinement_status"`
	AudioPath           string     `json:"audio_path"`
	SyncStatus          string     `json:"sync_status"`
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
			refined_transcript TEXT NOT NULL DEFAULT '',
			refinement_status TEXT NOT NULL DEFAULT 'pending',
			audio_path TEXT NOT NULL DEFAULT '',
			sync_status TEXT NOT NULL DEFAULT 'pending',
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
		`ALTER TABLE sessions ADD COLUMN title TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN sync_status TEXT NOT NULL DEFAULT 'pending'`,
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
	} {
		if _, err := s.db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("migration failed: %w", err)
		}
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

	if _, err := s.db.Exec("CREATE INDEX IF NOT EXISTS idx_sessions_started_at ON sessions(started_at)"); err != nil {
		return fmt.Errorf("create sessions index: %w", err)
	}
	if _, err := s.db.Exec("CREATE INDEX IF NOT EXISTS idx_segments_session_id ON segments(session_id, timestamp)"); err != nil {
		return fmt.Errorf("create segments index: %w", err)
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

func (s *SQLiteStore) CreateSession(id string, startedAt time.Time) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("session id is required")
	}

	_, err := s.db.Exec(
		`INSERT INTO sessions(id, started_at, status, summary_status) VALUES(?, ?, 'active', ?)`,
		id,
		startedAt.UTC().Format(time.RFC3339Nano),
		SummaryPending,
	)
	if err != nil {
		return fmt.Errorf("create session %s: %w", id, err)
	}
	return nil
}

func (s *SQLiteStore) EndSession(id string, endedAt time.Time, audioPath string) error {
	res, err := s.db.Exec(
		`UPDATE sessions SET ended_at = ?, status = 'ended', audio_path = ? WHERE id = ?`,
		endedAt.UTC().Format(time.RFC3339Nano),
		audioPath,
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

	markQuery := fmt.Sprintf(
		`UPDATE sessions SET status = 'merged', merged_into = ? WHERE id IN (%s)`,
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
	query := `SELECT id, title, started_at, ended_at, status, summary, summary_status, summary_preset, refined_transcript, refinement_status, audio_path, sync_status, sync_state, retry_count, last_sync_attempt, error_message, gdrive_folder_id, merged_into, canonical_transcript, transcript_source
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
		`SELECT id, title, started_at, ended_at, status, summary, summary_status, summary_preset, refined_transcript, refinement_status, audio_path, sync_status, sync_state, retry_count, last_sync_attempt, error_message, gdrive_folder_id, merged_into, canonical_transcript, transcript_source
		 FROM sessions WHERE (title = '' OR title IS NULL) AND status NOT IN ('discarded', 'merged') ORDER BY started_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("query sessions with empty titles: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return scanSessions(rows)
}

func (s *SQLiteStore) GetSession(id string) (Session, error) {
	row := s.db.QueryRow(
		`SELECT id, title, started_at, ended_at, status, summary, summary_status, summary_preset, refined_transcript, refinement_status, audio_path, sync_status, sync_state, retry_count, last_sync_attempt, error_message, gdrive_folder_id, merged_into, canonical_transcript, transcript_source FROM sessions WHERE id = ?`,
		id,
	)

	var sess Session
	var startedAt string
	var endedAt sql.NullString
	var lastSyncAttempt sql.NullString
	if err := row.Scan(&sess.ID, &sess.Title, &startedAt, &endedAt, &sess.Status, &sess.Summary, &sess.SummaryStatus, &sess.SummaryPreset, &sess.RefinedTranscript, &sess.RefinementStatus, &sess.AudioPath, &sess.SyncStatus, &sess.SyncState, &sess.RetryCount, &lastSyncAttempt, &sess.ErrorMessage, &sess.GDriveFolderID, &sess.MergedInto, &sess.CanonicalTranscript, &sess.TranscriptSource); err != nil {
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
	rows, err := s.db.Query(
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

func (s *SQLiteStore) UpdateSummary(sessionID, title, summary, status, preset string) error {
	res, err := s.db.Exec(
		`UPDATE sessions SET title = CASE WHEN ? != '' THEN ? ELSE title END, summary = ?, summary_status = ?, summary_preset = ? WHERE id = ?`,
		title,
		title,
		summary,
		status,
		preset,
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
		if err := rows.Scan(&sess.ID, &sess.Title, &startedAt, &endedAt, &sess.Status, &sess.Summary, &sess.SummaryStatus, &sess.SummaryPreset, &sess.RefinedTranscript, &sess.RefinementStatus, &sess.AudioPath, &sess.SyncStatus, &sess.SyncState, &sess.RetryCount, &lastSyncAttempt, &sess.ErrorMessage, &sess.GDriveFolderID, &sess.MergedInto, &sess.CanonicalTranscript, &sess.TranscriptSource); err != nil {
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
			`UPDATE sessions SET ended_at = ?, status = 'ended' WHERE id = ? AND status = 'active'`,
			now, id,
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

func (s *SQLiteStore) GetSessionsBySyncState(syncState string) ([]Session, error) {
	rows, err := s.db.Query(
		`SELECT id, title, started_at, ended_at, status, summary, summary_status, summary_preset, refined_transcript, refinement_status, audio_path, sync_status, sync_state, retry_count, last_sync_attempt, error_message, gdrive_folder_id, merged_into, canonical_transcript, transcript_source
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

// GetGCEligibleSessions returns session IDs eligible for garbage collection.
// Sessions must be ended. For normal GC, summary must be completed.
// For disk-pressure GC (diskPressure=true), failed/pending summaries are also eligible
// as long as the session has been synced (if syncGated) — this is the last line of defense.
// If maxAgeDays > 0, only sessions older than that are returned.
// If maxAgeDays == 0, all eligible sessions are returned.
// Results are ordered oldest first (for disk-pressure GC to delete oldest).
func (s *SQLiteStore) GetGCEligibleSessions(maxAgeDays int, syncGated bool, diskPressure bool) ([]string, error) {
	query := `SELECT id FROM sessions WHERE status = 'ended'`
	if !diskPressure {
		query += ` AND summary_status = 'completed'`
	}
	var args []any
	if maxAgeDays > 0 {
		cutoff := time.Now().UTC().Add(-time.Duration(maxAgeDays) * 24 * time.Hour).Format(time.RFC3339Nano)
		query += ` AND started_at < ?`
		args = append(args, cutoff)
	}
	if syncGated {
		query += ` AND sync_status = 'synced'`
	}
	query += ` ORDER BY started_at ASC`

	rows, err := s.db.Query(query, args...)
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

// ImportSession inserts a fully-formed session and its segments in a single transaction.
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
		`INSERT INTO sessions(id, title, started_at, ended_at, status, summary, summary_status, summary_preset, refined_transcript, refinement_status, audio_path, sync_status, sync_state, retry_count, last_sync_attempt, error_message, gdrive_folder_id, canonical_transcript, transcript_source)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.ID,
		sess.Title,
		sess.StartedAt.UTC().Format(time.RFC3339Nano),
		endedAt,
		sess.Status,
		sess.Summary,
		sess.SummaryStatus,
		sess.SummaryPreset,
		sess.RefinedTranscript,
		sess.RefinementStatus,
		sess.AudioPath,
		sess.SyncStatus,
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

func (s *SQLiteStore) Search(query string) ([]SearchResult, error) {
	matchQuery := buildFTS5MatchQuery(query)
	if matchQuery == "" {
		return []SearchResult{}, nil
	}

	rows, err := s.db.Query(
		`SELECT sessions.id,
		        sessions.title,
		        COALESCE(
		          NULLIF(snippet(sessions_fts, 2, '<mark>', '</mark>', ' … ', 24), ''),
		          NULLIF(snippet(sessions_fts, 1, '<mark>', '</mark>', ' … ', 24), ''),
		          sessions.title
		        ) AS snippet,
		        bm25(sessions_fts) AS rank
		 FROM sessions_fts
		 JOIN sessions ON sessions.rowid = sessions_fts.rowid
		 WHERE sessions_fts MATCH ?
		   AND sessions.status NOT IN ('discarded', 'merged')
		 ORDER BY rank ASC
		 LIMIT 50`,
		matchQuery,
	)
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

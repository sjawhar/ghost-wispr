package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

const (
	SummaryPending   = "pending"
	SummaryRunning   = "running"
	SummaryCompleted = "completed"
	SummaryFailed    = "failed"
)

type Session struct {
	ID            string     `json:"id"`
	StartedAt     time.Time  `json:"started_at"`
	EndedAt       *time.Time `json:"ended_at,omitempty"`
	Status        string     `json:"status"`
	Summary       string     `json:"summary"`
	SummaryStatus string     `json:"summary_status"`
	AudioPath     string     `json:"audio_path"`
}

type SQLiteStore struct {
	db *sql.DB
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
	if err := store.init(); err != nil {
		_ = db.Close()
		return nil, err
	}

	return store, nil
}

func (s *SQLiteStore) init() error {
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

	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			started_at TEXT NOT NULL,
			ended_at TEXT,
			status TEXT NOT NULL,
			summary TEXT NOT NULL DEFAULT '',
			summary_status TEXT NOT NULL DEFAULT 'pending',
			audio_path TEXT NOT NULL DEFAULT ''
		);
	`); err != nil {
		return fmt.Errorf("create sessions table: %w", err)
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

func (s *SQLiteStore) GetSessionsByDate(date string) ([]Session, error) {
	rows, err := s.db.Query(
		`SELECT id, started_at, ended_at, status, summary, summary_status, audio_path
		 FROM sessions
		 WHERE substr(started_at, 1, 10) = ?
		 ORDER BY started_at DESC`,
		date,
	)
	if err != nil {
		return nil, fmt.Errorf("query sessions by date %s: %w", date, err)
	}
	defer func() { _ = rows.Close() }()

	return scanSessions(rows)
}

func (s *SQLiteStore) GetDates() ([]string, error) {
	rows, err := s.db.Query(
		`SELECT DISTINCT substr(started_at, 1, 10) AS date FROM sessions ORDER BY date DESC`,
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

func (s *SQLiteStore) GetSession(id string) (Session, error) {
	row := s.db.QueryRow(
		`SELECT id, started_at, ended_at, status, summary, summary_status, audio_path FROM sessions WHERE id = ?`,
		id,
	)

	var sess Session
	var startedAt string
	var endedAt sql.NullString
	if err := row.Scan(&sess.ID, &startedAt, &endedAt, &sess.Status, &sess.Summary, &sess.SummaryStatus, &sess.AudioPath); err != nil {
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

func (s *SQLiteStore) UpdateSummary(sessionID, summary, status string) error {
	res, err := s.db.Exec(
		`UPDATE sessions SET summary = ?, summary_status = ? WHERE id = ?`,
		summary,
		status,
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
		if err := rows.Scan(&sess.ID, &startedAt, &endedAt, &sess.Status, &sess.Summary, &sess.SummaryStatus, &sess.AudioPath); err != nil {
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

		sessions = append(sessions, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sessions rows: %w", err)
	}

	return sessions, nil
}

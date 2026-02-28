package storage

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

func newTestSQLiteStore(t *testing.T) *SQLiteStore {
	t.Helper()

	path := filepath.Join(t.TempDir(), "test.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	return store
}

func TestSQLitePragmas(t *testing.T) {
	store := newTestSQLiteStore(t)

	var mode string
	if err := store.DB().QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode failed: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("expected journal_mode wal, got %q", mode)
	}

	var timeout int
	if err := store.DB().QueryRow("PRAGMA busy_timeout").Scan(&timeout); err != nil {
		t.Fatalf("PRAGMA busy_timeout failed: %v", err)
	}
	if timeout < 5000 {
		t.Fatalf("expected busy_timeout >= 5000, got %d", timeout)
	}
}

func TestSQLiteCRUD(t *testing.T) {
	store := newTestSQLiteStore(t)

	startedAt := time.Date(2026, 2, 26, 10, 0, 0, 0, time.UTC)
	sessionID := startedAt.Format("20060102150405")
	if err := store.CreateSession(sessionID, startedAt); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	seg := transcribe.Segment{
		Speaker:   1,
		Text:      "Ship the polished app.",
		StartTime: 1.0,
		EndTime:   2.5,
		Timestamp: startedAt.Add(2 * time.Second),
	}
	if err := store.AppendSegment(sessionID, seg); err != nil {
		t.Fatalf("AppendSegment failed: %v", err)
	}

	if err := store.UpdateSummary(sessionID, "## Summary\n- done", SummaryCompleted, "default"); err != nil {
		t.Fatalf("UpdateSummary failed: %v", err)
	}

	if err := store.EndSession(sessionID, startedAt.Add(30*time.Second), "data/audio/20260226100000.mp3"); err != nil {
		t.Fatalf("EndSession failed: %v", err)
	}

	session, err := store.GetSession(sessionID)
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}
	if session.Status != "ended" {
		t.Fatalf("expected status ended, got %q", session.Status)
	}
	if session.SummaryStatus != SummaryCompleted {
		t.Fatalf("expected summary_status %q, got %q", SummaryCompleted, session.SummaryStatus)
	}
	if session.SummaryPreset != "default" {
		t.Fatalf("expected summary_preset %q, got %q", "default", session.SummaryPreset)
	}

	segments, err := store.GetSegments(sessionID)
	if err != nil {
		t.Fatalf("GetSegments failed: %v", err)
	}
	if len(segments) != 1 {
		t.Fatalf("expected 1 segment, got %d", len(segments))
	}
	if segments[0].Text != seg.Text {
		t.Fatalf("expected segment text %q, got %q", seg.Text, segments[0].Text)
	}

	sessionsByDate, err := store.GetSessionsByDate("2026-02-26")
	if err != nil {
		t.Fatalf("GetSessionsByDate failed: %v", err)
	}
	if len(sessionsByDate) != 1 {
		t.Fatalf("expected 1 session for date, got %d", len(sessionsByDate))
	}

	dates, err := store.GetDates()
	if err != nil {
		t.Fatalf("GetDates failed: %v", err)
	}
	if len(dates) != 1 || dates[0] != "2026-02-26" {
		t.Fatalf("expected dates [2026-02-26], got %#v", dates)
	}
}

func TestUpdateSummaryWithPreset(t *testing.T) {
	store := newTestSQLiteStore(t)

	startedAt := time.Date(2026, 2, 26, 10, 0, 0, 0, time.UTC)
	sessionID := startedAt.Format("20060102150405")
	if err := store.CreateSession(sessionID, startedAt); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	if err := store.UpdateSummary(sessionID, "## Summary\n- done", SummaryCompleted, "concise"); err != nil {
		t.Fatalf("UpdateSummary failed: %v", err)
	}

	session, err := store.GetSession(sessionID)
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}

	if session.SummaryPreset != "concise" {
		t.Fatalf("expected summary_preset %q, got %q", "concise", session.SummaryPreset)
	}
}

func TestSQLiteSummaryClaimIsIdempotent(t *testing.T) {
	store := newTestSQLiteStore(t)

	claimed, err := store.ClaimSummaryRequest("s1", "hash-1")
	if err != nil {
		t.Fatalf("first claim failed: %v", err)
	}
	if !claimed {
		t.Fatal("expected first claim to be accepted")
	}

	claimed, err = store.ClaimSummaryRequest("s1", "hash-1")
	if err != nil {
		t.Fatalf("second claim failed: %v", err)
	}
	if claimed {
		t.Fatal("expected second claim to be ignored")
	}
}

func TestSQLiteConcurrentAccess(t *testing.T) {
	store := newTestSQLiteStore(t)

	startedAt := time.Now().UTC()
	sessionID := startedAt.Format("20060102150405")
	if err := store.CreateSession(sessionID, startedAt); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_ = store.AppendSegment(sessionID, transcribe.Segment{
				Speaker:   idx % 3,
				Text:      fmt.Sprintf("segment-%d", idx),
				StartTime: float64(idx),
				EndTime:   float64(idx) + 0.5,
				Timestamp: startedAt.Add(time.Duration(idx) * time.Second),
			})
			_, _ = store.GetSession(sessionID)
		}(i)
	}
	wg.Wait()

	segments, err := store.GetSegments(sessionID)
	if err != nil {
		t.Fatalf("GetSegments failed: %v", err)
	}
	if len(segments) != 20 {
		t.Fatalf("expected 20 segments, got %d", len(segments))
	}
}

package storage

import (
	"database/sql"
	"errors"
	"testing"
	"time"
)

// TestASessionKeepsTheSampleRateOfItsRawRecording fails if the rate a
// session's raw recording was captured at is not kept with the session, which
// a kept .pcm needs to be encoded at its true duration; if a session from
// before the rate was kept reads as anything but 0, the rate its reader takes
// as 16000 Hz; or if an unknown session is not refused.
func TestASessionKeepsTheSampleRateOfItsRawRecording(t *testing.T) {
	store := newTestSQLiteStore(t)
	const id = "20261118100000"
	if err := store.CreateSession(id, time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if rate, err := store.SessionSampleRate(id); err != nil || rate != 0 {
		t.Fatalf("a session with no rate recorded = %d, %v; want 0", rate, err)
	}
	if err := store.SetSessionSampleRate(id, 48000); err != nil {
		t.Fatalf("SetSessionSampleRate: %v", err)
	}
	if rate, err := store.SessionSampleRate(id); err != nil || rate != 48000 {
		t.Fatalf("the session's rate = %d, %v; want 48000", rate, err)
	}
	if err := store.SetSessionSampleRate("20261118110000", 48000); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("SetSessionSampleRate of an unknown session = %v, want sql.ErrNoRows", err)
	}
	if _, err := store.SessionSampleRate("20261118110000"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("SessionSampleRate of an unknown session = %v, want sql.ErrNoRows", err)
	}
}

package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// foreverHeldAppendStore holds every segment write once hold is set, until
// the process exits, as a write held past a stop's deadline is.
type foreverHeldAppendStore struct {
	*storage.SQLiteStore
	hold    chan struct{}
	entered chan struct{}
}

//nolint:gocritic // Store's append API passes one segment by value.
func (s *foreverHeldAppendStore) AppendSegment(sessionID string, seg transcribe.Segment) error {
	select {
	case <-s.hold:
		close(s.entered)
		select {}
	default:
		return s.SQLiteStore.AppendSegment(sessionID, seg)
	}
}

// TestARecoveredSessionEndsWhenItsRecordingClosed fails if a session whose
// end the stop's deadline cut off, after its recording was saved (so its raw
// recording is gone) and while a pause's flush still held its words, is
// recovered at the next start with an end other than the one the end took
// when it closed the recording. Before, the end lived only in memory, and the
// next start, finding no raw recording, ended the session at the restart, so
// its end stretched over the downtime (GPT-6.1 Sol's probe at 8c38d80e).
// The shutdown runs in a process of its own, which exits as the app does.
func TestARecoveredSessionEndsWhenItsRecordingClosed(t *testing.T) {
	type record struct {
		ID           string
		Before, Stop time.Time
	}
	if root := os.Getenv("GHOST_WISPR_END_TIME_CHILD"); root != "" {
		db, err := storage.NewSQLiteStore(filepath.Join(root, "db.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		store := &foreverHeldAppendStore{SQLiteStore: db, hold: make(chan struct{}), entered: make(chan struct{})}
		recorder := audio.NewRecorder(filepath.Join(root, "audio"))
		manager := session.NewManager(store, recorder, nil, nil, session.NewDetector(time.Hour), 0)
		gate := newSessionCaptureGate(t.Context(), manager, recorder, func() deepgramFinalizer { return nil }, time.Millisecond, testEndRetry, nopLogf)
		gate.background = manager
		id, err := manager.ManualStartSession(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := gate.Writer(recorder.Writer(io.Discard)).Write(tone(2)); err != nil {
			t.Fatal(err)
		}
		first := bufferedFinal(t, "first")
		first.SpeechFinal = true
		if err := manager.Message(first); err != nil {
			t.Fatal(err)
		}
		close(store.hold)
		if err := manager.Message(bufferedFinal(t, "last")); err != nil {
			t.Fatal(err)
		}
		gate.Pause()
		awaitClosed(t, store.entered, "the pause's flush to hold its words")
		before := time.Now().UTC()
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		recoverAtStart(db).stop(ctx, manager, lifecyclePublishWait)
		stopped := time.Now().UTC()
		if sess, err := db.GetSession(id); err != nil || sess.Status != storage.SessionActive {
			t.Fatalf("control: the session after the stop is %+v (%v), want it active: the deadline cut its end off", sess, err)
		}
		if _, err := os.Stat(filepath.Join(root, "audio", id+".pcm")); !os.IsNotExist(err) {
			t.Fatalf("control: the raw recording is still there (%v): the end did not save it before its deadline", err)
		}
		closeStore(ctx, db)
		payload, err := json.Marshal(record{ID: id, Before: before, Stop: stopped})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "record.json"), payload, 0o600); err != nil {
			t.Fatal(err)
		}
		return // the process exits, as the app does, with the flush still held
	}

	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestARecoveredSessionEndsWhenItsRecordingClosed$")
	child.Env = append(os.Environ(), "GHOST_WISPR_END_TIME_CHILD="+root)
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatalf("the shutdown process: %v\n%s", err, out)
	}
	payload, err := os.ReadFile(filepath.Join(root, "record.json"))
	if err != nil {
		t.Fatal(err)
	}
	var shutdown record
	if err := json.Unmarshal(payload, &shutdown); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second) // the downtime, which must not enter the session
	db := openResumeStore(t, filepath.Join(root, "db.sqlite"))
	recoverStaleSessions(db, &config.Config{AudioDir: filepath.Join(root, "audio")})
	sess, err := db.GetSession(shutdown.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.EndedAt == nil || sess.EndedAt.Before(shutdown.Before) || sess.EndedAt.After(shutdown.Stop) {
		t.Fatalf("the recovered session ends at %v; want the time its end closed the recording, during the stop (%s to %s)",
			sess.EndedAt, shutdown.Before.Format(time.RFC3339Nano), shutdown.Stop.Format(time.RFC3339Nano))
	}
}

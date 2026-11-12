package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// canonicalizeFailingStore is the real store, with a Canonicalize that runs
// before (a segment deletion and its own summary run starting) and then fails.
type canonicalizeFailingStore struct {
	*storage.SQLiteStore
	before func()
}

func (s *canonicalizeFailingStore) Canonicalize(string) (string, int, error) {
	s.before()
	return "", 0, errors.New("canonicalize: disk I/O error")
}

// A summary run whose transcript read fails after a segment deletion has
// changed the session, and after the deletion's own summary run started,
// records nothing: it fails only at the transcript generation it read before
// it started, so the newer run stays running.
func TestACanonicalizeFailureAfterADeletionLeavesTheNewerRunRunning(t *testing.T) {
	for _, run := range []struct {
		name       string
		summarizer Summarizer
	}{
		{"with a summarizer", &r3GateSummarizer{}},
		{"with no summarizer", nil},
	} {
		t.Run(run.name, func(t *testing.T) {
			real, segmentID := r3SessionStore(t)
			store := &canonicalizeFailingStore{SQLiteStore: real, before: func() {
				deleteR3Session(t, real, segmentID)
				if err := real.StartSummary("r3"); err != nil {
					t.Fatalf("start the deletion's summary run: %v", err)
				}
			}}
			manager := NewManager(store, nil, run.summarizer, nil, nil, 0)

			manager.generateSummary(context.Background(), "r3", time.Now())

			sess, err := real.GetSession("r3")
			if err != nil {
				t.Fatalf("get session: %v", err)
			}
			if sess.SummaryStatus != storage.SummaryRunning || sess.SummaryError != "" {
				t.Errorf("summary status %q, error %q; want the newer run's: %q, no error", sess.SummaryStatus, sess.SummaryError, storage.SummaryRunning)
			}
		})
	}
}

// A summary run that cannot read the session's transcript generation does not
// start: it could record a failure only over whatever run owns the summary.
func TestASummaryRunThatCannotReadTheGenerationDoesNotStart(t *testing.T) {
	store := newStoreMock()
	store.deletionGenerationErr = errors.New("database is locked")
	called := make(chan string, 1)
	manager := NewManager(store, nil, summarizerMock{called: called}, nil, NewDetector(time.Hour), 0)

	manager.generateSummary(context.Background(), "session", time.Time{})

	select {
	case id := <-called:
		t.Errorf("summarizer called for %q", id)
	default:
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if status := store.status["session"]; status != "" {
		t.Errorf("summary status %q; want it untouched", status)
	}
}

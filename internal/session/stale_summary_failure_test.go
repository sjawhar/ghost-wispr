package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// A session's summary run that read the text from before a segment deletion,
// and ends after the deletion's own summary run started, records nothing:
// neither its refused summary nor its failed model call marks the newer run
// failed, which would let a Drive replacement go ahead before the newer
// summary is stored.
func TestAStaleSummaryRunDoesNotFailTheNewerRun(t *testing.T) {
	for _, old := range []struct {
		name string
		err  error
	}{
		{"summary refused", nil},
		{"model call failed", errors.New("summarize: model unavailable (503)")},
	} {
		t.Run(old.name, func(t *testing.T) {
			store, segmentID := r3SessionStore(t)
			summarizer := &r3GateSummarizer{entered: make(chan struct{}), release: make(chan struct{}), err: old.err}
			manager := NewManager(store, nil, summarizer, nil, nil, 0)

			done := make(chan struct{})
			go func() { manager.generateSummary(context.Background(), "r3", time.Now()); close(done) }()
			waitR3(t, summarizer.entered, "the summarizer did not start")
			deleteR3Session(t, store, segmentID)
			if err := store.StartSummary("r3"); err != nil {
				t.Fatalf("start the deletion's summary run: %v", err)
			}
			close(summarizer.release)
			waitR3(t, done, "the summary run did not finish")

			sess, err := store.GetSession("r3")
			if err != nil {
				t.Fatalf("get session: %v", err)
			}
			if sess.SummaryStatus != storage.SummaryRunning || sess.SummaryError != "" {
				t.Errorf("summary status %q, error %q; want the newer run's: %q, no error", sess.SummaryStatus, sess.SummaryError, storage.SummaryRunning)
			}
		})
	}
}

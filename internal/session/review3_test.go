package session

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// TestReview3NoSummarizerCompletesSummaryAfterDelete proves that the
// no-summarizer path aligns the summary generation before completing it after
// a segment deletion.
func TestReview3NoSummarizerCompletesSummaryAfterDelete(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "review3.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	if err := store.CreateSession("s", started); err != nil {
		t.Fatal(err)
	}
	for i, text := range []string{"before bridge", "private deletion token", "after bridge"} {
		if err := store.AppendSegment("s", transcribe.Segment{Text: text, StartTime: float64(i), EndTime: float64(i + 1), Timestamp: started.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.EndSession("s", started.Add(3*time.Second), "recording.mp3"); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store, nil, nil, nil, nil, 0)
	manager.generateSummary(context.Background(), "s", started)

	segments, err := store.GetSegments("s")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSegment("s", segments[1].ID, storage.DeletedSpan{AudioPath: "recording.mp3", Start: 1, End: 2, SHA256Before: "b", SHA256After: "a"}); err != nil {
		t.Fatal(err)
	}
	manager.generateSummary(context.Background(), "s", started)

	after, err := store.GetSession("s")
	if err != nil {
		t.Fatal(err)
	}
	if after.SummaryStatus != storage.SummaryCompleted && after.SummaryStatus != storage.SummaryFailed {
		t.Errorf("after a deletion with no summarizer, summary_status = %q: the Drive replacement waits forever", after.SummaryStatus)
	}
}

type emptySummarySummarizer struct{}

func (emptySummarySummarizer) Summarize(context.Context, string, string) (string, string, string, string, error) {
	return "Auto title", "", "default", "{}", nil
}

func TestGenerateSummaryDoesNotPublishEmptySummaryReady(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "empty-summary.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.CreateSession("empty", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSegment("empty", transcribe.Segment{Text: "transcript"}); err != nil {
		t.Fatal(err)
	}
	published := make(chan string, 1)
	manager := NewManager(store, nil, emptySummarySummarizer{}, nil, NewDetector(time.Hour), 0)
	manager.SetEventPublisher(publisherMock{called: published})

	manager.generateSummary(context.Background(), "empty", time.Now().UTC())

	select {
	case sessionID := <-published:
		t.Fatalf("published summary_ready for empty summary from session %q", sessionID)
	case <-time.After(100 * time.Millisecond):
	}
}

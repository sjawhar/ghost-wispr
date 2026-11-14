package storage

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const nestedUntimedSecret = "nesteduntimedsecret"

func TestNestedUntimedDeleteKeepsUnattributedSiblingText(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "nested-untimed-r19.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("n", started); err != nil {
		t.Fatal(err)
	}
	for i, text := range []string{"november one " + nestedUntimedSecret, "november two stays"} {
		if err := store.AppendSegment("n", transcribeSegment(text, i, started)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.EndSession("n", started.Add(time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession("s", started.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSegment("s", transcribeSegment("sierra live", 0, started.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("s", started.Add(61*time.Minute), "/audio/s.mp3"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET canonical_transcript = 'sierra live' WHERE id = 's'`); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("m1", []string{"n", "s"}, started, started.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET canonical_transcript = ? WHERE id = 'm1'`, "november one "+nestedUntimedSecret+"\nnovember two stays\nsierra live"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession("t", started.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("t", started.Add(181*time.Minute), "/audio/t.mp3"); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("m2", []string{"m1", "t"}, started, started.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("m2")
	if err != nil {
		t.Fatal(err)
	}
	for _, segment := range segments {
		if strings.Contains(segment.Text, nestedUntimedSecret) {
			if err := store.DeleteRefinedSegmentText("m2", segment.ID, 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	middle, err := store.GetSession("m1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(middle.CanonicalTranscript, nestedUntimedSecret) || !strings.Contains(middle.CanonicalTranscript, "november two stays") || !strings.Contains(middle.CanonicalTranscript, "sierra live") {
		t.Fatalf("intermediate canonical after untimed deletion = %q", middle.CanonicalTranscript)
	}
}

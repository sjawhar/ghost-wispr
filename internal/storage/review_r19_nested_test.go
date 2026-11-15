package storage

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const reviewR19Secret = "november deletion target"

func TestReviewR19NestedUntimedDeleteKeepsRepeatedPhrase(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	start := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	_ = store.CreateSession("n", start)
	_ = store.AppendSegment("n", transcribeSegment("okay", 0, start))
	_ = store.EndSession("n", start.Add(time.Minute), "")
	_ = store.CreateSession("s", start.Add(time.Hour))
	_ = store.AppendSegment("s", transcribeSegment("okay", 0, start.Add(time.Hour)))
	_ = store.AppendSegment("s", transcribeSegment("sierra live", 1, start.Add(time.Hour)))
	_ = store.EndSession("s", start.Add(61*time.Minute), "/audio/s.mp3")
	if err := store.MergeSessions("m1", []string{"n", "s"}, start, start.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET canonical_transcript = 'okay\nokay\nsierra live' WHERE id = 'm1'`); err != nil {
		t.Fatal(err)
	}
	_ = store.CreateSession("t", start.Add(3*time.Hour))
	_ = store.EndSession("t", start.Add(181*time.Minute), "/audio/t.mp3")
	if err := store.MergeSessions("m2", []string{"m1", "t"}, start, start.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	segs, _ := store.GetSegments("m2")
	deleted := false
	for _, s := range segs {
		if s.Text == "okay" && s.Untimed && s.UntimedSourceID == "n" && !deleted {
			if err := store.DeleteRefinedSegmentText("m2", s.ID, 0); err != nil {
				t.Fatal(err)
			}
			deleted = true
		}
	}
	if !deleted {
		t.Fatalf("control: n's untimed row was not found: %+v", segs)
	}
	m1, _ := store.GetSession("m1")
	if n := strings.Count(m1.CanonicalTranscript, "okay"); n != 1 {
		t.Errorf("m1 holds %d copies of the phrase, want 1 (s's copy kept)", n)
	}
	if !strings.Contains(m1.CanonicalTranscript, "sierra live") {
		t.Errorf("m1 lost s's other text")
	}
}

func TestReviewR19NestedUntimedDeleteKeepsAllOtherText(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "n.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	start := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	_ = store.CreateSession("n", start)
	_ = store.AppendSegment("n", transcribeSegment("november one "+reviewR19Secret, 0, start))
	_ = store.AppendSegment("n", transcribeSegment("november two stays", 1, start))
	_ = store.EndSession("n", start.Add(time.Minute), "")
	_ = store.CreateSession("s", start.Add(time.Hour))
	_ = store.AppendSegment("s", transcribeSegment("sierra live", 0, start.Add(time.Hour)))
	_ = store.EndSession("s", start.Add(61*time.Minute), "/audio/s.mp3")
	if _, err := store.DB().Exec(`UPDATE sessions SET canonical_transcript = 'sierra live' WHERE id = 's'`); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("m1", []string{"n", "s"}, start, start.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET canonical_transcript = 'november one ` + reviewR19Secret + `\nnovember two stays\nsierra live' WHERE id = 'm1'`); err != nil {
		t.Fatal(err)
	}
	_ = store.CreateSession("t", start.Add(3*time.Hour))
	_ = store.EndSession("t", start.Add(181*time.Minute), "/audio/t.mp3")
	if err := store.MergeSessions("m2", []string{"m1", "t"}, start, start.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	segs, _ := store.GetSegments("m2")
	var sawSierra bool
	for _, s := range segs {
		sawSierra = sawSierra || strings.Contains(s.Text, "sierra live")
		if strings.Contains(s.Text, reviewR19Secret) {
			if err := store.DeleteRefinedSegmentText("m2", s.ID, 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !sawSierra {
		t.Fatalf("control: m2 does not show s's text before the delete")
	}
	m1, _ := store.GetSession("m1")
	s, _ := store.GetSession("s")
	for name, got := range map[string]string{"m1": m1.CanonicalTranscript, "s": s.CanonicalTranscript} {
		if strings.Contains(got, reviewR19Secret) {
			t.Errorf("%s kept the deleted words", name)
		}
	}
	if !strings.Contains(m1.CanonicalTranscript, "november two stays") || !strings.Contains(m1.CanonicalTranscript, "sierra live") {
		t.Errorf("m1 lost text it still holds: %q", m1.CanonicalTranscript)
	}
	if !strings.Contains(s.CanonicalTranscript, "sierra live") {
		t.Errorf("s lost its own text: %q", s.CanonicalTranscript)
	}
}

// A row an intermediate holder cannot credit, because it names no source and
// no recording, or a source outside the merge, refuses the deletion rather
// than rebuilding the holder without that row.
func TestReviewR19NestedUntimedDeleteRefusesUncreditableHolderRow(t *testing.T) {
	for name, provenance := range map[string]string{
		"no source":      `source_session_id = '', recording_path = ''`,
		"unknown source": `source_session_id = 'gone', recording_path = ''`,
	} {
		t.Run(name, func(t *testing.T) {
			reviewR19RefusesUncreditableHolderRow(t, provenance)
		})
	}
}

func reviewR19RefusesUncreditableHolderRow(t *testing.T, provenance string) {
	t.Helper()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "uncreditable.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	start := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	for _, id := range []string{"n", "s"} {
		if err := store.CreateSession(id, start); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AppendSegment("n", transcribeSegment("november delete", 0, start)); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("n", start.Add(time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSegment("s", transcribeSegment("uncreditable holder text", 0, start)); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("s", start.Add(time.Minute), "/audio/s.mp3"); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("m1", []string{"n", "s"}, start, start.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession("t", start.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("t", start.Add(181*time.Minute), "/audio/t.mp3"); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("m2", []string{"m1", "t"}, start, start.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE segments SET ` + provenance + ` WHERE session_id = 'm2' AND text = 'uncreditable holder text'`); err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("m2")
	if err != nil {
		t.Fatal(err)
	}
	var target int64
	for i := range segments {
		if segments[i].Text == "november delete" {
			target = segments[i].ID
		}
	}
	if target == 0 {
		t.Fatal("control: missing untimed deletion target")
	}
	if err := store.DeleteRefinedSegmentText("m2", target, 0); !errors.Is(err, ErrMergeProvenanceUnavailable) {
		t.Fatalf("delete with an uncreditable intermediate-holder row = %v, want %v", err, ErrMergeProvenanceUnavailable)
	}
}

// n and x, neither recorded, merge into m1, which has no recording either, so
// merging m1 into m2 marks n's rows untimed a second time. They keep n as
// their source: deleting one scrubs n itself, not only m1.
func TestNestedUntimedDeleteScrubsTheInnerSourceItself(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "inner-untimed.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	start := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	for i, id := range []string{"n", "x"} {
		at := start.Add(time.Duration(i) * time.Hour)
		if err := store.CreateSession(id, at); err != nil {
			t.Fatal(err)
		}
		if err := store.AppendSegment(id, transcribeSegment(id+" one "+reviewR19Secret, 0, at)); err != nil {
			t.Fatal(err)
		}
		if err := store.AppendSegment(id, transcribeSegment(id+" two stays", 1, at)); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, at.Add(time.Minute), ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET canonical_transcript = 'n one ` + reviewR19Secret + `\nn two stays' WHERE id = 'n'`); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("m1", []string{"n", "x"}, start, start.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession("t", start.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("t", start.Add(181*time.Minute), "/audio/t.mp3"); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("m2", []string{"m1", "t"}, start, start.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("m2")
	if err != nil {
		t.Fatal(err)
	}
	var target int64
	for i := range segments {
		if segments[i].Text == "n one "+reviewR19Secret {
			target = segments[i].ID
		}
	}
	if target == 0 {
		t.Fatalf("control: n's row is missing from m2: %+v", segments)
	}
	if err := store.DeleteRefinedSegmentText("m2", target, 0); err != nil {
		t.Fatal(err)
	}
	n, err := store.GetSession("n")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(n.CanonicalTranscript, reviewR19Secret) || !strings.Contains(n.CanonicalTranscript, "n two stays") {
		t.Fatalf("n after deleting its row in m2 = %q, want its other row only", n.CanonicalTranscript)
	}
}

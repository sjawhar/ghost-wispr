package storage

import (
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"
)

// Sol's review of #195 at 17bd4e47 left these probes. The untimed one and the
// cleanup of erasures outside the app failed there; the other two passed,
// and stay as regression tests of the edge cases the review asked for.

// TestSol195MultipleCuts fails if a segment delete after any order of earlier
// "Delete text and audio" cuts, before the segment, after it or both, keeps
// the question heard in the segment, or erases one heard elsewhere.
func TestSol195MultipleCuts(t *testing.T) {
	for _, tc := range []struct {
		name string
		cuts []string
	}{
		{"uncut control", nil},
		{"two before", []string{"A.", "B."}},
		{"two after", []string{"C.", "D."}},
		{"before then after", []string{"A.", "D."}},
		{"after then before", []string{"D.", "A."}},
		{"four mixed", []string{"C.", "A.", "D.", "B."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestSQLiteStore(t)
			at := time.Now().UTC().Add(-time.Hour)
			if err := s.CreateSession("s", at); err != nil {
				t.Fatal(err)
			}
			if err := s.EndSession("s", at.Add(2*time.Minute), "recording.mp3", ""); err != nil {
				t.Fatal(err)
			}
			words := []RefinedWord{}
			for i, w := range []struct {
				text       string
				start, end float64
			}{
				{"A.", 2, 4}, {"B.", 10, 12}, {"Target.", 30, 31}, {"C.", 50, 52}, {"D.", 60, 62}, {"Kept.", 80, 81},
			} {
				words = append(words, RefinedWord{RecordingPath: "recording.mp3", RecordingSHA256: "hash0", Ordinal: i, Speaker: i, Text: w.text, Start: w.start, End: w.end})
			}
			if _, err := s.PublishRefinementAtGeneration("s", RefinedTranscript(words), "test", words, BuildRefinedSegments(words), 0); err != nil {
				t.Fatal(err)
			}
			askVoiceQuestion(t, s, "s", "target", at.Add(30200*time.Millisecond), at.Add(30700*time.Millisecond))
			askVoiceQuestion(t, s, "s", "kept", at.Add(80200*time.Millisecond), at.Add(80700*time.Millisecond))
			for i, cut := range tc.cuts {
				session, err := s.GetSession("s")
				if err != nil {
					t.Fatal(err)
				}
				segments, err := s.StoredSegments("s")
				if err != nil {
					t.Fatal(err)
				}
				var span *DeletedSpan
				var id int64
				for _, segment := range segments {
					if segment.Text == cut {
						id = segment.ID
						span = &DeletedSpan{SessionID: "s", AudioPath: segment.RecordingPath, Start: segment.StartTime, End: segment.EndTime, SHA256Before: segment.RecordingSHA256, SHA256After: "hash" + strconv.Itoa(i+1)}
					}
				}
				if span == nil {
					t.Fatalf("the cut %s has no segment", cut)
				}
				if err := s.DeleteRefinedSegmentAudio("s", id, session.RefinedRevision, span); err != nil {
					t.Fatal(err)
				}
				if err := s.MarkSpliceComplete(span); err != nil {
					t.Fatal(err)
				}
			}
			if got := voiceQuestionHeard(t, s); len(got) != 2 {
				t.Fatalf("control: questions before Target's delete = %q", got)
			}
			session, err := s.GetSession("s")
			if err != nil {
				t.Fatal(err)
			}
			if err := s.DeleteRefinedSegmentText("s", segmentID(t, s, "s", "Target."), session.RefinedRevision); err != nil {
				t.Fatal(err)
			}
			if got := voiceQuestionHeard(t, s); !slices.Equal(got, []string{"heard kept"}) {
				t.Fatalf("questions after Target's delete = %q, want only the kept one", got)
			}
		})
	}
}

// TestSol195ErasureFollowsNestedMerge fails if a segment's erasure stops
// refusing a late question of its source session once the session is merged
// twice and renamed, or if deleting the merged session leaves the erasure.
func TestSol195ErasureFollowsNestedMerge(t *testing.T) {
	s := newTestSQLiteStore(t)
	segmentTestSessionWith(t, s, "s", func(time.Time) {})
	session, err := s.GetSession("s")
	if err != nil {
		t.Fatal(err)
	}
	if err := deleteRefinedTextTestSegment(t, s, "s", segmentID(t, s, "s", "deleted segment words"), TextDeletionSpan{AudioPath: "recording.mp3", Start: 2, End: 4, SHA256: "b"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"b", "c"} {
		if err := s.CreateSession(id, session.StartedAt.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := s.EndSession(id, session.StartedAt.Add(2*time.Hour), "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MergeSessions("m1", []string{"s", "b"}, session.StartedAt, session.StartedAt.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.MergeSessions("m2", []string{"m1", "c"}, session.StartedAt, session.StartedAt.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`UPDATE sessions SET title = 'renamed' WHERE id = 'm2'`); err != nil {
		t.Fatal(err)
	}
	q := VoiceQuestion{SessionID: "s", AskedAt: session.StartedAt.Add(3 * time.Second), HeardFrom: session.StartedAt.Add(2500 * time.Millisecond), HeardTo: session.StartedAt.Add(3500 * time.Millisecond), Heard: "erased", Outcome: VoiceQuestionAnswered}
	if err := s.RecordVoiceQuestion(&q); !errors.Is(err, ErrVoiceQuestionErased) {
		t.Fatalf("a late question of the source session after two merges: %v, want ErrVoiceQuestionErased", err)
	}
	if _, err := s.DeleteSession("m2", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM voice_question_erasures`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("erasures after the delete: %d (%v), want 0", n, err)
	}
}

// TestSol195ErasureOutsideDelete fails if a session deleted with foreign keys
// off, as the sqlite3 shell does by default, leaves its erasures once the unit
// starts again.
func TestSol195ErasureOutsideDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "review.db")
	s, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	segmentTestSessionWith(t, s, "s", func(time.Time) {})
	if err := deleteRefinedTextTestSegment(t, s, "s", segmentID(t, s, "s", "deleted segment words"), TextDeletionSpan{AudioPath: "recording.mp3", Start: 2, End: 4, SHA256: "b"}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM voice_question_erasures WHERE session_id = 's'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("control: %d erasures (%v), want 1", n, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	outside, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := outside.Exec(`DELETE FROM sessions WHERE id = 's'`); err != nil {
		t.Fatal(err)
	}
	if err := outside.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.DB().QueryRow(`SELECT count(*) FROM voice_question_erasures WHERE session_id = 's'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("erasures after the outside delete and a start: %d (%v), want 0", n, err)
	}
}

// TestSol195BackupScrubDropsErasures fails if the backup's scrub for a
// session delete leaves the session's erasures in the backup, and keeps
// another session's: the scrub deletes the rows of every table with a
// session column, and the erasures have one.
func TestSol195BackupScrubDropsErasures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup.db")
	s, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"s", "kept"} {
		segmentTestSessionWith(t, s, id, func(time.Time) {})
		if err := deleteRefinedTextTestSegment(t, s, id, segmentID(t, s, id, "deleted segment words"), TextDeletionSpan{AudioPath: "recording.mp3", Start: 2, End: 4, SHA256: "b"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	backup, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backup.Close() })
	if err := scrubBackupSessions(backup, []string{"s"}); err != nil {
		t.Fatal(err)
	}
	rows, err := backup.Query(`SELECT session_id FROM voice_question_erasures ORDER BY session_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var left []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		left = append(left, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(left, []string{"kept"}) {
		t.Fatalf("backup erasures after the scrub name %q, want only the kept session", left)
	}
}

// TestSol195UntimedEarlierReceiptNotCapture fails if a delete of an untimed
// segment keeps the question heard in its words when the result before it
// was stored long after its own words, as results replayed after an outage
// are: a stored time is no capture time, and bounds nothing before it. The
// control's earlier result is stored promptly.
func TestSol195UntimedEarlierReceiptNotCapture(t *testing.T) {
	for _, late := range []bool{false, true} {
		name := "prompt earlier result control"
		earlierStored := 5 * time.Second
		if late {
			name = "earlier result stored late"
			earlierStored = 20 * time.Second
		}
		t.Run(name, func(t *testing.T) {
			s := newTestSQLiteStore(t)
			at := time.Now().UTC().Add(-time.Hour)
			if err := s.CreateSession("source", at); err != nil {
				t.Fatal(err)
			}
			earlier := transcribeSegment("Earlier utterance.", 0, at.Add(earlierStored))
			earlier.StartTime, earlier.EndTime = 0, 2
			target := transcribeSegment("Target utterance.", 1, at.Add(30*time.Second))
			target.StartTime, target.EndTime = 10, 12
			if err := s.AppendSegment("source", earlier); err != nil {
				t.Fatal(err)
			}
			if err := s.AppendSegment("source", target); err != nil {
				t.Fatal(err)
			}
			if err := s.EndSession("source", at.Add(time.Minute), "", ""); err != nil {
				t.Fatal(err)
			}
			if err := s.CreateSession("other", at.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			if err := s.EndSession("other", at.Add(2*time.Minute), "", ""); err != nil {
				t.Fatal(err)
			}
			askVoiceQuestion(t, s, "source", "deleted-words-marker", at.Add(10500*time.Millisecond), at.Add(11500*time.Millisecond))
			askVoiceQuestion(t, s, "source", "kept-words-marker", at.Add(40*time.Second), at.Add(41*time.Second))
			if got := voiceQuestionHeard(t, s); len(got) != 2 {
				t.Fatalf("control: questions = %q", got)
			}
			if err := s.MergeSessions("merged", []string{"source", "other"}, at, at.Add(2*time.Minute)); err != nil {
				t.Fatal(err)
			}
			if err := s.DeleteRefinedSegmentText("merged", segmentID(t, s, "merged", "Target utterance."), 0); err != nil {
				t.Fatal(err)
			}
			if got := voiceQuestionHeard(t, s); !slices.Equal(got, []string{"heard kept-words-marker"}) {
				t.Fatalf("questions after the untimed segment's delete = %q, want only the kept one", got)
			}
		})
	}
}

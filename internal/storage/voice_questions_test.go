package storage

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// askVoiceQuestion records a question of session sessionID heard from from
// to to, whose heard text, vendor wording and answer all hold words.
func askVoiceQuestion(t *testing.T, store *SQLiteStore, sessionID, words string, from, to time.Time) {
	t.Helper()
	q := VoiceQuestion{
		SessionID: sessionID, AskedAt: to.Add(300 * time.Millisecond), HeardFrom: from, HeardTo: to,
		Heard: "heard " + words, VendorWording: "vendor " + words, Engine: "tts",
		Outcome: VoiceQuestionAnswered, Answer: "answer " + words, Took: 2 * time.Second,
	}
	if err := store.RecordVoiceQuestion(&q); err != nil {
		t.Fatalf("record the question %q: %v", words, err)
	}
}

// voiceQuestionHeard lists the heard text of every question the database
// holds, in the order they were asked.
func voiceQuestionHeard(t *testing.T, store *SQLiteStore) []string {
	t.Helper()
	questions, err := ReadVoiceQuestions(store.DB(), time.Time{})
	if err != nil {
		t.Fatalf("read the questions: %v", err)
	}
	heard := []string{}
	for i := range questions {
		heard = append(heard, questions[i].Heard)
	}
	return heard
}

// TestAVoiceQuestionKeepsWhatWasAskedAndAnswered fails if a question's row
// does not read back as it was recorded, its absent first audio included, or
// if --since's time does not keep only the questions asked at or after it.
func TestAVoiceQuestionKeepsWhatWasAskedAndAnswered(t *testing.T) {
	store := newTestSQLiteStore(t)
	started := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("s1", started); err != nil {
		t.Fatal(err)
	}
	firstAudio := 1250 * time.Millisecond
	want := []VoiceQuestion{
		{
			SessionID: "s1", AskedAt: started.Add(10*time.Second + 500*time.Millisecond),
			HeardFrom: started.Add(8 * time.Second), HeardTo: started.Add(10 * time.Second),
			Heard: "what is happening with ABC-12?", VendorWording: "What's the status of ABC-12?", Engine: "gptlive",
			IssueKey: "ABC-12", Outcome: VoiceQuestionAskedAgent, Answer: "ABC-12: PR 80 is in review. I asked the agent on ABC-12.",
			FirstAudio: &firstAudio, Took: 4200 * time.Millisecond,
		},
		{
			SessionID: "s1", AskedAt: started.Add(time.Minute),
			HeardFrom: started.Add(58 * time.Second), HeardTo: started.Add(59 * time.Second),
			Heard: "okay, let's move on", Engine: "tts", Outcome: VoiceQuestionSideTalk, Took: 100 * time.Millisecond,
		},
	}
	for i := range want {
		if err := store.RecordVoiceQuestion(&want[i]); err != nil {
			t.Fatalf("record question %d: %v", i, err)
		}
	}
	got, err := ReadVoiceQuestions(store.DB(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("read %d questions, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].ID == 0 {
			t.Errorf("question %d has no id", i)
		}
		g, w := got[i], want[i]
		g.ID, w.ID = 0, 0
		if (g.FirstAudio == nil) != (w.FirstAudio == nil) || g.FirstAudio != nil && *g.FirstAudio != *w.FirstAudio {
			t.Errorf("question %d first audio = %v, want %v", i, g.FirstAudio, w.FirstAudio)
		}
		g.FirstAudio, w.FirstAudio = nil, nil
		if !g.AskedAt.Equal(w.AskedAt) || !g.HeardFrom.Equal(w.HeardFrom) || !g.HeardTo.Equal(w.HeardTo) {
			t.Errorf("question %d times = %v %v..%v, want %v %v..%v", i, g.AskedAt, g.HeardFrom, g.HeardTo, w.AskedAt, w.HeardFrom, w.HeardTo)
		}
		g.AskedAt, g.HeardFrom, g.HeardTo, w.AskedAt, w.HeardFrom, w.HeardTo = time.Time{}, time.Time{}, time.Time{}, time.Time{}, time.Time{}, time.Time{}
		if g != w {
			t.Errorf("question %d = %+v, want %+v", i, g, w)
		}
	}
	since, err := ReadVoiceQuestions(store.DB(), started.Add(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(since) != 1 || since[0].Heard != "okay, let's move on" {
		t.Fatalf("questions since 09:00:30 = %+v, want only the side talk", since)
	}
}

// TestAVoiceQuestionNeedsItsSessionAndAKnownOutcome: a question names a
// session the database holds, so no question outlives the session a delete
// removed, and one of the outcomes the log knows.
func TestAVoiceQuestionNeedsItsSessionAndAKnownOutcome(t *testing.T) {
	store := newTestSQLiteStore(t)
	at := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	if err := store.RecordVoiceQuestion(&VoiceQuestion{SessionID: "no-such-session", AskedAt: at, HeardFrom: at, HeardTo: at, Outcome: VoiceQuestionAnswered}); err == nil {
		t.Error("a question was recorded for a session the database does not hold")
	}
	if err := store.CreateSession("s1", at); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordVoiceQuestion(&VoiceQuestion{SessionID: "s1", AskedAt: at, HeardFrom: at, HeardTo: at, Outcome: "shrugged"}); err == nil {
		t.Error("a question was recorded with an outcome the log does not know")
	}
	for _, outcome := range VoiceQuestionOutcomes {
		if err := store.RecordVoiceQuestion(&VoiceQuestion{SessionID: "s1", AskedAt: at, HeardFrom: at, HeardTo: at, Outcome: outcome}); err != nil {
			t.Errorf("outcome %q refused: %v", outcome, err)
		}
	}
}

// TestReadingQuestionsOfADatabaseWithoutTheLogSaysSo fails if a database
// from a build before the question log reads as one with no questions.
func TestReadingQuestionsOfADatabaseWithoutTheLogSaysSo(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if questions, err := ReadVoiceQuestions(db, time.Time{}); !errors.Is(err, ErrNoQuestionLog) {
		t.Fatalf("ReadVoiceQuestions = %+v, %v; want ErrNoQuestionLog", questions, err)
	}
}

// TestSessionDeleteRemovesVoiceQuestions: a person's delete of a merged
// session removes the questions of it and of the sessions merged into it,
// and keeps another session's.
func TestSessionDeleteRemovesVoiceQuestions(t *testing.T) {
	store := newTestSQLiteStore(t)
	started := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	for i, id := range []string{"a", "b", "kept"} {
		at := started.Add(time.Duration(i) * 5 * time.Minute)
		if err := store.CreateSession(id, at); err != nil {
			t.Fatal(err)
		}
		askVoiceQuestion(t, store, id, "of "+id, at.Add(10*time.Second), at.Add(12*time.Second))
		if err := store.EndSession(id, at.Add(time.Minute), "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.MergeSessions("merged", []string{"a", "b"}, started, started.Add(15*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteSession("merged", t.TempDir()); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if got := voiceQuestionHeard(t, store); !slices.Equal(got, []string{"heard of kept"}) {
		t.Fatalf("questions after the delete = %q, want only the kept session's", got)
	}
}

// TestAgeDeleteRemovesItsVoiceQuestions: GC's age delete removes the deleted
// session's questions with it.
func TestAgeDeleteRemovesItsVoiceQuestions(t *testing.T) {
	store := newTestSQLiteStore(t)
	audioDir := t.TempDir()
	endedAndSynced(t, store, "old", filepath.Join(audioDir, "old.mp3"), RefinementCompleted)
	endedWith(t, store, "kept", filepath.Join(audioDir, "kept.mp3"), RefinementCompleted)
	at := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	askVoiceQuestion(t, store, "old", "of old", at, at.Add(time.Second))
	askVoiceQuestion(t, store, "kept", "of kept", at, at.Add(time.Second))

	if _, removed, err := store.DeleteEndedSessionAndSources("old", filepath.Join(audioDir, "old.mp3"), audioDir); err != nil || !removed {
		t.Fatalf("age delete = %v, %v; want deleted", removed, err)
	}
	if got := voiceQuestionHeard(t, store); !slices.Equal(got, []string{"heard of kept"}) {
		t.Fatalf("questions after the age delete = %q, want only the kept session's", got)
	}
}

// TestWALEraseCoversVoiceQuestions: a deleted session's questions, their
// heard text, vendor wording and answer, are in no file of the database once
// the delete returns, the write-ahead log included, while a kept session's
// question still is.
func TestWALEraseCoversVoiceQuestions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "voice.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	for _, id := range []string{"deleted", "kept"} {
		if err := store.CreateSession(id, started); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, started.Add(time.Minute), "", ""); err != nil {
			t.Fatal(err)
		}
	}
	askVoiceQuestion(t, store, "deleted", "deleted question words", started, started.Add(time.Second))
	askVoiceQuestion(t, store, "kept", "kept question words", started, started.Add(time.Second))
	log, err := os.ReadFile(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"heard", "vendor", "answer"} {
		if !bytes.Contains(log, []byte(column+" deleted question words")) {
			t.Fatalf("control: the write-ahead log does not hold the %s text before the delete", column)
		}
	}

	if _, err := store.DeleteSession("deleted", t.TempDir()); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	for _, column := range []string{"heard", "vendor", "answer"} {
		assertDatabaseFilesLack(t, path, column+" deleted question words", column+" kept question words")
	}
}

// TestSegmentDeleteErasesTheQuestionsHeardInIt: "Delete text" and "Delete
// text and audio" of a segment erase, in their transaction and from every
// file of the database, each question whose heard span overlaps the
// segment's span on the wall clock, and keep a question heard in another
// segment of the session.
func TestSegmentDeleteErasesTheQuestionsHeardInIt(t *testing.T) {
	for _, deletion := range []string{"text", "text and audio"} {
		t.Run(deletion, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "voice.db")
			store, err := NewSQLiteStore(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			// "deleted segment words" is heard 10:00:02 to 10:00:04, "after
			// bridge" 10:00:04 to 10:00:06.
			segmentTestSessionWith(t, store, "s", func(started time.Time) {
				askVoiceQuestion(t, store, "s", "deleted question words", started.Add(2500*time.Millisecond), started.Add(3500*time.Millisecond))
				askVoiceQuestion(t, store, "s", "kept question words", started.Add(5500*time.Millisecond), started.Add(5900*time.Millisecond))
			})
			deleted := segmentID(t, store, "s", "deleted segment words")
			if deletion == "text" {
				err = deleteRefinedTextTestSegment(t, store, "s", deleted, TextDeletionSpan{AudioPath: "recording.mp3", Start: 2, End: 4, SHA256: "b"})
			} else {
				err = deleteRefinedAudioTestSegment(t, store, "s", deleted, DeletedSpan{AudioPath: "recording.mp3", Start: 2, End: 4, SHA256Before: "b", SHA256After: "a"})
			}
			if err != nil {
				t.Fatalf("delete the segment: %v", err)
			}
			if got := voiceQuestionHeard(t, store); !slices.Equal(got, []string{"heard kept question words"}) {
				t.Fatalf("questions after the delete = %q, want only the one heard after it", got)
			}
			for _, column := range []string{"heard", "vendor", "answer"} {
				assertDatabaseFilesLack(t, path, column+" deleted question words", column+" kept question words")
			}
		})
	}
}

// TestSegmentDeleteInAMergedSessionErasesByItsRecordingsTime: a segment of a
// merged session is placed on the wall clock by the start of the session
// that recorded it, not the merged session's: a question heard in that
// recording is erased, and one heard at the same offset into the merged
// session's start is kept.
func TestSegmentDeleteInAMergedSessionErasesByItsRecordingsTime(t *testing.T) {
	store := newTestSQLiteStore(t)
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("first", started); err != nil {
		t.Fatal(err)
	}
	askVoiceQuestion(t, store, "first", "at the merge's start", started.Add(200*time.Millisecond), started.Add(800*time.Millisecond))
	if err := store.EndSession("first", started.Add(time.Minute), "/audio/first.mp3", ""); err != nil {
		t.Fatal(err)
	}
	later := started.Add(time.Hour)
	if err := store.CreateSession("second", later); err != nil {
		t.Fatal(err)
	}
	askVoiceQuestion(t, store, "second", "in the deleted words", later.Add(200*time.Millisecond), later.Add(800*time.Millisecond))
	if err := store.EndSession("second", later.Add(time.Minute), "/audio/second.mp3", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("merged", []string{"first", "second"}, started, later.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	words := []RefinedWord{
		{RecordingPath: "/audio/first.mp3", RecordingSHA256: "first", RecordingOrdinal: 0, Ordinal: 0, Text: "Alpha words.", Start: 30, End: 31},
		{RecordingPath: "/audio/second.mp3", RecordingSHA256: "second", RecordingOrdinal: 1, Ordinal: 0, Text: "Bravo words.", Start: 0, End: 1},
	}
	revision, err := store.PublishRefinementAtGeneration("merged", RefinedTranscript(words), "test", words, BuildRefinedSegments(words), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteRefinedSegmentText("merged", segmentID(t, store, "merged", "Bravo words."), revision); err != nil {
		t.Fatalf("delete the second recording's segment: %v", err)
	}
	if got := voiceQuestionHeard(t, store); !slices.Equal(got, []string{"heard at the merge's start"}) {
		t.Fatalf("questions after the delete = %q, want only the one heard at the merge's start", got)
	}
}

// TestUntimedSegmentDeleteErasesTheQuestionsHeardInIt: an untimed segment, of
// a merged source with no recording, holds the time it was stored, just after
// its words: a question heard in it is erased, and one heard a minute later
// is kept.
func TestUntimedSegmentDeleteErasesTheQuestionsHeardInIt(t *testing.T) {
	store := newTestSQLiteStore(t)
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("without-recording", started); err != nil {
		t.Fatal(err)
	}
	// Its words are said 09:00:00 to 09:00:02 and stored at 09:00:03.
	stored := started.Add(3 * time.Second)
	if err := store.AppendSegment("without-recording", transcribeSegment("alpha words without a recording", 0, stored)); err != nil {
		t.Fatal(err)
	}
	askVoiceQuestion(t, store, "without-recording", "in the untimed words", started.Add(500*time.Millisecond), started.Add(2*time.Second))
	askVoiceQuestion(t, store, "without-recording", "a minute later", started.Add(time.Minute), started.Add(61*time.Second))
	if err := store.EndSession("without-recording", started.Add(2*time.Minute), "", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession("with-recording", started.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("with-recording", started.Add(61*time.Minute), "/audio/with-recording.mp3", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("merged", []string{"without-recording", "with-recording"}, started, started.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	words := []RefinedWord{{RecordingPath: "/audio/with-recording.mp3", RecordingSHA256: "recording", Ordinal: 0, Text: "Bravo words.", Start: 0, End: 1}}
	if _, err := store.PublishRefinementAtGeneration("merged", RefinedTranscript(words), "test", words, BuildRefinedSegments(words), 0); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteRefinedSegmentText("merged", segmentID(t, store, "merged", "alpha words"), 0); err != nil {
		t.Fatalf("delete the untimed segment: %v", err)
	}
	if got := voiceQuestionHeard(t, store); !slices.Equal(got, []string{"heard a minute later"}) {
		t.Fatalf("questions after the delete = %q, want only the one a minute later", got)
	}
}

// TestADeleteMadeOutsideTheAppLeavesNoQuestion: a session deleted with
// foreign keys off, as the sqlite3 shell does by default, leaves its
// questions and its questions to agents behind; the next start deletes them
// with what else it left.
func TestADeleteMadeOutsideTheAppLeavesNoQuestion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "voice.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	for _, id := range []string{"deleted", "kept"} {
		if err := store.CreateSession(id, started); err != nil {
			t.Fatal(err)
		}
		askVoiceQuestion(t, store, id, "of "+id, started, started.Add(time.Second))
		askVoiceTask(t, store, "task-"+id, id, "answer of "+id)
		if err := store.EndSession(id, started.Add(time.Minute), "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	outside, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := outside.Exec(`DELETE FROM sessions WHERE id = 'deleted'`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"voice_questions", "voice_tasks"} {
		var orphans int
		if err := outside.QueryRow(`SELECT count(*) FROM ` + table + ` WHERE session_id = 'deleted'`).Scan(&orphans); err != nil || orphans != 1 {
			t.Fatalf("control: the outside delete left %d rows in %s (%v), want 1", orphans, table, err)
		}
	}
	if err := outside.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if got := voiceQuestionHeard(t, store); !slices.Equal(got, []string{"heard of kept"}) {
		t.Fatalf("questions after the start = %q, want only the kept session's", got)
	}
	if got := voiceTaskIDs(t, store); !slices.Equal(got, []string{"task-kept"}) {
		t.Fatalf("questions to agents after the start = %q, want only the kept session's", got)
	}
}

// segmentTestSessionWith is segmentTestSession, with during run while the
// session records, before it ends.
func segmentTestSessionWith(t *testing.T, store *SQLiteStore, id string, during func(started time.Time)) {
	t.Helper()
	started := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	if err := store.CreateSession(id, started); err != nil {
		t.Fatalf("create session: %v", err)
	}
	for i, text := range []string{"before bridge", "deleted segment words", "after bridge"} {
		segment := transcribeSegment(text, i, started)
		segment.Speaker = i % 2
		if err := store.AppendSegment(id, segment); err != nil {
			t.Fatalf("append segment: %v", err)
		}
	}
	during(started)
	if err := store.EndSession(id, started.Add(6*time.Second), "recording.mp3", ""); err != nil {
		t.Fatalf("end session: %v", err)
	}
}

// segmentID is the id of the segment of session sessionID whose text holds
// words.
func segmentID(t *testing.T, store *SQLiteStore, sessionID, words string) int64 {
	t.Helper()
	segments, err := store.GetSegments(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	for i := range segments {
		if strings.Contains(segments[i].Text, words) {
			return segments[i].ID
		}
	}
	t.Fatalf("session %s has no segment that holds %q", sessionID, words)
	return 0
}

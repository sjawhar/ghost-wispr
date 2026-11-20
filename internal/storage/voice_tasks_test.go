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

// askVoiceTask records a working voice task for session sessionID whose
// written answer, once it lands, is answer.
func askVoiceTask(t *testing.T, store *SQLiteStore, id, sessionID, answer string) {
	t.Helper()
	asked := time.Date(2026, 11, 19, 9, 30, 0, 0, time.UTC)
	if err := store.CreateVoiceTask(&VoiceTask{ID: id, SessionID: sessionID, AskedAt: asked, IssueKey: "ABC-1", Holder: "session:agent-1"}); err != nil {
		t.Fatalf("create voice task %s: %v", id, err)
	}
	if answer == "" {
		return
	}
	if _, err := store.AnswerVoiceTask(id, answer); err != nil {
		t.Fatalf("answer voice task %s: %v", id, err)
	}
}

// voiceTaskIDs lists every voice task id the database holds, sorted.
func voiceTaskIDs(t *testing.T, store *SQLiteStore) []string {
	t.Helper()
	rows, err := store.DB().Query(`SELECT id FROM voice_tasks ORDER BY id`)
	if err != nil {
		t.Fatalf("list voice tasks: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

// TestVoiceTasksGainTheirAskedNumber fails if a database whose voice_tasks
// table predates asked_number does not gain it, with 0 (asked first) on the
// rows it holds; or if a new task does not keep its number, through a rekey
// too, or the highest number is not read back.
func TestVoiceTasksGainTheirAskedNumber(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unit.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession("s1", time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	askVoiceTask(t, store, "old", "s1", "")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// The database as a build before asked_number left it.
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`ALTER TABLE voice_tasks DROP COLUMN asked_number`); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	task, err := store.VoiceTask("old")
	if err != nil || task.AskedNumber != 0 {
		t.Fatalf("a task from before asked_number = %+v, %v; want it read with number 0", task, err)
	}
	asked := time.Date(2026, 11, 19, 9, 31, 0, 0, time.UTC)
	if err := store.CreateVoiceTask(&VoiceTask{ID: "local", SessionID: "s1", AskedAt: asked, Holder: "session:agent-2", AskedNumber: 7}); err != nil {
		t.Fatal(err)
	}
	if err := store.RekeyVoiceTask("local", "evt-7"); err != nil {
		t.Fatal(err)
	}
	if task, err := store.VoiceTask("evt-7"); err != nil || task.AskedNumber != 7 {
		t.Fatalf("the rekeyed task = %+v, %v; want number 7", task, err)
	}
	if highest, err := store.MaxVoiceTaskNumber(); err != nil || highest != 7 {
		t.Fatalf("MaxVoiceTaskNumber = %d, %v; want 7", highest, err)
	}
}

// TestVoiceTasksListInAskedOrderWithinASecond: two tasks asked 0.5 s and
// 0.55 s into one second list in the order they were asked.
func TestVoiceTasksListInAskedOrderWithinASecond(t *testing.T) {
	store := newTestSQLiteStore(t)
	if err := store.CreateSession("s1", time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	for id, nanos := range map[string]int{"b-first": 500_000_000, "a-second": 550_000_000} {
		asked := time.Date(2026, 11, 19, 9, 30, 0, nanos, time.UTC)
		if err := store.CreateVoiceTask(&VoiceTask{ID: id, SessionID: "s1", AskedAt: asked}); err != nil {
			t.Fatal(err)
		}
	}
	tasks, err := store.VoiceTasks("s1")
	if err != nil || len(tasks) != 2 || tasks[0].ID != "b-first" || !tasks[0].AskedAt.Equal(time.Date(2026, 11, 19, 9, 30, 0, 500_000_000, time.UTC)) {
		t.Fatalf("tasks = %+v, %v; want b-first, asked at .5 s, before a-second", tasks, err)
	}
}

// TestAVoiceTaskKeepsItsWrittenAnswer: a task starts working, takes the
// holder's written answer once, takes the Envoy event id as its id once the
// question is sent, and records when its answer was spoken.
func TestAVoiceTaskKeepsItsWrittenAnswer(t *testing.T) {
	store := newTestSQLiteStore(t)
	if err := store.CreateSession("s1", time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	askVoiceTask(t, store, "local-1", "s1", "")
	if err := store.RekeyVoiceTask("local-1", "event-1"); err != nil {
		t.Fatalf("rekey: %v", err)
	}
	if _, err := store.VoiceTask("local-1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("the task is still found under its local id: %v", err)
	}
	task, err := store.VoiceTask("event-1")
	if err != nil {
		t.Fatal(err)
	}
	if task.State != VoiceTaskWorking || task.SessionID != "s1" || task.IssueKey != "ABC-1" || task.Holder != "session:agent-1" || task.AnswerWritten != "" || task.SpokenAt != nil {
		t.Fatalf("new task = %+v", task)
	}

	answered, err := store.AnswerVoiceTask("event-1", "PR #12 merged; INFRA-7 is next.")
	if err != nil || !answered {
		t.Fatalf("answer = %v, %v; want answered", answered, err)
	}
	if again, err := store.AnswerVoiceTask("event-1", "a second answer"); err != nil || again {
		t.Fatalf("a second answer = %v, %v; want ignored", again, err)
	}
	spoken := time.Date(2026, 11, 19, 9, 31, 0, 0, time.UTC)
	if err := store.MarkVoiceTaskSpoken("event-1", spoken); err != nil {
		t.Fatal(err)
	}
	task, err = store.VoiceTask("event-1")
	if err != nil {
		t.Fatal(err)
	}
	if task.State != VoiceTaskDone || task.AnswerWritten != "PR #12 merged; INFRA-7 is next." || task.SpokenAt == nil || !task.SpokenAt.Equal(spoken) {
		t.Fatalf("answered task = %+v", task)
	}
	if _, err := store.AnswerVoiceTask("missing", "x"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("answer of a missing task: %v, want sql.ErrNoRows", err)
	}
}

// TestAWorkingVoiceTaskCanBeReplacedOrCancelled: only a working task moves to
// replaced or cancelled; an answer still lands on a replaced one, for the
// session's page, without making it done.
func TestAWorkingVoiceTaskCanBeReplacedOrCancelled(t *testing.T) {
	store := newTestSQLiteStore(t)
	if err := store.CreateSession("s1", time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	askVoiceTask(t, store, "old", "s1", "")
	askVoiceTask(t, store, "gone", "s1", "")
	if moved, err := store.EndVoiceTask("old", VoiceTaskReplaced); err != nil || !moved {
		t.Fatalf("replace = %v, %v", moved, err)
	}
	if moved, err := store.EndVoiceTask("gone", VoiceTaskCancelled); err != nil || !moved {
		t.Fatalf("cancel = %v, %v", moved, err)
	}
	if moved, err := store.EndVoiceTask("old", VoiceTaskCancelled); err != nil || moved {
		t.Fatalf("cancel of a replaced task = %v, %v; want left", moved, err)
	}
	if _, err := store.EndVoiceTask("old", VoiceTaskDone); err == nil {
		t.Fatal("EndVoiceTask accepted done, which only an answer sets")
	}
	if answered, err := store.AnswerVoiceTask("old", "late answer"); err != nil || answered {
		t.Fatalf("late answer = %v, %v; want kept but not done", answered, err)
	}
	task, err := store.VoiceTask("old")
	if err != nil {
		t.Fatal(err)
	}
	if task.State != VoiceTaskReplaced || task.AnswerWritten != "late answer" {
		t.Fatalf("replaced task after a late answer = %+v", task)
	}
}

// TestEndingASessionCancelsItsWorkingTasks: when a meeting ends with a
// question still out, its task becomes cancelled; an answered task and
// another session's working task stay as they are.
func TestEndingASessionCancelsItsWorkingTasks(t *testing.T) {
	store := newTestSQLiteStore(t)
	for _, id := range []string{"s1", "s2"} {
		if err := store.CreateSession(id, time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)); err != nil {
			t.Fatal(err)
		}
	}
	askVoiceTask(t, store, "open", "s1", "")
	askVoiceTask(t, store, "answered", "s1", "It merged.")
	askVoiceTask(t, store, "other", "s2", "")
	cancelled, err := store.CancelWorkingVoiceTasks("s1")
	if err != nil || len(cancelled) != 1 || cancelled[0].ID != "open" {
		t.Fatalf("CancelWorkingVoiceTasks = %+v, %v; want the open task", cancelled, err)
	}
	for id, want := range map[string]VoiceTaskState{"open": VoiceTaskCancelled, "answered": VoiceTaskDone, "other": VoiceTaskWorking} {
		if task, err := store.VoiceTask(id); err != nil || task.State != want {
			t.Errorf("task %s = %+v, %v; want %s", id, task, err, want)
		}
	}
}

// TestAVoiceTaskNeedsItsSession: a task names a session the database holds,
// so no task outlives the session a delete removed.
func TestAVoiceTaskNeedsItsSession(t *testing.T) {
	store := newTestSQLiteStore(t)
	err := store.CreateVoiceTask(&VoiceTask{ID: "t", SessionID: "no-such-session", AskedAt: time.Now()})
	if err == nil {
		t.Fatal("a voice task was recorded for a session the database does not hold")
	}
}

// TestSessionDeleteRemovesVoiceTasks (c-record): a person's delete of a
// merged session removes the voice tasks of it and of the sessions merged
// into it, and keeps another session's.
func TestSessionDeleteRemovesVoiceTasks(t *testing.T) {
	store := newTestSQLiteStore(t)
	started := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	for i, id := range []string{"a", "b", "kept"} {
		at := started.Add(time.Duration(i) * 5 * time.Minute)
		if err := store.CreateSession(id, at); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, at.Add(time.Minute), ""); err != nil {
			t.Fatal(err)
		}
	}
	askVoiceTask(t, store, "task-a", "a", "answer a")
	askVoiceTask(t, store, "task-b", "b", "")
	if err := store.MergeSessions("merged", []string{"a", "b"}, started, started.Add(15*time.Minute)); err != nil {
		t.Fatal(err)
	}
	askVoiceTask(t, store, "task-merged", "merged", "answer merged")
	askVoiceTask(t, store, "task-kept", "kept", "answer kept")

	if _, err := store.DeleteSession("merged", t.TempDir()); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if got := voiceTaskIDs(t, store); !slices.Equal(got, []string{"task-kept"}) {
		t.Fatalf("voice tasks after the delete = %q, want only task-kept", got)
	}
}

// TestAgeDeleteRemovesItsVoiceTasks: GC's age delete removes the deleted
// session's voice tasks with it.
func TestAgeDeleteRemovesItsVoiceTasks(t *testing.T) {
	store := newTestSQLiteStore(t)
	audioDir := t.TempDir()
	endedAndSynced(t, store, "old", filepath.Join(audioDir, "old.mp3"), RefinementCompleted)
	endedWith(t, store, "kept", filepath.Join(audioDir, "kept.mp3"), RefinementCompleted)
	askVoiceTask(t, store, "task-old", "old", "answer old")
	askVoiceTask(t, store, "task-kept", "kept", "")

	if _, removed, err := store.DeleteEndedSessionAndSources("old", filepath.Join(audioDir, "old.mp3"), audioDir); err != nil || !removed {
		t.Fatalf("age delete = %v, %v; want deleted", removed, err)
	}
	if got := voiceTaskIDs(t, store); !slices.Equal(got, []string{"task-kept"}) {
		t.Fatalf("voice tasks after the age delete = %q, want only task-kept", got)
	}
}

// TestWALEraseCoversVoiceTasks (c-record): a deleted session's written
// answers are in no file of the database once the delete returns, the
// write-ahead log included, while a kept session's answer still is.
func TestWALEraseCoversVoiceTasks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "voice.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	for _, id := range []string{"deleted", "kept"} {
		if err := store.CreateSession(id, started); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, started.Add(time.Minute), ""); err != nil {
			t.Fatal(err)
		}
	}
	const secret, kept = "private voice answer token", "kept voice answer words"
	askVoiceTask(t, store, "task-deleted", "deleted", secret)
	askVoiceTask(t, store, "task-kept", "kept", kept)
	if log, err := os.ReadFile(path + "-wal"); err != nil || !bytes.Contains(log, []byte(secret)) {
		t.Fatalf("control: the write-ahead log does not hold the answer before the delete (%v)", err)
	}

	if _, err := store.DeleteSession("deleted", t.TempDir()); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	assertDatabaseFilesLack(t, path, secret, kept)
}

// TestSegmentDeleteLeavesNoVoiceText (c-record): the voice keeps no words of
// the room, so once a segment is deleted no table holds its words; the
// session's question keeps only the agent's written answer.
func TestSegmentDeleteLeavesNoVoiceText(t *testing.T) {
	store := newTestSQLiteStore(t)
	segmentTestSession(t, store, "s")
	revision, segments := publishRefinedSegmentTestSession(t, store, "s", "recording.mp3", "b")
	askVoiceTask(t, store, "task", "s", "the agent's written answer")
	span := DeletedSpan{AudioPath: "recording.mp3", Start: 2, End: 4, SHA256Before: "b", SHA256After: "a"}
	if err := store.DeleteRefinedSegmentAudio("s", segments[1].ID, revision, &span); err != nil {
		t.Fatalf("DeleteRefinedSegmentAudio: %v", err)
	}
	tables, err := store.DB().Query(`SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	_ = tables.Close()
	sawAnswer := false
	for _, table := range names {
		rows, err := store.DB().Query(`SELECT * FROM "` + table + `"`)
		if err != nil {
			t.Fatalf("read %s: %v", table, err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			for i, value := range values {
				text := ""
				switch v := value.(type) {
				case string:
					text = v
				case []byte:
					text = string(v)
				}
				if strings.Contains(text, "private deletion token") {
					t.Errorf("%s.%s holds the deleted segment's words", table, columns[i])
				}
				if table == "voice_tasks" && strings.Contains(text, "the agent's written answer") {
					sawAnswer = columns[i] == "answer_written"
				}
			}
		}
		_ = rows.Close()
	}
	if !sawAnswer {
		t.Error("control: the question's written answer is not in voice_tasks.answer_written")
	}
}

package storage

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/recording"
)

// Re-review of #76: a person's delete and GC's age delete record, in the
// transaction that deletes the rows, each session they delete and the files
// of it the kept recordings pass erases if a stop cuts the delete's own erase
// short (DeletedSessionAudio), as absolute paths fixed at the delete, the
// merged sources included. A person's delete lists every audio file of the
// session: the recordings its row names, its raw capture, <id>.mp3 and
// <id>.wav, and a save's temporary files. GC's age delete lists only what it
// checked on Drive or erases itself as redundant: the recordings the row
// names and the raw captures (review 5936938542). A session start never takes
// a recorded id, and a cleared record frees it.
func TestDeletesRecordTheSessionsWhoseAudioThePassErases(t *testing.T) {
	store := newTestSQLiteStore(t)
	audioDir := t.TempDir()
	at := func(id string) string { return filepath.Join(audioDir, id+".mp3") }
	started := time.Date(2026, 11, 6, 14, 0, 0, 0, time.UTC)
	const deleted, source, gcDeleted, kept = "20261106140000", "20261106141000", "20261106150000", "20261106160000"
	endedWith(t, store, deleted, at(deleted), RefinementCompleted)
	endedWith(t, store, source, at(source), RefinementCompleted)
	if _, err := store.DB().Exec(`UPDATE sessions SET status = ?, merged_into = ? WHERE id = ?`, SessionMerged, deleted, source); err != nil {
		t.Fatal(err)
	}
	endedAndSynced(t, store, gcDeleted, at(gcDeleted), RefinementCompleted)
	endedWith(t, store, kept, at(kept), RefinementCompleted)

	if _, err := store.DeleteSession(deleted, audioDir); err != nil {
		t.Fatalf("delete %s: %v", deleted, err)
	}
	if _, removed, err := store.DeleteEndedSessionAndSources(gcDeleted, at(gcDeleted), audioDir); err != nil || !removed {
		t.Fatalf("age delete %s = %v, %v; want deleted", gcDeleted, removed, err)
	}

	records, err := store.DeletedSessionAudio()
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(records, func(a, b DeletedSessionAudio) int { return strings.Compare(a.SessionID, b.SessionID) })
	byID := map[string][]string{}
	for _, record := range records {
		byID[record.SessionID] = record.Files
	}
	if len(records) != 3 {
		t.Errorf("DeletedSessionAudio = %+v, want records of %s, %s and %s", records, deleted, source, gcDeleted)
	}
	in := func(id, name string) string { return filepath.Join(audioDir, id+name) }
	for _, id := range []string{deleted, source} {
		for _, want := range append([]string{at(id), in(id, ".pcm"), in(id, ".wav")}, recording.KeptTempPaths(audioDir, id)...) {
			if !slices.Contains(byID[id], want) {
				t.Errorf("the person's delete of %s lists %q, want %s among them", id, byID[id], want)
			}
		}
	}
	if want := []string{at(gcDeleted), in(gcDeleted, ".pcm")}; !slices.Equal(byID[gcDeleted], want) {
		t.Errorf("the age delete of %s lists %q, want only %q", gcDeleted, byID[gcDeleted], want)
	}

	if err := store.CreateSession(deleted, started); !errors.Is(err, ErrSessionExists) {
		t.Errorf("a session start at a recorded deleted id = %v, want ErrSessionExists", err)
	}
	if err := store.ClearDeletedSessionAudio(deleted); err != nil {
		t.Fatal(err)
	}
	if records, err := store.DeletedSessionAudio(); err != nil || slices.ContainsFunc(records, func(r DeletedSessionAudio) bool { return r.SessionID == deleted }) {
		t.Errorf("after the clear DeletedSessionAudio = %+v (%v), want no %s", records, err, deleted)
	}
	if err := store.CreateSession(deleted, started); err != nil {
		t.Errorf("a session start at a cleared id = %v, want it created", err)
	}
}

// Review 5936222350 of #76, P2 (the reviewer's repro): GC's age delete keeps
// a session whose only audio is its raw capture, rather than deleting the row
// and recording the delete, after which the kept recordings pass would zero
// that capture. A session with a saved recording beside its capture is
// deleted as before.
func TestTheAgeDeleteKeepsASessionWhoseOnlyAudioIsItsRawCapture(t *testing.T) {
	store := newTestSQLiteStore(t)
	audioDir := t.TempDir()
	const rawOnly, saved = "20261001100000", "20261001110000"
	for _, id := range []string{rawOnly, saved} {
		if err := os.WriteFile(filepath.Join(audioDir, id+".pcm"), []byte("conversation "+id), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	savedPath := filepath.Join(audioDir, saved+".mp3")
	if err := os.WriteFile(savedPath, []byte("saved recording"), 0o600); err != nil {
		t.Fatal(err)
	}
	endedAndSynced(t, store, rawOnly, "", RefinementNoAudio)
	endedAndSynced(t, store, saved, savedPath, RefinementCompleted)

	_, removed, err := store.DeleteEndedSessionAndSources(rawOnly, "", audioDir)
	var block *GCSessionBlockError
	if removed || !errors.As(err, &block) || block.BlockingSessionID != rawOnly || !block.BlockingSessionIsTarget {
		t.Errorf("age delete of a session whose only audio is its raw capture = %v, %v; want refused, naming it", removed, err)
	}
	if _, err := store.GetSession(rawOnly); err != nil {
		t.Errorf("the session whose only audio is its raw capture is gone: %v", err)
	}
	if _, removed, err := store.DeleteEndedSessionAndSources(saved, savedPath, audioDir); err != nil || !removed {
		t.Errorf("control: age delete of a session with a saved recording = %v, %v; want deleted", removed, err)
	}
	records, err := store.DeletedSessionAudio()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].SessionID != saved || !slices.Equal(records[0].Files, []string{savedPath, filepath.Join(audioDir, saved+".pcm")}) {
		t.Errorf("DeletedSessionAudio = %+v, want only %s listing its recording and raw capture", records, saved)
	}
}

// Review 5936222350 of #76, P3: a restore from Drive and a merge do not
// bring back an id whose recorded delete the kept recordings pass has not
// cleared yet, as a session start does not: the pass would erase the files
// the restored session's Retry refinement downloads under that id. Once the
// record is cleared, both go ahead.
func TestARestoreOrMergeWaitsForARecordedDeleteToClear(t *testing.T) {
	store := newTestSQLiteStore(t)
	audioDir := t.TempDir()
	const deleted, source = "20261106140000", "20261106150000"
	endedWith(t, store, deleted, filepath.Join(audioDir, deleted+".mp3"), RefinementCompleted)
	endedWith(t, store, source, filepath.Join(audioDir, source+".mp3"), RefinementCompleted)
	if _, err := store.DeleteSession(deleted, audioDir); err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 11, 6, 14, 0, 0, 0, time.UTC)
	ended := started.Add(time.Hour)
	restored := Session{ID: deleted, StartedAt: started, EndedAt: &ended, Status: SessionEnded, SummaryStatus: SummaryCompleted, RefinementStatus: RefinementNoAudio}

	if err := store.ImportSession(&restored, nil, []string{deleted + ".mp3"}); !errors.Is(err, ErrSessionAudioPending) {
		t.Errorf("restore of an id whose delete is pending = %v, want ErrSessionAudioPending", err)
	}
	if err := store.MergeSessions(deleted, []string{source}, started, ended); !errors.Is(err, ErrSessionAudioPending) {
		t.Errorf("merge into an id whose delete is pending = %v, want ErrSessionAudioPending", err)
	}
	if _, err := store.GetSession(deleted); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("an id whose delete is pending has a row again (%v)", err)
	}

	if err := store.ClearDeletedSessionAudio(deleted); err != nil {
		t.Fatal(err)
	}
	if err := store.ImportSession(&restored, nil, []string{deleted + ".mp3"}); err != nil {
		t.Errorf("restore once the record is cleared = %v, want it imported", err)
	}
}

// Review 5936222350 of #76, P3: NamedAudio is every file a session names,
// the recordings a restored session's Drive folder holds included: a merge or
// a Retry refinement names those later, so the kept recordings pass must not
// give one to another session meanwhile. A restored name is a Drive file
// name: it names a recording in the audio directory only when it has a
// recording's form, and is never resolved against the working directory
// (review 5936938542).
func TestNamedAudioHasTheRecordingsOfARestoredSession(t *testing.T) {
	store := newTestSQLiteStore(t)
	audioDir := t.TempDir()
	const live, restoredID = "20261106140000", "20261106150000"
	a, b := filepath.Join(audioDir, "a.mp3"), filepath.Join(audioDir, "b.mp3")
	endedWith(t, store, live, a+","+b, RefinementCompleted)
	started := time.Date(2026, 11, 6, 15, 0, 0, 0, time.UTC)
	ended := started.Add(time.Hour)
	restored := Session{ID: restoredID, StartedAt: started, EndedAt: &ended, Status: SessionEnded, SummaryStatus: SummaryCompleted, RefinementStatus: RefinementNoAudio}
	if err := store.ImportSession(&restored, nil, []string{"20261106160000.mp3", ".env", "x,y.mp3", "notes.txt"}); err != nil {
		t.Fatal(err)
	}
	got, err := store.NamedAudio(audioDir)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if want := []string{filepath.Join(audioDir, "20261106160000.mp3"), a, b}; !slices.Equal(got, want) {
		t.Errorf("NamedAudio = %q, want %q", got, want)
	}
}

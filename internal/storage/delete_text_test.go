package storage

import (
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// transcribeSegment is the i-th two-second segment of a session started at
// started.
func transcribeSegment(text string, i int, started time.Time) transcribe.Segment {
	return transcribe.Segment{Text: text, StartTime: float64(2 * i), EndTime: float64(2*i + 2), Timestamp: started.Add(time.Duration(2*i) * time.Second)}
}

// textSpan is the span of "private deletion token" in segmentTestSession's
// recording, as the deletion runner locates it.
func textSpan() TextDeletionSpan {
	return TextDeletionSpan{AudioPath: "recording.mp3", Start: 2, End: 4, SHA256: "recording-sha"}
}

// TestDeleteRefinedSegmentTextRemovesTextEverywhere fails if "Delete text"
// leaves a selected refined occurrence in a text store, or starts the audio
// action's Drive replacement or splice work.
func TestDeleteRefinedSegmentTextRemovesTextEverywhere(t *testing.T) {
	store := newTestSQLiteStore(t)
	_, _ = segmentTestSession(t, store, "text-everywhere")
	revision, segments := publishRefinedSegmentTestSession(t, store, "text-everywhere", "recording.mp3", "recording-sha")
	if err := store.UpdateSummaryAtGeneration("text-everywhere", "private deletion token title", "summary names private deletion token", SummaryCompleted, "default", `{"1":{"name":"private deletion token","confidence":"mentioned"}}`, "", 0); err != nil {
		t.Fatalf("set summary: %v", err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET envoy_publish_status = ?, sync_status = ?, sync_state = ?, gdrive_folder_id = 'folder' WHERE id = ?`,
		EnvoyPublishPublished, SyncSynced, SyncStateSynced, "text-everywhere"); err != nil {
		t.Fatalf("set completed session state: %v", err)
	}
	if _, err := store.DB().Exec(`INSERT INTO embeddings(session_id, chunk_index, embedding, text_hash, model) VALUES(?, 0, X'01', 'hash', 'model')`, "text-everywhere"); err != nil {
		t.Fatalf("insert embedding: %v", err)
	}
	if err := store.StoreEvent("summary_ready", `{"type":"summary_ready","session_id":"text-everywhere","title":"private deletion token title","summary":"summary names private deletion token"}`); err != nil {
		t.Fatalf("store event: %v", err)
	}
	before := storeSession(t, store, "text-everywhere")
	generationBefore, err := store.DeletionGeneration("text-everywhere")
	if err != nil {
		t.Fatal(err)
	}

	if err := store.DeleteRefinedSegmentText("text-everywhere", segments[1].ID, revision); err != nil {
		t.Fatalf("DeleteRefinedSegmentText: %v", err)
	}

	remaining, err := store.GetSegments("text-everywhere")
	if err != nil {
		t.Fatalf("get remaining segments: %v", err)
	}
	if len(remaining) != 2 || remaining[0].Text != "before bridge" || remaining[1].Text != "after bridge" {
		t.Fatalf("remaining segments = %+v, want the two neighbours", remaining)
	}
	session := storeSession(t, store, "text-everywhere")
	if session.Title != "" || session.Summary != "" || strings.Contains(session.RefinedTranscript, "private deletion token") || session.SpeakerNames != "{}" {
		t.Errorf("session still has deleted derived text: title %q summary %q refined %q speakers %q", session.Title, session.Summary, session.RefinedTranscript, session.SpeakerNames)
	}
	if session.CanonicalTranscript != "before bridge after bridge" || session.TranscriptSource != TranscriptSourceRefined {
		t.Errorf("canonical transcript = %q from %q, want remaining refined words", session.CanonicalTranscript, session.TranscriptSource)
	}
	if session.RefinementStatus != RefinementCompleted || session.SummaryStatus != SummaryPending || session.EnvoyPublishStatus != EnvoyPublishPending {
		t.Errorf("follow-up states = refinement:%q summary:%q envoy:%q, want refined completion and pending follow-up work", session.RefinementStatus, session.SummaryStatus, session.EnvoyPublishStatus)
	}
	if session.SummaryVersion != before.SummaryVersion+1 {
		t.Errorf("summary version = %d, want %d", session.SummaryVersion, before.SummaryVersion+1)
	}
	if generation, err := store.DeletionGeneration("text-everywhere"); err != nil || generation != generationBefore+1 {
		t.Errorf("deletion generation = %d (%v), want %d: stale refinements and summaries must be fenced", generation, err, generationBefore+1)
	}
	// The ordinary sync updates the Drive documents in place.
	if session.SyncStatus != SyncPending || session.SyncState != SyncStatePending {
		t.Errorf("sync = %q/%q, want pending so the ordinary sync updates the Drive documents", session.SyncStatus, session.SyncState)
	}
	// None of the audio action's work.
	if session.DriveReplacePending {
		t.Error("Delete text set drive_replace_pending")
	}
	if session.AudioPath != before.AudioPath {
		t.Errorf("audio path = %q, want it unchanged (%q)", session.AudioPath, before.AudioPath)
	}
	for table, want := range map[string]int{"deleted_segments": 0, "drive_replacements": 0, "drive_replacement_folders": 0, "deleted_text_spans": 1} {
		var count int
		if err := store.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != want {
			t.Errorf("%s has %d rows after Delete text, want %d", table, count, want)
		}
	}
	spans, err := store.TextDeletionSpans("/somewhere/else/recording.mp3")
	if err != nil {
		t.Fatalf("TextDeletionSpans: %v", err)
	}
	if want := (TextDeletionSpan{AudioPath: "recording.mp3", Start: 2, End: 4, SHA256: "recording-sha"}); len(spans) != 1 || spans[0] != want {
		t.Errorf("recorded spans = %+v, want [%+v]", spans, want)
	}

	if results, err := store.Search("private deletion token", SearchOptions{}); err != nil || len(results) != 0 {
		t.Errorf("search after Delete text = %+v (%v), want nothing", results, err)
	}
	var embeddings int
	if err := store.DB().QueryRow(`SELECT count(*) FROM embeddings WHERE session_id = ?`, "text-everywhere").Scan(&embeddings); err != nil {
		t.Fatal(err)
	}
	if embeddings != 0 {
		t.Errorf("%d embeddings remain, want none", embeddings)
	}
	if events, err := store.GetEventsSince(0, 10); err != nil || len(events) != 0 {
		t.Errorf("stored events after Delete text = %+v (%v), want none", events, err)
	}
}

// TestDeleteSegmentTextKeepsNoTextInTheSpanTable fails if the span a
// "Delete text" deletion records holds any of the deleted words.
func TestDeleteSegmentTextKeepsNoTextInTheSpanTable(t *testing.T) {
	store := newTestSQLiteStore(t)
	_, _ = segmentTestSession(t, store, "span-columns")
	segments, err := store.GetSegments("span-columns")
	if err != nil {
		t.Fatal(err)
	}
	if err := deleteRefinedTextTestSegment(t, store, "span-columns", segments[1].ID, textSpan()); err != nil {
		t.Fatalf("delete refined text: %v", err)
	}
	rows, err := store.DB().Query(`SELECT * FROM deleted_text_spans`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	values := make([]sql.NullString, len(columns))
	pointers := make([]any, len(columns))
	for i := range values {
		pointers[i] = &values[i]
	}
	seen := 0
	for rows.Next() {
		seen++
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		for i, value := range values {
			for _, word := range []string{"private", "deletion", "token"} {
				if strings.Contains(value.String, word) {
					t.Errorf("deleted_text_spans.%s = %q holds a deleted word", columns[i], value.String)
				}
			}
		}
	}
	if seen != 1 {
		t.Fatalf("control: read %d span rows, want 1", seen)
	}
}

// TestDeleteSegmentTextErasesTheDatabaseFilesAndTheBackup fails if "Delete
// text" leaves the words in the database files, read while the store is
// open, or in the pre-migration backup, of either schema: the text-side
// erasure is the audio action's.
func TestDeleteSegmentTextErasesTheDatabaseFilesAndTheBackup(t *testing.T) {
	for _, tc := range []struct {
		name    string
		migrate bool
	}{
		{name: "backup of the same schema"},
		{name: "backup of an older schema", migrate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ghost-wispr.db")
			openWithSecret(t, path)
			if tc.migrate {
				old, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := old.Exec(`ALTER TABLE sessions DROP COLUMN drive_replace_error`); err != nil {
					t.Fatal(err)
				}
				if err := old.Close(); err != nil {
					t.Fatal(err)
				}
			}
			store := openStoreWithBackup(t, path)
			segments, err := store.GetSegments("backed-up")
			if err != nil {
				t.Fatal(err)
			}
			if err := deleteRefinedTextTestSegment(t, store, "backed-up", segments[1].ID, textSpan()); err != nil {
				t.Fatalf("delete refined text: %v", err)
			}
			assertDatabaseFilesLack(t, path, backupSecret, "after bridge")
			assertBackupHolds(t, path, backupSecret, false)
		})
	}
}

// TestDeleteRefinedSegmentTextScrubsMergedSources fails if "Delete text" on
// a merged refined session leaves the words in a merged source's row, search
// index, embeddings or database files.
func TestDeleteRefinedSegmentTextScrubsMergedSources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "merged.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	secret := mergedSessionWithSecret(t, store)

	if err := store.DeleteRefinedSegmentText("merged", secret.id, secret.revision); err != nil {
		t.Fatalf("DeleteRefinedSegmentText: %v", err)
	}

	source := storeSession(t, store, "source-a")
	for field, value := range map[string]string{"title": source.Title, "summary": source.Summary, "canonical transcript": source.CanonicalTranscript, "refined transcript": source.RefinedTranscript} {
		if strings.Contains(value, mergedSourceSecret) {
			t.Errorf("the merged source's %s still holds the deleted words: %q", field, value)
		}
	}
	if !strings.Contains(source.CanonicalTranscript, "alpha stays") {
		t.Errorf("the merged source's canonical transcript lost the words it keeps: %q", source.CanonicalTranscript)
	}
	var indexed, embeddings int
	if err := store.DB().QueryRow(`SELECT count(*) FROM sessions_fts WHERE sessions_fts MATCH ?`, mergedSourceSecret).Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT count(*) FROM embeddings WHERE session_id = 'source-a'`).Scan(&embeddings); err != nil {
		t.Fatal(err)
	}
	if indexed != 0 || embeddings != 0 {
		t.Errorf("after Delete text: %d search index matches, %d source embeddings; want none", indexed, embeddings)
	}
	for _, id := range []string{"merged", "source-a", "source-b"} {
		if storeSession(t, store, id).DriveReplacePending {
			t.Errorf("Delete text set drive_replace_pending on %s", id)
		}
	}
	assertDatabaseFilesLack(t, path, mergedSourceSecret, "alpha stays")
}

type refinedSegmentSelection struct {
	id       int64
	revision int
}

// mergedSessionWithSecret makes a merged session whose refined segment owns
// the secret occurrence from source-a.
func mergedSessionWithSecret(t *testing.T, store *SQLiteStore) refinedSegmentSelection {
	t.Helper()
	return mergedSessionWithSecretAfter(t, store, func() {})
}

// mergedSessionWithSecretAfter runs beforeMerge just before the sources are
// merged, then publishes the merged session's timed word occurrences.
func mergedSessionWithSecretAfter(t *testing.T, store *SQLiteStore, beforeMerge func()) refinedSegmentSelection {
	t.Helper()
	startA := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	startB := startA.Add(time.Hour)
	sessions := map[string][]string{
		"source-a": {"alpha stays", "the " + mergedSourceSecret + " words"},
		"source-b": {"charlie stays"},
	}
	for id, started := range map[string]time.Time{"source-a": startA, "source-b": startB} {
		if err := store.CreateSession(id, started); err != nil {
			t.Fatal(err)
		}
		for i, text := range sessions[id] {
			if err := store.AppendSegment(id, transcribeSegment(text, i, started)); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.EndSession(id, started.Add(time.Minute), "recording-"+id+".mp3"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Canonicalize(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.UpdateSummaryAtGeneration("source-a", "About "+mergedSourceSecret, "The summary of "+mergedSourceSecret+".", SummaryCompleted, "default", "{}", "", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET refined_transcript = ?, refinement_status = ? WHERE id = 'source-a'`, "alpha stays. The "+mergedSourceSecret+" words.", RefinementCompleted); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO embeddings(session_id, chunk_index, embedding, text_hash, model) VALUES('source-a', 0, X'01', 'hash', 'model')`); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreEvent("summary_ready", `{"type":"summary_ready","session_id":"source-a","summary":"The summary of `+mergedSourceSecret+`."}`); err != nil {
		t.Fatal(err)
	}
	beforeMerge()
	if err := store.MergeSessions("merged", []string{"source-a", "source-b"}, startA, startB.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	words := []RefinedWord{
		{RecordingPath: "recording-source-a.mp3", RecordingSHA256: "a-sha", Ordinal: 0, Text: "alpha stays", Start: 0, End: 2},
		{RecordingPath: "recording-source-a.mp3", RecordingSHA256: "a-sha", Ordinal: 1, Text: "the " + mergedSourceSecret + " words", Start: 2, End: 4},
		{RecordingPath: "recording-source-b.mp3", RecordingSHA256: "b-sha", RecordingOrdinal: 1, Ordinal: 0, Text: "charlie stays", Start: 0, End: 2},
	}
	segments := []RefinedSegment{
		{RecordingPath: "recording-source-a.mp3", RecordingSHA256: "a-sha", FirstWordOrdinal: 0, LastWordOrdinal: 0, Text: words[0].Text, Start: 0, End: 2},
		{RecordingPath: "recording-source-a.mp3", RecordingSHA256: "a-sha", FirstWordOrdinal: 1, LastWordOrdinal: 1, Text: words[1].Text, Start: 2, End: 4},
		{RecordingPath: "recording-source-b.mp3", RecordingSHA256: "b-sha", RecordingOrdinal: 1, FirstWordOrdinal: 0, LastWordOrdinal: 0, Text: words[2].Text, Start: 0, End: 2},
	}
	revision, err := store.PublishRefinementAtGeneration("merged", RefinedTranscript(words), "nova-3", words, segments, 0)
	if err != nil {
		t.Fatal(err)
	}
	refined, err := store.GetSegments("merged")
	if err != nil {
		t.Fatal(err)
	}
	for i := range refined {
		if strings.Contains(refined[i].Text, mergedSourceSecret) {
			return refinedSegmentSelection{id: refined[i].ID, revision: revision}
		}
	}
	t.Fatal("control: no merged refined segment holds the secret")
	return refinedSegmentSelection{}
}

// TestDeleteRefinedSegmentTextFencesStaleRefinementAndSummary fails if a
// refinement or summary started before "Delete text" can still store text
// read before it.
func TestDeleteRefinedSegmentTextFencesStaleRefinementAndSummary(t *testing.T) {
	store := newTestSQLiteStore(t)
	_, _ = segmentTestSession(t, store, "text-fence")
	revision, segments := publishRefinedSegmentTestSession(t, store, "text-fence", "recording.mp3", "recording-sha")
	if err := store.StartSummary("text-fence"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteRefinedSegmentText("text-fence", segments[1].ID, revision); err != nil {
		t.Fatalf("DeleteRefinedSegmentText: %v", err)
	}
	if err := store.UpdateRefinementAtGeneration("text-fence", "stale private deletion token", RefinementCompleted, "", 0); err == nil {
		t.Error("a refinement started before Delete text stored its transcript")
	}
	if err := store.UpdateSummaryAtGeneration("text-fence", "stale", "stale private deletion token", SummaryCompleted, "default", "{}", "", 0); err == nil {
		t.Error("a summary started before Delete text stored its summary")
	}
	session := storeSession(t, store, "text-fence")
	if strings.Contains(session.RefinedTranscript, "private") || strings.Contains(session.Summary, "private") {
		t.Fatalf("stale words returned to the session: refined %q summary %q", session.RefinedTranscript, session.Summary)
	}
}

// TestASpliceMovesTheTextDeletionSpansAfterIt fails if "Delete text and
// audio" cutting a recording leaves the spans of earlier "Delete text"
// deletions in that recording at their old times: a later refinement of the
// cut recording would drop the wrong words and bring the deleted ones back.
func TestASpliceMovesTheTextDeletionSpansAfterIt(t *testing.T) {
	store := newTestSQLiteStore(t)
	_, _ = segmentTestSession(t, store, "text-then-audio")
	segments, err := store.GetSegments("text-then-audio")
	if err != nil {
		t.Fatal(err)
	}
	// Spans before, inside, across the end of and after the cut [2, 4], in
	// the recording as it is now (sha "original"); and one in another
	// recording and one in a copy of this one with other bytes.
	for _, span := range []TextDeletionSpan{
		{AudioPath: "/audio/recording.mp3", Start: 0.5, End: 1.5, SHA256: "original"},
		{AudioPath: "recording.mp3", Start: 2.5, End: 3.5, SHA256: "original"},
		{AudioPath: "recording.mp3", Start: 3.5, End: 5, SHA256: "original"},
		{AudioPath: "recording.mp3", Start: 6, End: 7, SHA256: "original"},
		{AudioPath: "other.mp3", Start: 6, End: 7, SHA256: "original"},
		{AudioPath: "recording.mp3", Start: 6, End: 7, SHA256: "other-bytes"},
	} {
		if _, err := store.DB().Exec(`INSERT INTO deleted_text_spans(session_id, recording, start, end, sha256, deleted_at) VALUES('text-then-audio', ?, ?, ?, ?, '2026-11-12T00:00:00Z')`,
			filepath.Base(span.AudioPath), span.Start, span.End, span.SHA256); err != nil {
			t.Fatal(err)
		}
	}
	cut := DeletedSpan{SessionID: "text-then-audio", AudioPath: "/audio/recording.mp3", Start: 2, End: 4, SHA256Before: "original", SHA256After: "spliced"}
	if err := deleteRefinedAudioTestSegment(t, store, "text-then-audio", segments[1].ID, cut); err != nil {
		t.Fatalf("delete refined segment: %v", err)
	}
	// Committed, not yet spliced: the spans still describe the recording on disk.
	if spans, err := store.TextDeletionSpans("recording.mp3"); err != nil || len(spans) != 5 || spans[0].SHA256 != "original" {
		t.Fatalf("spans before the splice completes = %+v (%v), want the five unchanged", spans, err)
	}
	for range 2 { // marking it complete twice moves the spans once
		if err := store.MarkSpliceComplete(&cut); err != nil {
			t.Fatalf("MarkSpliceComplete: %v", err)
		}
	}
	got, err := store.TextDeletionSpans("recording.mp3")
	if err != nil {
		t.Fatal(err)
	}
	want := []TextDeletionSpan{
		{AudioPath: "recording.mp3", Start: 0.5, End: 1.5, SHA256: "spliced"},
		{AudioPath: "recording.mp3", Start: 2, End: 3, SHA256: "spliced"},
		{AudioPath: "recording.mp3", Start: 4, End: 5, SHA256: "spliced"},
		{AudioPath: "recording.mp3", Start: 6, End: 7, SHA256: "other-bytes"},
	}
	if len(got) != len(want) {
		t.Fatalf("spans after the splice = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("span %d after the splice = %+v, want %+v", i, got[i], want[i])
		}
	}
	if other, err := store.TextDeletionSpans("other.mp3"); err != nil || len(other) != 1 || other[0].Start != 6 || other[0].SHA256 != "original" {
		t.Errorf("a span in another recording moved: %+v (%v)", other, err)
	}
}

// TestDeleteTextFencesMergedSourcesToo fails if a refinement or summary of a
// merged source, started before "Delete text" in the session it was merged
// into, can still store the words it read: the scrub must move the source's
// deletion generation as it moves the session's.
func TestDeleteTextFencesMergedSourcesToo(t *testing.T) {
	store := newTestSQLiteStore(t)
	claimedAt, claimed, err := 0, false, error(nil)
	target := mergedSessionWithSecretAfter(t, store, func() {
		claimedAt, claimed, err = store.ClaimRefinement("source-a", "recording-source-a.mp3")
	})
	if err != nil || !claimed {
		t.Fatalf("claim the source's refinement before the merge = %v, %v", claimed, err)
	}
	if err := store.DeleteRefinedSegmentText("merged", target.id, target.revision); err != nil {
		t.Fatalf("DeleteRefinedSegmentText: %v", err)
	}
	if err := store.UpdateRefinementAtGeneration("source-a", "stale "+mergedSourceSecret, RefinementCompleted, "", claimedAt); err == nil {
		t.Error("a refinement of a merged source started before Delete text stored its transcript")
	}
	if err := store.UpdateSummaryAtGeneration("source-a", "stale", "stale "+mergedSourceSecret, SummaryCompleted, "default", "{}", "", 0); err == nil {
		t.Error("a summary of a merged source started before Delete text stored its summary")
	}
	if source := storeSession(t, store, "source-a"); strings.Contains(source.RefinedTranscript+source.Summary, mergedSourceSecret) {
		t.Errorf("stale words returned to the merged source: refined %q summary %q", source.RefinedTranscript, source.Summary)
	}
}

// TestASessionMergedBeforeItsDocumentsSyncedStillOwesThem fails if "Delete
// text" in a session with a Drive folder, merged into another before a sync
// updated its Drive documents, is owed no update: nothing else syncs a merged
// source, and its folder would keep the words.
func TestASessionMergedBeforeItsDocumentsSyncedStillOwesThem(t *testing.T) {
	store := newTestSQLiteStore(t)
	_, _ = segmentTestSession(t, store, "text-then-merge")
	if _, err := store.DB().Exec(`UPDATE sessions SET gdrive_folder_id = 'folder', sync_status = ?, sync_state = ? WHERE id = 'text-then-merge'`, SyncSynced, SyncStateSynced); err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("text-then-merge")
	if err != nil {
		t.Fatal(err)
	}
	if err := deleteRefinedTextTestSegment(t, store, "text-then-merge", segments[1].ID, textSpan()); err != nil {
		t.Fatalf("delete refined text: %v", err)
	}
	started := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	if err := store.MergeSessions("text-then-merge-merged", []string{"text-then-merge"}, started, started.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	ids, err := store.GetSessionsNeedingSync()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(ids, "text-then-merge") {
		t.Errorf("the merged source whose documents no sync updated is not listed for the sweep: %v", ids)
	}
}

// TestDeletingTheSessionADeletionWasMadeInKeepsItsSpans fails if a delete can
// remove a merged source's row and its "Delete text" span while the recording
// lives on in the session it was merged into: a refinement there would bring
// the words back.
func TestDeletingTheSessionADeletionWasMadeInKeepsItsSpans(t *testing.T) {
	store := newTestSQLiteStore(t)
	_, _ = segmentTestSession(t, store, "text-then-gone")
	segments, err := store.GetSegments("text-then-gone")
	if err != nil {
		t.Fatal(err)
	}
	if err := deleteRefinedTextTestSegment(t, store, "text-then-gone", segments[1].ID, textSpan()); err != nil {
		t.Fatalf("delete refined text: %v", err)
	}
	started := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	if err := store.MergeSessions("text-then-gone-merged", []string{"text-then-gone"}, started, started.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteSession("text-then-gone", t.TempDir()); !errors.Is(err, ErrDeleteMergeActive) {
		t.Fatalf("delete the merged source = %v, want %v", err, ErrDeleteMergeActive)
	}
	if spans, err := store.TextDeletionSpans("recording.mp3"); err != nil || len(spans) != 1 {
		t.Errorf("spans after refused source delete = %+v (%v), want the one recorded", spans, err)
	}
}

// TestARefreshThatLostARaceIsOwedAgain fails if a refresh of a merged
// source's Drive documents whose session changed while it uploaded leaves
// the source owing nothing: a refresh that read before the change may have
// uploaded after a newer one, and only another refresh puts the current
// documents back.
func TestARefreshThatLostARaceIsOwedAgain(t *testing.T) {
	store := newTestSQLiteStore(t)
	_, _ = segmentTestSession(t, store, "raced")
	if cleared, err := store.ClearDriveDocumentsPending("raced", func(Session) bool { return false }); err != nil || cleared {
		t.Fatalf("ClearDriveDocumentsPending with a changed session = %v, %v; want not cleared", cleared, err)
	}
	var pending int
	if err := store.DB().QueryRow(`SELECT drive_documents_pending FROM sessions WHERE id = 'raced'`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Errorf("drive_documents_pending = %d after a refresh that lost a race, want 1", pending)
	}
}

// TestDeleteRefinedTextKeepsAMergedSourcesDocumentsOwedBeforeItsFolderArrives
// fails if a source whose Drive folder is recorded after a refined deletion
// never has its changed documents refreshed.
func TestDeleteRefinedTextKeepsAMergedSourcesDocumentsOwedBeforeItsFolderArrives(t *testing.T) {
	store := newTestSQLiteStore(t)
	started := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	recordedSession(t, store, "source", started, started.Add(time.Minute), map[float64]string{1: "deleted"})
	recordedSession(t, store, "other", started.Add(time.Hour), started.Add(61*time.Minute), map[float64]string{1: "other"})
	if err := store.MergeSessions("merged", []string{"source", "other"}, started, started.Add(61*time.Minute)); err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("merged")
	if err != nil {
		t.Fatal(err)
	}
	if err := deleteRefinedTextTestSegment(t, store, "merged", segments[0].ID, TextDeletionSpan{AudioPath: "recording-source.mp3", Start: 1, End: 3, SHA256: "before"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET gdrive_folder_id = 'late-folder' WHERE id = 'source'`); err != nil {
		t.Fatal(err)
	}
	owed, err := store.DriveDocumentsOwed("merged")
	if err != nil {
		t.Fatal(err)
	}
	if len(owed) != 1 || owed[0] != "source" {
		t.Fatalf("owed text-document folders = %v, want source", owed)
	}
}

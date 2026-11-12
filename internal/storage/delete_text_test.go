package storage

import (
	"database/sql"
	"path/filepath"
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

// TestDeleteSegmentTextRemovesTextEverywhere fails if "Delete text" leaves
// the segment's words in any text store DeleteSegment clears, or if it
// starts any of the audio action's work: a Drive replacement, a splice, a
// new audio path.
func TestDeleteSegmentTextRemovesTextEverywhere(t *testing.T) {
	store := newTestSQLiteStore(t)
	_, _ = segmentTestSession(t, store, "text-everywhere")
	if err := store.UpdateSummaryAtGeneration("text-everywhere", "private deletion token title", "summary names private deletion token", SummaryCompleted, "default", `{"1":{"name":"private deletion token","confidence":"mentioned"}}`, "", 0); err != nil {
		t.Fatalf("set summary: %v", err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET refined_transcript = ?, refinement_status = ?, canonical_transcript = ?, envoy_publish_status = ?, sync_status = ?, sync_state = ?, gdrive_folder_id = 'folder' WHERE id = ?`,
		"before bridge private deletion token after bridge", RefinementCompleted, "before bridge private deletion token after bridge", EnvoyPublishPublished, SyncSynced, SyncStateSynced, "text-everywhere"); err != nil {
		t.Fatalf("set completed session state: %v", err)
	}
	if _, err := store.DB().Exec(`INSERT INTO embeddings(session_id, chunk_index, embedding, text_hash, model) VALUES(?, 0, X'01', 'hash', 'model')`, "text-everywhere"); err != nil {
		t.Fatalf("insert embedding: %v", err)
	}
	if err := store.StoreEvent("summary_ready", `{"type":"summary_ready","session_id":"text-everywhere","title":"private deletion token title","summary":"summary names private deletion token"}`); err != nil {
		t.Fatalf("store event: %v", err)
	}
	segments, err := store.GetSegments("text-everywhere")
	if err != nil {
		t.Fatalf("get segments: %v", err)
	}
	before := storeSession(t, store, "text-everywhere")
	generationBefore, err := store.DeletionGeneration("text-everywhere")
	if err != nil {
		t.Fatal(err)
	}

	if err := store.DeleteSegmentText("text-everywhere", segments[1].ID, textSpan()); err != nil {
		t.Fatalf("DeleteSegmentText: %v", err)
	}

	remaining, err := store.GetSegments("text-everywhere")
	if err != nil {
		t.Fatalf("get remaining segments: %v", err)
	}
	if len(remaining) != 2 || remaining[0].Text != "before bridge" || remaining[1].Text != "after bridge" {
		t.Fatalf("remaining segments = %+v, want the two neighbours", remaining)
	}
	session := storeSession(t, store, "text-everywhere")
	if session.Title != "" || session.Summary != "" || session.RefinedTranscript != "" || session.SpeakerNames != "{}" {
		t.Errorf("session still has deleted derived text: title %q summary %q refined %q speakers %q", session.Title, session.Summary, session.RefinedTranscript, session.SpeakerNames)
	}
	if session.CanonicalTranscript != "before bridge\nafter bridge\n" || session.TranscriptSource != TranscriptSourceStreaming {
		t.Errorf("canonical transcript = %q from %q, want remaining streaming segments", session.CanonicalTranscript, session.TranscriptSource)
	}
	if session.RefinementStatus != RefinementPending || session.SummaryStatus != SummaryPending || session.EnvoyPublishStatus != EnvoyPublishPending {
		t.Errorf("follow-up states = refinement:%q summary:%q envoy:%q, want pending", session.RefinementStatus, session.SummaryStatus, session.EnvoyPublishStatus)
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
	if err := store.DeleteSegmentText("span-columns", segments[1].ID, textSpan()); err != nil {
		t.Fatalf("DeleteSegmentText: %v", err)
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
			if err := store.DeleteSegmentText("backed-up", segments[1].ID, textSpan()); err != nil {
				t.Fatalf("DeleteSegmentText: %v", err)
			}
			assertDatabaseFilesLack(t, path, backupSecret, "after bridge")
			assertBackupHolds(t, path, backupSecret, false)
		})
	}
}

// TestDeleteSegmentTextScrubsMergedSources fails if "Delete text" on a
// merged session leaves the words in a merged source's row, the search
// index, its embeddings or the database files.
func TestDeleteSegmentTextScrubsMergedSources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "merged.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	secretID := mergedSessionWithSecret(t, store)

	if err := store.DeleteSegmentText("merged", secretID, TextDeletionSpan{AudioPath: "recording-source-a.mp3", Start: 2, End: 4, SHA256: "a-sha"}); err != nil {
		t.Fatalf("DeleteSegmentText: %v", err)
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

// mergedSessionWithSecret makes session "merged" of "source-a" (recording
// recording-source-a.mp3, whose second segment, title, summary, refined
// transcript, embedding and events hold mergedSourceSecret) and "source-b",
// and returns the id of the segment holding the secret.
func mergedSessionWithSecret(t *testing.T, store *SQLiteStore) int64 {
	t.Helper()
	return mergedSessionWithSecretAfter(t, store, func() {})
}

// mergedSessionWithSecretAfter is mergedSessionWithSecret, running beforeMerge
// just before the sources are merged.
func mergedSessionWithSecretAfter(t *testing.T, store *SQLiteStore, beforeMerge func()) int64 {
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
	if _, _, err := store.Canonicalize("merged"); err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("merged")
	if err != nil {
		t.Fatal(err)
	}
	for _, segment := range segments {
		if strings.Contains(segment.Text, mergedSourceSecret) {
			return segment.ID
		}
	}
	t.Fatal("control: no merged segment holds the secret")
	return 0
}

// TestDeleteSegmentTextFencesStaleRefinementAndSummary fails if a
// refinement or summary started before "Delete text" can still store text
// read before it.
func TestDeleteSegmentTextFencesStaleRefinementAndSummary(t *testing.T) {
	store := newTestSQLiteStore(t)
	_, _ = segmentTestSession(t, store, "text-fence")
	segments, err := store.GetSegments("text-fence")
	if err != nil {
		t.Fatal(err)
	}
	claimedAt, claimed, err := store.ClaimRefinement("text-fence", "recording.mp3")
	if err != nil || !claimed {
		t.Fatalf("claim refinement = %v, %v", claimed, err)
	}
	if err := store.StartSummary("text-fence"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSegmentText("text-fence", segments[1].ID, textSpan()); err != nil {
		t.Fatalf("DeleteSegmentText: %v", err)
	}
	if err := store.UpdateRefinementAtGeneration("text-fence", "stale private deletion token", RefinementCompleted, "", claimedAt); err == nil {
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

// TestDeleteSegmentTextRefusesAnInvalidSpan fails if "Delete text" changes
// anything without a span to record: refinement would bring the words back.
func TestDeleteSegmentTextRefusesAnInvalidSpan(t *testing.T) {
	store := newTestSQLiteStore(t)
	_, _ = segmentTestSession(t, store, "text-invalid")
	segments, err := store.GetSegments("text-invalid")
	if err != nil {
		t.Fatal(err)
	}
	for name, span := range map[string]TextDeletionSpan{
		"no recording":  {Start: 2, End: 4, SHA256: "sha"},
		"empty span":    {AudioPath: "recording.mp3", Start: 4, End: 4, SHA256: "sha"},
		"negative time": {AudioPath: "recording.mp3", Start: -1, End: 4, SHA256: "sha"},
		"no hash":       {AudioPath: "recording.mp3", Start: 2, End: 4},
	} {
		if err := store.DeleteSegmentText("text-invalid", segments[1].ID, span); err == nil {
			t.Errorf("%s: DeleteSegmentText accepted %+v", name, span)
		}
	}
	if remaining, err := store.GetSegments("text-invalid"); err != nil || len(remaining) != 3 {
		t.Fatalf("segments after refused deletions = %d (%v), want 3", len(remaining), err)
	}
}

// TestDeleteSegmentTextWithoutLocalAudioLeavesNothingToRefine fails if
// "Delete text" on a session whose audio was removed to free space leaves
// its refinement pending: no refinement can run without audio, and a Drive
// sync or summary would wait for it.
func TestDeleteSegmentTextWithoutLocalAudioLeavesNothingToRefine(t *testing.T) {
	store := newTestSQLiteStore(t)
	_, _ = segmentTestSession(t, store, "text-no-audio")
	if _, err := store.DB().Exec(`UPDATE sessions SET audio_path = '', audio_removed_at = '2026-11-11T11:00:00Z', refinement_status = ? WHERE id = 'text-no-audio'`, RefinementCompleted); err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("text-no-audio")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSegmentText("text-no-audio", segments[1].ID, textSpan()); err != nil {
		t.Fatalf("DeleteSegmentText: %v", err)
	}
	session := storeSession(t, store, "text-no-audio")
	if session.RefinementStatus != RefinementNoAudio || session.AudioPath != "" || session.AudioRemovedAt == "" {
		t.Errorf("refinement %q, audio path %q, removed at %q; want no_audio and the audio still removed", session.RefinementStatus, session.AudioPath, session.AudioRemovedAt)
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
	if err := store.DeleteSegment("text-then-audio", segments[1].ID, cut); err != nil {
		t.Fatalf("DeleteSegment: %v", err)
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

// TestATextDeletionCutOffBeforeItsRefinementIsRefinedAtStartup fails if the
// startup recovery does not list a session whose "Delete text" committed
// but whose refinement and summary a stop cut off: nothing else would run
// them, and its summary would stay empty. A session with no audio left is
// not listed (its summary is made by the startup summary pass).
func TestATextDeletionCutOffBeforeItsRefinementIsRefinedAtStartup(t *testing.T) {
	store := newTestSQLiteStore(t)
	_, _ = segmentTestSession(t, store, "text-cut-off")
	_, _ = segmentTestSession(t, store, "text-cut-off-no-audio")
	if _, err := store.DB().Exec(`UPDATE sessions SET audio_path = '', audio_removed_at = '2026-11-11T11:00:00Z' WHERE id = 'text-cut-off-no-audio'`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"text-cut-off", "text-cut-off-no-audio"} {
		segments, err := store.GetSegments(id)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.DeleteSegmentText(id, segments[1].ID, textSpan()); err != nil {
			t.Fatalf("DeleteSegmentText %s: %v", id, err)
		}
	}
	ids, err := store.DeletionsToRefine()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "text-cut-off" {
		t.Errorf("DeletionsToRefine = %v, want [text-cut-off]", ids)
	}
}

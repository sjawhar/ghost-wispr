package storage

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

func segmentTestSession(t *testing.T, store *SQLiteStore, id string) (time.Time, []transcribe.Segment) {
	t.Helper()
	started := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	if err := store.CreateSession(id, started); err != nil {
		t.Fatalf("create session: %v", err)
	}
	segments := []transcribe.Segment{
		{Speaker: 0, Text: "before bridge", StartTime: 0, EndTime: 2, Timestamp: started},
		{Speaker: 1, Text: "private deletion token", StartTime: 2, EndTime: 4, Timestamp: started.Add(2 * time.Second)},
		{Speaker: 0, Text: "after bridge", StartTime: 4, EndTime: 6, Timestamp: started.Add(4 * time.Second)},
	}
	for i := range segments {
		if err := store.AppendSegment(id, segments[i]); err != nil {
			t.Fatalf("append segment: %v", err)
		}
	}
	if err := store.EndSession(id, started.Add(6*time.Second), "recording.mp3"); err != nil {
		t.Fatalf("end session: %v", err)
	}
	return started, segments
}

func TestStoredSegmentsCarryIDs(t *testing.T) {
	store := newTestSQLiteStore(t)
	started, _ := segmentTestSession(t, store, "segment-ids")

	segments, err := store.GetSegments("segment-ids")
	if err != nil {
		t.Fatalf("get segments: %v", err)
	}
	if len(segments) != 3 {
		t.Fatalf("got %d segments, want 3", len(segments))
	}
	for i, segment := range segments {
		if segment.ID == 0 {
			t.Fatalf("segment %d has no database ID", i)
		}
		if i > 0 && segment.ID <= segments[i-1].ID {
			t.Fatalf("segment IDs are not ordered: %d then %d", segments[i-1].ID, segment.ID)
		}
	}

	inRange, err := store.GetSegmentsInTimeRange("segment-ids", started.Add(time.Second).Sub(started).Seconds(), 5)
	if err != nil {
		t.Fatalf("get segments in range: %v", err)
	}
	if len(inRange) != 1 || inRange[0].ID != segments[1].ID {
		t.Fatalf("range segment IDs = %+v, want segment %d", inRange, segments[1].ID)
	}
}

func TestDeleteSegmentRemovesTextEverywhere(t *testing.T) {
	store := newTestSQLiteStore(t)
	started, _ := segmentTestSession(t, store, "delete-everywhere")
	if err := store.UpdateSummaryAtGeneration("delete-everywhere", "private deletion token title", "summary names private deletion token", SummaryCompleted, "default", "{}", "", 0); err != nil {
		t.Fatalf("set summary: %v", err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET refined_transcript = ?, refinement_status = ?, canonical_transcript = ?, envoy_publish_status = ?, sync_status = ?, sync_state = ? WHERE id = ?`,
		"before bridge private deletion token after bridge", RefinementCompleted, "before bridge private deletion token after bridge", EnvoyPublishPublished, SyncSynced, SyncStateSynced, "delete-everywhere"); err != nil {
		t.Fatalf("set completed session state: %v", err)
	}
	if _, err := store.DB().Exec(`INSERT INTO embeddings(session_id, chunk_index, embedding, text_hash, model) VALUES(?, 0, X'01', 'hash', 'model')`, "delete-everywhere"); err != nil {
		t.Fatalf("insert embedding: %v", err)
	}
	if err := store.StoreEvent("summary_ready", `{"type":"summary_ready","session_id":"delete-everywhere","title":"private deletion token title","summary":"summary names private deletion token"}`); err != nil {
		t.Fatalf("store event: %v", err)
	}

	segments, err := store.GetSegments("delete-everywhere")
	if err != nil {
		t.Fatalf("get segments before delete: %v", err)
	}
	before := storeSession(t, store, "delete-everywhere")
	span := DeletedSpan{AudioPath: "recording.mp3", Start: 2, End: 4, SHA256Before: "before", SHA256After: "after"}
	if err := store.DeleteSegment("delete-everywhere", segments[1].ID, span); err != nil {
		t.Fatalf("DeleteSegment: %v", err)
	}

	remaining, err := store.GetSegments("delete-everywhere")
	if err != nil {
		t.Fatalf("get remaining segments: %v", err)
	}
	if len(remaining) != 2 || strings.Contains(strings.Join([]string{remaining[0].Text, remaining[1].Text}, " "), "private deletion token") {
		t.Fatalf("remaining segments = %+v, deleted text remains", remaining)
	}
	session := storeSession(t, store, "delete-everywhere")
	if session.Title != "" || session.Summary != "" || session.RefinedTranscript != "" {
		t.Fatalf("session still has deleted derived text: %+v", session)
	}
	if session.CanonicalTranscript != "before bridge\nafter bridge\n" || session.TranscriptSource != TranscriptSourceStreaming {
		t.Fatalf("canonical transcript = %q from %q, want remaining streaming segments", session.CanonicalTranscript, session.TranscriptSource)
	}
	if session.RefinementStatus != RefinementPending || session.SummaryStatus != SummaryPending || session.EnvoyPublishStatus != EnvoyPublishPending {
		t.Fatalf("follow-up states = refinement:%q summary:%q envoy:%q", session.RefinementStatus, session.SummaryStatus, session.EnvoyPublishStatus)
	}
	if session.SummaryVersion != before.SummaryVersion+1 {
		t.Fatalf("summary version = %d, want %d", session.SummaryVersion, before.SummaryVersion+1)
	}
	if !session.DriveReplacePending {
		t.Fatal("drive_replace_pending is false after deletion")
	}

	results, err := store.Search("private deletion token", SearchOptions{})
	if err != nil {
		t.Fatalf("search deleted text: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("search still finds deleted text: %+v", results)
	}
	var embeddingCount, summaryRequestCount int
	if err := store.DB().QueryRow(`SELECT count(*) FROM embeddings WHERE session_id = ?`, "delete-everywhere").Scan(&embeddingCount); err != nil {
		t.Fatalf("count embeddings: %v", err)
	}
	if err := store.DB().QueryRow(`SELECT count(*) FROM summary_requests WHERE session_id = ?`, "delete-everywhere").Scan(&summaryRequestCount); err != nil {
		t.Fatalf("count summary requests: %v", err)
	}
	if embeddingCount != 0 || summaryRequestCount != 1 {
		t.Fatalf("embeddings = %d, summary requests = %d; want 0 and 1", embeddingCount, summaryRequestCount)
	}

	events, err := store.GetEventsSince(0, 10)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("summary event remains after deletion: %+v", events)
	}

	var deleted DeletedSpan
	if err := store.DB().QueryRow(`SELECT audio_path, start, end, sha256_before, sha256_after FROM deleted_segments WHERE session_id = ?`, "delete-everywhere").Scan(&deleted.AudioPath, &deleted.Start, &deleted.End, &deleted.SHA256Before, &deleted.SHA256After); err != nil {
		t.Fatalf("read deleted segment: %v", err)
	}
	if deleted != span {
		t.Fatalf("deleted span = %+v, want %+v", deleted, span)
	}
	_ = started
}

func TestDeleteSegmentClearsOverlappingLiveEvents(t *testing.T) {
	store := newTestSQLiteStore(t)
	_, _ = segmentTestSession(t, store, "event-clear")
	if err := store.CreateSession("other-session", time.Now().UTC()); err != nil {
		t.Fatalf("create other session: %v", err)
	}
	segments, err := store.GetSegments("event-clear")
	if err != nil {
		t.Fatalf("get segments: %v", err)
	}
	for _, event := range []struct {
		eventType string
		payload   string
	}{
		{"live_transcript_interim", `{"type":"live_transcript_interim","session_id":"event-clear","start_time":2.1,"text":"private"}`},
		{"live_transcript_interim", `{"type":"live_transcript_interim","session_id":"event-clear","start_time":2.4,"text":"private deletion"}`},
		{"live_transcript", `{"type":"live_transcript","session_id":"event-clear","start_time":2,"end_time":4,"text":"private deletion token"}`},
		{"summary_ready", `{"type":"summary_ready","session_id":"event-clear","title":"Purple elephant seventeen","summary":"A paraphrase without the segment text"}`},
		{"live_transcript", `{"type":"live_transcript","session_id":"event-clear","start_time":4,"end_time":6,"text":"after bridge"}`},
		{"live_transcript", `{"type":"live_transcript","session_id":"other-session","start_time":2,"end_time":4,"text":"other session"}`},
	} {
		if err := store.StoreEvent(event.eventType, event.payload); err != nil {
			t.Fatalf("store event: %v", err)
		}
	}

	if err := store.DeleteSegment("event-clear", segments[1].ID, DeletedSpan{
		AudioPath:    "recording.mp3",
		Start:        20,
		End:          22,
		StreamStart:  2,
		StreamEnd:    4,
		SHA256Before: "before",
		SHA256After:  "after",
	}); err != nil {
		t.Fatalf("DeleteSegment: %v", err)
	}
	events, err := store.GetEventsSince(0, 10)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(events) != 2 || !strings.Contains(events[0].Payload, "after bridge") || !strings.Contains(events[1].Payload, "other session") {
		t.Fatalf("remaining events = %+v, want the neighbouring and other-session events only", events)
	}
}

func TestOpeningStoreDiscardsUnattributableLegacyLiveEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	if _, err := legacy.Exec(`CREATE TABLE mcp_events (id INTEGER PRIMARY KEY AUTOINCREMENT, event_type TEXT NOT NULL, payload TEXT NOT NULL, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("create legacy events: %v", err)
	}
	if _, err := legacy.Exec(`INSERT INTO mcp_events(event_type, payload) VALUES('live_transcript', '{"type":"live_transcript","text":"legacy secret","start_time":1,"end_time":2}')`); err != nil {
		t.Fatalf("insert legacy live event: %v", err)
	}
	if _, err := legacy.Exec(`INSERT INTO mcp_events(event_type, payload) VALUES('status_changed', '{"type":"status_changed","paused":true}')`); err != nil {
		t.Fatalf("insert unrelated legacy event: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("open migrated store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	events, err := store.GetEventsSince(0, 10)
	if err != nil {
		t.Fatalf("read migrated events: %v", err)
	}
	if len(events) != 1 || events[0].EventType != "status_changed" {
		t.Fatalf("migrated events = %+v, want only the attributable event", events)
	}
}

func TestPendingSplicesReturnsOnlyRecordingsAtTheirBeforeHash(t *testing.T) {
	store := newTestSQLiteStore(t)
	_, _ = segmentTestSession(t, store, "pending-splice")
	segments, err := store.GetSegments("pending-splice")
	if err != nil {
		t.Fatalf("get segments: %v", err)
	}
	path := filepath.Join(t.TempDir(), "recording.mp3")
	original := []byte("before splice")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatalf("write recording: %v", err)
	}
	changed := []byte("after splice")
	span := DeletedSpan{AudioPath: path, Start: 2, End: 4, SHA256Before: hashBytes(original), SHA256After: hashBytes(changed)}
	if err := store.DeleteSegment("pending-splice", segments[1].ID, span); err != nil {
		t.Fatalf("DeleteSegment: %v", err)
	}

	pending, err := store.PendingSplices()
	if err != nil {
		t.Fatalf("PendingSplices before rename: %v", err)
	}
	if len(pending) != 1 || pending[0].AudioPath != span.AudioPath || pending[0].Start != span.Start || pending[0].End != span.End || pending[0].SHA256Before != span.SHA256Before || pending[0].SHA256After != span.SHA256After || pending[0].SessionID != "pending-splice" {
		t.Fatalf("pending splices = %+v, want session %q and span %+v", pending, "pending-splice", span)
	}
	if err := os.WriteFile(path, changed, 0o600); err != nil {
		t.Fatalf("replace recording: %v", err)
	}
	pending, err = store.PendingSplices()
	if err != nil {
		t.Fatalf("PendingSplices after rename: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending splices after rename = %+v, want none", pending)
	}
	var complete int
	if err := store.DB().QueryRow(`SELECT splice_complete FROM deleted_segments WHERE session_id = ?`, "pending-splice").Scan(&complete); err != nil {
		t.Fatalf("read splice completion: %v", err)
	}
	if complete != 1 {
		t.Fatalf("splice completion = %d, want 1 after the new hash is present", complete)
	}
}

func TestDeleteSegmentClearsPriorDriveReplacementCycle(t *testing.T) {
	store := newTestSQLiteStore(t)
	_, _ = segmentTestSession(t, store, "replacement-cycle")
	if _, err := store.DB().Exec(`INSERT INTO drive_replacements(session_id, generation, name, old_id, new_id, state) VALUES('replacement-cycle', 0, 'transcript.md', 'old', 'new', 'deleted')`); err != nil {
		t.Fatalf("seed replacement: %v", err)
	}
	segments, err := store.GetSegments("replacement-cycle")
	if err != nil {
		t.Fatalf("get segments: %v", err)
	}
	if err := store.DeleteSegment("replacement-cycle", segments[1].ID, DeletedSpan{AudioPath: "recording.mp3", Start: 2, End: 4, SHA256Before: "before", SHA256After: "after"}); err != nil {
		t.Fatalf("DeleteSegment: %v", err)
	}
	var count int
	if err := store.DB().QueryRow(`SELECT count(*) FROM drive_replacements WHERE session_id = 'replacement-cycle'`).Scan(&count); err != nil {
		t.Fatalf("count replacement rows: %v", err)
	}
	if count != 0 {
		t.Fatalf("replacement rows = %d, want cleared before new Drive replacement cycle", count)
	}
}

// syncedWithoutOwedReplacement marks session id synced and owing Drive no
// replacement, so only a pending splice can keep its recording from GC.
func syncedWithoutOwedReplacement(t *testing.T, store *SQLiteStore, id string) {
	t.Helper()
	if _, err := store.DB().Exec(`UPDATE sessions SET sync_status = ?, sync_state = ?, drive_replace_pending = 0 WHERE id = ?`, SyncSynced, SyncStateSynced, id); err != nil {
		t.Fatalf("mark session %s synced: %v", id, err)
	}
}

func TestRecordingsStayRemovableWithoutPendingSplice(t *testing.T) {
	store := newTestSQLiteStore(t)
	_, _ = segmentTestSession(t, store, "gc-baseline")
	syncedWithoutOwedReplacement(t, store, "gc-baseline")
	recordings, err := store.Recordings()
	if err != nil {
		t.Fatalf("recordings: %v", err)
	}
	if len(recordings) != 1 || !recordings[0].Removable {
		t.Fatalf("recordings = %+v, want a removable recording", recordings)
	}
}

func TestRecordingsSkipPendingSegmentSplice(t *testing.T) {
	store := newTestSQLiteStore(t)
	_, _ = segmentTestSession(t, store, "pending-gc")
	segments, err := store.GetSegments("pending-gc")
	if err != nil {
		t.Fatalf("get segments: %v", err)
	}
	if err := store.DeleteSegment("pending-gc", segments[1].ID, DeletedSpan{AudioPath: "recording.mp3", Start: 2, End: 4, SHA256Before: "before", SHA256After: "after"}); err != nil {
		t.Fatalf("DeleteSegment: %v", err)
	}
	syncedWithoutOwedReplacement(t, store, "pending-gc")
	recordings, err := store.Recordings()
	if err != nil {
		t.Fatalf("Recordings: %v", err)
	}
	if len(recordings) != 1 || recordings[0].Removable {
		t.Fatalf("recordings = %+v, want pending splice held from GC", recordings)
	}
}

func TestDeleteSegmentRestoresTheFullAudioPath(t *testing.T) {
	store := newTestSQLiteStore(t)
	_, _ = segmentTestSession(t, store, "restored-audio")
	segments, err := store.GetSegments("restored-audio")
	if err != nil {
		t.Fatalf("get segments: %v", err)
	}
	const first = "/audio/20261111120000.mp3"
	const second = "/audio/20261111130000.mp3"
	if _, err := store.DB().Exec(`UPDATE sessions SET audio_path = ?, audio_removed_at = ? WHERE id = ?`, first, time.Now().UTC().Format(time.RFC3339Nano), "restored-audio"); err != nil {
		t.Fatalf("mark audio removed: %v", err)
	}
	if err := store.DeleteSegment("restored-audio", segments[1].ID, DeletedSpan{
		AudioPath:        second,
		SessionAudioPath: first + "," + second,
		Start:            2,
		End:              4,
		SHA256Before:     "before",
		SHA256After:      "after",
	}); err != nil {
		t.Fatalf("DeleteSegment: %v", err)
	}
	session := storeSession(t, store, "restored-audio")
	if session.AudioPath != first+","+second || session.AudioRemovedAt != "" {
		t.Fatalf("audio path=%q removed_at=%q, want restored full path and no marker", session.AudioPath, session.AudioRemovedAt)
	}
}

func TestDeleteSegmentRejectsStaleRefinementAndSummaryWrites(t *testing.T) {
	store := newTestSQLiteStore(t)
	_, _ = segmentTestSession(t, store, "generation-fence")
	segments, err := store.GetSegments("generation-fence")
	if err != nil {
		t.Fatalf("get segments: %v", err)
	}
	claimedAt, claimed, err := store.ClaimRefinement("generation-fence", "recording.mp3")
	if err != nil || !claimed {
		t.Fatalf("claim refinement = %v, %v", claimed, err)
	}
	// A summary writer reads the text, and with it the generation, before
	// the deletion.
	_, summaryReadAt, err := store.Canonicalize("generation-fence")
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	if err := store.DeleteSegment("generation-fence", segments[1].ID, DeletedSpan{AudioPath: "recording.mp3", Start: 2, End: 4, SHA256Before: "before", SHA256After: "after"}); err != nil {
		t.Fatalf("DeleteSegment: %v", err)
	}
	if err := store.UpdateRefinementAtGeneration("generation-fence", "stale private deletion token", RefinementCompleted, "", claimedAt); err == nil {
		t.Fatal("stale refinement write succeeded after deletion")
	}
	if err := store.UpdateSummaryAtGeneration("generation-fence", "stale private deletion token", "stale private deletion token", SummaryCompleted, "default", "{}", "", summaryReadAt); err == nil {
		t.Fatal("stale summary write succeeded after deletion")
	}
	session := storeSession(t, store, "generation-fence")
	if strings.Contains(session.RefinedTranscript, "private") || strings.Contains(session.Summary, "private") {
		t.Fatalf("stale words returned to session: %+v", session)
	}
}
func TestDeleteSegmentSecurelyErasesDeletedText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "delete.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	_, _ = segmentTestSession(t, store, "secure-delete")
	segments, err := store.GetSegments("secure-delete")
	if err != nil {
		t.Fatalf("get segments: %v", err)
	}
	const unique = "unrecoverable-deletion-phrase-93d3ef"
	if _, err := store.DB().Exec(`UPDATE segments SET text = ? WHERE id = ?`, unique, segments[1].ID); err != nil {
		t.Fatalf("plant unique text: %v", err)
	}
	if err := store.DeleteSegment("secure-delete", segments[1].ID, DeletedSpan{AudioPath: "recording.mp3", Start: 2, End: 4, SHA256Before: "before", SHA256After: "after"}); err != nil {
		t.Fatalf("DeleteSegment: %v", err)
	}
	assertDatabaseFilesLack(t, path, unique, "after bridge")
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
}

// TestEveryConnectionErasesDeletedText fails if a connection that
// database/sql opens again, after dropping the first, lacks secure_delete or
// foreign keys. No VACUUM runs after a deletion, so secure_delete on the
// connection that commits it is what zeroes the deleted words in the pages it
// frees.
func TestEveryConnectionErasesDeletedText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reconnect.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	// Every statement from here on runs on a connection opened for it.
	store.DB().SetMaxIdleConns(0)
	var secureDelete, foreignKeys, tempStore int
	if err := store.DB().QueryRow(`PRAGMA secure_delete`).Scan(&secureDelete); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`PRAGMA temp_store`).Scan(&tempStore); err != nil {
		t.Fatal(err)
	}
	if secureDelete != 1 || foreignKeys != 1 || tempStore != 2 {
		t.Errorf("a reopened connection has secure_delete=%d foreign_keys=%d temp_store=%d, want 1, 1 and 2 (memory: no temporary file frees blocks holding text)", secureDelete, foreignKeys, tempStore)
	}
	_, _ = segmentTestSession(t, store, "reconnect")
	segments, err := store.GetSegments("reconnect")
	if err != nil {
		t.Fatal(err)
	}
	const unique = "reconnected-deletion-phrase-5be1a0"
	if _, err := store.DB().Exec(`UPDATE segments SET text = ? WHERE id = ?`, unique, segments[1].ID); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSegment("reconnect", segments[1].ID, DeletedSpan{AudioPath: "recording.mp3", Start: 2, End: 4, SHA256Before: "before", SHA256After: "after"}); err != nil {
		t.Fatalf("DeleteSegment: %v", err)
	}
	store.DB().SetMaxIdleConns(1)
	if _, err := store.DB().Exec(`SELECT 1`); err != nil {
		t.Fatal(err)
	}
	assertDatabaseFilesLack(t, path, unique, "after bridge")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

// assertDatabaseFilesLack fails if secret is in any file of the open SQLite
// database at path: the database, its write-ahead log and its shared-memory
// index. It reads them while the store is open, when they all exist (SQLite
// removes the log and index when the last connection closes, so a read
// after Close would pass by reading nothing), fails if one is missing, and
// fails unless kept, text the database still holds, is found in one of them,
// so the read is shown able to see text at all.
func assertDatabaseFilesLack(t *testing.T, path, secret, kept string) {
	t.Helper()
	seenKept := false
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		contents, err := os.ReadFile(candidate)
		if err != nil {
			t.Fatalf("read %s while the store is open: %v", candidate, err)
		}
		t.Logf("%s: %d bytes", filepath.Base(candidate), len(contents))
		if strings.Contains(string(contents), secret) {
			t.Errorf("the deleted words remain in %s", candidate)
		}
		seenKept = seenKept || strings.Contains(string(contents), kept)
	}
	if !seenKept {
		t.Fatalf("no database file holds the kept text %q: the read saw no data", kept)
	}
}

func hashBytes(value []byte) string {
	hash := sha256.Sum256(value)
	return hex.EncodeToString(hash[:])
}

func storeSession(t *testing.T, store *SQLiteStore, id string) Session {
	t.Helper()
	session, err := store.GetSession(id)
	if err != nil {
		t.Fatalf("get session %s: %v", id, err)
	}
	return session
}

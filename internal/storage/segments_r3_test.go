package storage

import "testing"

func TestR3BatchSummaryAtSubmitGenerationIsRefusedAfterRefinedDelete(t *testing.T) {
	store := newTestSQLiteStore(t)
	_, _ = segmentTestSession(t, store, "s")
	revision, segs := publishRefinedSegmentTestSession(t, store, "s", "recording.mp3", "b")
	submitted, err := store.DeletionGeneration("s")
	if err != nil {
		t.Fatal(err)
	}
	_ = store.StartSummary("s")
	span := DeletedSpan{AudioPath: "recording.mp3", Start: 2, End: 4, SHA256Before: "b", SHA256After: "a"}
	if err := store.DeleteRefinedSegmentAudio("s", segs[1].ID, revision, &span); err != nil {
		t.Fatal(err)
	}
	_ = store.StartSummary("s")
	err = store.UpdateSummaryAtGeneration("s", "Private deletion token", "about private deletion token", SummaryCompleted, "default", "{}", "", submitted)
	sess, _ := store.GetSession("s")
	if err == nil || sess.Title != "" {
		t.Fatal("stale batch summary stored")
	}
}

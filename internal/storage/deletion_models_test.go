package storage

import "testing"

// TestDeleteRefinedSegmentKeepsTheRefinementModel fails if deleting a
// provenance-carrying occurrence clears the model that produced the retained
// refined words. The summary is cleared because it may repeat the deletion.
func TestDeleteRefinedSegmentKeepsTheRefinementModel(t *testing.T) {
	store := newTestSQLiteStore(t)
	segment := nestedMerge(t, store)
	for _, id := range []string{"m2", "m1"} {
		if _, err := store.DB().Exec(`UPDATE sessions SET summary_model = 'summary-model', refinement_model = 'refinement-model' WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
	}
	deleteSecret(t, store, segment)
	for _, id := range []string{"m2", "m1"} {
		sess, err := store.GetSession(id)
		if err != nil {
			t.Fatal(err)
		}
		wantRefinementModel := ""
		if id == "m2" {
			wantRefinementModel = "refinement-model"
		}
		if sess.SummaryModel != "" || sess.RefinementModel != wantRefinementModel {
			t.Errorf("session %s after the deletion: summary_model %q, refinement_model %q; want cleared summary model and refinement model %q", id, sess.SummaryModel, sess.RefinementModel, wantRefinementModel)
		}
	}
}

// TestDeleteSegmentClearsTheSpeakerNames fails if a segment deletion leaves
// the speaker names of the session it was deleted from, or of a session
// merged into it: the summary that named the speakers is cleared, and the
// names it read from the transcript can come from the deleted words.
func TestDeleteSegmentClearsTheSpeakerNames(t *testing.T) {
	store := newTestSQLiteStore(t)
	segmentID := nestedMerge(t, store)
	for _, id := range []string{"m2", "m1"} {
		if _, err := store.DB().Exec(`UPDATE sessions SET speaker_names = '{"0":"Purple Elephant"}' WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
	}
	deleteSecret(t, store, segmentID)
	for _, id := range []string{"m2", "m1"} {
		sess, err := store.GetSession(id)
		if err != nil {
			t.Fatal(err)
		}
		if sess.SpeakerNames != "{}" {
			t.Errorf("session %s after the deletion: speaker_names %q, want {} (no names)", id, sess.SpeakerNames)
		}
	}
}

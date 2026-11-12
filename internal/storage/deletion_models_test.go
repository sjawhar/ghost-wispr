package storage

import "testing"

// TestDeleteSegmentClearsTheModels fails if a segment deletion leaves the
// summary and refinement models of the session it was deleted from, or of a
// session merged into it: the summary and refined transcript they name are
// cleared, so the models would label text that no longer exists.
func TestDeleteSegmentClearsTheModels(t *testing.T) {
	store := newTestSQLiteStore(t)
	segmentID := nestedMerge(t, store)
	for _, id := range []string{"m2", "m1"} {
		if _, err := store.DB().Exec(`UPDATE sessions SET summary_model = 'summary-model', refinement_model = 'refinement-model' WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
	}
	deleteSecret(t, store, segmentID)
	for _, id := range []string{"m2", "m1"} {
		sess, err := store.GetSession(id)
		if err != nil {
			t.Fatal(err)
		}
		if sess.SummaryModel != "" || sess.RefinementModel != "" {
			t.Errorf("session %s after the deletion: summary_model %q, refinement_model %q; want both cleared", id, sess.SummaryModel, sess.RefinementModel)
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

package storage

import "testing"

func TestRestoreAudioForRefinementFencesTheAudioPath(t *testing.T) {
	store := newTestSQLiteStore(t)
	endedWith(t, store, "session", "before.mp3", RefinementNoAudio)

	restored, err := store.RestoreAudioForRefinement("session", "after-gc.mp3", "restored.mp3")
	if err != nil {
		t.Fatal(err)
	}
	if restored {
		t.Fatal("restored audio after the session audio path changed")
	}
	session, err := store.GetSession("session")
	if err != nil {
		t.Fatal(err)
	}
	if session.AudioPath != "before.mp3" {
		t.Fatalf("audio path = %q, want the path GC had not changed", session.AudioPath)
	}

	restored, err = store.RestoreAudioForRefinement("session", "before.mp3", "restored.mp3")
	if err != nil {
		t.Fatal(err)
	}
	if !restored {
		t.Fatal("did not restore audio while the path still matched")
	}
}

package gdrive

import (
	"context"
	"testing"
)

// TestARetryDoesNotAdoptADeletedSessionsFolder fails if the retry of a sync
// cut off before it recorded its folder adopts the same-named Drive folder of
// a session deleted here, which no session records any more: the upload would
// replace that session's documents on Drive with this one's and leave its
// recording in this session's folder.
func TestARetryDoesNotAdoptADeletedSessionsFolder(t *testing.T) {
	store := openStore(t)
	_, syncer := newWireDrive(t)
	o := NewOrchestrator(syncer, store)
	_, _ = round2Session(t, store, "deleted", []string{"deleted", "session"})
	_, _ = round2Session(t, store, "later", []string{"later", "session"})

	deleted := syncedSession(t, store, o, "deleted")
	if _, err := store.DeleteSession("deleted", t.TempDir()); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	cutOffBeforeFolderRecorded(t, store, "later")
	if later := syncedSession(t, store, o, "later"); later == deleted {
		t.Fatalf("the retry adopted folder %q of the deleted session", deleted)
	}
	if folders, err := syncer.ListSessionFolders(context.Background()); err != nil || len(folders) != 2 {
		t.Fatalf("app-made folders = %+v, %v; want the deleted session's and the new session's", folders, err)
	}
}

// TestARetryAdoptsAnOrphanHoldingItsOwnTranscript fails if the retry of a
// sync cut off after it uploaded into an unrecorded folder makes a second
// folder instead of adopting the one whose transcript names this session.
func TestARetryAdoptsAnOrphanHoldingItsOwnTranscript(t *testing.T) {
	store := openStore(t)
	const id = "20261106130000"
	recording, _ := round2Session(t, store, id, []string{"kept", "words"})
	wire, syncer := newWireDrive(t)
	session, err := store.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments(id)
	if err != nil {
		t.Fatal(err)
	}
	syncSession := toSyncSession(&session)
	_, folderName, _, err := buildSyncFiles(&syncSession, segments, recording)
	if err != nil {
		t.Fatal(err)
	}
	orphan := wire.add(wire.rootID, folderName, "application/vnd.google-apps.folder", nil, nil)
	wire.add(orphan, "transcript.md", "application/vnd.google-apps.document", []byte("---\nschema_version: 1\nid: \""+id+"\"\n---\n\n# Transcript\n"), nil)
	cutOffBeforeFolderRecorded(t, store, id)
	if got := syncedSession(t, store, NewOrchestrator(syncer, store), id); got != orphan {
		t.Fatalf("sync stored Drive folder %q, want the orphan %q that holds this session's transcript", got, orphan)
	}
}

// TestARetryDoesNotAdoptAFolderWithOnlyAnotherTranscript fails if removing
// the frontmatter identity check lets a foreign folder without any audio
// short-circuit the rejection.
func TestARetryDoesNotAdoptAFolderWithOnlyAnotherTranscript(t *testing.T) {
	store := openStore(t)
	const own, foreign = "own-session", "foreign-session"
	_, _ = round2Session(t, store, own, []string{"own", "session"})
	_, _ = round2Session(t, store, foreign, []string{"foreign", "session"})
	wire, syncer := newWireDrive(t)
	foreignFolder := wire.add(wire.rootID, "any-name", "application/vnd.google-apps.folder", nil, nil)
	wire.add(foreignFolder, "transcript.md", "application/vnd.google-apps.document", []byte("---\nid: \""+foreign+"\"\n---\n"), nil)
	cutOffBeforeFolderRecorded(t, store, own)
	if got := syncedSession(t, store, NewOrchestrator(syncer, store), own); got == foreignFolder {
		t.Fatalf("retry adopted foreign folder %q from its transcript alone", foreignFolder)
	}
}

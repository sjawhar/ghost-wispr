package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"path/filepath"

	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// refusedCuts puts on /api/status the cuts "Delete text and audio" refused,
// so the deleted words are still in the recording. The store keeps them
// (storage.RefusedCut), so a restart keeps the warning; a splice of the
// recording moves or drops them in its own transaction. A warning goes once
// the segment's words are gone: deleted with "Delete text", cut out by a
// later "Delete text and audio", or gone with the session.
type refusedCuts struct {
	store *storage.SQLiteStore
}

func newRefusedCuts(store *storage.SQLiteStore) *refusedCuts {
	return &refusedCuts{store: store}
}

// Refused keeps a refused cut.
func (r *refusedCuts) Refused(cut storage.RefusedCut) error {
	return r.store.RecordRefusedCut(cut)
}

// Warnings returns a warning for each refused cut whose segment is still
// there, naming the session and the segment's time, never its words, and
// forgets the others.
func (r *refusedCuts) Warnings() []string {
	cuts, err := r.store.RefusedCuts()
	if err != nil {
		return []string{fmt.Sprintf("The cuts \"Delete text and audio\" refused could not be read, so a recording may still hold words someone deleted: %v", err)}
	}
	var warnings []string
	for _, cut := range cuts {
		if !r.present(cut) {
			if err := r.store.ForgetRefusedCut(cut); err != nil {
				log.Printf("warning: %v", err)
			}
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"\"Delete text and audio\" was refused for the segment %s–%s into recording %s of session %s: the check after the cut did not confirm its words were gone, so the recording is unchanged. Use \"Delete text\" to remove its words.",
			clockOffset(cut.Start), clockOffset(cut.End), cut.Recording, cut.SessionID))
	}
	return warnings
}

// present reports whether the refused cut's segment still has words in the
// session's refined transcript. When the store cannot say, the warning stays,
// also while the session has no completed refinement, as a merged session
// before its first refinement.
func (r *refusedCuts) present(cut storage.RefusedCut) bool {
	session, err := r.store.GetSession(cut.SessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil || session.RefinementStatus != storage.RefinementCompleted || session.RefinedRevision == 0 {
		return true
	}
	words, err := r.store.RefinedWords(cut.SessionID, session.RefinedRevision)
	if err != nil {
		return true
	}
	for _, word := range words {
		if filepath.Base(word.RecordingPath) == cut.Recording && word.End > cut.Start && word.Start < cut.End {
			return true
		}
	}
	return false
}

// clockOffset writes seconds into a recording as m:ss.
func clockOffset(seconds float64) string {
	whole := int(seconds)
	return fmt.Sprintf("%d:%02d", whole/60, whole%60)
}

// apiStatusWarnings is what GET /api/status lists: the registry's warnings
// and the refused cuts'. It first clears the registry's warnings about a
// session the store no longer has (forgetWarningsOfGoneSessions): a delete
// clears them, but work that was running then can set one after.
func apiStatusWarnings(registry *warningsRegistry, refused *refusedCuts) func() []string {
	return func() []string {
		if refused.store != nil {
			forgetWarningsOfGoneSessions(registry, refused.store)
		}
		return append(registry.List(), refused.Warnings()...)
	}
}

// wireSegmentDeletion hands the server the two segment deletion actions and
// GET /api/status's warnings, which list the cuts "Delete text and audio"
// refused: one refused-cut record serves both.
func wireSegmentDeletion(hooks *server.ControlHooks, registry *warningsRegistry, refused *refusedCuts, runner *segmentDeleteRunner) {
	hooks.Warnings = apiStatusWarnings(registry, refused)
	hooks.DeleteSegment = runner.DeleteRefinedAudio
	hooks.DeleteSegmentText = runner.DeleteRefinedText
}

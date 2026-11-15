package main

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// refusedCut is a "Delete text and audio" the check after the cut refused:
// where the segment is in its recording. Its status warning names the session
// and the segment's time, never the words, which the person asked to delete.
type refusedCut struct {
	SessionID  string
	Recording  string // the recording's file name
	Start, End float64
}

// refusedCuts keeps the status warnings of refused cuts while their segments
// are there. A warning goes once the segment's words are gone: deleted with
// "Delete text", cut out by a later "Delete text and audio", or gone with the
// session. A successful cut elsewhere in the recording moves the segments
// after it, and their warnings with them.
type refusedCuts struct {
	mu   sync.Mutex
	cuts []refusedCut
	// present reports whether the segment still has words. Status reads call
	// it for each refused cut.
	present func(refusedCut) bool
}

func newRefusedCuts(present func(refusedCut) bool) *refusedCuts {
	return &refusedCuts{present: present}
}

// Refused records a refused cut.
func (r *refusedCuts) Refused(cut refusedCut) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !slices.Contains(r.cuts, cut) {
		r.cuts = append(r.cuts, cut)
	}
}

// Cut records a successful cut of start to end from the session's recording:
// a refused cut inside it is gone, and one after it moves back by its length.
func (r *refusedCuts) Cut(sessionID, recording string, start, end float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := r.cuts[:0]
	for _, cut := range r.cuts {
		if cut.SessionID == sessionID && cut.Recording == recording {
			if cut.End > start && cut.Start < end {
				continue
			}
			if cut.Start >= end {
				cut.Start -= end - start
				cut.End -= end - start
			}
		}
		kept = append(kept, cut)
	}
	r.cuts = kept
}

// Warnings returns a warning for each refused cut whose segment is still
// there, and forgets the others.
func (r *refusedCuts) Warnings() []string {
	r.mu.Lock()
	cuts := slices.Clone(r.cuts)
	r.mu.Unlock()
	var warnings []string
	var gone []refusedCut
	for _, cut := range cuts {
		if r.present != nil && !r.present(cut) {
			gone = append(gone, cut)
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"\"Delete text and audio\" was refused for the segment %s–%s into recording %s of session %s: part of it was still heard after the cut, so the recording is unchanged. Use \"Delete text\" to remove its words.",
			clockOffset(cut.Start), clockOffset(cut.End), cut.Recording, cut.SessionID))
	}
	if len(gone) > 0 {
		r.mu.Lock()
		r.cuts = slices.DeleteFunc(r.cuts, func(cut refusedCut) bool { return slices.Contains(gone, cut) })
		r.mu.Unlock()
	}
	return warnings
}

// clockOffset writes seconds into a recording as m:ss.
func clockOffset(seconds float64) string {
	whole := int(seconds)
	return fmt.Sprintf("%d:%02d", whole/60, whole%60)
}

// refusedCutPresent reports whether the refused cut's segment still has
// words in the session's refined transcript. When the store cannot say, the
// warning stays.
func refusedCutPresent(store *storage.SQLiteStore) func(refusedCut) bool {
	return func(cut refusedCut) bool {
		session, err := store.GetSession(cut.SessionID)
		if errors.Is(err, sql.ErrNoRows) {
			return false
		}
		if err != nil {
			return true
		}
		words, err := store.RefinedWords(cut.SessionID, session.RefinedRevision)
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
}

// apiStatusWarnings is what GET /api/status lists: the registry's warnings
// and the refused cuts'.
func apiStatusWarnings(registry *warningsRegistry, refused *refusedCuts) func() []string {
	return func() []string {
		return append(registry.List(), refused.Warnings()...)
	}
}

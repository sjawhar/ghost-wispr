package main

import (
	"fmt"
	"log"
	"strings"
)

// refinementModelWarningKeyPrefix keys the warning a refinement sets when it
// refused to publish because the model a "Delete text" was recorded under
// cannot be used, followed by the session's id.
const refinementModelWarningKeyPrefix = "refinement-model-"

// refinementModelWarningHook is the session manager's refinement warning
// hook: a refinement that cannot use the model a "Delete text" was recorded
// under publishes nothing and says so on GET /api/status, until the session
// refines (an empty warning).
func refinementModelWarningHook(registry *warningsRegistry) func(sessionID, warning string) {
	return func(sessionID, warning string) {
		key := refinementModelWarningKeyPrefix + sessionID
		if warning == "" {
			registry.Clear(key)
			return
		}
		registry.Set(key, fmt.Sprintf("Session %s was not refined, and keeps its deleted text out: %s", sessionID, warning))
	}
}

// sessionWarningKeyPrefixes are the registry keys of the warnings about one
// session, each followed by the session's id:
//   - recordingMismatchWarningPrefix: GC found the recording does not match
//     its Drive copy;
//   - refinementRetryWarningKeyPrefix: a "Retry refinement" failed;
//   - refinementModelWarningKeyPrefix: a refinement could not use the model a
//     "Delete text" was recorded under;
//   - driveReplacementWarningKeyPrefix: Drive has been owed a replacement too
//     long (refreshDriveReplacementWarnings sets it from the store again);
//   - pendingSpliceWarningKeyPrefix and pendingSpliceRefinementKeyPrefix: a
//     segment deletion needs audio recovery, or its refinement failed; the id
//     ends at a NUL, and the recording follows (segmentDeletionWarningKey).
//
// The raw captures a restore could not put back
// (rawEraseReservationWarningKeyPrefix) are set from the store, whose rows of
// a session go with it, so refreshRawEraseReservationWarnings clears them.
var sessionWarningKeyPrefixes = []string{
	recordingMismatchWarningPrefix,
	refinementRetryWarningKeyPrefix,
	refinementModelWarningKeyPrefix,
	driveReplacementWarningKeyPrefix,
	pendingSpliceWarningKeyPrefix,
	pendingSpliceRefinementKeyPrefix,
}

// warningSession returns the session the registry key of a warning names
// (sessionWarningKeyPrefixes), or "" for a warning about no one session.
func warningSession(key string) string {
	for _, prefix := range sessionWarningKeyPrefixes {
		rest, ok := strings.CutPrefix(key, prefix)
		if !ok {
			continue
		}
		if prefix != pendingSpliceWarningKeyPrefix && prefix != pendingSpliceRefinementKeyPrefix {
			return rest
		}
		// The start-up cleanup and recovery warnings share the prefix and
		// name no session: they have no NUL.
		id, _, found := strings.Cut(rest, "\x00")
		if !found {
			return ""
		}
		return id
	}
	return ""
}

// forgetSessionWarnings clears every registry warning about a session in
// sessionIDs, which the database no longer has: none may name a deleted
// session.
func forgetSessionWarnings(registry *warningsRegistry, sessionIDs []string) {
	deleted := make(map[string]bool, len(sessionIDs))
	for _, id := range sessionIDs {
		deleted[id] = true
	}
	registry.WithRecordingMismatch(func() {
		registry.ClearWhere(func(key string) bool { return deleted[warningSession(key)] })
	})
}

// sessionLookup says whether the database has a session.
type sessionLookup interface {
	HasSession(id string) (bool, error)
}

// forgetWarningsOfGoneSessions clears every registry warning about a session
// the database does not have: one set after the session's delete, by work
// that was running when it was deleted (a "Retry refinement" that then
// failed). A warning whose session the store cannot look up stays.
func forgetWarningsOfGoneSessions(registry *warningsRegistry, sessions sessionLookup) {
	gone := map[string]bool{}
	for _, key := range registry.KeysWithPrefix("") {
		id := warningSession(key)
		if id == "" {
			continue
		}
		if _, seen := gone[id]; seen {
			continue
		}
		exists, err := sessions.HasSession(id)
		if err != nil {
			log.Printf("warning: could not check whether session %s of a status warning exists: %v", id, err)
			exists = true
		}
		gone[id] = !exists
	}
	registry.WithRecordingMismatch(func() {
		registry.ClearWhere(func(key string) bool { return gone[warningSession(key)] })
	})
}

// sessionsDeletedHook is what runs once DELETE /api/sessions/{id} has
// deleted sessionIDs (the session, then those merged into it): it clears
// every warning that names one of them (forgetSessionWarnings), and sets the
// raw capture warnings from the store again.
func sessionsDeletedHook(store rawEraseReservationStore, registry *warningsRegistry) func(sessionIDs []string) {
	return func(sessionIDs []string) {
		forgetSessionWarnings(registry, sessionIDs)
		if err := refreshRawEraseReservationWarnings(store, registry); err != nil {
			log.Printf("warning: raw erase reservations after deleting sessions %s: %v", strings.Join(sessionIDs, ", "), err)
		}
	}
}

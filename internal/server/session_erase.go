package server

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/sjawhar/ghost-wispr/internal/erase"
	recordingname "github.com/sjawhar/ghost-wispr/internal/recording"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// registerSessionDeleteRoute registers DELETE /api/sessions/{id}: it deletes
// the session and the sessions merged into it, with their audio
// (deleteSessionErasingAudio), then tells every connected web client which
// sessions are gone (Hub.BroadcastSessionDeleted) and runs
// controls.OnSessionDeleted. It refuses an active session.
func registerSessionDeleteRoute(mux *http.ServeMux, store SessionStore, controls *ControlHooks, hub *Hub, audioDir func() string) {
	mux.HandleFunc("DELETE /api/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.PathValue("id")
		if !storage.ValidSessionID(sessionID) {
			writeJSONError(w, http.StatusForbidden, "invalid session id")
			return
		}

		sessionData, err := store.GetSession(sessionID)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, sql.ErrNoRows) {
				status = http.StatusNotFound
			}
			writeJSONError(w, status, fmt.Sprintf("get session: %v", err))
			return
		}
		if sessionData.Status == storage.SessionActive {
			writeJSONError(w, http.StatusConflict, "cannot delete active session")
			return
		}

		err = deleteSessionErasingAudio(store, sessionID, audioDir(), controls.RefreshRawEraseWarnings, controls.HoldSessionAudio, func(sessionIDs []string) {
			if hub != nil {
				hub.BroadcastSessionDeleted(sessionIDs[0], sessionIDs[1:])
			}
			if controls.OnSessionDeleted != nil {
				controls.OnSessionDeleted(sessionIDs)
			}
		})
		switch {
		case err == nil:
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, storage.ErrDeleteReplacementPending), errors.Is(err, storage.ErrDeleteMergeActive):
			writeJSONError(w, http.StatusConflict, err.Error())
		default:
			writeJSONError(w, http.StatusInternalServerError, err.Error())
		}
	})
}

// deleteSessionErasingAudio deletes session sessionID and its merged sources
// and erases their audio in recordingDir, in this order:
//   - it holds their audio (hold, when not nil), which stops a save of a kept
//     raw recording of one of them under way, until it returns;
//   - it hides the raw microphone captures while their rows still exist
//     (hideRawCaptures), so a delete that does not commit leaves them to
//     restore;
//   - it deletes the rows. If that fails it restores the raw captures (or,
//     for a session already gone, finishes erasing them) and returns why:
//     storage.ErrDeleteReplacementPending or storage.ErrDeleteMergeActive as
//     DeleteSession returned it, any other error prefixed "delete session";
//   - it calls onDeleted with the deleted sessions: sessionID, then the
//     sessions merged into it;
//   - it hides each recording the deleted row named, and every other audio
//     file of each deleted session in recordingDir, whatever the row named
//     (sessionAudioFiles), and zeroes the hidden raw captures and recordings
//     in the background.
//
// Removing a whole audio file overwrites it with zeros before the file
// system frees its blocks (Sami, 2026-11-12, "Zero every audio removal",
// dispatch://INFRA-2961/ask/172349fa-0019-488c-9d3b-9d6b7af26a81).
// DeleteSession records every erase directory before the row goes, so an
// erase cut off by a stop is finished as the app starts. The files leave
// their names before this returns; the zeros follow.
func deleteSessionErasingAudio(store SessionStore, sessionID, recordingDir string, refreshWarning func(), hold func([]string) func(), onDeleted func(sessionIDs []string)) error {
	sourceIDs, err := store.SourceSessionIDs(sessionID)
	if err != nil {
		return fmt.Errorf("list source sessions for raw erase: %w", err)
	}
	sessionIDs := append([]string{sessionID}, sourceIDs...)
	if hold != nil {
		defer hold(sessionIDs)()
	}
	rawReservations, err := hideRawCaptures(store, sessionIDs, recordingDir, refreshWarning)
	if err != nil {
		return err
	}
	audioPath, err := store.DeleteSession(sessionID, recordingDir)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			finishRawErasing(store, rawReservations)
		} else {
			restoreRawSessionErasures(store, rawReservations, refreshWarning)
		}
		if errors.Is(err, storage.ErrDeleteReplacementPending) || errors.Is(err, storage.ErrDeleteMergeActive) {
			return err
		}
		return fmt.Errorf("delete session: %w", err)
	}
	onDeleted(sessionIDs)

	pending := make([]erase.Pending, 0)
	for _, p := range sessionAudioFiles(audioPath, recordingDir, sessionIDs) {
		hidden, err := beginErasing(p)
		switch {
		case errors.Is(err, erase.ErrLeftover):
			// The file has its hidden name, but the rename may not be on
			// disk yet; Finish flushes the directory again.
			slog.Warn("could not flush the removal of a deleted session's recording; it is erased after the answer", "session_id", sessionID, "audio_path", p, "error", err)
		case err != nil:
			if !errors.Is(err, os.ErrNotExist) {
				slog.Warn("could not remove a deleted session's recording", "session_id", sessionID, "audio_path", p, "error", err)
			}
			continue
		}
		pending = append(pending, hidden)
	}
	if len(rawReservations) > 0 {
		go finishRawErasing(store, rawReservations)
	}
	if len(pending) > 0 {
		go finishErasing(sessionID, pending)
	}
	return nil
}

// sessionAudioFiles returns, once each, the files a delete of sessionIDs
// erases besides their raw captures: each recording the deleted row named
// (audioPath, found where recording.Resolve finds it in recordingDir), and,
// for each deleted session, its <id>.mp3 and <id>.wav in recordingDir and the
// temporary files a save of its kept raw recording writes
// (recording.KeptTempPaths), whatever the row named: a save cut off by a stop
// or a failed WAV write can leave one that no row names.
func sessionAudioFiles(audioPath, recordingDir string, sessionIDs []string) []string {
	var files []string
	seen := map[string]bool{}
	add := func(path string) {
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
		if !seen[path] {
			seen[path] = true
			files = append(files, path)
		}
	}
	for _, entry := range recordingname.Entries(audioPath) {
		add(recordingname.Resolve(entry, recordingDir))
	}
	if recordingDir != "" {
		for _, id := range sessionIDs {
			add(filepath.Join(recordingDir, id+".mp3"))
			add(filepath.Join(recordingDir, id+".wav"))
			for _, temp := range recordingname.KeptTempPaths(recordingDir, id) {
				add(temp)
			}
		}
	}
	return files
}

// beginErasing hides a whole recording before it is zeroed. Tests replace it
// to exercise failures before a session row is deleted.
var beginErasing = erase.Begin

// beginReservedErasing moves a raw file to the hidden name already persisted
// by ReserveRawSessionErase. Tests replace it to stop after an earlier raw
// file is hidden.
var beginReservedErasing = func(reservation erase.Reservation) (erase.Pending, error) {
	return reservation.Begin()
}

type rawEraseReservation struct {
	sessionID, originalPath, hiddenPath string
	pending                             erase.Pending
	keepHidden                          bool
}

// hideRawCaptures reserves, then hides, the raw microphone capture of each
// session in sessionIDs (a session and those merged into it), in
// recordingDir, while their rows still exist: each reservation commits before
// its rename, so a delete that does not commit leaves the capture to restore.
// When one capture cannot be hidden it restores every capture it hid
// (restoreRawSessionErasures) and returns why.
func hideRawCaptures(store SessionStore, sessionIDs []string, recordingDir string, refreshWarning func()) (reservations []rawEraseReservation, err error) {
	defer func() {
		if err != nil {
			restoreRawSessionErasures(store, reservations, refreshWarning)
			reservations = nil
		}
	}()
	for _, rawID := range sessionIDs {
		rawPath, pathErr := filepath.Abs(recordingname.RawPath(recordingDir, rawID))
		if pathErr != nil {
			return reservations, fmt.Errorf("find raw session recording: %w", pathErr)
		}
		reservation, reserveErr := erase.Reserve(rawPath)
		if reserveErr != nil {
			return reservations, fmt.Errorf("reserve raw session erase: %w", reserveErr)
		}
		persistedHiddenPath, reserveErr := store.ReserveRawSessionErase(rawID, reservation.OriginalPath(), reservation.HiddenPath())
		if reserveErr != nil {
			return reservations, fmt.Errorf("reserve raw session erase: %w", reserveErr)
		}
		reusedReservation := persistedHiddenPath != reservation.HiddenPath()
		if reservation, reserveErr = erase.ReservationAt(rawPath, persistedHiddenPath); reserveErr != nil {
			_ = store.CancelRawSessionErase(rawID, persistedHiddenPath)
			return reservations, fmt.Errorf("reuse raw session erase reservation: %w", reserveErr)
		}
		hidden, rawErr := beginReservedErasing(reservation)
		reservations = append(reservations, rawEraseReservation{
			sessionID: rawID, originalPath: rawPath, hiddenPath: persistedHiddenPath, pending: hidden,
			keepHidden: reusedReservation && rawErr != nil && !errors.Is(rawErr, erase.ErrLeftover),
		})
		switch {
		case errors.Is(rawErr, erase.ErrLeftover):
			slog.Warn("could not flush the removal of a deleted session's raw recording; it is erased after the answer", "session_id", rawID, "audio_path", rawPath, "error", rawErr)
		case errors.Is(rawErr, os.ErrNotExist):
		case rawErr != nil:
			return reservations, fmt.Errorf("hide raw session recording: %w", rawErr)
		}
	}
	return reservations, nil
}

// restoreRawSessionErasures restores every hidden raw file after a delete
// aborts. A failed restore stays reserved for startup recovery. Each error
// names the raw file's own session, which can be a merged source rather than
// the API target.
func restoreRawSessionErasures(store SessionStore, reservations []rawEraseReservation, refreshWarning func()) {
	for _, reservation := range reservations {
		if reservation.keepHidden {
			continue
		}
		if err := reservation.pending.Restore(); err != nil {
			slog.Warn("could not restore a raw session recording after its delete failed", "session_id", reservation.sessionID, "error", err)
			if refreshWarning != nil {
				refreshWarning()
			}
			continue
		}
		if err := store.CancelRawSessionErase(reservation.sessionID, reservation.hiddenPath); err != nil {
			slog.Error("could not cancel a restored raw session erase", "session_id", reservation.sessionID, "error", err)
		}
	}
}

// finishRawErasing zeroes hidden raw files after their session rows are gone.
// It also reaches a persisted hidden name that a retry reuses but Begin cannot
// return because its original name is already gone.
var finishRawErasing = func(store SessionStore, reservations []rawEraseReservation) {
	for _, reservation := range reservations {
		var err error
		if reservation.keepHidden {
			err = erase.Finish(reservation.originalPath, reservation.hiddenPath)
		} else {
			err = reservation.pending.Finish()
		}
		if err != nil {
			slog.Warn("could not overwrite a deleted raw session recording; it is erased as the app starts", "session_id", reservation.sessionID, "audio_path", reservation.originalPath, "error", err)
			continue
		}
		if err := store.CancelRawSessionErase(reservation.sessionID, reservation.hiddenPath); err != nil {
			slog.Warn("could not clear a completed raw erase reservation", "session_id", reservation.sessionID, "hidden_path", reservation.hiddenPath, "error", err)
		}
	}
}

// finishErasing overwrites with zeros and removes the recordings a session
// delete moved to hidden names, after the response. One it cannot finish
// stays under its hidden name for erase.Leftovers as the app starts. Tests
// replace it to see the response come first.
var finishErasing = func(sessionID string, pending []erase.Pending) {
	for _, p := range pending {
		if err := p.Finish(); err != nil {
			slog.Warn("could not overwrite a deleted session's recording; it is erased as the app starts", "session_id", sessionID, "error", err)
		}
	}
}

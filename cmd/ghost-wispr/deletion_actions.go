package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	recordingname "github.com/sjawhar/ghost-wispr/internal/recording"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// DeleteRefinedText removes one selected batch-derived occurrence and then
// regenerates its summary. The segment id and revision are the authority; no
// text is aligned against another transcription. An untimed segment
// (revision 0) needs no refinement.
func (d *segmentDeleteRunner) DeleteRefinedText(ctx context.Context, sessionID string, segmentID int64, revision int) error {
	if revision != 0 {
		if err := d.readyToDelete(sessionID); err != nil {
			return err
		}
	}
	if d.locks.isRecovering(sessionID) {
		return recordingRestoreInProgress()
	}
	unlock := d.locks.lock(sessionID)
	defer unlock()
	session, err := d.store.GetSession(sessionID)
	if err != nil {
		return err
	}
	if session.Status == storage.SessionActive {
		return storage.ErrSegmentSessionActive
	}
	if revision == 0 {
		if err := d.store.DeleteRefinedSegmentText(sessionID, segmentID, revision); err != nil {
			return err
		}
		if err := d.summarize(ctx, sessionID, session.StartedAt); err != nil {
			return fmt.Errorf("summarize untimed text deletion: %w", err)
		}
		return nil
	}
	if err := d.refinedSegmentsAvailable(&session); err != nil {
		return err
	}
	if err := d.store.DeleteRefinedSegmentText(sessionID, segmentID, revision); err != nil {
		return err
	}
	if err := d.summarize(ctx, sessionID, session.StartedAt); err != nil {
		return fmt.Errorf("summarize refined text deletion: %w", err)
	}
	return nil
}

// readyToDelete reads session sessionID and refuses a deletion from it while
// it is recording or has no timed words to delete from
// (refinedSegmentsAvailable). A deletion asks it before it checks for a
// recovery under way, so a session both recovering and not refined is told
// it is not refined, as the segment-delete route answered before the runner
// made this decision.
func (d *segmentDeleteRunner) readyToDelete(sessionID string) error {
	session, err := d.store.GetSession(sessionID)
	if err != nil {
		return err
	}
	if session.Status == storage.SessionActive {
		return storage.ErrSegmentSessionActive
	}
	return d.refinedSegmentsAvailable(&session)
}

// refinedSegmentsAvailable returns nil when session has a completed
// refinement with timed words, which a deletion selects from. Otherwise it
// refuses, and starts what makes the session deletable:
//   - completed without timed words (an older session with live-only
//     segments): it starts Retry refinement, which restores the recording
//     from Drive first, and returns storage.ErrRefinedSegmentsUnavailable;
//   - no recording on this unit: when one can be had
//     (refinementRecordingAvailable) it starts Retry refinement and returns
//     server.ErrSegmentRecordingRestoring, else a
//     server.SegmentRecordingUnavailableError saying why;
//   - failed: server.ErrSegmentRefinementFailed, for the user to retry;
//   - pending or running: storage.ErrRefinedSegmentsUnavailable.
func (d *segmentDeleteRunner) refinedSegmentsAvailable(session *storage.Session) error {
	switch session.RefinementStatus {
	case storage.RefinementCompleted:
		if session.RefinedRevision != 0 {
			return nil
		}
		d.startRetryRefinement(session.ID)
		return storage.ErrRefinedSegmentsUnavailable
	case storage.RefinementNoAudio:
		if err := refinementRecordingAvailable(session, d.audioDir()); err != nil {
			return &server.SegmentRecordingUnavailableError{Reason: err}
		}
		d.startRetryRefinement(session.ID)
		return server.ErrSegmentRecordingRestoring
	case storage.RefinementFailed:
		return server.ErrSegmentRefinementFailed
	default:
		return storage.ErrRefinedSegmentsUnavailable
	}
}

// startRetryRefinement starts Retry refinement of session sessionID in the
// background; a failure is kept on /api/status (retryRefinement).
func (d *segmentDeleteRunner) startRetryRefinement(sessionID string) {
	go func() { _ = d.retryRefinement(context.Background(), sessionID) }()
}

// DeleteRefinedAudio removes the selected stored word span from its recording.
// Its boundary comes from the persisted occurrence; a small margin is used
// only when no neighbouring stored word occupies it.
func (d *segmentDeleteRunner) DeleteRefinedAudio(ctx context.Context, sessionID string, segmentID int64, revision int) error {
	if err := d.readyToDelete(sessionID); err != nil {
		return err
	}
	if d.locks.isRecovering(sessionID) {
		return recordingRestoreInProgress()
	}
	unlock := d.locks.lock(sessionID)
	defer unlock()
	session, err := d.store.GetSession(sessionID)
	if err != nil {
		return err
	}
	if session.Status == storage.SessionActive {
		return storage.ErrSegmentSessionActive
	}
	if err := d.refinedSegmentsAvailable(&session); err != nil {
		return err
	}
	if revision != session.RefinedRevision {
		return storage.ErrStaleTranscriptRevision
	}
	if pending, err := d.store.HasPendingSplice(sessionID); err != nil {
		return err
	} else if pending {
		return storage.ErrSegmentSplicePending
	}
	segments, err := d.store.GetSegments(sessionID)
	if err != nil {
		return err
	}
	var selected *transcribe.Segment
	for i := range segments {
		if segments[i].ID == segmentID {
			selected = &segments[i]
			break
		}
	}
	if selected == nil {
		return storage.ErrSegmentNotFound
	}
	if selected.TranscriptRevision != revision || selected.RecordingPath == "" || selected.RecordingSHA256 == "" {
		return storage.ErrRefinedSegmentsUnavailable
	}
	if audioDir := d.audioDir(); strings.TrimSpace(audioDir) != "" {
		rawSessionIDs := []string{sessionID}
		sources, err := d.store.SourceSessionIDs(sessionID)
		if err != nil {
			return err
		}
		rawSessionIDs = append(rawSessionIDs, sources...)
		for _, rawSessionID := range rawSessionIDs {
			rawPath := recordingname.RawPath(audioDir, rawSessionID)
			if info, err := os.Stat(rawPath); err == nil && !info.IsDir() {
				return fmt.Errorf("%w: %s", server.ErrSegmentRawAudioKept, rawPath)
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("check raw recording %s: %w", rawPath, err)
			}
		}
	}
	recordingSet, err := d.ensureRecordings(ctx, session)
	if err != nil {
		return fmt.Errorf("get recordings: %w", err)
	}
	keepDownloaded := false
	defer func() {
		if !keepDownloaded {
			removeFiles(recordingSet.Downloaded)
		}
	}()
	recording := recordingNamed(recordingSet.Paths, selected.RecordingPath)
	if recording == "" {
		return fmt.Errorf("recording %s is unavailable", selected.RecordingPath)
	}
	beforeHash, err := recordingname.SHA256(recording)
	if err != nil {
		return fmt.Errorf("hash recording %s before splice: %w", recording, err)
	}
	if beforeHash != selected.RecordingSHA256 {
		return fmt.Errorf("recording %s changed since refinement; refresh it before deleting", recording)
	}
	words, err := d.store.RefinedWords(sessionID, revision)
	if err != nil {
		return err
	}
	duration, err := d.duration(ctx, recording)
	if err != nil {
		return fmt.Errorf("probe recording %s: %w", recording, err)
	}
	// The check hears the cut with the model that timed the segment's words:
	// a merged session's recordings can come from two models.
	checkModel := session.RefinementModel
	for _, word := range words {
		if word.RecordingPath == selected.RecordingPath && word.RecordingSHA256 == selected.RecordingSHA256 &&
			word.Start >= selected.StartTime && word.End <= selected.EndTime && word.Model != "" {
			checkModel = word.Model
			break
		}
	}
	tempPath, start, end, afterHash, err := d.cutAndCheck(ctx, checkModel, recording, words, selected, duration)
	if err != nil {
		if errors.Is(err, errDeletionCutHeard) || errors.Is(err, errDeletionCutUnchecked) {
			cut := storage.RefusedCut{SessionID: sessionID, Recording: filepath.Base(selected.RecordingPath), Start: selected.StartTime, End: selected.EndTime}
			if keepErr := d.refused.Refused(cut); keepErr != nil {
				return fmt.Errorf("%w; its status warning could not be kept: %v", err, keepErr)
			}
		}
		return err
	}
	keepTemp := false
	defer func() {
		if !keepTemp {
			_ = os.Remove(tempPath)
		}
	}()
	span := storage.DeletedSpan{
		SessionID: sessionID, AudioPath: recording, SessionAudioPath: strings.Join(recordingSet.Paths, ","),
		Start: start, End: end, SHA256Before: beforeHash, SHA256After: afterHash,
	}
	if err := d.store.DeleteRefinedSegmentAudio(sessionID, segmentID, revision, &span); err != nil {
		return err
	}
	keepDownloaded = true
	if err := os.Rename(tempPath, recording); err != nil {
		return fmt.Errorf("replace recording %s: %w", recording, err)
	}
	keepTemp = true
	if err := syncRecordingDirectory(recording); err != nil {
		return err
	}
	if err := d.store.MarkSpliceComplete(&span); err != nil {
		return err
	}
	if err := d.summarize(ctx, sessionID, session.StartedAt); err != nil {
		return fmt.Errorf("summarize refined audio deletion: %w", err)
	}
	return nil
}

// recordingNamed returns the path in paths with recording's base name, or "".
func recordingNamed(paths []string, recording string) string {
	for _, path := range paths {
		if filepath.Base(path) == filepath.Base(recording) {
			return path
		}
	}
	return ""
}

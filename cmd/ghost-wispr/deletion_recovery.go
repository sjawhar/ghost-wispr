package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/api/drive/v3"

	recordingname "github.com/sjawhar/ghost-wispr/internal/recording"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

type pendingSpliceOutcome struct {
	Span      storage.DeletedSpan
	Recovered bool
	Err       error
}

type refinementAfterSegmentDeletionError struct {
	sessionID string
	err       error
}

func (e refinementAfterSegmentDeletionError) Error() string {
	return fmt.Sprintf("refinement after segment deletion failed for session %s: %v", e.sessionID, e.err)
}

func (e refinementAfterSegmentDeletionError) Unwrap() error { return e.err }

// recoverAtStart runs recoverPendingSplices as the app starts and keeps its
// outcomes on /api/status, as work the stop cuts off and waits for
// (d.background.Run).
func (d *segmentDeleteRunner) recoverAtStart(ctx context.Context, source deletionRecordingSource, registry *warningsRegistry) {
	_ = d.background.Run(ctx, func(ctx context.Context) error {
		outcomes, err := d.recoverPendingSplices(ctx, source)
		applyPendingSpliceRecoveryWarnings(registry, outcomes)
		if err != nil {
			recordPendingSpliceRecoveryWarning(registry, err)
			return nil
		}
		recovered := 0
		for i := range outcomes {
			if outcomes[i].Recovered {
				recovered++
			}
		}
		if recovered > 0 {
			log.Printf("recovered %d pending segment splices", recovered)
		}
		return nil
	})
}

// recoverPendingSplices completes, as the app starts, recordings whose
// deletion transaction committed before its replacement file reached disk,
// restoring them from source (Drive, or nil when the app has none) as needed,
// then refines, and with it summarizes, every session whose deletion has its
// splice done but whose refinement or summary a stop cut off
// (storage.DeletionsToRefine): a splice found complete is not the end of the
// deletion, whose Drive replacement waits for both. It returns one outcome
// per pending span and per session it refines only, so the caller owns the
// lifetime of each status warning.
func (d *segmentDeleteRunner) recoverPendingSplices(ctx context.Context, source deletionRecordingSource) (outcomes []pendingSpliceOutcome, err error) {
	audioDir := d.audioDir()
	pending, err := d.store.PendingSplices(audioDir)
	if err != nil {
		return nil, err
	}
	lost, err := d.store.MissingCompletedSplices(audioDir)
	if err != nil {
		return nil, err
	}
	outcomes = make([]pendingSpliceOutcome, 0, len(pending)+len(lost))
	refined := make(map[string]bool)
	recoveredRecordings := make(map[string]bool)
	for i := range lost {
		span := lost[i]
		key := span.SessionID + "\x00" + filepath.Base(span.AudioPath)
		if recoveredRecordings[key] {
			continue
		}
		recoveredRecordings[key] = true
		recovered, recoveryErr := d.recoverLostSplicedRecording(ctx, source, audioDir, &span)
		outcomes = append(outcomes, pendingSpliceOutcome{Span: span, Recovered: recovered, Err: recoveryErr})
		// A failed lost-cut recovery already has the actionable outcome and
		// must not also emit a second, generic Drive-refinement failure.
		refined[span.SessionID] = true
	}
	for i := range pending {
		span := pending[i]
		recovered, recoveryErr := d.recoverPendingSplice(ctx, source, audioDir, &span)
		outcomes = append(outcomes, pendingSpliceOutcome{Span: span, Recovered: recovered, Err: recoveryErr})
		// A splice recovered here was refined with it.
		refined[span.SessionID] = refined[span.SessionID] || recovered
	}
	sessionIDs, err := d.store.DeletionsToRefine()
	if err != nil {
		return outcomes, err
	}
	for _, sessionID := range sessionIDs {
		if !refined[sessionID] {
			outcomes = append(outcomes, d.refineUnfinishedDeletion(ctx, source, audioDir, sessionID))
		}
	}
	return outcomes, nil
}

func (d *segmentDeleteRunner) recoverLostSplicedRecording(ctx context.Context, source deletionRecordingSource, audioDir string, first *storage.DeletedSpan) (bool, error) {
	unlock, done := d.locks.lockForRecovery(first.SessionID)
	defer done()

	session, err := d.store.GetSession(first.SessionID)
	if err != nil {
		return false, fmt.Errorf("get session %s for lost cut recovery: %w", first.SessionID, err)
	}
	recording := recordingname.Resolve(first.AudioPath, audioDir)
	if _, err := os.Stat(recording); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("stat lost cut recording %s: %w", recording, err)
	}
	if source == nil || session.GDriveFolderID == "" {
		return false, fmt.Errorf("recover lost cut recording %s: no Drive recording is available", recording)
	}
	files, err := source.ListFolderFiles(ctx, session.GDriveFolderID)
	if err != nil {
		return false, fmt.Errorf("list Drive recordings for lost cut %s: %w", recording, err)
	}
	var candidates []*drive.File
	for _, file := range files {
		name, ok := recordingname.LocalName(file.Name)
		if ok && strings.HasPrefix(file.MimeType, "audio/") && name == filepath.Base(recording) {
			candidates = append(candidates, file)
		}
	}
	downloaded, err := downloadDestination(recording)
	if err != nil {
		return false, err
	}
	if err := downloadDriveRecording(ctx, d.store, session.ID, session.GDriveFolderID, source, candidates, downloaded, recording, recordingname.FileName(recording), true); err != nil {
		_ = os.Remove(downloaded)
		return false, fmt.Errorf("restore pre-cut recording %s: %w", recording, err)
	}
	keepDownloaded := false
	defer func() {
		if !keepDownloaded {
			_ = os.Remove(downloaded)
		}
	}()
	spans, err := d.store.CompletedSplicesForRecording(first.SessionID, first.AudioPath)
	if err != nil {
		return false, err
	}
	if len(spans) == 0 {
		return false, fmt.Errorf("recover lost cut recording %s: no completed cut is recorded", recording)
	}
	hash, err := recordingname.SHA256(downloaded)
	if err != nil {
		return false, fmt.Errorf("hash restored pre-cut recording %s: %w", downloaded, err)
	}
	for i := range spans {
		span := &spans[i]
		if hash == span.SHA256After {
			continue
		}
		if hash != span.SHA256Before {
			return false, fmt.Errorf("restored pre-cut recording %s has SHA-256 %s, want %s", downloaded, hash, span.SHA256Before)
		}
		if err := d.resplice(ctx, downloaded, span); err != nil {
			return false, fmt.Errorf("recover lost cut recording %s: %w", recording, err)
		}
		if err := syncRecordingDirectory(downloaded); err != nil {
			return false, err
		}
		hash = span.SHA256After
	}
	if err := os.Rename(downloaded, recording); err != nil {
		return false, fmt.Errorf("install recovered cut recording %s: %w", recording, err)
	}
	if err := syncRecordingDirectory(recording); err != nil {
		return false, err
	}
	keepDownloaded = true
	unlock()
	if err := d.refine(ctx, first.SessionID, session.AudioPath, session.StartedAt); err != nil {
		return true, refinementAfterSegmentDeletionError{sessionID: first.SessionID, err: err}
	}
	return true, nil
}

// refineUnfinishedDeletion runs the refinement, and with it the summary, of
// session sessionID, whose segment deletion has its splice done but whose
// refinement or summary a stop cut off, as the deletion runner runs them
// after its splice. A deletion of the session is refused while it runs, as
// during a splice recovery; one already under way finishes first, and the
// session is read as it left it.
func (d *segmentDeleteRunner) refineUnfinishedDeletion(ctx context.Context, source deletionRecordingSource, audioDir, sessionID string) pendingSpliceOutcome {
	unlock, done := d.locks.lockForRecovery(sessionID)
	defer done()
	session, err := d.store.GetSession(sessionID)
	unlock()
	outcome := pendingSpliceOutcome{Span: storage.DeletedSpan{SessionID: sessionID, AudioPath: session.AudioPath}}
	if err == nil {
		err = d.restoreRefinementRecording(ctx, d.store, &session, audioDir, func(ctx context.Context, current storage.Session) (deletionRecordingSet, error) {
			return restoreDeletionRecordings(ctx, d.store, &current, audioDir, source, "")
		})
	}
	if err == nil {
		session, err = d.store.GetSession(sessionID)
	}
	if err == nil {
		err = d.refine(ctx, sessionID, session.AudioPath, session.StartedAt)
	} else {
		err = fmt.Errorf("get or restore session %s to refine it: %w", sessionID, err)
	}
	if err != nil {
		outcome.Err = refinementAfterSegmentDeletionError{sessionID: sessionID, err: err}
	}
	return outcome
}

func (d *segmentDeleteRunner) recoverPendingSplice(ctx context.Context, source deletionRecordingSource, audioDir string, span *storage.DeletedSpan) (bool, error) {
	unlock, done := d.locks.lockForRecovery(span.SessionID)
	defer done()

	session, err := d.store.GetSession(span.SessionID)
	if err != nil {
		return false, fmt.Errorf("get session %s for pending splice: %w", span.SessionID, err)
	}
	recordingSet, err := restoreDeletionRecordings(ctx, d.store, &session, audioDir, source, filepath.Base(span.AudioPath))
	if err != nil {
		return false, fmt.Errorf("restore pending splice %s: %w", span.AudioPath, err)
	}
	recording := recordingNamed(recordingSet.Paths, span.AudioPath)
	if recording == "" {
		removeFiles(recordingSet.Downloaded)
		return false, fmt.Errorf("pending splice recording %s is unavailable", span.AudioPath)
	}
	hash, err := recordingname.SHA256(recording)
	if err != nil {
		removeFiles(recordingSet.Downloaded)
		return false, fmt.Errorf("hash pending splice recording %s: %w", recording, err)
	}
	if hash == span.SHA256After {
		if err := syncRecordingDirectory(recording); err != nil {
			return false, err
		}
		if err := d.store.MarkSpliceComplete(span); err != nil {
			return false, err
		}
		return false, nil
	}
	if hash != span.SHA256Before {
		removeFiles(recordingSet.Downloaded)
		return false, fmt.Errorf("pending splice recording %s has SHA-256 %s, want %s", recording, hash, span.SHA256Before)
	}
	if err := d.resplice(ctx, recording, span); err != nil {
		removeFiles(recordingSet.Downloaded)
		return false, err
	}
	if err := syncRecordingDirectory(recording); err != nil {
		return false, err
	}
	if err := d.store.MarkSpliceComplete(span); err != nil {
		return false, err
	}
	unlock()
	if err := d.refine(ctx, span.SessionID, session.AudioPath, session.StartedAt); err != nil {
		return true, refinementAfterSegmentDeletionError{sessionID: span.SessionID, err: err}
	}
	return true, nil
}

// resplice cuts span out of recording again, through a copy, and puts the
// copy in recording's place only when it has the SHA-256 span records after
// the cut. The caller flushes the directory.
func (d *segmentDeleteRunner) resplice(ctx context.Context, recording string, span *storage.DeletedSpan) error {
	tempPath, err := copyForSplice(recording)
	if err != nil {
		return fmt.Errorf("copy %s to re-splice it: %w", recording, err)
	}
	afterHash, err := d.splice(ctx, tempPath, span.Start, span.End)
	if err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("re-splice %s: %w", recording, err)
	}
	if afterHash != span.SHA256After {
		_ = os.Remove(tempPath)
		return fmt.Errorf("re-splice %s produced SHA-256 %s, want %s", recording, afterHash, span.SHA256After)
	}
	if err := os.Rename(tempPath, recording); err != nil {
		return fmt.Errorf("replace %s with its re-splice: %w", recording, err)
	}
	return nil
}

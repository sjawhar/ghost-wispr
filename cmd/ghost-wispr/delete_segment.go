package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"google.golang.org/api/drive/v3"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/erase"
	recordingname "github.com/sjawhar/ghost-wispr/internal/recording"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

type segmentDeleteRunner struct {
	store            *storage.SQLiteStore
	transcriber      audio.Transcriber
	audioDir         string
	ensureRecordings func(context.Context, storage.Session) (deletionRecordingSet, error)
	duration         func(context.Context, string) (float64, error)
	splice           func(context.Context, string, float64, float64) (string, error)
	refine           func(context.Context, string, string, time.Time) error
	deleteSegment    func(sessionID string, segmentID int64, span storage.DeletedSpan) error
	summarize        func(context.Context, string, time.Time) error
}

func requireSegmentForDeletion(store *storage.SQLiteStore, sessionID string, segmentID int64) error {
	segments, err := store.GetSegments(sessionID)
	if err != nil {
		return err
	}
	for _, segment := range segments {
		if segment.ID == segmentID {
			return nil
		}
	}
	return storage.ErrSegmentNotFound
}

type deletionRecordingSet struct {
	Paths      []string
	Downloaded []string
}

type deletionRecordingSource interface {
	ListFolderFiles(ctx context.Context, folderID string) ([]*drive.File, error)
	DownloadRecording(ctx context.Context, folderID, name, destination string) error
}

type sessionDeletionLocks struct {
	mu         sync.Mutex
	locks      map[string]*sync.Mutex
	recovering map[string]bool
}

func (l *sessionDeletionLocks) lock(sessionID string) func() {
	l.mu.Lock()
	if l.locks == nil {
		l.locks = make(map[string]*sync.Mutex)
	}
	lock := l.locks[sessionID]
	if lock == nil {
		lock = &sync.Mutex{}
		l.locks[sessionID] = lock
	}
	l.mu.Unlock()
	lock.Lock()
	return lock.Unlock
}

func (l *sessionDeletionLocks) setRecovering(sessionID string, recovering bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.recovering == nil {
		l.recovering = make(map[string]bool)
	}
	if recovering {
		l.recovering[sessionID] = true
	} else {
		delete(l.recovering, sessionID)
	}
}

func (l *sessionDeletionLocks) isRecovering(sessionID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.recovering[sessionID]
}

var deleteLocks sessionDeletionLocks

func usableRecordingSource(source deletionRecordingSource) bool {
	if source == nil {
		return false
	}
	value := reflect.ValueOf(source)
	return value.Kind() != reflect.Ptr || !value.IsNil()
}

// restoreDeletionRecordings combines the session's locally known recordings
// with every audio file Drive has for it, restoring only missing local files.
// Each entry of the session's audio path is read where recording.Resolve
// finds it (as written, then in audioDir), so the recording spliced is the one
// the app plays and syncs; one found in neither place is restored into
// audioDir.
func restoreDeletionRecordings(ctx context.Context, session *storage.Session, audioDir string, source deletionRecordingSource) (deletionRecordingSet, error) {
	byName := make(map[string]string)
	for _, entry := range strings.Split(session.AudioPath, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		path := recordingname.Resolve(entry, audioDir)
		byName[filepath.Base(path)] = path
	}
	if session.GDriveFolderID != "" && usableRecordingSource(source) {
		files, err := source.ListFolderFiles(ctx, session.GDriveFolderID)
		if err != nil {
			return deletionRecordingSet{}, fmt.Errorf("list Drive recordings: %w", err)
		}
		for _, file := range files {
			name, ok := recordingname.LocalName(file.Name)
			if !strings.HasPrefix(file.MimeType, "audio/") || !ok {
				continue
			}
			if _, known := byName[name]; !known {
				if strings.TrimSpace(audioDir) == "" {
					return deletionRecordingSet{}, fmt.Errorf("restore Drive recording %s: audio directory is not configured", file.Name)
				}
				byName[name] = filepath.Join(audioDir, name)
			}
		}
	}
	if len(byName) == 0 {
		return deletionRecordingSet{}, nil
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	set := deletionRecordingSet{Paths: make([]string, 0, len(names))}
	for _, name := range names {
		path := byName[name]
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			set.Paths = append(set.Paths, path)
			continue
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return deletionRecordingSet{}, fmt.Errorf("stat recording %s: %w", path, err)
		}
		if source == nil || session.GDriveFolderID == "" {
			return deletionRecordingSet{}, fmt.Errorf("recording %s is missing locally and cannot be restored from Drive", path)
		}
		if err := source.DownloadRecording(ctx, session.GDriveFolderID, recordingname.FileName(path), path); err != nil {
			return deletionRecordingSet{}, fmt.Errorf("restore recording %s: %w", name, err)
		}
		set.Paths = append(set.Paths, path)
		set.Downloaded = append(set.Downloaded, path)
	}
	return set, nil
}

// Delete is "Delete text and audio": it removes the segment's words from
// every text store, cuts its span out of the recording, and owes Google
// Drive a replacement of the session's files (storage.DeleteSegment).
func (d *segmentDeleteRunner) Delete(ctx context.Context, sessionID string, segmentID int64) error {
	return d.delete(ctx, sessionID, segmentID, false)
}

// DeleteText is "Delete text": it removes the segment's words from every
// text store as Delete does, and records where they are in the recording,
// which it leaves as it is, so refinement leaves them out
// (storage.DeleteSegmentText). It splices nothing, keeps no recording it
// downloads to find the words, and owes Drive no replacement: the ordinary
// sync updates the session's Drive documents in place. A segment whose words
// cannot be found in the recording is refused, as Delete refuses it.
func (d *segmentDeleteRunner) DeleteText(ctx context.Context, sessionID string, segmentID int64) error {
	return d.delete(ctx, sessionID, segmentID, true)
}

func (d *segmentDeleteRunner) delete(ctx context.Context, sessionID string, segmentID int64, keepAudio bool) error {
	if d.store == nil {
		return fmt.Errorf("segment deletion storage is not configured")
	}
	if deleteLocks.isRecovering(sessionID) {
		return storage.ErrSegmentSplicePending
	}
	unlock := deleteLocks.lock(sessionID)
	defer unlock()
	if deleteLocks.isRecovering(sessionID) {
		return storage.ErrSegmentSplicePending
	}
	session, err := d.store.GetSession(sessionID)
	if err != nil {
		return err
	}
	if session.Status == storage.SessionActive {
		return storage.ErrSegmentSessionActive
	}
	segments, err := d.store.GetSegments(sessionID)
	if err != nil {
		return err
	}
	index := -1
	for i, segment := range segments {
		if segment.ID == segmentID {
			index = i
			break
		}
	}
	if index == -1 {
		return storage.ErrSegmentNotFound
	}
	if pending, err := d.store.HasPendingSplice(sessionID); err != nil {
		return err
	} else if pending {
		return storage.ErrSegmentSplicePending
	}
	if d.transcriber == nil || d.ensureRecordings == nil || !keepAudio && d.splice == nil {
		return fmt.Errorf("segment deletion is not configured")
	}
	// A raw recording kept beside the session's would still hold the audio
	// a splice cuts; "Delete text" cuts none.
	if !keepAudio && strings.TrimSpace(d.audioDir) != "" {
		rawSessionIDs := []string{sessionID}
		sources, err := d.store.SourceSessionIDs(sessionID)
		if err != nil {
			return err
		}
		rawSessionIDs = append(rawSessionIDs, sources...)
		for _, rawSessionID := range rawSessionIDs {
			rawPath := filepath.Join(d.audioDir, rawSessionID+".pcm")
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
	recordings := recordingSet.Paths
	if len(recordings) == 0 {
		return fmt.Errorf("session %s has no recording", sessionID)
	}
	keepDownloaded := false
	defer func() {
		if keepDownloaded {
			return
		}
		for _, downloaded := range recordingSet.Downloaded {
			_ = os.Remove(downloaded)
		}
	}()
	// The words "Delete text" already removed are still in the recordings,
	// but they are no segment's: they are neither the segment's nor its
	// neighbours' when it is located and its cut is checked.
	candidates := make([]audio.Recording, len(recordings))
	textSpans := make([][]storage.TextDeletionSpan, len(recordings))
	for i, recording := range recordings {
		words, err := d.transcriber.Words(ctx, recording)
		if err != nil {
			return fmt.Errorf("transcribe recording %s: %w", recording, err)
		}
		duration := wordDuration(words)
		if !keepAudio && d.duration != nil {
			duration, err = d.duration(ctx, recording)
			if err != nil {
				return fmt.Errorf("probe recording %s: %w", recording, err)
			}
		}
		if textSpans[i], err = d.deletedTextSpans(recording); err != nil {
			return err
		}
		candidates[i] = audio.Recording{Words: wordsNotDeleted(words, textSpans[i], 0, 0), Duration: duration}
	}
	before, after := neighbourText(segments, index)
	if keepAudio {
		return d.deleteText(ctx, sessionID, segmentID, segments, index, &recordingSet, candidates)
	}
	recordIndex, start, end, _, err := audio.Locate(candidates, segments[index].Text, before, after)
	if err := locatedRecording(recordIndex, len(recordings), err); err != nil {
		return err
	}

	recording := recordings[recordIndex]
	beforeHash, err := fileSHA256(recording)
	if err != nil {
		return fmt.Errorf("hash recording %s before splice: %w", recording, err)
	}
	tempPath, err := copyForSplice(recording)
	if err != nil {
		return err
	}
	keepTemp := false
	defer func() {
		if !keepTemp {
			_ = os.Remove(tempPath)
		}
	}()
	afterHash, err := d.splice(ctx, tempPath, start, end)
	if err != nil {
		return fmt.Errorf("splice recording %s: %w", recording, err)
	}
	if afterHash == "" {
		afterHash, err = fileSHA256(tempPath)
		if err != nil {
			return fmt.Errorf("hash spliced recording %s: %w", tempPath, err)
		}
	}
	wordsAfter, err := d.transcriber.Words(ctx, tempPath)
	if err != nil {
		return fmt.Errorf("transcribe spliced recording %s: %w", recording, err)
	}
	wordsAfter = wordsNotDeleted(wordsAfter, textSpans[recordIndex], start, end-start)
	if survivors := audio.Survivors(wordsAfter, start, segments[index].Text, before, after); len(survivors) > 0 {
		return fmt.Errorf("%w: surviving words: %s", server.ErrSegmentNotRemovedCleanly, strings.Join(survivors, ", "))
	}
	// A neighbour in another recording has no words on this recording's side
	// of the cut, so it cannot have been over-cut here. Validate only sides
	// for which the selected recording supplied pre-cut words.
	beforeCheck := recordingHasWordsOnSide(candidates[recordIndex].Words, start, end, true)
	afterCheck := recordingHasWordsOnSide(candidates[recordIndex].Words, start, end, false)
	if beforeCheck {
		if beforeMissing := missingNeighbourTokens(wordsAfter, start, before, true); len(beforeMissing) > 1 {
			return fmt.Errorf("%w: before-neighbour words missing: %s", server.ErrSegmentNotRemovedCleanly, strings.Join(beforeMissing, ", "))
		}
	}
	if afterCheck {
		if afterMissing := missingNeighbourTokens(wordsAfter, start, after, false); len(afterMissing) > 1 {
			return fmt.Errorf("%w: after-neighbour words missing: %s", server.ErrSegmentNotRemovedCleanly, strings.Join(afterMissing, ", "))
		}
	}

	span := storage.DeletedSpan{SessionID: sessionID, AudioPath: recording, SessionAudioPath: strings.Join(recordings, ","), Start: start, End: end, StreamStart: segments[index].StartTime, StreamEnd: segments[index].EndTime, SHA256Before: beforeHash, SHA256After: afterHash}
	deleteSegment := d.store.DeleteSegment
	if d.deleteSegment != nil {
		deleteSegment = d.deleteSegment
	}
	maintenanceErr := deleteSegment(sessionID, segmentID, span)
	if maintenanceErr != nil && !errors.Is(maintenanceErr, storage.ErrSegmentPostCommitMaintenance) {
		return maintenanceErr
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
	if d.refine != nil {
		updated, err := d.store.GetSession(sessionID)
		if err != nil {
			return fmt.Errorf("get spliced session %s: %w", sessionID, err)
		}
		if err := d.refine(ctx, sessionID, updated.AudioPath, updated.StartedAt); err != nil {
			return fmt.Errorf("refine spliced session %s: %w", sessionID, err)
		}
	}
	if maintenanceErr != nil {
		return maintenanceErr
	}
	return nil
}

// deletedTextSpans returns the spans "Delete text" recorded in recording that
// describe its bytes as they are now (storage.TextDeletionSpan.SHA256).
func (d *segmentDeleteRunner) deletedTextSpans(recording string) ([]storage.TextDeletionSpan, error) {
	spans, err := d.store.TextDeletionSpans(recording)
	if err != nil || len(spans) == 0 {
		return nil, err
	}
	hash, err := fileSHA256(recording)
	if err != nil {
		return nil, fmt.Errorf("hash recording %s: %w", recording, err)
	}
	current := spans[:0]
	for _, span := range spans {
		if span.SHA256 == hash {
			current = append(current, span)
		}
	}
	return current, nil
}

// wordsNotDeleted returns words without the ones "Delete text" removed from
// their recording (storage.TextDeleted by spans). For the words of a copy
// with cutLength seconds cut out at cutStart, a word after the cut is that
// much later in the recording the spans describe.
func wordsNotDeleted(words []audio.Word, spans []storage.TextDeletionSpan, cutStart, cutLength float64) []audio.Word {
	if len(spans) == 0 {
		return words
	}
	kept := make([]audio.Word, 0, len(words))
	for _, word := range words {
		start, end := word.Start, word.End
		if cutLength > 0 && (start+end)/2 >= cutStart {
			start, end = start+cutLength, end+cutLength
		}
		if !storage.TextDeleted(spans, start, end) {
			kept = append(kept, word)
		}
	}
	return kept
}

// locatedRecording is the runner's error for a locator's answer: none for a
// match in one of n recordings, server.ErrSegmentNotFoundInRecording when the
// segment's words were not found, so the deletion is refused.
func locatedRecording(record, n int, err error) error {
	if err != nil {
		if errors.Is(err, audio.ErrSegmentNotFound) {
			return server.ErrSegmentNotFoundInRecording
		}
		return fmt.Errorf("locate segment in recording: %w", err)
	}
	if record < 0 || record >= n {
		return fmt.Errorf("locator returned recording index %d for %d recordings", record, n)
	}
	return nil
}

// deleteText is DeleteText's work once the recordings are transcribed
// (candidates, the recordings' words, in session order): it locates the
// segment's aligned words, records their span with the deletion, removes the
// recordings it downloaded to find them, and makes the refinement and
// summary again, without the words: the refinement when the session still
// has audio, the summary alone when it has none.
func (d *segmentDeleteRunner) deleteText(ctx context.Context, sessionID string, segmentID int64, segments []transcribe.Segment, index int, recordings *deletionRecordingSet, candidates []audio.Recording) error {
	text := segments[index].Text
	before, after := neighbourText(segments, index)
	recordIndex, start, end, _, err := audio.LocateWords(candidates, text, before, after)
	if err := locatedRecording(recordIndex, len(recordings.Paths), err); err != nil {
		return err
	}
	if err := validateTextDeletionSpan(candidates, recordIndex, start, end, segments, index); err != nil {
		return err
	}
	recording := recordings.Paths[recordIndex]
	hash, err := fileSHA256(recording)
	if err != nil {
		return fmt.Errorf("hash recording %s: %w", recording, err)
	}
	maintenanceErr := d.store.DeleteSegmentText(sessionID, segmentID, storage.TextDeletionSpan{AudioPath: recording, Start: start, End: end, SHA256: hash})
	if maintenanceErr != nil && !errors.Is(maintenanceErr, storage.ErrSegmentPostCommitMaintenance) {
		return maintenanceErr
	}
	removeFiles(recordings.Downloaded)
	updated, err := d.store.GetSession(sessionID)
	if err != nil {
		return fmt.Errorf("get session %s after deleting text: %w", sessionID, err)
	}
	// The deletion leaves nothing to refine (no_audio) when the session's
	// audio, or part of it, was removed to free space.
	if updated.RefinementStatus != storage.RefinementNoAudio && strings.TrimSpace(updated.AudioPath) != "" {
		if d.refine != nil {
			if err := d.refine(ctx, sessionID, updated.AudioPath, updated.StartedAt); err != nil {
				return fmt.Errorf("refine session %s after deleting text: %w", sessionID, err)
			}
		}
	} else if d.summarize != nil {
		if err := d.summarize(ctx, sessionID, updated.StartedAt); err != nil {
			return fmt.Errorf("summarize session %s after deleting text: %w", sessionID, err)
		}
	}
	return maintenanceErr
}

// textDeletionBounds has explicit presence flags so a valid word boundary at
// zero seconds is not mistaken for a missing recording-edge bound.
type textDeletionBounds struct {
	lower, upper       float64
	hasLower, hasUpper bool
}

// validateTextDeletionSpan proves that the words a later refinement would
// keep after leaving out start to end contain no named segment word, lose no
// more than one word from either neighbour, and leave no word between the
// span and each aligned neighbour (or recording edge). A text deletion has
// no audio splice to validate afterwards, so it validates the words already
// transcribed to locate it before it records the span.

func validateTextDeletionSpan(candidates []audio.Recording, recordIndex int, start, end float64, segments []transcribe.Segment, index int) error {
	words := candidates[recordIndex].Words
	text := segments[index].Text
	before, after := neighbourText(segments, index)
	beforeInRecord, afterInRecord := false, false
	var bounds textDeletionBounds
	length := end - start

	if index > 0 {
		beforeBefore := ""
		if index > 1 {
			beforeBefore = segments[index-2].Text
		}
		// Do not use text here: it is the disputed segment and an exact
		// decoy can otherwise pull the neighbour match around itself.
		neighbourRecord, neighbourStart, neighbourEnd, _, err := audio.LocateWords(candidates, before, beforeBefore, "")
		if err != nil {
			return fmt.Errorf("%w: before-neighbour could not be aligned", server.ErrSegmentNotRemovedCleanly)
		}
		if neighbourRecord == recordIndex {
			if neighbourEnd > start || wordsRemainBetween(words, neighbourEnd, start) {
				return fmt.Errorf("%w: words between the segment and its before-neighbour are outside the aligned span", server.ErrSegmentNotRemovedCleanly)
			}
			// Bound survivors by the neighbour's outer edge, not merely
			// the edge beside the deleted span.
			bounds.lower, bounds.hasLower, beforeInRecord = neighbourStart, true, true
		} else if recordingHasWordsOnSide(words, start, end, true) {
			return fmt.Errorf("%w: words before the segment reach the recording edge", server.ErrSegmentNotRemovedCleanly)
		}
	} else if recordingHasWordsOnSide(words, start, end, true) {
		return fmt.Errorf("%w: words before the segment reach the recording edge", server.ErrSegmentNotRemovedCleanly)
	}

	if index+1 < len(segments) {
		afterAfter := ""
		if index+2 < len(segments) {
			afterAfter = segments[index+2].Text
		}
		neighbourRecord, neighbourStart, neighbourEnd, _, err := audio.LocateWords(candidates, after, "", afterAfter)
		if err != nil {
			return fmt.Errorf("%w: after-neighbour could not be aligned", server.ErrSegmentNotRemovedCleanly)
		}
		if neighbourRecord == recordIndex {
			if neighbourStart < end || wordsRemainBetween(words, end, neighbourStart) {
				return fmt.Errorf("%w: words between the segment and its after-neighbour are outside the aligned span", server.ErrSegmentNotRemovedCleanly)
			}
			bounds.upper, bounds.hasUpper, afterInRecord = neighbourEnd-length, true, true
		} else if recordingHasWordsOnSide(words, start, end, false) {
			return fmt.Errorf("%w: words after the segment reach the recording edge", server.ErrSegmentNotRemovedCleanly)
		}
	} else if recordingHasWordsOnSide(words, start, end, false) {
		return fmt.Errorf("%w: words after the segment reach the recording edge", server.ErrSegmentNotRemovedCleanly)
	}

	afterWords := wordsAfterTextDeletion(words, start, end)
	if survivors := audio.Survivors(wordsBetweenBounds(afterWords, bounds), start, text, before, after); len(survivors) > 0 {
		return fmt.Errorf("%w: surviving words: %s", server.ErrSegmentNotRemovedCleanly, strings.Join(survivors, ", "))
	}
	if beforeInRecord {
		if missing := missingNeighbourTokens(afterWords, start, before, true); len(missing) > 1 {
			return fmt.Errorf("%w: before-neighbour words missing: %s", server.ErrSegmentNotRemovedCleanly, strings.Join(missing, ", "))
		}
	}
	if afterInRecord {
		if missing := missingNeighbourTokens(afterWords, start, after, false); len(missing) > 1 {
			return fmt.Errorf("%w: after-neighbour words missing: %s", server.ErrSegmentNotRemovedCleanly, strings.Join(missing, ", "))
		}
	}
	return nil
}

// wordsAfterTextDeletion returns words as a text deletion's later refinement
// sees them: words whose middle is in the aligned span go, later words move
// back by the missing duration.
func wordsAfterTextDeletion(words []audio.Word, start, end float64) []audio.Word {
	length := end - start
	kept := make([]audio.Word, 0, len(words))
	for _, word := range words {
		middle := (word.Start + word.End) / 2
		switch {
		case middle >= start && middle <= end:
			continue
		case middle > end:
			word.Start -= length
			word.End -= length
		}
		kept = append(kept, word)
	}
	return kept
}

func wordsRemainBetween(words []audio.Word, lower, upper float64) bool {
	for _, word := range words {
		middle := (word.Start + word.End) / 2
		if middle > lower && middle < upper {
			return true
		}
	}
	return false
}

// wordsBetweenBounds restricts Survivors to the aligned neighbouring
// segments. A missing boundary is a recording edge; validateTextDeletionSpan
// has already proved it holds no unspanned words. Explicit flags keep a real
// word boundary at 0.0 distinct from no boundary.
func wordsBetweenBounds(words []audio.Word, bounds textDeletionBounds) []audio.Word {
	near := make([]audio.Word, 0, len(words))
	for _, word := range words {
		if (!bounds.hasLower || word.End >= bounds.lower) &&
			(!bounds.hasUpper || word.Start <= bounds.upper) {
			near = append(near, word)
		}
	}
	return near
}

// recordingHasWordsOnSide reports whether the selected recording has words on
// the corresponding side of the cut.
func recordingHasWordsOnSide(words []audio.Word, start, end float64, before bool) bool {
	for _, word := range words {
		if before && word.Start < start {
			return true
		}
		if !before && word.End > end {
			return true
		}
	}
	return false
}

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

// recoverPendingSplices completes recordings whose deletion transaction
// committed before its replacement file reached disk, then refines, and
// with it summarizes, every session whose deletion has its splice done but
// whose refinement or summary a stop cut off (storage.DeletionsToRefine): a
// splice found complete is not the end of the deletion, whose Drive
// replacement waits for both. It returns one outcome per pending span and
// per session it refines only, so the caller owns the lifetime of each
// status warning.
func recoverPendingSplices(ctx context.Context, store *storage.SQLiteStore, audioDir string, source deletionRecordingSource, splice func(context.Context, string, float64, float64) (string, error), refine func(context.Context, string, string, time.Time) error) (outcomes []pendingSpliceOutcome, err error) {
	spans, err := store.PendingSplices(audioDir)
	if err != nil {
		return nil, err
	}
	outcomes = make([]pendingSpliceOutcome, 0, len(spans))
	refined := make(map[string]bool)
	for i := range spans {
		span := spans[i]
		recovered, recoveryErr := recoverPendingSplice(ctx, store, audioDir, source, splice, refine, &span)
		outcomes = append(outcomes, pendingSpliceOutcome{Span: span, Recovered: recovered, Err: recoveryErr})
		// A splice recovered here was refined with it.
		refined[span.SessionID] = refined[span.SessionID] || recovered
	}
	if refine == nil {
		return outcomes, nil
	}
	sessionIDs, err := store.DeletionsToRefine()
	if err != nil {
		return outcomes, err
	}
	for _, sessionID := range sessionIDs {
		if !refined[sessionID] {
			outcomes = append(outcomes, refineUnfinishedDeletion(ctx, store, refine, sessionID))
		}
	}
	return outcomes, nil
}

// refineUnfinishedDeletion runs the refinement, and with it the summary, of
// session sessionID, whose segment deletion has its splice done but whose
// refinement or summary a stop cut off, as the deletion runner runs them
// after its splice. A deletion of the session is refused while it runs, as
// during a splice recovery; one already under way finishes first, and the
// session is read as it left it.
func refineUnfinishedDeletion(ctx context.Context, store *storage.SQLiteStore, refine func(context.Context, string, string, time.Time) error, sessionID string) pendingSpliceOutcome {
	deleteLocks.setRecovering(sessionID, true)
	defer deleteLocks.setRecovering(sessionID, false)
	unlock := deleteLocks.lock(sessionID)
	session, err := store.GetSession(sessionID)
	unlock()
	outcome := pendingSpliceOutcome{Span: storage.DeletedSpan{SessionID: sessionID, AudioPath: session.AudioPath}}
	if err == nil {
		err = refine(ctx, sessionID, session.AudioPath, session.StartedAt)
	} else {
		err = fmt.Errorf("get session %s to refine it: %w", sessionID, err)
	}
	if err != nil {
		outcome.Err = refinementAfterSegmentDeletionError{sessionID: sessionID, err: err}
	}
	return outcome
}

func recoverPendingSplice(ctx context.Context, store *storage.SQLiteStore, audioDir string, source deletionRecordingSource, splice func(context.Context, string, float64, float64) (string, error), refine func(context.Context, string, string, time.Time) error, span *storage.DeletedSpan) (bool, error) {
	deleteLocks.setRecovering(span.SessionID, true)
	defer deleteLocks.setRecovering(span.SessionID, false)

	unlock := deleteLocks.lock(span.SessionID)
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()

	session, err := store.GetSession(span.SessionID)
	if err != nil {
		return false, fmt.Errorf("get session %s for pending splice: %w", span.SessionID, err)
	}
	recordingSet, err := restoreDeletionRecordings(ctx, &session, audioDir, source)
	if err != nil {
		return false, fmt.Errorf("restore pending splice %s: %w", span.AudioPath, err)
	}
	recording := ""
	for _, candidate := range recordingSet.Paths {
		if filepath.Base(candidate) == filepath.Base(span.AudioPath) {
			recording = candidate
			break
		}
	}
	if recording == "" {
		removeFiles(recordingSet.Downloaded)
		return false, fmt.Errorf("pending splice recording %s is unavailable", span.AudioPath)
	}
	hash, err := fileSHA256(recording)
	if err != nil {
		removeFiles(recordingSet.Downloaded)
		return false, fmt.Errorf("hash pending splice recording %s: %w", recording, err)
	}
	if hash == span.SHA256After {
		if err := syncRecordingDirectory(recording); err != nil {
			return false, err
		}
		if err := store.MarkSpliceComplete(span); err != nil {
			return false, err
		}
		return false, nil
	}
	if hash != span.SHA256Before {
		removeFiles(recordingSet.Downloaded)
		return false, fmt.Errorf("pending splice recording %s has SHA-256 %s, want %s", recording, hash, span.SHA256Before)
	}
	tempPath, err := copyForSplice(recording)
	if err != nil {
		removeFiles(recordingSet.Downloaded)
		return false, fmt.Errorf("copy pending splice %s: %w", recording, err)
	}
	afterHash, err := splice(ctx, tempPath, span.Start, span.End)
	if err != nil {
		_ = os.Remove(tempPath)
		removeFiles(recordingSet.Downloaded)
		return false, fmt.Errorf("re-splice %s: %w", recording, err)
	}
	if afterHash != span.SHA256After {
		_ = os.Remove(tempPath)
		removeFiles(recordingSet.Downloaded)
		return false, fmt.Errorf("re-splice %s produced SHA-256 %s, want %s", recording, afterHash, span.SHA256After)
	}
	if err := os.Rename(tempPath, recording); err != nil {
		return false, fmt.Errorf("replace recovered recording %s: %w", recording, err)
	}
	if err := syncRecordingDirectory(recording); err != nil {
		return false, err
	}
	if err := store.MarkSpliceComplete(span); err != nil {
		return false, err
	}
	unlock()
	locked = false
	if refine != nil {
		if err := refine(ctx, span.SessionID, session.AudioPath, session.StartedAt); err != nil {
			return true, refinementAfterSegmentDeletionError{sessionID: span.SessionID, err: err}
		}
	}
	return true, nil
}

// syncRecordingDirectory flushes the directory holding recording, so the
// rename that put the spliced recording there is on disk before the splice is
// marked complete: after a power loss the mark must not stand beside the
// unspliced recording, which the Drive replacement would then upload as the
// clean copy.
var syncRecordingDirectory = func(recording string) error {
	dir := filepath.Dir(recording)
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open recording directory %s: %w", dir, err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("flush recording directory %s: %w", dir, err)
	}
	return nil
}

// removeFiles removes paths with a plain unlink: a segment deletion zeroes
// no whole audio file (Sami, 2026-11-12, dispatch://INFRA-2961/ask/172349fa-0019-488c-9d3b-9d6b7af26a81).
func removeFiles(paths []string) {
	for _, path := range paths {
		_ = os.Remove(path)
	}
}

// cleanupSpliceTempCopies removes, as the app starts and before any deletion
// or recovery runs, the temporary files a stopped app can leave in audioDir,
// each a whole copy of a recording: a splice's copy (.delete-segment-*) and
// output (.splice-*), and a recording downloaded from Drive for a splice
// (.download-*, gdrive.Syncer.DownloadRecording). It removes them with a
// plain unlink, since a segment deletion zeroes no whole audio file, and
// finishes overwriting any file a whole-file removal (GC, the session delete
// route, a saved session's raw recording) left part way erased
// (erase.Leftovers).
func cleanupSpliceTempCopies(audioDir string) error {
	return cleanupSpliceTempCopiesExcept(audioDir, nil)
}

// cleanupSpliceTempCopiesExcept leaves reserved hidden raw captures whole:
// their session still exists, and startup could not restore them yet.
func cleanupSpliceTempCopiesExcept(audioDir string, reserved map[string]bool) error {
	entries, err := os.ReadDir(audioDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read audio directory %s: %w", audioDir, err)
	}
	var cleanupErr error
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, ".delete-segment-") && !strings.HasPrefix(name, ".splice-") && !strings.HasPrefix(name, ".download-") {
			continue
		}
		if err := os.Remove(filepath.Join(audioDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	return errors.Join(cleanupErr, erase.LeftoversExcept(audioDir, func(path string) bool { return reserved[path] }))
}

// recordingDirectoryLister lists the directories recordings are kept or
// erased in outside the audio directory, forgets the erase directories the
// startup cleanup has finished, and records the audio directory as one.
type recordingDirectoryLister interface {
	RecordingDirectories() ([]string, error)
	ReservedRawErasePaths() (map[string]bool, error)
	ForgetEraseDirectories(dirs []string) error
	RecordEraseDirectory(dir string) error
}

// cleanupRecordingDirectories runs cleanupSpliceTempCopies, as the app
// starts, in audioDir and in every directory a session keeps a recording in
// or a whole-file erase was started in (storage.RecordingDirectories):
// splice copies, downloads and cut-off erasures are made beside the
// recording they copy or erase. Each directory is named by its absolute path,
// as the store lists it, so a relative audio_dir is cleaned once. It then
// hands the directories it cleaned without an error to
// ForgetEraseDirectories, which forgets those that were there to clean, and
// last records audioDir as an erase directory: the recorder erases a saved
// session's raw recording there without recording where, so an erase of it a
// stop cuts off is still finished at a start with another audio_dir.
func cleanupRecordingDirectories(store recordingDirectoryLister, audioDir string) error {
	audioDir, err := filepath.Abs(audioDir)
	if err != nil {
		return fmt.Errorf("find the audio directory: %w", err)
	}
	reserved, err := store.ReservedRawErasePaths()
	if err != nil {
		return fmt.Errorf("list reserved raw erases: %w", err)
	}
	dirs := []string{audioDir}
	others, err := store.RecordingDirectories()
	if err != nil {
		return errors.Join(err, cleanupSpliceTempCopiesExcept(audioDir, reserved), store.RecordEraseDirectory(audioDir))
	}
	for _, dir := range others {
		if dir, err = filepath.Abs(dir); err != nil {
			return fmt.Errorf("find the recording directory: %w", err)
		}
		if !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}
	var cleanupErr error
	var cleaned []string
	for _, dir := range dirs {
		if err := cleanupSpliceTempCopiesExcept(dir, reserved); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
			continue
		}
		cleaned = append(cleaned, dir)
	}
	return errors.Join(cleanupErr, store.ForgetEraseDirectories(cleaned), store.RecordEraseDirectory(audioDir))
}

func neighbourText(segments []transcribe.Segment, index int) (before, after string) {
	if index > 0 {
		before = segments[index-1].Text
	}
	if index+1 < len(segments) {
		after = segments[index+1].Text
	}
	return before, after
}

func wordDuration(words []audio.Word) float64 {
	var duration float64
	for _, word := range words {
		if word.End > duration {
			duration = word.End
		}
	}
	return duration
}

const neighbourTokenLimit = 6

func missingNeighbourTokens(words []audio.Word, splice float64, neighbour string, before bool) []string {
	expected := nearestTokens(deletionTokens(neighbour), before)
	if len(expected) == 0 {
		return nil
	}
	var actual []string
	for _, word := range words {
		if before && word.End <= splice {
			actual = append(actual, deletionTokens(word.Text)...)
		}
		if !before && word.Start >= splice {
			actual = append(actual, deletionTokens(word.Text)...)
		}
	}
	return orderedMissing(expected, nearestTokens(actual, before))
}

func nearestTokens(tokens []string, before bool) []string {
	if len(tokens) <= neighbourTokenLimit {
		return tokens
	}
	if before {
		return tokens[len(tokens)-neighbourTokenLimit:]
	}
	return tokens[:neighbourTokenLimit]
}

func orderedMissing(expected, actual []string) []string {
	rows := make([][]int, len(expected)+1)
	for i := range rows {
		rows[i] = make([]int, len(actual)+1)
	}
	for i := len(expected) - 1; i >= 0; i-- {
		for j := len(actual) - 1; j >= 0; j-- {
			switch {
			case expected[i] == actual[j]:
				rows[i][j] = rows[i+1][j+1] + 1
			case rows[i+1][j] >= rows[i][j+1]:
				rows[i][j] = rows[i+1][j]
			default:
				rows[i][j] = rows[i][j+1]
			}
		}
	}
	missing := make([]string, 0, len(expected)-rows[0][0])
	for i, j := 0, 0; i < len(expected); {
		switch {
		case j < len(actual) && expected[i] == actual[j]:
			i++
			j++
		case j < len(actual) && rows[i][j+1] > rows[i+1][j]:
			j++
		default:
			missing = append(missing, expected[i])
			i++
		}
	}
	return missing
}

func deletionTokens(text string) []string {
	var tokens []string
	for _, field := range strings.Fields(text) {
		var token strings.Builder
		for _, r := range field {
			if unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r) {
				token.WriteRune(unicode.ToLower(r))
			}
		}
		if token.Len() > 0 {
			tokens = append(tokens, token.String())
		}
	}
	return tokens
}

func copyForSplice(source string) (string, error) {
	input, err := os.Open(source)
	if err != nil {
		return "", fmt.Errorf("open recording %s: %w", source, err)
	}
	defer func() { _ = input.Close() }()
	ext := filepath.Ext(source)
	output, err := os.CreateTemp(filepath.Dir(source), ".delete-segment-*"+ext)
	if err != nil {
		return "", fmt.Errorf("create splice copy beside %s: %w", source, err)
	}
	path := output.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	if _, err = io.Copy(output, input); err != nil {
		_ = output.Close()
		return "", fmt.Errorf("copy recording %s: %w", source, err)
	}
	if err = output.Sync(); err != nil {
		_ = output.Close()
		return "", fmt.Errorf("flush splice copy %s: %w", path, err)
	}
	if err = output.Close(); err != nil {
		return "", fmt.Errorf("close splice copy %s: %w", path, err)
	}
	return path, nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

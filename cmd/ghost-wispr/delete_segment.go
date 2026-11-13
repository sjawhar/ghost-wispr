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

	"google.golang.org/api/drive/v3"

	"github.com/sjawhar/ghost-wispr/internal/erase"
	recordingname "github.com/sjawhar/ghost-wispr/internal/recording"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

type segmentDeleteRunner struct {
	store            *storage.SQLiteStore
	audioDir         string
	ensureRecordings func(context.Context, storage.Session) (deletionRecordingSet, error)
	duration         func(context.Context, string) (float64, error)
	splice           func(context.Context, string, float64, float64) (string, error)
	refine           func(context.Context, string, string, time.Time) error
	summarize        func(context.Context, string, time.Time) error
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

// DeleteRefinedText removes one selected batch-derived occurrence and then
// regenerates its summary. The segment id and revision are the authority; no
// text is aligned against another transcription.
func (d *segmentDeleteRunner) DeleteRefinedText(ctx context.Context, sessionID string, segmentID int64, revision int) error {
	if d.store == nil {
		return fmt.Errorf("segment deletion storage is not configured")
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
	if session.RefinementStatus != storage.RefinementCompleted || session.RefinedRevision == 0 {
		// Old completed sessions have live-only segments. Start the existing
		// batch refinement path, then require selection from its new table.
		if session.RefinementStatus == storage.RefinementCompleted && session.RefinedRevision == 0 &&
			d.refine != nil && strings.TrimSpace(session.AudioPath) != "" {
			go func() { _ = d.refine(context.Background(), sessionID, session.AudioPath, session.StartedAt) }()
		}
		return storage.ErrRefinedSegmentsUnavailable
	}
	if err := d.store.DeleteRefinedSegmentText(sessionID, segmentID, revision); err != nil {
		return err
	}
	if d.summarize != nil {
		if err := d.summarize(ctx, sessionID, session.StartedAt); err != nil {
			return fmt.Errorf("summarize refined text deletion: %w", err)
		}
	}
	return nil
}

// DeleteRefinedAudio removes the selected stored word span from its recording.
// Its boundary comes from the persisted occurrence; a small margin is used
// only when no neighbouring stored word occupies it.
func (d *segmentDeleteRunner) DeleteRefinedAudio(ctx context.Context, sessionID string, segmentID int64, revision int) error {
	if d.store == nil || d.ensureRecordings == nil || d.splice == nil {
		return fmt.Errorf("segment deletion is not configured")
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
	if session.RefinementStatus != storage.RefinementCompleted || session.RefinedRevision == 0 {
		if session.RefinementStatus == storage.RefinementCompleted && session.RefinedRevision == 0 &&
			d.refine != nil && strings.TrimSpace(session.AudioPath) != "" {
			go func() { _ = d.refine(context.Background(), sessionID, session.AudioPath, session.StartedAt) }()
		}
		return storage.ErrRefinedSegmentsUnavailable
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
	if strings.TrimSpace(d.audioDir) != "" {
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
	keepDownloaded := false
	defer func() {
		if !keepDownloaded {
			removeFiles(recordingSet.Downloaded)
		}
	}()
	recording := ""
	for _, path := range recordingSet.Paths {
		if filepath.Base(path) == filepath.Base(selected.RecordingPath) {
			recording = path
			break
		}
	}
	if recording == "" {
		return fmt.Errorf("recording %s is unavailable", selected.RecordingPath)
	}
	beforeHash, err := fileSHA256(recording)
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
	start, end := refinedSpliceBounds(words, selected)
	if d.duration != nil {
		duration, err := d.duration(ctx, recording)
		if err != nil {
			return fmt.Errorf("probe recording %s: %w", recording, err)
		}
		if end > duration {
			end = duration
		}
	}
	if end <= start {
		return fmt.Errorf("stored word span for segment %d has no removable audio", segmentID)
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
	if d.summarize != nil {
		if err := d.summarize(ctx, sessionID, session.StartedAt); err != nil {
			return fmt.Errorf("summarize refined audio deletion: %w", err)
		}
	}
	return nil
}

func refinedSpliceBounds(words []storage.RefinedWord, selected *transcribe.Segment) (float64, float64) {
	const margin = 0.25
	start, end := selected.StartTime-margin, selected.EndTime+margin
	if start < 0 {
		start = 0
	}
	for _, word := range words {
		if word.RecordingPath != selected.RecordingPath || word.RecordingSHA256 != selected.RecordingSHA256 {
			continue
		}
		if word.End <= selected.StartTime && word.End > start {
			start = word.End
		}
		if word.Start >= selected.EndTime && word.Start < end {
			end = word.Start
		}
	}
	return start, end
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
	spans, err := store.PendingSplices()
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
	return errors.Join(cleanupErr, erase.Leftovers(audioDir))
}

// recordingDirectoryLister lists the directories recordings are kept or
// erased in outside the audio directory, forgets the erase directories the
// startup cleanup has finished, and records the audio directory as one.
type recordingDirectoryLister interface {
	RecordingDirectories() ([]string, error)
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
	dirs := []string{audioDir}
	others, err := store.RecordingDirectories()
	if err != nil {
		return errors.Join(err, cleanupSpliceTempCopies(audioDir), store.RecordEraseDirectory(audioDir))
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
		if err := cleanupSpliceTempCopies(dir); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
			continue
		}
		cleaned = append(cleaned, dir)
	}
	return errors.Join(cleanupErr, store.ForgetEraseDirectories(cleaned), store.RecordEraseDirectory(audioDir))
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

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
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"google.golang.org/api/drive/v3"

	"github.com/sjawhar/ghost-wispr/internal/audio"
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
	mu    sync.Mutex
	locks map[string]*sync.Mutex
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
func restoreDeletionRecordings(ctx context.Context, session *storage.Session, audioDir string, source deletionRecordingSource) (deletionRecordingSet, error) {
	byName := make(map[string]string)
	for _, entry := range strings.Split(session.AudioPath, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		path := entry
		if !filepath.IsAbs(path) && audioDir != "" {
			path = filepath.Join(audioDir, filepath.Base(path))
		}
		byName[filepath.Base(path)] = path
	}
	if session.GDriveFolderID != "" && usableRecordingSource(source) {
		files, err := source.ListFolderFiles(ctx, session.GDriveFolderID)
		if err != nil {
			return deletionRecordingSet{}, fmt.Errorf("list Drive recordings: %w", err)
		}
		for _, file := range files {
			if !strings.HasPrefix(file.MimeType, "audio/") || !strings.HasPrefix(file.Name, "audio-") {
				continue
			}
			name := strings.TrimPrefix(file.Name, "audio-")
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
		if err := source.DownloadRecording(ctx, session.GDriveFolderID, "audio-"+name, path); err != nil {
			return deletionRecordingSet{}, fmt.Errorf("restore recording %s: %w", name, err)
		}
		set.Paths = append(set.Paths, path)
		set.Downloaded = append(set.Downloaded, path)
	}
	return set, nil
}

func (d segmentDeleteRunner) Delete(ctx context.Context, sessionID string, segmentID int64) error {
	if d.store == nil {
		return fmt.Errorf("segment deletion storage is not configured")
	}
	unlock := deleteLocks.lock(sessionID)
	defer unlock()
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
	if d.transcriber == nil || d.ensureRecordings == nil || d.splice == nil {
		return fmt.Errorf("segment deletion is not configured")
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
	candidates := make([]audio.Recording, len(recordings))
	for i, recording := range recordings {
		words, err := d.transcriber.Words(ctx, recording)
		if err != nil {
			return fmt.Errorf("transcribe recording %s: %w", recording, err)
		}
		duration := wordDuration(words)
		if d.duration != nil {
			duration, err = d.duration(ctx, recording)
			if err != nil {
				return fmt.Errorf("probe recording %s: %w", recording, err)
			}
		}
		candidates[i] = audio.Recording{Words: words, Duration: duration}
	}
	before, after := neighbourText(segments, index)
	recordIndex, start, end, _, err := audio.Locate(candidates, segments[index].Text, before, after)
	if err != nil {
		if errors.Is(err, audio.ErrSegmentNotFound) {
			return server.ErrSegmentNotFoundInRecording
		}
		return fmt.Errorf("locate segment in recording: %w", err)
	}
	if recordIndex < 0 || recordIndex >= len(recordings) {
		return fmt.Errorf("locator returned recording index %d for %d recordings", recordIndex, len(recordings))
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
	if survivors := audio.Survivors(wordsAfter, start, segments[index].Text, before, after); len(survivors) > 0 {
		return fmt.Errorf("%w: surviving words: %s", server.ErrSegmentNotRemovedCleanly, strings.Join(survivors, ", "))
	}
	if beforeMissing := missingNeighbourTokens(wordsAfter, start, before, true); len(beforeMissing) > 1 {
		return fmt.Errorf("%w: before-neighbour words missing: %s", server.ErrSegmentNotRemovedCleanly, strings.Join(beforeMissing, ", "))
	}
	if afterMissing := missingNeighbourTokens(wordsAfter, start, after, false); len(afterMissing) > 1 {
		return fmt.Errorf("%w: after-neighbour words missing: %s", server.ErrSegmentNotRemovedCleanly, strings.Join(afterMissing, ", "))
	}

	span := storage.DeletedSpan{SessionID: sessionID, AudioPath: recording, SessionAudioPath: strings.Join(recordings, ","), Start: start, End: end, StreamStart: segments[index].StartTime, StreamEnd: segments[index].EndTime, SHA256Before: beforeHash, SHA256After: afterHash}
	maintenanceErr := d.store.DeleteSegment(sessionID, segmentID, span)
	if maintenanceErr != nil && !errors.Is(maintenanceErr, storage.ErrSegmentPostCommitMaintenance) {
		return maintenanceErr
	}
	keepDownloaded = true
	if err := os.Rename(tempPath, recording); err != nil {
		return fmt.Errorf("replace recording %s: %w", recording, err)
	}
	keepTemp = true
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

// recoverPendingSplices completes recordings whose deletion transaction
// committed before its replacement file reached disk. One bad row is reported
// through warn and never prevents later rows from recovering.
func recoverPendingSplices(ctx context.Context, store *storage.SQLiteStore, audioDir string, source deletionRecordingSource, splice func(context.Context, string, float64, float64) (string, error), refine func(context.Context, string, string, time.Time) error, warn func(error)) (int, error) {
	if warn == nil {
		warn = func(error) {}
	}
	spans, err := store.PendingSplices()
	if err != nil {
		return 0, err
	}
	recovered := 0
	for _, span := range spans {
		session, err := store.GetSession(span.SessionID)
		if err != nil {
			warn(fmt.Errorf("get session %s for pending splice: %w", span.SessionID, err))
			continue
		}
		recordingSet, err := restoreDeletionRecordings(ctx, &session, audioDir, source)
		if err != nil {
			warn(fmt.Errorf("restore pending splice %s: %w", span.AudioPath, err))
			continue
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
			warn(fmt.Errorf("pending splice recording %s is unavailable", span.AudioPath))
			continue
		}
		hash, err := fileSHA256(recording)
		if err != nil {
			removeFiles(recordingSet.Downloaded)
			warn(fmt.Errorf("hash pending splice recording %s: %w", recording, err))
			continue
		}
		if hash == span.SHA256After {
			if err := store.MarkSpliceComplete(&span); err != nil {
				warn(err)
			}
			continue
		}
		if hash != span.SHA256Before {
			removeFiles(recordingSet.Downloaded)
			warn(fmt.Errorf("pending splice recording %s has SHA-256 %s, want %s", recording, hash, span.SHA256Before))
			continue
		}
		afterHash, err := splice(ctx, recording, span.Start, span.End)
		if err != nil {
			removeFiles(recordingSet.Downloaded)
			warn(fmt.Errorf("re-splice %s: %w", recording, err))
			continue
		}
		if afterHash != span.SHA256After {
			warn(fmt.Errorf("re-splice %s produced SHA-256 %s, want %s", recording, afterHash, span.SHA256After))
			continue
		}
		if err := store.MarkSpliceComplete(&span); err != nil {
			warn(err)
			continue
		}
		if refine != nil {
			if err := refine(ctx, span.SessionID, session.AudioPath, session.StartedAt); err != nil {
				warn(fmt.Errorf("refine re-spliced session %s: %w", span.SessionID, err))
				continue
			}
		}
		recovered++
	}
	return recovered, nil
}

func removeFiles(paths []string) {
	for _, path := range paths {
		_ = os.Remove(path)
	}
}

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
		if entry.IsDir() || !strings.HasPrefix(name, ".delete-segment-") && !strings.HasPrefix(name, ".splice-") {
			continue
		}
		if err := os.Remove(filepath.Join(audioDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	return cleanupErr
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
			if expected[i] == actual[j] {
				rows[i][j] = rows[i+1][j+1] + 1
			} else if rows[i+1][j] >= rows[i][j+1] {
				rows[i][j] = rows[i+1][j]
			} else {
				rows[i][j] = rows[i][j+1]
			}
		}
	}
	missing := make([]string, 0, len(expected)-rows[0][0])
	for i, j := 0, 0; i < len(expected); {
		if j < len(actual) && expected[i] == actual[j] {
			i++
			j++
		} else if j < len(actual) && rows[i][j+1] > rows[i+1][j] {
			j++
		} else {
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

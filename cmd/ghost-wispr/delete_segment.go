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
	"sort"
	"strings"
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
	ensureRecordings func(context.Context, storage.Session) ([]string, error)
	splice           func(context.Context, string, float64, float64) (string, error)
	refine           func(context.Context, string, string, time.Time) error
}

type deletionRecordingSource interface {
	ListFolderFiles(ctx context.Context, folderID string) ([]*drive.File, error)
	DownloadRecording(ctx context.Context, folderID, name, destination string) error
}

// restoreDeletionRecordings combines the session's locally known recordings
// with every audio file Drive has for it, restoring only missing local files.
func restoreDeletionRecordings(ctx context.Context, session *storage.Session, audioDir string, source deletionRecordingSource) ([]string, error) {
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
	if session.GDriveFolderID != "" && source != nil {
		files, err := source.ListFolderFiles(ctx, session.GDriveFolderID)
		if err != nil {
			return nil, fmt.Errorf("list Drive recordings: %w", err)
		}
		for _, file := range files {
			if !strings.HasPrefix(file.MimeType, "audio/") || !strings.HasPrefix(file.Name, "audio-") {
				continue
			}
			name := strings.TrimPrefix(file.Name, "audio-")
			if _, known := byName[name]; !known {
				if strings.TrimSpace(audioDir) == "" {
					return nil, fmt.Errorf("restore Drive recording %s: audio directory is not configured", file.Name)
				}
				byName[name] = filepath.Join(audioDir, name)
			}
		}
	}
	if len(byName) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	recordings := make([]string, 0, len(names))
	for _, name := range names {
		path := byName[name]
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			recordings = append(recordings, path)
			continue
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("stat recording %s: %w", path, err)
		}
		if source == nil || session.GDriveFolderID == "" {
			return nil, fmt.Errorf("recording %s is missing locally and cannot be restored from Drive", path)
		}
		if err := source.DownloadRecording(ctx, session.GDriveFolderID, "audio-"+name, path); err != nil {
			return nil, fmt.Errorf("restore recording %s: %w", name, err)
		}
		recordings = append(recordings, path)
	}
	return recordings, nil
}

func (d segmentDeleteRunner) Delete(ctx context.Context, sessionID string, segmentID int64) error {
	if d.store == nil || d.transcriber == nil || d.ensureRecordings == nil || d.splice == nil {
		return fmt.Errorf("segment deletion is not configured")
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
	if strings.TrimSpace(d.audioDir) != "" {
		rawPath := filepath.Join(d.audioDir, sessionID+".pcm")
		if info, err := os.Stat(rawPath); err == nil && !info.IsDir() {
			return fmt.Errorf("%w: %s", server.ErrSegmentRawAudioKept, rawPath)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("check raw recording %s: %w", rawPath, err)
		}
	}

	recordings, err := d.ensureRecordings(ctx, session)
	if err != nil {
		return fmt.Errorf("get recordings: %w", err)
	}
	if len(recordings) == 0 {
		return fmt.Errorf("session %s has no recording", sessionID)
	}
	candidates := make([]audio.Recording, len(recordings))
	for i, recording := range recordings {
		words, err := d.transcriber.Words(ctx, recording)
		if err != nil {
			return fmt.Errorf("transcribe recording %s: %w", recording, err)
		}
		candidates[i] = audio.Recording{Words: words, Duration: wordDuration(words)}
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

	span := storage.DeletedSpan{SessionID: sessionID, AudioPath: recording, Start: start, End: end, SHA256Before: beforeHash, SHA256After: afterHash}
	if err := d.store.DeleteSegment(sessionID, segmentID, span); err != nil {
		return err
	}
	if err := os.Rename(tempPath, recording); err != nil {
		return fmt.Errorf("replace recording %s: %w", recording, err)
	}
	keepTemp = true
	if err := d.store.MarkSpliceComplete(&span); err != nil {
		return err
	}
	if d.refine != nil {
		if err := d.refine(ctx, sessionID, recording, session.StartedAt); err != nil {
			return fmt.Errorf("refine spliced session %s: %w", sessionID, err)
		}
	}
	return nil
}

// recoverPendingSplices completes recordings whose deletion transaction
// committed before its replacement file reached disk.
func recoverPendingSplices(ctx context.Context, store *storage.SQLiteStore, audioDir string, source deletionRecordingSource, splice func(context.Context, string, float64, float64) (string, error), refine func(context.Context, string, string, time.Time) error) (int, error) {
	spans, err := store.PendingSplices()
	if err != nil {
		return 0, err
	}
	for _, span := range spans {
		session, err := store.GetSession(span.SessionID)
		if err != nil {
			return 0, fmt.Errorf("get session %s for pending splice: %w", span.SessionID, err)
		}
		recordings, err := restoreDeletionRecordings(ctx, &session, audioDir, source)
		if err != nil {
			return 0, err
		}
		recording := ""
		for _, candidate := range recordings {
			if filepath.Base(candidate) == filepath.Base(span.AudioPath) {
				recording = candidate
				break
			}
		}
		if recording == "" {
			return 0, fmt.Errorf("pending splice recording %s is unavailable", span.AudioPath)
		}
		afterHash, err := splice(ctx, recording, span.Start, span.End)
		if err != nil {
			return 0, fmt.Errorf("re-splice %s: %w", recording, err)
		}
		if afterHash != span.SHA256After {
			return 0, fmt.Errorf("re-splice %s produced SHA-256 %s, want %s", recording, afterHash, span.SHA256After)
		}
		if err := store.MarkSpliceComplete(&span); err != nil {
			return 0, err
		}
		if refine != nil {
			if err := refine(ctx, span.SessionID, recording, session.StartedAt); err != nil {
				return 0, fmt.Errorf("refine re-spliced session %s: %w", span.SessionID, err)
			}
		}
	}
	return len(spans), nil
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

func missingNeighbourTokens(words []audio.Word, splice float64, neighbour string, before bool) []string {
	expected := deletionTokens(neighbour)
	if len(expected) == 0 {
		return nil
	}
	nearby := make(map[string]struct{})
	for _, word := range words {
		if word.End < splice-10 || word.Start > splice+10 {
			continue
		}
		if before && word.End > splice {
			continue
		}
		if !before && word.Start < splice {
			continue
		}
		for _, token := range deletionTokens(word.Text) {
			nearby[token] = struct{}{}
		}
	}
	missing := make([]string, 0, len(expected))
	seen := make(map[string]struct{})
	for _, token := range expected {
		if _, already := seen[token]; already {
			continue
		}
		seen[token] = struct{}{}
		if _, found := nearby[token]; !found {
			missing = append(missing, token)
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

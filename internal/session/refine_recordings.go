package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// batchRefinementResult is the one atomic unit of a completed batch
// refinement: transcript text, every retained word occurrence, and the
// segment table derived from those occurrences.
type batchRefinementResult struct {
	Transcript string
	Model      string
	Words      []storage.RefinedWord
	Segments   []storage.RefinedSegment
}

// transcribeRecordings transcribes each recording on its own, preserving the
// recording revision and ordinal of every batch word. Text-deletion spans are
// applied before segments and transcripts are built, so a later refinement
// cannot reintroduce deleted words.
func (m *Manager) transcribeRecordings(ctx context.Context, t transcribe.BatchTranscriber, audioPath string) (batchRefinementResult, error) {
	var result batchRefinementResult
	recordingOrdinal := 0
	for _, recording := range strings.Split(audioPath, ",") {
		recording = strings.TrimSpace(recording)
		if recording == "" {
			continue
		}
		spans, err := m.store.TextDeletionSpans(recording)
		if err != nil {
			return batchRefinementResult{}, err
		}
		if err := spansDescribe(recording, spans); err != nil {
			return batchRefinementResult{}, err
		}
		batch, err := t.Transcribe(ctx, recording)
		if err != nil {
			return batchRefinementResult{}, err
		}
		if len(batch.Words) == 0 && strings.TrimSpace(batch.Transcript) != "" {
			return batchRefinementResult{}, fmt.Errorf("refine %s: the transcription gave no word times", recording)
		}
		recordingHash, err := recordingSHA256(recording)
		if err != nil {
			return batchRefinementResult{}, fmt.Errorf("hash recording %s: %w", recording, err)
		}
		switch {
		case result.Model == "":
			result.Model = batch.Model
		case batch.Model != "" && batch.Model != result.Model:
			return batchRefinementResult{}, fmt.Errorf("refine %s: its transcription reports model %q, another recording of the session %q", recording, batch.Model, result.Model)
		}

		var kept []storage.RefinedWord
		for ordinal, word := range batch.Words {
			if storage.TextDeleted(spans, word.Start, word.End) {
				continue
			}
			kept = append(kept, storage.RefinedWord{
				RecordingPath: recording, RecordingSHA256: recordingHash, RecordingOrdinal: recordingOrdinal,
				Ordinal: ordinal, Text: word.Text, Speaker: word.Speaker, Start: word.Start, End: word.End,
			})
		}
		result.Words = append(result.Words, kept...)
		recordingOrdinal++
	}
	result.Transcript = storage.RefinedTranscript(result.Words)
	result.Segments = storage.BuildRefinedSegments(result.Words)
	return result, nil
}

// spansDescribe returns an error unless recording's bytes are the ones each
// of its text deletion spans was recorded in (storage.TextDeletionSpan.SHA256).
func spansDescribe(recording string, spans []storage.TextDeletionSpan) error {
	if len(spans) == 0 {
		return nil
	}
	hash, err := recordingSHA256(recording)
	if err != nil {
		return fmt.Errorf("hash %s to check its deleted text spans: %w", recording, err)
	}
	for _, span := range spans {
		if span.SHA256 != hash {
			return fmt.Errorf("recording %s changed since text was deleted from it (SHA-256 %s, the deletion's %s): refining it could bring the deleted words back", recording, hash, span.SHA256)
		}
	}
	return nil
}

func recordingSHA256(path string) (string, error) {
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

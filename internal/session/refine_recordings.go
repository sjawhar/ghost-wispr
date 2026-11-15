package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

// ErrRefinementModel is why a refinement refused to publish: it cannot
// transcribe with the model the recordings' "Delete text" spans were recorded
// under, so another model's word times could bring deleted words back.
var ErrRefinementModel = errors.New("refinement refused")

// transcribeRecordings transcribes each recording on its own, preserving the
// recording revision and ordinal of every batch word. Text-deletion spans are
// applied before segments and transcripts are built, so a later refinement
// cannot reintroduce deleted words. Spans hold their model's word times, so a
// recording with spans is transcribed with that model
// (transcribe.ModelSelector), each recording with its own: a merge can bring
// together recordings whose text was deleted under two models. The result is
// refused (ErrRefinementModel) when a recording's model cannot be used: its
// spans name no model or two, the transcription with it fails, as for a model
// Deepgram no longer serves, or the transcription reports another model. Each
// refined word keeps the model that timed it (storage.RefinedWord.Model), so a
// later "Delete text" records its words' own model; the session's model is
// that of its recordings without spans.
func (m *Manager) transcribeRecordings(ctx context.Context, t transcribe.BatchTranscriber, audioPath string) (batchRefinementResult, error) {
	type planned struct {
		recording string
		spans     []storage.TextDeletionSpan
		model     string // the spans' model, "" without spans
	}
	var plan []planned
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
		p := planned{recording: recording, spans: spans}
		for _, span := range spans {
			switch {
			case span.Model == "":
				return batchRefinementResult{}, fmt.Errorf("%w: the model text was deleted under in %s is unknown, so another model could bring the deleted words back", ErrRefinementModel, filepath.Base(recording))
			case p.model == "":
				p.model = span.Model
			case span.Model != p.model:
				return batchRefinementResult{}, fmt.Errorf("%w: text was deleted in %s under two models, %q and %q", ErrRefinementModel, filepath.Base(recording), p.model, span.Model)
			}
		}
		plan = append(plan, p)
	}
	var result batchRefinementResult
	unspannedModel, spannedModel := "", ""
	recordingOrdinal, transcribedWords := 0, 0
	for _, p := range plan {
		recording, spans := p.recording, p.spans
		transcriber := t
		if p.model != "" {
			if selector, ok := t.(transcribe.ModelSelector); ok {
				transcriber = selector.WithModel(p.model)
			}
		}
		batch, err := transcriber.Transcribe(ctx, recording)
		if err != nil {
			if p.model != "" {
				return batchRefinementResult{}, fmt.Errorf("%w: transcribing %s with model %q, which text was deleted under, failed: %v", ErrRefinementModel, filepath.Base(recording), p.model, err)
			}
			return batchRefinementResult{}, err
		}
		if p.model != "" && batch.Model != p.model {
			return batchRefinementResult{}, fmt.Errorf("%w: text was deleted under model %q, and the transcription reports model %q, whose word times could bring the deleted words back", ErrRefinementModel, p.model, batch.Model)
		}
		transcribedWords += len(batch.Words)
		recordingHash, err := recordingSHA256(recording)
		if err != nil {
			return batchRefinementResult{}, fmt.Errorf("hash recording %s: %w", recording, err)
		}
		switch {
		case p.model != "":
			if spannedModel == "" {
				spannedModel = batch.Model
			}
		case unspannedModel == "":
			unspannedModel = batch.Model
		case batch.Model != "" && batch.Model != unspannedModel:
			return batchRefinementResult{}, fmt.Errorf("refine %s: its transcription reports model %q, another recording of the session %q", recording, batch.Model, unspannedModel)
		}

		var kept []storage.RefinedWord
		for ordinal, word := range batch.Words {
			if storage.TextDeleted(spans, word.Start, word.End) {
				continue
			}
			kept = append(kept, storage.RefinedWord{
				RecordingPath: recording, RecordingSHA256: recordingHash, RecordingOrdinal: recordingOrdinal,
				Ordinal: ordinal, Text: word.Text, Speaker: word.Speaker, Start: word.Start, End: word.End,
				Model: batch.Model,
			})
		}
		result.Words = append(result.Words, kept...)
		recordingOrdinal++
	}
	if transcribedWords == 0 {
		return batchRefinementResult{}, fmt.Errorf("batch refinement returned no timed words")
	}
	result.Model = unspannedModel
	if result.Model == "" {
		result.Model = spannedModel
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

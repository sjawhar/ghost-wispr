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

// transcribeRecordings is a session's batch refinement: each recording
// audioPath names (a merged session's names several, in session order,
// separated by commas) transcribed alone, with the words of every span
// "Delete text" recorded in it dropped (withoutDeletedText), and the
// transcripts joined, one paragraph each. The recordings must report one
// model. A recording whose bytes are not the ones its spans were recorded in
// is refused: the spans' times may no longer be where the deleted words are,
// and a refinement must not bring them back.
func (m *Manager) transcribeRecordings(ctx context.Context, t transcribe.BatchTranscriber, audioPath string) (transcribe.BatchResult, error) {
	var transcripts []string
	var model string
	for _, recording := range strings.Split(audioPath, ",") {
		recording = strings.TrimSpace(recording)
		if recording == "" {
			continue
		}
		spans, err := m.store.TextDeletionSpans(recording)
		if err != nil {
			return transcribe.BatchResult{}, err
		}
		if err := spansDescribe(recording, spans); err != nil {
			return transcribe.BatchResult{}, err
		}
		result, err := t.Transcribe(ctx, recording)
		if err != nil {
			return transcribe.BatchResult{}, err
		}
		transcript := result.Transcript
		if len(spans) > 0 {
			if transcript, err = withoutDeletedText(&result, spans); err != nil {
				return transcribe.BatchResult{}, fmt.Errorf("refine %s: %w", recording, err)
			}
		}
		switch {
		case model == "":
			model = result.Model
		case result.Model != "" && result.Model != model:
			return transcribe.BatchResult{}, fmt.Errorf("refine %s: its transcription reports model %q, another recording of the session %q", recording, result.Model, model)
		}
		if transcript = strings.TrimSpace(transcript); transcript != "" {
			transcripts = append(transcripts, transcript)
		}
	}
	return transcribe.BatchResult{Transcript: strings.Join(transcripts, "\n\n"), Model: model}, nil
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

// withoutDeletedText rebuilds result's transcript from its words, leaving out
// every word whose middle is in one of spans: the words of segments "Delete
// text" removed, which the recording still holds. A transcription with text
// and no word times cannot say which words those are, and is refused.
func withoutDeletedText(result *transcribe.BatchResult, spans []storage.TextDeletionSpan) (string, error) {
	if len(result.Words) == 0 {
		if strings.TrimSpace(result.Transcript) != "" {
			return "", fmt.Errorf("the transcription gave no word times, so the words of deleted text cannot be left out")
		}
		return "", nil
	}
	kept := make([]string, 0, len(result.Words))
	for _, word := range result.Words {
		if !storage.TextDeleted(spans, word.Start, word.End) {
			kept = append(kept, word.Text)
		}
	}
	return strings.Join(kept, " "), nil
}

package session

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// crossModelWords is what another model hears in deletedTextSession's
// recording: the same words, with the deleted segment's times later than the
// model the "Delete text" span was recorded under gave them, as nova-2 heard
// 37 to 47 of about 140 segments over nova-3 spans (#65 review, B3).
func crossModelWords(model string) transcribe.BatchResult {
	return refinementResult(model, timedWords(0, "So we met."), timedWords(1.6, "Orange tractor ninety."), timedWords(2.7, "And then left."))
}

// modelTranscriber hears a recording as each model's entry says, and can be
// asked for another model, as the Deepgram batch transcriber can.
type modelTranscriber struct {
	model   string
	results map[string]transcribe.BatchResult
	mu      *sync.Mutex
	asked   *[]string
}

func newModelTranscriber(model string, results map[string]transcribe.BatchResult) *modelTranscriber {
	return &modelTranscriber{model: model, results: results, mu: &sync.Mutex{}, asked: &[]string{}}
}

func (m *modelTranscriber) Transcribe(context.Context, string) (transcribe.BatchResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	*m.asked = append(*m.asked, m.model)
	return m.results[m.model], nil
}

func (m *modelTranscriber) WithModel(model string) transcribe.BatchTranscriber {
	return &modelTranscriber{model: model, results: m.results, mu: m.mu, asked: m.asked}
}

// fixedModelTranscriber is a transcriber that cannot be asked for a model.
type fixedModelTranscriber struct{ result transcribe.BatchResult }

func (f fixedModelTranscriber) Transcribe(context.Context, string) (transcribe.BatchResult, error) {
	return f.result, nil
}

func TestCrossModelControlLeaksWithoutTheModelRule(t *testing.T) {
	// Control: the other model's times put deleted words outside the span, so
	// applying the span alone, as a refinement did, keeps them.
	span := []storage.TextDeletionSpan{{Start: 1.0, End: 1.95}}
	var kept []string
	for _, word := range crossModelWords("nova-2").Words {
		if !storage.TextDeleted(span, word.Start, word.End) {
			kept = append(kept, word.Text)
		}
	}
	if !strings.Contains(strings.Join(kept, " "), "tractor") {
		t.Fatalf("control: the cross-model fake keeps no deleted word (%q); it cannot show the leak", kept)
	}
}

// TestRefinementUsesTheModelOfDeletedText fails if a refinement after a
// change of the configured batch model transcribes with the new model and
// publishes a word "Delete text" removed.
func TestRefinementUsesTheModelOfDeletedText(t *testing.T) {
	store, recording, started := deletedTextSession(t, "20261112100000", "audio")
	transcriber := newModelTranscriber("nova-2", map[string]transcribe.BatchResult{
		"nova-3": deletedTextWords(),
		"nova-2": crossModelWords("nova-2"),
	})
	manager := NewManager(store, nil, nil, nil, NewDetector(time.Hour), 0)
	manager.SetBatchTranscriber(transcriber)
	if err := manager.RefineSession(context.Background(), "20261112100000", recording, started); err != nil {
		t.Fatalf("RefineSession: %v", err)
	}
	sess, err := store.GetSession("20261112100000")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sess.CanonicalTranscript, "tractor") || strings.Contains(sess.RefinedTranscript, "tractor") {
		t.Fatalf("a refinement after a model change published a deleted word: canonical %q, refined %q", sess.CanonicalTranscript, sess.RefinedTranscript)
	}
	if got := strings.Join(*transcriber.asked, ","); got != "nova-3" {
		t.Fatalf("models transcribed = %q, want only nova-3, the model the deletion was recorded under", got)
	}
	if sess.RefinementStatus != storage.RefinementCompleted || sess.RefinedTranscript != "So we met. And then left." || sess.RefinementModel != "nova-3" {
		t.Errorf("refinement = %q %q (model %q), want completed \"So we met. And then left.\" from nova-3", sess.RefinementStatus, sess.RefinedTranscript, sess.RefinementModel)
	}
}

// TestRefinementRefusesAModelItCannotUse fails if a refinement whose
// transcriber cannot use the model a "Delete text" was recorded under
// publishes its transcription, or refuses without a warning that names the
// session and the reason.
func TestRefinementRefusesAModelItCannotUse(t *testing.T) {
	store, recording, started := deletedTextSession(t, "20261112100000", "audio")
	manager := NewManager(store, nil, nil, nil, NewDetector(time.Hour), 0)
	manager.SetBatchTranscriber(fixedModelTranscriber{result: crossModelWords("nova-2")})
	var warnings []string
	manager.SetRefinementWarningHook(func(sessionID, warning string) {
		warnings = append(warnings, sessionID+": "+warning)
	})
	_ = manager.RefineSession(context.Background(), "20261112100000", recording, started)
	sess, err := store.GetSession("20261112100000")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sess.CanonicalTranscript, "tractor") || strings.Contains(sess.RefinedTranscript, "tractor") {
		t.Fatalf("a refinement with another model published a deleted word: canonical %q, refined %q", sess.CanonicalTranscript, sess.RefinedTranscript)
	}
	if sess.RefinementStatus == storage.RefinementCompleted {
		t.Fatalf("refinement with another model completed, want it refused")
	}
	if len(warnings) != 1 || !strings.HasPrefix(warnings[0], "20261112100000: ") || !strings.Contains(warnings[0], "nova-3") || !strings.Contains(warnings[0], "nova-2") {
		t.Fatalf("refusal warnings = %q, want one naming the session and both models", warnings)
	}
}

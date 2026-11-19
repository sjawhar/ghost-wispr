package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	api "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/listen/v1/websocket/interfaces"
)

// resumeFilter is a session manager that keeps, of each result, only the words
// at or after resumedAt on the stream, as session.Manager.MessageKept does
// after a pause: a late result from before the pause is dropped whole.
type resumeFilter struct {
	resumedAt float64
	messages  int
}

func (f *resumeFilter) MessageKept(mr *api.MessageResponse) (*api.MessageResponse, error) {
	f.messages++
	alt := mr.Channel.Alternatives[0]
	var kept []api.Word
	for _, w := range alt.Words {
		if (w.Start+w.End)/2 >= f.resumedAt {
			kept = append(kept, w)
		}
	}
	switch len(kept) {
	case 0:
		return nil, nil
	case len(alt.Words):
		return mr, nil
	}
	trimmed := *mr
	words := make([]string, len(kept))
	for i, w := range kept {
		words[i] = w.PunctuatedWord
	}
	alt.Words, alt.Transcript = kept, strings.Join(words, " ")
	trimmed.Channel.Alternatives = []api.Alternative{alt}
	return &trimmed, nil
}

func (f *resumeFilter) Message(mr *api.MessageResponse) error {
	_, err := f.MessageKept(mr)
	return err
}
func (*resumeFilter) UtteranceEnd(*api.UtteranceEndResponse) error { return nil }
func (*resumeFilter) ForceEndSession(context.Context) error         { return nil }

func deepgramResult(t *testing.T, raw string) *api.MessageResponse {
	t.Helper()
	var mr api.MessageResponse
	if err := json.Unmarshal([]byte(raw), &mr); err != nil {
		t.Fatal(err)
	}
	return &mr
}

// TestTheVoiceReadsOnlyWhatTheTranscriptKeeps fails if the voice reads a
// result, or the words of one, that the session manager's transcript drops as
// coming from before the latest pause: a late result from before a pause must
// never wake the voice after the resume.
func TestTheVoiceReadsOnlyWhatTheTranscriptKeeps(t *testing.T) {
	manager := &resumeFilter{resumedAt: 10}
	heard := &heardTranscripts{}
	cb := transcriptCallback{manager: manager, voice: heard, stamp: func() (int, bool) { return 1, true }}

	late := deepgramResult(t, `{"is_final":true,"speech_final":true,"channel":{"alternatives":[{"transcript":"Wispr, what is happening?","words":[
		{"word":"wispr","punctuated_word":"Wispr,","start":7.0,"end":7.3},
		{"word":"what","punctuated_word":"what","start":7.4,"end":7.6},
		{"word":"is","punctuated_word":"is","start":7.6,"end":7.7},
		{"word":"happening","punctuated_word":"happening?","start":7.7,"end":8.1}]}]}}`)
	if err := cb.Message(late); err != nil {
		t.Fatal(err)
	}
	straddling := deepgramResult(t, `{"is_final":true,"channel":{"alternatives":[{"transcript":"Wispr, stop. Wispr, hello","words":[
		{"word":"wispr","punctuated_word":"Wispr,","start":9.0,"end":9.3},
		{"word":"stop","punctuated_word":"stop.","start":9.4,"end":9.6},
		{"word":"wispr","punctuated_word":"Wispr,","start":10.2,"end":10.5},
		{"word":"hello","punctuated_word":"hello","start":10.6,"end":10.9}]}]}}`)
	if err := cb.Message(straddling); err != nil {
		t.Fatal(err)
	}

	heard.mu.Lock()
	defer heard.mu.Unlock()
	if manager.messages != 2 {
		t.Fatalf("the manager read %d results, want 2", manager.messages)
	}
	if len(heard.got) != 1 {
		t.Fatalf("the voice read %d results, want 1: the late result from before the pause must not reach it", len(heard.got))
	}
	got := heard.got[0]
	if got.Text != "Wispr, hello" || len(got.Words) != 2 || got.Words[0].Start != 10.2 {
		t.Fatalf("the voice read %q with %d words, want only the kept \"Wispr, hello\"", got.Text, len(got.Words))
	}
}

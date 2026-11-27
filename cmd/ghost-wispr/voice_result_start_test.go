package main

import (
	"testing"
	"time"

	api "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/listen/v1/websocket/interfaces"

	"github.com/sjawhar/ghost-wispr/internal/monoclock"
)

// TestTheVoiceTranscriptCarriesTheResultsStart: the result's own start
// (Deepgram's start), timed by capture, reaches the voice as Transcript.From,
// apart from its first word's, which can move between an interim and its
// final while the result's start does not (Sol's twenty-eighth review of #191,
// after https://developers.deepgram.com/docs/interim-results).
func TestTheVoiceTranscriptCarriesTheResultsStart(t *testing.T) {
	t.Parallel()
	base := time.Now().Add(-time.Minute)
	heardAt := func(offset time.Duration) (time.Time, bool) { return base.Add(offset), true }
	var mr api.MessageResponse
	mr.Start = 5
	mr.Channel.Alternatives = []api.Alternative{{
		Transcript: "Whisper. No. Wait,",
		Words:      []api.Word{{Word: "whisper", PunctuatedWord: "Whisper.", Start: 5.00141402, End: 5.2}},
	}}
	tr := voiceTranscript(&mr, monoclock.Now(), nil, heardAt)
	if want := base.Add(5 * time.Second); !tr.From.Equal(want) {
		t.Fatalf("From = %v, want the result's start %v", tr.From.Sub(base), want.Sub(base))
	}
	if want := base.Add(5001414020 * time.Nanosecond); !tr.Words[0].At.Equal(want) {
		t.Fatalf("the first word's At = %v, want %v", tr.Words[0].At.Sub(base), want.Sub(base))
	}
	if untimed := voiceTranscript(&mr, monoclock.Now(), nil, nil); !untimed.From.IsZero() {
		t.Fatalf("with no capture clock, From = %v, want zero", untimed.From)
	}
}

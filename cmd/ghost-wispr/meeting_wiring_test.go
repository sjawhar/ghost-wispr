package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// The tests here hold the unit's wiring to the recorder's meeting context
// (session.Manager.MeetingContext): the roster and the voice each end what
// they do for a meeting the moment the recorder ends it, with no
// session-ended hook.

// recorderMeeting is the recorder's meeting context of session "meeting",
// and an ended one for any other.
func recorderMeeting(t *testing.T) (meeting func(string) context.Context, end context.CancelFunc) {
	ctx, end := context.WithCancel(context.Background())
	t.Cleanup(end)
	ended, endNow := context.WithCancel(context.Background())
	endNow()
	return func(id string) context.Context {
		if id == "meeting" {
			return ctx
		}
		return ended
	}, end
}

// TestTheRosterReadsTheRecordersMeeting fails if startJoinedAgents does not
// give the roster the recorder's meeting context, so a membership outlives
// the recorder's end of its meeting until a hook runs.
func TestTheRosterReadsTheRecordersMeeting(t *testing.T) {
	meeting, end := recorderMeeting(t)
	roster := startJoinedAgents(t.Context(), &joinedDeps{
		session:   func() string { return "meeting" },
		meeting:   meeting,
		store:     conciergeStore(t),
		floor:     &voiceLink{},
		notify:    func(string, string) {},
		publicURL: "https://recorder.example",
	})
	t.Cleanup(roster.Close)
	roster.Open().SetAgents(&recordingAgents{})
	if _, err := roster.Deliver(t.Context(), &envoy.Message{EventID: "e1", SourceSession: "agent-1", Text: "ghost-wispr join"}); err != nil {
		t.Fatal(err)
	}
	if len(roster.Joined()) != 1 {
		t.Fatal("control: the join was refused")
	}
	end()
	if got := roster.Joined(); len(got) != 0 {
		t.Fatalf("a membership outlived the recorder's end of its meeting: %+v", got)
	}
}

// openingVoice is a voice whose Open hands the test its exchange's context
// and holds until that context ends.
type openingVoice struct{ opened chan context.Context }

func (v openingVoice) Open(ctx context.Context, _ *voice.Exchange) (voice.Session, error) {
	v.opened <- ctx
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestTheVoiceReadsTheRecordersMeeting fails if startVoice does not give the
// Supervisor the recorder's meeting context, so an exchange outlives the
// recorder's end of its meeting.
func TestTheVoiceReadsTheRecordersMeeting(t *testing.T) {
	meeting, end := recorderMeeting(t)
	output := audio.NewOutput(audio.OutputConfig{Open: func(string) (audio.PlaybackStream, int, error) { return nil, 0, context.Canceled }})
	t.Cleanup(func() { _ = output.Close() })
	opened := make(chan context.Context, 1)
	var link voiceLink
	if err := startVoice(&voiceDeps{
		output: output, tap: voice.NewTap(), concierge: stubConcierge{answers: make(chan voice.PendingAnswer)}, link: &link,
		voices:  func() (voice.Voice, error) { return openingVoice{opened: opened}, nil },
		enabled: func() bool { return true }, wakePhrases: []string{"Wispr"},
		session: func() string { return "meeting" }, meeting: meeting,
		red: func() bool { return true }, notify: func(string, string) {},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(link.Close)
	text := "Wispr, what is the status of the release?"
	tr := voice.Transcript{Text: text, IsFinal: true, SpeechFinal: true, GateOpen: true, At: time.Now()}
	for i, w := range strings.Fields(text) {
		tr.Words = append(tr.Words, voice.Word{Text: w, Start: 0.2 * float64(i), End: 0.2*float64(i) + 0.15})
	}
	link.OnTranscript(&tr)
	var exchange context.Context
	select {
	case exchange = <-opened:
	case <-time.After(2 * time.Second):
		t.Fatal("control: the wake opened no exchange")
	}
	select {
	case <-exchange.Done():
		t.Fatal("control: the exchange ended while its meeting records on")
	case <-time.After(200 * time.Millisecond):
	}
	end()
	select {
	case <-exchange.Done():
	case <-time.After(time.Second):
		t.Fatal("the exchange outlived the recorder's end of its meeting")
	}
}

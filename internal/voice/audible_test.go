package voice

import (
	"sync"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/monoclock"
)

// hearingSpeaker is the fake speaker with the audible events audio.Output
// gives (OnAudible), which the test sends (hear).
type hearingSpeaker struct {
	*fakeSpeaker
	mu   sync.Mutex
	fns  map[int]func(audio.AudibleEvent)
	next int
}

func (s *hearingSpeaker) OnAudible(fn func(audio.AudibleEvent)) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fns == nil {
		s.fns = map[int]func(audio.AudibleEvent){}
	}
	id := s.next
	s.next++
	s.fns[id] = fn
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.fns, id)
	}
}

func (s *hearingSpeaker) hear(e audio.AudibleEvent) {
	s.mu.Lock()
	fns := make([]func(audio.AudibleEvent), 0, len(s.fns))
	for _, fn := range s.fns {
		fns = append(fns, fn)
	}
	s.mu.Unlock()
	for _, fn := range fns {
		fn(e)
	}
}

// TestTheSpeakersAudibleTimesReachTheTimingLog fails if an audible event of
// the speaker, the chime's or the voice's, does not reach the timing log as an
// "audible" line with its input, its time, lead and error on CLOCK_MONOTONIC,
// and its disruptions, which the acceptance driver reads to time the answer
// against the chime; or if the Supervisor still hears the speaker once it is
// closed.
func TestTheSpeakersAudibleTimesReachTheTimingLog(t *testing.T) {
	var mu sync.Mutex
	var lines [][]any
	var speaker *hearingSpeaker
	r := newRigWith(t, newFakeConcierge("ok"), func(cfg *SupervisorConfig, r *rig) {
		speaker = &hearingSpeaker{fakeSpeaker: r.speaker}
		cfg.Speaker = speaker
		cfg.Timing = func(event, sessionID string, at monoclock.Stamp, kv ...any) {
			if event == "audible" {
				mu.Lock()
				lines = append(lines, kv)
				mu.Unlock()
			}
			r.timing(event, sessionID, at, kv...)
		}
	})
	sent := []audio.AudibleEvent{
		{Input: audio.InputChime, At: 1_000_000_123, Lead: 0, Err: 1001, Disruptions: 2},
		{Input: audio.InputVoice, At: 1_900_000_456, Lead: 666_667, Err: 1500, Disruptions: 2},
	}
	for _, e := range sent {
		speaker.hear(e)
	}
	mu.Lock()
	got := append([][]any(nil), lines...)
	mu.Unlock()
	if len(got) != len(sent) {
		t.Fatalf("%d audible lines, want %d: %v", len(got), len(sent), got)
	}
	for i, e := range sent {
		want := []any{"input", e.Input, "audible_mono_ns", e.At, "audible_lead_ns", e.Lead, "audible_err_ns", e.Err, "disruptions", e.Disruptions}
		if len(got[i]) != len(want) {
			t.Fatalf("audible line %d: %v, want %v", i, got[i], want)
		}
		for j := range want {
			if got[i][j] != want[j] {
				t.Fatalf("audible line %d: %v, want %v", i, got[i], want)
			}
		}
	}

	r.s.Close()
	speaker.hear(sent[0])
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != len(sent) {
		t.Fatalf("the closed Supervisor logged %d more audible lines", len(lines)-len(sent))
	}
}

// TestAnAudibleLineNamesItsExchangesSession fails if an audible line heard
// while an exchange waits for its recording session names no session, or a
// later one: like the exchange's other timing lines (#130), it waits for the
// session that started after the exchange opened, and keeps its own time. The
// control, a sound of no exchange, names the session now.
func TestAnAudibleLineNamesItsExchangesSession(t *testing.T) {
	var speaker *hearingSpeaker
	r := newRigWith(t, newFakeConcierge("The voice issue is in review."), func(cfg *SupervisorConfig, r *rig) {
		speaker = &hearingSpeaker{fakeSpeaker: r.speaker}
		cfg.Speaker = speaker
	})
	audible := func() []timingLine {
		var out []timingLine
		for _, l := range r.timingLines() {
			if l.event == "audible" {
				out = append(out, l)
			}
		}
		return out
	}
	r.setSession("sess-0")
	speaker.hear(audio.AudibleEvent{Input: audio.InputChime, At: 1_000, Err: 1001})
	if got := audible(); len(got) != 1 || got[0].sessionID != "sess-0" {
		t.Fatalf("control: a sound of no exchange gave %+v, want one audible line naming sess-0", got)
	}
	r.setSession("")
	r.heard("Wispr, what is happening?", true, false)
	eventually(t, "the exchange to open", func() bool {
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		return r.s.ex != nil
	})
	before := monoclock.Now()
	speaker.hear(audio.AudibleEvent{Input: audio.InputChime, At: 2_000, Err: 1001})
	if got := audible(); len(got) != 1 {
		t.Fatalf("before its session the exchange's audible line was written: %+v", got[1:])
	}
	r.startSession("sess-1")
	got := audible()
	if len(got) != 2 || got[1].sessionID != "sess-1" {
		t.Fatalf("as its session started the exchange wrote %+v, want its audible line naming sess-1", got)
	}
	if got[1].at.Mono < before.Mono {
		t.Fatalf("the held audible line is stamped %d, before the event was heard at %d", got[1].at.Mono, before.Mono)
	}
}

package concierge

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/voice"
	"github.com/sjawhar/ghost-wispr/internal/voice/tts"
)

// markedEngine is a text-to-speech engine for voice A: each text's audio is
// its length long, every sample the text's marker, and comes in 200 ms
// chunks 100 ms apart after a first-chunk latency, as a streaming engine's
// does.
type markedEngine struct {
	latency time.Duration
	length  func(text string) time.Duration
	marker  func(text string) int16
}

func (markedEngine) Name() string { return "marked" }

func (markedEngine) Format() audio.AudioFormat {
	return audio.AudioFormat{SampleRate: 24000, Channels: 1, Encoding: audio.EncodingPCM16}
}

func (e markedEngine) Stream(ctx context.Context, text string, emit func([]byte) error) error {
	wait := func(d time.Duration) error {
		select {
		case <-time.After(d):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := wait(e.latency); err != nil {
		return err
	}
	marker := uint16(e.marker(text))
	for left := e.length(text); left > 0; left -= 200 * time.Millisecond {
		pcm := make([]byte, int(min(left, 200*time.Millisecond)/time.Millisecond)*48)
		for i := 0; i < len(pcm); i += 2 {
			pcm[i], pcm[i+1] = byte(marker), byte(marker>>8)
		}
		if err := emit(pcm); err != nil {
			return err
		}
		if err := wait(100 * time.Millisecond); err != nil {
			return err
		}
	}
	return nil
}

// markerStream is a 24 kHz speaker stream that plays each 10 ms frame in
// about real time and counts the samples of each value it played.
type markerStream struct {
	mu     sync.Mutex
	counts map[int16]int
}

func (s *markerStream) Write(p []int16) error {
	s.mu.Lock()
	for _, v := range p {
		s.counts[v]++
	}
	s.mu.Unlock()
	time.Sleep(10 * time.Millisecond)
	return nil
}

func (*markerStream) Drop() (int, error) { return 0, nil }
func (*markerStream) Close() error       { return nil }

func (s *markerStream) played(marker int16) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[marker]
}

// TestTheLastLineOfAnAnswerPlaysToItsEndOnVoiceA (INFRA-4623, the office Pi
// on 2026-11-24 at 03:29–03:31Z): asked about a search with no one clear
// match, the real Concierge offers its top two issues ("Did you mean …?")
// through the real Supervisor and the real voice A, whose Say returns once a
// text's first frame is queued and whose text plays on under the context Say
// was given. It fails if any sample of the routing line or of the question
// does not reach the speaker, or if the exchange ends before the person can
// pick an issue with no wake phrase. On the Pi the room heard "Reading the
// record." and then nothing: Answer's return cancelled the context the
// question was still playing under, and the exchange ended.
func TestTheLastLineOfAnAnswerPlaysToItsEndOnVoiceA(t *testing.T) {
	const (
		routingMarker  int16 = 0x1111
		questionMarker int16 = 0x2222
		routingLength        = 600 * time.Millisecond
		questionLength       = 1200 * time.Millisecond
	)
	for _, tc := range []struct {
		name string
		// searchBound bounds the full search, which never answers; past
		// fullWait the keyword result is taken at the mark.
		searchBound time.Duration
		routing     bool
	}{
		// Exchange 1: the full search had not answered by its mark, after
		// the routing line.
		{name: "after the routing line", searchBound: 5 * time.Second, routing: true},
		// Exchange 2: the full search failed at its bound before the
		// routing mark, so the question is all the voice says.
		{name: "alone", searchBound: 300 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := &lockedBuffer{}
			t.Cleanup(func() {
				if t.Failed() {
					t.Logf("logs:\n%s", logs.String())
				}
			})
			store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "unit.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			if err := store.CreateSession("meeting-1", time.Now().Add(-20*time.Minute)); err != nil {
				t.Fatal(err)
			}
			records := &fakeRecords{
				issues: map[string]*dispatch.Issue{}, events: map[string][]dispatch.Event{}, docs: map[string]string{},
				projects:    []string{"ABC"},
				searchDelay: time.Minute,
				kwHits: []dispatch.SearchHit{
					{Kind: "issue", IssueKey: "ABC-41", Title: "parser eval contracts", Status: "done", Score: 0.0104},
					{Kind: "issue", IssueKey: "ABC-42", Title: "parser evals first answers", Status: "in_progress", Score: 0.0102},
				},
			}
			c, err := New(&Config{
				Model:     "test-model",
				Generator: &scriptedModel{},
				Records:   records,
				Store:     store,
				PublicURL: "https://unit.example",
				Now:       time.Now,
				Logger:    slog.New(slog.NewTextHandler(logs, nil)),
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(c.Stop)
			c.routingAfter, c.fullWait, c.searchBound, c.searchSettle = time.Second, 1300*time.Millisecond, tc.searchBound, 50*time.Millisecond
			eventually(t, "the project list was read", func() bool {
				c.mu.Lock()
				defer c.mu.Unlock()
				return c.projects != nil
			})
			engine := markedEngine{
				latency: 150 * time.Millisecond,
				length: func(text string) time.Duration {
					if text == "Reading the record." {
						return routingLength
					}
					return questionLength
				},
				marker: func(text string) int16 {
					if text == "Reading the record." {
						return routingMarker
					}
					return questionMarker
				},
			}
			stream := &markerStream{counts: map[int16]int{}}
			out := audio.NewOutput(audio.OutputConfig{Open: func(string) (audio.PlaybackStream, int, error) { return stream, 24000, nil }})
			t.Cleanup(func() { _ = out.Close() })
			var endedMu sync.Mutex
			var ended []string
			s := voice.NewSupervisor(&voice.SupervisorConfig{
				Voice:        func() (voice.Voice, error) { return tts.New(engine), nil },
				Concierge:    c,
				Speaker:      voice.SpeakerOf(out),
				WakePhrases:  []string{"Wispr", "Ghost Wispr"},
				Session:      func() string { return "meeting-1" },
				Red:          func() bool { return true },
				FollowUpWait: 300 * time.Millisecond,
				Logf: func(format string, args ...any) {
					line := fmt.Sprintf(format, args...)
					_, _ = fmt.Fprintln(logs, "supervisor:", line)
					if reason, ok := strings.CutPrefix(line, "voice: exchange ended "); ok {
						endedMu.Lock()
						ended = append(ended, reason)
						endedMu.Unlock()
					}
				},
			})
			t.Cleanup(s.Close)

			text := "Wispr, what's happening with the parser evals?"
			tr := voice.Transcript{Text: text, IsFinal: true, SpeechFinal: true, GateOpen: true, At: time.Now()}
			for i, w := range strings.Fields(text) {
				ws := 0.2 * float64(i)
				tr.Words = append(tr.Words, voice.Word{Text: w, Start: ws, End: ws + 0.15})
			}
			s.OnTranscript(&tr)

			wantRouting, wantQuestion := 0, int(questionLength/time.Millisecond)*24
			if tc.routing {
				wantRouting = int(routingLength/time.Millisecond) * 24
			}
			firstEnd := func() string {
				endedMu.Lock()
				defer endedMu.Unlock()
				if len(ended) == 0 {
					return ""
				}
				return ended[0]
			}
			deadline := time.Now().Add(8 * time.Second)
			for firstEnd() == "" && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			time.Sleep(300 * time.Millisecond) // what the speaker still held when the exchange ended
			if got := stream.played(routingMarker); got != wantRouting {
				t.Errorf("the routing line played %d of its samples, want %d", got, wantRouting)
			}
			if got := stream.played(questionMarker); got != wantQuestion {
				t.Errorf("the question which issue played %d of its %d samples: the room heard it cut", got, wantQuestion)
			}
			if got := firstEnd(); got != "(no follow-up)" {
				t.Errorf("the exchange ended %q, want (no follow-up): the person's pick must be heard with no wake phrase", got)
			}
		})
	}
}

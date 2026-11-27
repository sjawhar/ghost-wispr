package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/call"
)

// rateMixed is mixed audio of n samples received at at on the call's bot's
// connection.
func rateMixed(h *botHarness, n int, at time.Time) *audioIn {
	return &audioIn{Mixed: true, PCM: make([]int16, n), At: at, Nonce: h.b.bot.nonce}
}

// rateFeed gives the bridge 10 s and one frame of mixed audio at rate
// samples a second, in 20 ms frames timed on the bridge's clock.
func rateFeed(h *botHarness, rate int) {
	start := h.clk.Now()
	for i := 0; i <= 500; i++ {
		h.b.onAudio(h.ctx, rateMixed(h, rate/50, start.Add(time.Duration(i)*20*time.Millisecond)))
	}
}

func rateLines(h *botHarness, t *testing.T) []string {
	var out []string
	for _, m := range h.logs.messages(t) {
		if strings.HasPrefix(m, "RATE") {
			out = append(out, m)
		}
	}
	return out
}

// rateFeedTotal gives the bridge one window of mixed audio of total samples
// at the clock's now, then the next window's first frame, which closes it.
func rateFeedTotal(h *botHarness, total int) {
	start := h.clk.Now()
	h.b.onAudio(h.ctx, rateMixed(h, total, start))
	h.b.onAudio(h.ctx, rateMixed(h, 480, start.Add(rateWindow)))
}

func TestTheRateCountReportsAFault(t *testing.T) {
	t.Run("22,050 a second", func(t *testing.T) {
		h, _ := joinOneBot(t)
		h.inCall()
		rateFeed(h, 22050)
		want := []string{"RATE samples_per_second=22050", "RATE-FAULT samples_per_second=22050"}
		if got := rateLines(h, t); !slices.Equal(got, want) {
			t.Fatalf("rate lines = %q, want %q", got, want)
		}
		faults := h.link.sentOf(call.TypeRateFault)
		if len(faults) != 1 || faults[0].RateFault.SamplesPerSecond != 22050 {
			t.Fatalf("rate_fault controls = %+v, want one of 22050", faults)
		}
	})
	t.Run("24,000 a second", func(t *testing.T) {
		h, _ := joinOneBot(t)
		h.inCall()
		rateFeed(h, 24000)
		if got, want := rateLines(h, t), []string{"RATE samples_per_second=24000"}; !slices.Equal(got, want) {
			t.Fatalf("rate lines = %q, want %q", got, want)
		}
		if faults := h.link.sentOf(call.TypeRateFault); len(faults) != 0 {
			t.Fatalf("rate_fault controls at 24,000 = %+v, want none", faults)
		}
	})
	// R10's 1 %: 23,760 and 24,240 are in, one sample a second beyond each
	// is a fault.
	for _, tc := range []struct {
		rate  int
		fault bool
	}{{23759, true}, {23760, false}, {24240, false}, {24241, true}} {
		t.Run(fmt.Sprintf("%d a second", tc.rate), func(t *testing.T) {
			h, _ := joinOneBot(t)
			h.inCall()
			rateFeedTotal(h, 10*tc.rate)
			want := []string{fmt.Sprintf("RATE samples_per_second=%d", tc.rate)}
			if tc.fault {
				want = append(want, fmt.Sprintf("RATE-FAULT samples_per_second=%d", tc.rate))
			}
			if got := rateLines(h, t); !slices.Equal(got, want) {
				t.Fatalf("rate lines = %q, want %q", got, want)
			}
			if faults := h.link.sentOf(call.TypeRateFault); (len(faults) == 1) != tc.fault || len(faults) > 1 {
				t.Fatalf("rate_fault controls = %+v, want a fault %v", faults, tc.fault)
			}
		})
	}
}

// The rate count closes each 10 s window on the bridge's clock: a window
// with no mixed audio reports 0 and a fault, and audio after a gap counts in
// the window it came in, never over a longer span taken as 10 s.
func TestTheRateCountClosesWindowsOnTheClock(t *testing.T) {
	h, _ := joinOneBot(t)
	h.inCall()
	t0 := h.clk.Now()
	feed := func(from time.Time, frames int) {
		for i := range frames {
			h.b.onAudio(h.ctx, rateMixed(h, 480, from.Add(time.Duration(i)*20*time.Millisecond)))
		}
	}
	feed(t0, 500) // 10 s at 24,000 a second, then nothing for 25 s
	h.advanceTo(t0.Add(35 * time.Second))
	feed(t0.Add(35*time.Second), 250) // 5 s of the window from 30 s
	h.advanceTo(t0.Add(42 * time.Second))
	want := []string{
		"RATE samples_per_second=24000",
		"RATE samples_per_second=0", "RATE-FAULT samples_per_second=0",
		"RATE samples_per_second=0", "RATE-FAULT samples_per_second=0",
		"RATE samples_per_second=12000", "RATE-FAULT samples_per_second=12000",
	}
	if got := rateLines(h, t); !slices.Equal(got, want) {
		t.Fatalf("rate lines = %q, want %q", got, want)
	}
	var sent []int
	for _, c := range h.link.sentOf(call.TypeRateFault) {
		sent = append(sent, c.RateFault.SamplesPerSecond)
	}
	if want := []int{0, 0, 12000}; !slices.Equal(sent, want) {
		t.Fatalf("rate_fault controls = %v, want %v", sent, want)
	}
}

// One call at a time (PC14): the rate count takes the call's bot's audio
// alone, and audio that another bot's connection carried is not the call's.
func TestAudioOfAnotherBotIsNotCounted(t *testing.T) {
	h, _ := joinOneBot(t)
	h.inCall()
	start := h.clk.Now()
	for i := 0; i <= 500; i++ {
		h.b.onAudio(h.ctx, &audioIn{Mixed: true, PCM: make([]int16, 480), At: start.Add(time.Duration(i) * 20 * time.Millisecond), Nonce: "another-bots-nonce"})
	}
	if got := rateLines(h, t); len(got) != 0 {
		t.Fatalf("rate lines from another bot's audio = %q, want none", got)
	}
	rateFeed(h, 24000)
	if got, want := rateLines(h, t), []string{"RATE samples_per_second=24000"}; !slices.Equal(got, want) {
		t.Fatalf("rate lines from the call's bot's audio = %q, want %q", got, want)
	}
}

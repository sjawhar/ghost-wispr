// Sol's twenty-fourth review of #191 at 12c293e
// (https://github.com/sjawhar/ghost-wispr/pull/191#pullrequestreview-6084899163):
// Sol's probes, kept as regression tests: the cache invalidation rows, with
// only what the linter asked changed (the room helpers take a pointer, and the
// session is read before the return); every check is Sol's.

package voice

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// This is a pure, deliberately small test double, not concierge.TakeBack.
// Exact fixtures select a provisional cut. Adding the closing quotation
// changes the input and restores the complete text. All other inputs survive.
// Thus these tests exercise Supervisor invalidation, not the real grammar.
func solR24CacheRule(finals string) (string, bool) {
	switch finals {
	case "cancel", "quote \"alpha\ncancel":
		return "", true
	case "cancel retry draft", "quote \"alpha\ncancel retry draft":
		return "retry draft", true
	case "cancel retry final", "quote \"alpha\ncancel retry final":
		return "retry final", true
	default:
		return finals, false
	}
}

type solR24CacheProbe struct {
	mu     sync.Mutex
	inputs []string
}

func (p *solR24CacheProbe) take(finals string) (string, bool) {
	p.mu.Lock()
	p.inputs = append(p.inputs, finals)
	p.mu.Unlock()
	return solR24CacheRule(finals)
}

func (p *solR24CacheProbe) total() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.inputs)
}

func (p *solR24CacheProbe) count(finals string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, input := range p.inputs {
		if input == finals {
			n++
		}
	}
	return n
}

func solR24OpenCache(t *testing.T, opening string, endpoint bool, change func(*SupervisorConfig)) (*rig, *fakeSession, *solR24CacheProbe) {
	t.Helper()
	p := new(solR24CacheProbe)
	r := newRigWith(t, newFakeConcierge("fact"), func(cfg *SupervisorConfig, r *rig) {
		cfg.Voice = func() (Voice, error) { return drivingVoice{r.voice}, nil }
		cfg.TakeBack = p.take
		cfg.TurnWait, cfg.IdleWait, cfg.FollowUpWait = time.Hour, time.Hour, time.Hour
		if change != nil {
			change(cfg)
		}
	})
	r.heard("Wispr, "+opening, true, endpoint)
	sess := r.waitSession()
	return r, sess, p
}

func solR24CheckRoom(t *testing.T, room *RoomView, asker, request string, recent []string) {
	t.Helper()
	if !room.Live || room.Asker != asker || room.Request != request {
		t.Fatalf("room live=%v asker=%q request=%q; want live, asker=%q request=%q", room.Live, room.Asker, room.Request, asker, request)
	}
	var got []string
	for _, line := range room.Recent {
		got = append(got, line.Text)
	}
	if !slices.Equal(got, recent) {
		t.Fatalf("room recent=%q; want %q", got, recent)
	}
}

// Each call checks output before inspecting instrumentation. The second read
// must preserve that output without parsing an unchanged full or turn input.
func solR24StableRoom(t *testing.T, sess *fakeSession, p *solR24CacheProbe, asker, request string, recent []string) RoomView {
	t.Helper()
	room := sess.x.Room()
	solR24CheckRoom(t, &room, asker, request, recent)
	before := p.total()
	again := sess.x.Room()
	solR24CheckRoom(t, &again, asker, request, recent)
	if got := p.total() - before; got != 0 {
		t.Fatalf("unchanged Room read performed %d parser calls", got)
	}
	return room
}

func solR24WantChanged(t *testing.T, room *RoomView) {
	t.Helper()
	select {
	case <-room.Changed:
	default:
		t.Fatal("the previous room view was not notified of its change")
	}
}

func TestSolR24CacheInstrumentControls(t *testing.T) {
	for _, tc := range []struct {
		input, rest string
		taken       bool
	}{
		{"plain request", "plain request", false},
		{"quote \"alpha\ncancel", "", true},
		{"quote \"alpha\ncancel retry final", "retry final", true},
		{"quote \"alpha\ncancel retry final\nomega\".", "quote \"alpha\ncancel retry final\nomega\".", false},
	} {
		if rest, taken := solR24CacheRule(tc.input); rest != tc.rest || taken != tc.taken {
			t.Fatalf("control for %q: (%q,%v), want (%q,%v)", tc.input, rest, taken, tc.rest, tc.taken)
		}
	}
}

func TestSolR24CacheFinalAndInterimReplacement(t *testing.T) {
	r, sess, p := solR24OpenCache(t, "quote \"alpha", false, nil)
	initial := solR24StableRoom(t, sess, p, "quote \"alpha", "quote \"alpha", []string{"Wispr, quote \"alpha"})
	for _, tc := range []struct {
		name, text, asker, request string
		final                      bool
		parses                     int
		recent                     []string
	}{
		{"interim takes everything back", "cancel", "", "quote \"alpha", false, 1, []string{"Wispr, quote \"alpha"}},
		{"replacement withdraws interim cancellation", "please continue", "quote \"alpha please continue", "quote \"alpha", false, 1, []string{"Wispr, quote \"alpha"}},
		{"different interim suffix", "cancel retry draft", "retry draft", "quote \"alpha", false, 1, []string{"Wispr, quote \"alpha"}},
		{"final replaces interim", "cancel retry final", "retry final", "retry final", true, 2, []string{"retry final"}},
		{"interim closes quote only for asker", "omega\".", "quote \"alpha cancel retry final omega\".", "retry final", false, 1, []string{"retry final"}},
		// The turn's input is identical to the preceding interim. Only the
		// full-owned parse needs to run when this becomes a final.
		{"same words become final and restore ownership", "omega\".", "quote \"alpha cancel retry final omega\".", "quote \"alpha cancel retry final omega\".", true, 1, []string{"Wispr, quote \"alpha", "cancel retry final", "omega\"."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := p.total()
			r.heard(tc.text, tc.final, false)
			solR24StableRoom(t, sess, p, tc.asker, tc.request, tc.recent)
			if got := p.total() - before; got != tc.parses {
				t.Fatalf("changed input parsed %d times; want %d", got, tc.parses)
			}
			if !tc.final {
				before = p.total()
				r.heard(tc.text, false, false)
				solR24StableRoom(t, sess, p, tc.asker, tc.request, tc.recent)
				if got := p.total() - before; got != 0 {
					t.Fatalf("duplicate interim performed %d parser calls", got)
				}
			}
		})
	}
	// A retained RoomView is a snapshot, not an alias into the store that
	// keepRecentLocked compacts and historyTextsLocked cuts.
	solR24CheckRoom(t, &initial, "quote \"alpha", "quote \"alpha", []string{"Wispr, quote \"alpha"})
}

func TestSolR24CacheClaimAnsweredAndLateQuote(t *testing.T) {
	for _, mode := range []string{"unsent", "claim", "answered"} {
		t.Run(mode, func(t *testing.T) {
			r, sess, p := solR24OpenCache(t, "quote \"alpha", true, nil)
			first := solR24StableRoom(t, sess, p, "quote \"alpha", "quote \"alpha", []string{"Wispr, quote \"alpha"})
			before := p.total()
			switch mode {
			case "claim":
				if !sess.x.Claim(first.Turn, first.Revision) {
					t.Fatal("control: the initial request was not claimable")
				}
			case "answered":
				sess.x.Answered(time.Now())
			}
			request := "quote \"alpha"
			if mode != "unsent" {
				request = ""
				solR24WantChanged(t, &first)
			}
			solR24StableRoom(t, sess, p, "quote \"alpha", request, []string{"Wispr, quote \"alpha"})
			if got := p.total() - before; got != 0 {
				t.Fatalf("changing only sent ownership reparsed %d inputs", got)
			}

			r.heard("cancel retry final", true, true)
			recent := []string{"retry final"}
			if mode != "unsent" {
				recent = []string{"Wispr, quote \"alpha", "retry final"}
			}
			cut := solR24StableRoom(t, sess, p, "retry final", "retry final", recent)
			if cut.Turn == first.Turn {
				t.Fatal("control: the endpoint did not move the current turn")
			}
			before = p.total()
			switch mode {
			case "claim":
				if !sess.x.Claim(cut.Turn, cut.Revision) {
					t.Fatal("the surviving suffix was not claimable")
				}
			case "answered":
				sess.x.Answered(time.Now())
				recent = []string{"Wispr, quote \"alpha", "cancel retry final"}
			}
			if mode != "unsent" {
				solR24StableRoom(t, sess, p, "retry final", "", recent)
				solR24WantChanged(t, &cut)
			}
			if got := p.total() - before; got != 0 {
				t.Fatalf("retiring the cut suffix reparsed %d inputs", got)
			}

			r.heard("omega\".", true, false)
			want := "quote \"alpha cancel retry final omega\"."
			switch mode {
			case "claim":
				// The opening was already sent. The cancelled prefix of the
				// second final was not; its suffix was claimed separately.
				want = "cancel omega\"."
			case "answered":
				// Answered retires all of each final, not just the visible
				// suffix, so the closing final alone remains pending.
				want = "omega\"."
			}
			solR24StableRoom(t, sess, p, "omega\".", want, []string{"Wispr, quote \"alpha", "cancel retry final", "omega\"."})
			if got := p.count("quote \"alpha\ncancel retry final\nomega\"."); got != 1 {
				t.Fatalf("closing quote reparsed full owned input %d times; want 1", got)
			}
		})
	}
}

func TestSolR24CacheLateFinalBornAnswered(t *testing.T) {
	r, sess, p := solR24OpenCache(t, "original request", false, nil)
	solR24StableRoom(t, sess, p, "original request", "original request", []string{"Wispr, original request"})
	answered := time.Now()
	sess.x.Answered(answered)
	solR24StableRoom(t, sess, p, "original request", "", []string{"Wispr, original request"})

	r.mu.Lock()
	start, closings := r.start, r.closings
	r.start += 5
	r.mu.Unlock()
	late := transcriptOf("late old words", start, true, false, closings)
	for i := range late.Words {
		late.Words[i].At = answered.Add(-time.Second).Add(time.Duration(i) * 100 * time.Millisecond)
	}
	r.s.OnTranscript(&late)
	solR24StableRoom(t, sess, p, "original request late old words", "", []string{"Wispr, original request", "late old words"})
	r.heard("new pending words", true, false)
	solR24StableRoom(t, sess, p, "original request late old words new pending words", "new pending words", []string{"Wispr, original request", "late old words", "new pending words"})
}

func TestSolR24CacheRecentExpiryAndDeletion(t *testing.T) {
	r, sess, p := solR24OpenCache(t, "old retained request", false, nil)
	r.heard("fresh retained request", true, false)
	beforeAge := solR24StableRoom(t, sess, p, "old retained request fresh retained request", "old retained request fresh retained request", []string{"Wispr, old retained request", "fresh retained request"})
	before := p.total()
	r.s.mu.Lock()
	if len(r.s.recent) != 2 {
		r.s.mu.Unlock()
		t.Fatal("control: did not retain both final segments")
	}
	r.s.recent[0].At = time.Now().Add(-recentKept - time.Second)
	r.s.mu.Unlock()
	solR24StableRoom(t, sess, p, "old retained request fresh retained request", "old retained request fresh retained request", []string{"fresh retained request"})
	if got := p.total() - before; got != 0 {
		t.Fatalf("history age change reparsed %d unchanged inputs", got)
	}
	// Unlike waiting on a shortened global recentKept, this uses the actual
	// two-minute limit and includes a fresh segment as a positive control.
	solR24CheckRoom(t, &beforeAge, "old retained request fresh retained request", "old retained request fresh retained request", []string{"Wispr, old retained request", "fresh retained request"})
	r.s.SegmentsDeleted("sess-1")
	solR24StableRoom(t, sess, p, "old retained request fresh retained request", "old retained request fresh retained request", nil)
	if got := p.total() - before; got != 0 {
		t.Fatalf("deleting recent text reparsed %d unchanged inputs", got)
	}
}

func TestSolR24CacheProjectKeysAndNameMode(t *testing.T) {
	var keysMu sync.Mutex
	keys := []string{"OPS"}
	r, sess, p := solR24OpenCache(t, "status of infra forty five ninety two?", false, func(cfg *SupervisorConfig) {
		cfg.ProjectKeys = func() []string {
			keysMu.Lock()
			defer keysMu.Unlock()
			return slices.Clone(keys)
		}
	})
	raw := "status of infra forty five ninety two?"
	solR24StableRoom(t, sess, p, raw, raw, []string{"Wispr, " + raw})
	before := p.total()
	for _, key := range []string{"INFRA", "OPS", "INFRA"} {
		keysMu.Lock()
		keys[0] = key // same backing slice, changed contents
		keysMu.Unlock()
		want := raw
		if key == "INFRA" {
			want = "status of INFRA-4592?"
		}
		solR24StableRoom(t, sess, p, want, want, []string{"Wispr, " + raw})
	}
	if got := p.total() - before; got != 0 {
		t.Fatalf("project-key changes reparsed %d unchanged take-back inputs", got)
	}

	// Isolate byName with identical raw text and project keys. Both memo
	// fields must invalidate even when no transcript or ownership changes.
	r.s.mu.Lock()
	ex := r.s.ex
	for _, memo := range []*keysMemo{&ex.askerKeys, &ex.requestKeys} {
		for _, byName := range []bool{false, true, false} {
			ex.turnByName = byName
			got := r.s.keysOfLocked(ex, memo, "in for 10/27?")
			want := "in for 10/27?"
			if byName {
				want = "INFRA-1027?"
			}
			if got != want {
				r.s.mu.Unlock()
				t.Fatalf("identical raw text, byName=%v: got %q, want %q", byName, got, want)
			}
		}
	}
	r.s.mu.Unlock()
}

func TestSolR24CacheEndAndFreshExchange(t *testing.T) {
	for _, end := range []string{"stop", "gate"} {
		t.Run(end, func(t *testing.T) {
			r, old, p := solR24OpenCache(t, "quote \"alpha", false, nil)
			r.heard("cancel retry final", true, false)
			cut := solR24StableRoom(t, old, p, "retry final", "retry final", []string{"retry final"})
			before := p.total()
			if end == "gate" {
				r.closeGate()
			} else {
				r.s.Stop("cache review boundary")
			}
			solR24WantChanged(t, &cut)
			dead := old.x.Room()
			if dead.Live || dead.Asker != "" || dead.Request != "" || len(dead.Recent) != 0 || dead.Changed != nil {
				t.Fatalf("ended exchange exposed cached room: %+v", dead)
			}
			if old.x.Claim(cut.Turn, cut.Revision) {
				t.Fatal("ended exchange accepted its old claim")
			}
			old.x.Answered(time.Now())
			if got := p.total() - before; got != 0 {
				t.Fatalf("ending and reading warm exchange reparsed %d inputs", got)
			}
			r.heard("Wispr, fresh request", true, false)
			fresh := r.waitSession()
			wantRecent := []string{"retry final", "Wispr, fresh request"}
			if end == "gate" {
				wantRecent = []string{"Wispr, fresh request"}
			}
			solR24StableRoom(t, fresh, p, "fresh request", "fresh request", wantRecent)
			// An old callback must not retire a new exchange's identically
			// positioned first owned final or reuse its cache object.
			old.x.Answered(time.Now())
			solR24StableRoom(t, fresh, p, "fresh request", "fresh request", wantRecent)
		})
	}
}

func TestSolR24CacheOneFullParseForEachNewFinal(t *testing.T) {
	r, sess, p := solR24OpenCache(t, "prefix 0", true, nil)
	finals := []string{"prefix 0"}
	recent := []string{"Wispr, prefix 0"}
	solR24StableRoom(t, sess, p, "prefix 0", "prefix 0", recent)
	for i := 1; i <= 12; i++ {
		text := fmt.Sprintf("prefix %d", i)
		finals = append(finals, text)
		recent = append(recent, text)
		before := p.total()
		r.heard(text, true, true)
		solR24StableRoom(t, sess, p, text, strings.Join(finals, " "), recent)
		full := strings.Join(finals, "\n")
		if got := p.count(full); got != 1 {
			t.Fatalf("whole owned prefix with %d finals parsed %d times, want once", len(finals), got)
		}
		if got := p.total() - before; got != 2 {
			t.Fatalf("one new final parsed %d inputs, want current turn and full prefix", got)
		}
	}
}

func TestSolR24CacheNameModeAcrossCallsAndEndpoints(t *testing.T) {
	r, sess, p := solR24OpenCache(t, "in for 10/27?", true, func(cfg *SupervisorConfig) {
		cfg.ProjectKeys = func() []string { return []string{"INFRA"} }
	})
	first := solR24StableRoom(t, sess, p, "INFRA-1027?", "INFRA-1027?", []string{"Wispr, in for 10/27?"})
	if !sess.x.Claim(first.Turn, first.Revision) {
		t.Fatal("control: the named request was not claimable")
	}
	r.heard("in for 10/27?", true, true)
	second := solR24StableRoom(t, sess, p, "in for 10/27?", "in for 10/27?", []string{"Wispr, in for 10/27?", "in for 10/27?"})
	if second.Turn == first.Turn {
		t.Fatal("control: the endpoint did not start an unnamed turn")
	}
	if !sess.x.Claim(second.Turn, second.Revision) {
		t.Fatal("the unnamed request was not claimable")
	}
	r.heard("Wispr, in for 10/27?", true, false)
	third := solR24StableRoom(t, sess, p, "INFRA-1027?", "INFRA-1027?", []string{"Wispr, in for 10/27?", "in for 10/27?", "Wispr, in for 10/27?"})
	if third.Turn == second.Turn {
		t.Fatal("control: a later wake did not start another named turn")
	}
}

func TestSolR24CacheNewInterimChangesPendingBackchannel(t *testing.T) {
	r, sess, p := solR24OpenCache(t, "okay", true, nil)
	first := solR24StableRoom(t, sess, p, "okay", "okay", []string{"Wispr, okay"})
	before := p.total()
	r.heard("next question", false, false)
	second := solR24StableRoom(t, sess, p, "next question", "", []string{"Wispr, okay"})
	if second.Turn == first.Turn {
		t.Fatal("control: the new interim did not advance the turn")
	}
	if got := p.total() - before; got != 1 {
		t.Fatalf("turn advance with no new owned final parsed %d inputs; want only the new turn", got)
	}
	before = p.total()
	r.heard("next question", true, false)
	solR24StableRoom(t, sess, p, "next question", "next question", []string{"Wispr, okay", "next question"})
	if got := p.total() - before; got != 1 {
		t.Fatalf("same interim becoming final parsed %d inputs; want only full owned input", got)
	}
}

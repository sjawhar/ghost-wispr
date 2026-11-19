package gptlive

import (
	"sync"
	"time"
)

// CutMargin is how far outside a cut speech, on the session timeline, a
// transcript fragment may fall and still be taken for that speech's words.
// It covers the offset between the session's own clock and GPT-Live's
// timeline, which is unmeasured until gptlive-probe runs against the real
// API; the probe prints the margin it measures.
const CutMargin = 300 * time.Millisecond

// timeline places the model's speech on the Live session's timeline, so that
// its transcript can be matched to the speech it belongs to. GPT-Live's
// transcript fragments carry no response or item id, only their place on that
// timeline (start_ms, milliseconds from the session's start), and they can
// arrive well after their audio: a fragment is matched by that place, never
// by when it arrives. The session's clock starts at session.started; the
// input audio it streams in real time keeps GPT-Live's timeline in step with
// it.
type timeline struct {
	mu       sync.Mutex
	started  time.Time
	speeches []spoken
	// offsets are how far each fragment of uncut speech fell outside its
	// speech, for the probe to size CutMargin.
	offsets []time.Duration
}

// spoken is one speech on the session's clock, in milliseconds since
// session.started: from its first audible audio to the later of its last
// audio and its start plus its length.
type spoken struct {
	start, end int64
	loud       int64 // its last audible audio
	samples    int
	cut        bool
}

// begin starts the session's clock.
func (t *timeline) begin(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.started = now
}

func (t *timeline) msLocked(now time.Time) int64 { return now.Sub(t.started).Milliseconds() }

// currentLocked is the speech that audio at ms continues, as the player
// decides it: one whose audible audio was less than speechGap ago.
func (t *timeline) currentLocked(ms int64) *spoken {
	if n := len(t.speeches); n > 0 && ms-t.speeches[n-1].loud < speechGap.Milliseconds() {
		return &t.speeches[n-1]
	}
	return nil
}

// audio places the model's audio that arrived at now.
func (t *timeline) audio(now time.Time, pcm []int16) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ms := t.msLocked(now)
	loud := audible(pcm)
	sp := t.currentLocked(ms)
	if sp == nil {
		if !loud {
			return
		}
		t.speeches = append(t.speeches, spoken{start: ms, end: ms, loud: ms})
		sp = &t.speeches[len(t.speeches)-1]
	}
	if loud {
		sp.loud = ms
	}
	sp.samples += len(pcm)
	sp.end = max(sp.end, ms, sp.start+int64(sp.samples)*1000/SampleRate)
}

// cut marks the speech under way at now as cut: Interrupt dropped it.
func (t *timeline) cut(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.started.IsZero() || len(t.speeches) == 0 {
		return
	}
	ms := t.msLocked(now)
	if sp := &t.speeches[len(t.speeches)-1]; ms < sp.end+speechGap.Milliseconds() {
		sp.cut = true
	}
}

// cutWords reports whether a transcript fragment at startMs on the session
// timeline is the words of a cut speech. The fragment belongs to the latest
// speech it falls within CutMargin of; it is cut words when that speech is
// cut. The latest wins because a server that sends audio faster than it
// plays can make a cut speech's span, its start plus its length, run past
// the start of the next, and a word of real speech dropped from the
// own-speech feed costs more than a word of cut speech kept. A fragment of
// uncut speech adds how far it fell outside its speech to the offsets.
func (t *timeline) cutWords(startMs int64, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := len(t.speeches) - 1; i >= 0; i-- {
		if abs(outside(startMs, &t.speeches[i])) <= CutMargin.Milliseconds() {
			if t.speeches[i].cut {
				return true
			}
			break
		}
	}
	// For the probe's measure, a fragment belongs to the latest speech that
	// had begun when it arrived.
	ms := t.msLocked(now)
	for i := len(t.speeches) - 1; i >= 0; i-- {
		if sp := &t.speeches[i]; sp.start <= ms {
			if !sp.cut {
				t.offsets = append(t.offsets, time.Duration(outside(startMs, sp))*time.Millisecond)
			}
			break
		}
	}
	return false
}

// measured is a copy of the offsets so far.
func (t *timeline) measured() []time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]time.Duration(nil), t.offsets...)
}

// outside is how far ms falls outside sp: negative before it, positive after
// it, 0 within it.
func outside(ms int64, sp *spoken) int64 {
	switch {
	case ms < sp.start:
		return ms - sp.start
	case ms > sp.end:
		return ms - sp.end
	}
	return 0
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

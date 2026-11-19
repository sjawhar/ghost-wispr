package gptlive

import (
	"sync"
	"time"
)

// CutMargin is how far outside a speech, on the session timeline, a
// transcript fragment may fall and still be taken for that speech's words.
// It covers the offset between the session's clock and GPT-Live's timeline,
// which is unmeasured until gptlive-probe runs against the real API; the
// probe prints the offset it measures and fails when this margin does not
// cover it.
const CutMargin = 300 * time.Millisecond

// wordHold is how long a transcript fragment that no playing speech covers
// waits for one before it is dropped: words of which no audio played are
// not Wispr's speech.
const wordHold = 3 * time.Second

// settleTick is how often held fragments are looked at again while some
// wait.
const settleTick = 250 * time.Millisecond

// timeline places the model's speech on the Live session's timeline and
// holds its transcript until the audio of it has played. GPT-Live's
// transcript fragments carry no response or item id, only their place on
// that timeline (start_ms), and they can arrive before their audio, or well
// after it; a fragment is matched by that place, never by when it arrives.
// The words of a speech reach the own-speech feed only once the audio at
// their place has played, so the words of a speech Interrupt cut, or that
// never played at all, never do.
//
// The session's clock counts the input audio it has sent, at SampleRate,
// plus the time since it last sent some: GPT-Live's timeline is taken to
// count the input audio it receives, so the wake replay the Supervisor sends
// at once when a session opens moves both clocks alike.
type timeline struct {
	mu       sync.Mutex
	started  bool
	inputed  int64     // input samples sent
	lastSent time.Time // when input was last sent, or the session started
	speeches []spoken
	pending  []fragment
	// unmatched counts fragments that no speech covered, for the probe.
	unmatched int
	// release reports a fragment whose audio played.
	release func(text string)
	timer   *time.Timer
	closed  bool
}

// spoken is one speech on the session's clock, in milliseconds: from its
// first audible audio to the later of its last audio and its start plus its
// length.
type spoken struct {
	start, end int64
	samples    int // audio of it that arrived
	played     int // audio of it that played
	cut        bool
	cutAt      time.Time // when Interrupt cut it
	done       bool      // it played to its end
	first      int64
	hasFirst   bool // first is its earliest released fragment's place
}

// fragment is one transcript fragment of the model's speech, held until the
// audio at its place has played.
type fragment struct {
	text    string
	at      int64 // start_ms
	arrived time.Time
}

// speechAudio says what one push of the model's audio was to the player.
type speechAudio int

const (
	audioOutside  speechAudio = iota // in no speech: silence between speeches
	audioNew                         // the start of a speech
	audioContinue                    // more of the speech under way
)

// begin starts the session's clock at session.started.
func (t *timeline) begin(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.started, t.lastSent = true, now
}

// sent counts n samples of input audio sent at now.
func (t *timeline) sent(n int, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.inputed += int64(n)
	t.lastSent = now
}

// msLocked is the session's clock at now.
func (t *timeline) msLocked(now time.Time) int64 {
	if !t.started {
		return 0
	}
	return t.inputed*1000/SampleRate + now.Sub(t.lastSent).Milliseconds()
}

// audio places the model's audio that arrived at now, as the player took
// it.
func (t *timeline) audio(now time.Time, pcm []int16, kind speechAudio) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ms := t.msLocked(now)
	switch {
	case kind == audioNew:
		t.speeches = append(t.speeches, spoken{start: ms, end: ms})
	case kind == audioOutside || len(t.speeches) == 0:
		return
	}
	sp := &t.speeches[len(t.speeches)-1]
	sp.samples += len(pcm)
	sp.end = max(sp.end, ms, sp.start+int64(sp.samples)*1000/SampleRate)
}

// cut marks the speech under way at now as cut: Interrupt dropped the rest
// of it. A speech that played to its end more than speechGap ago is not
// under way.
func (t *timeline) cut(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.speeches) == 0 {
		return
	}
	if sp := &t.speeches[len(t.speeches)-1]; t.msLocked(now) < sp.end+speechGap.Milliseconds() {
		sp.cut, sp.cutAt = true, now
	}
	t.settleLocked(now)
}

// played counts audio of speech seq that played.
func (t *timeline) played(seq, n int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if seq < len(t.speeches) {
		t.speeches[seq].played += n
	}
	t.settleLocked(time.Now())
}

// finished marks speeches as having played to their end.
func (t *timeline) finished(seqs []int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, seq := range seqs {
		if seq < len(t.speeches) && !t.speeches[seq].cut {
			t.speeches[seq].done = true
		}
	}
	t.settleLocked(time.Now())
}

// said holds a transcript fragment at startMs until its audio has played.
func (t *timeline) said(text string, startMs int64, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.pending = append(t.pending, fragment{text: text, at: startMs, arrived: now})
	t.settleLocked(now)
}

// close drops every held fragment: the session is closing.
func (t *timeline) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed, t.pending = true, nil
	if t.timer != nil {
		t.timer.Stop()
	}
}

// settleLocked releases each held fragment whose audio has played, drops
// each that no speech will play, and keeps the rest.
func (t *timeline) settleLocked(now time.Time) {
	if t.closed {
		return
	}
	kept := t.pending[:0]
	for _, f := range t.pending {
		switch t.placeLocked(&f, now) {
		case fragmentPlayed:
			if t.release != nil {
				t.release(f.text)
			}
		case fragmentWaits:
			kept = append(kept, f)
		}
	}
	clear(t.pending[len(kept):])
	t.pending = kept
	if len(t.pending) > 0 && t.timer == nil {
		t.timer = time.AfterFunc(settleTick, t.tick)
	}
}

func (t *timeline) tick() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.timer = nil
	t.settleLocked(time.Now())
}

type fragmentFate int

const (
	fragmentWaits fragmentFate = iota
	fragmentPlayed
	fragmentDropped
)

// placeLocked decides f: played once the speech it belongs to has played to
// its place; waiting while that speech still plays, or for wordHold for a
// speech to come; then dropped. It belongs to a speech that covers its place
// within CutMargin. A fragment that arrived before a covering speech was cut
// is that speech's: the server sent it with that speech, before any later
// one began. Otherwise, of the speeches that cover it, the latest not cut
// comes first: a server that sends audio faster than it plays can make a cut
// speech's span run past the start of the next.
func (t *timeline) placeLocked(f *fragment, now time.Time) fragmentFate {
	covers := func(sp *spoken) bool { return abs(outside(f.at, sp)) <= CutMargin.Milliseconds() }
	for i := range t.speeches {
		if sp := &t.speeches[i]; sp.cut && f.arrived.Before(sp.cutAt) && covers(sp) {
			if t.playedToLocked(sp, f) {
				return fragmentPlayed
			}
			return fragmentDropped
		}
	}
	covered := false
	for i := len(t.speeches) - 1; i >= 0; i-- {
		sp := &t.speeches[i]
		if !covers(sp) {
			continue
		}
		covered = true
		if sp.cut {
			continue
		}
		if t.playedToLocked(sp, f) {
			return fragmentPlayed
		}
		return fragmentWaits
	}
	if now.Sub(f.arrived) < wordHold {
		return fragmentWaits
	}
	if !covered {
		t.unmatched++
	}
	return fragmentDropped
}

// playedToLocked reports whether sp has played to f's place, and if so
// takes f for sp's words.
func (t *timeline) playedToLocked(sp *spoken, f *fragment) bool {
	place := min(max(f.at, sp.start), sp.end)
	if !sp.done && (sp.played == 0 || sp.start+int64(sp.played)*1000/SampleRate < place) {
		return false
	}
	if !sp.hasFirst || f.at < sp.first {
		sp.first, sp.hasFirst = f.at, true
	}
	return true
}

// offsets are, for each speech whose words played, how far its earliest
// words' place falls from its audio's start on the session's clock.
func (t *timeline) offsets() []time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []time.Duration
	for i := range t.speeches {
		if sp := &t.speeches[i]; sp.hasFirst {
			out = append(out, time.Duration(sp.first-sp.start)*time.Millisecond)
		}
	}
	return out
}

// unmatchedWords is how many fragments no speech covered.
func (t *timeline) unmatchedWords() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.unmatched
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

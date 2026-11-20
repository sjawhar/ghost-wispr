package gptlive

import (
	"strings"

	"github.com/sjawhar/ghost-wispr/internal/earcheck"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// factAudit compares a written fact with what GPT-Live actually said. It
// keeps text only in memory and emits a count, never words, when the speech
// ends.
type factAudit struct {
	source string
	actual strings.Builder
}

// beginFactAudit starts measuring a fact or an answer. Empty sources need no
// audit. The shared R8 checker is the sole check for Concierge rewrites and
// GPT-Live speech: names, key and figure rules, number words and ear form
// all stay the same.
func (s *session) beginFactAudit(source string) *factAudit {
	if strings.TrimSpace(source) == "" {
		return nil
	}
	a := &factAudit{source: source}
	s.factMu.Lock()
	s.facts = append(s.facts, a)
	s.factMu.Unlock()
	return a
}

// recordFactText adds actual model words to every fact that is waiting for
// its speech to finish. Sessions normally speak one answer at a time; this
// makes a concurrent app-owned acknowledgement conservative: its words are
// checked rather than disappearing from the measurement.
func (s *session) recordFactText(text string) {
	s.factMu.Lock()
	defer s.factMu.Unlock()
	for _, a := range s.facts {
		a.actual.WriteString(text)
	}
}

// discardFactAudit removes a fact whose commentary was not accepted, so no
// speech and therefore no comparison is claimed.
func (s *session) discardFactAudit(audit *factAudit) {
	s.factMu.Lock()
	defer s.factMu.Unlock()
	for i, a := range s.facts {
		if a == audit {
			s.facts = append(s.facts[:i], s.facts[i+1:]...)
			return
		}
	}
}

// finishFactAudits reports every fact whose speech just ended. It is called
// from the player after its playback drains, so the comparison never blocks
// the words the room hears.
func (s *session) finishFactAudits() {
	s.factMu.Lock()
	audits := s.facts
	s.facts = nil
	s.factMu.Unlock()
	for _, a := range audits {
		n := spokenFactErrors(a.source, a.actual.String())
		s.factErrors.Add(int64(n))
		s.emit(&voice.Event{Kind: voice.EventFactCheck, Count: n})
	}
}

// SpokenFactErrors is the running count for probes and room tests. It holds
// no source or spoken words.
func (s *session) SpokenFactErrors() int { return int(s.factErrors.Load()) }

// spokenFactErrors counts R8 verdicts: a valid spoken rewrite is zero and an
// invented name, changed key/figure or failed ear form is one. The count has
// no fact text, so the room test can record it safely.
func spokenFactErrors(source, spoken string) int {
	if earcheck.Check(spoken, source, "") != "" {
		return 1
	}
	return 0
}

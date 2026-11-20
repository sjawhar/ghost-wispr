package main

import (
	"testing"
	"time"
)

// The backend marker can arrive at the first audio chunk. The probe must not
// pass until the delegated answer's final audio tail has stopped and the
// response has ended.
func TestSolReviewProbeWaitsForDelegatedAnswerTail(t *testing.T) {
	s := newStandIns(t)
	s.delegate = true
	s.delegationTail = 100 * time.Millisecond
	cfg := s.config(t)
	cfg.Settle, cfg.Turns = time.Second, 2
	cfg.Ask = question(time.Second)
	begin := time.Now()
	report, err := runProbe(t, s, &cfg)
	if err != nil {
		t.Fatalf("probe: %v\n%s", err, report)
	}
	var tail time.Time
	select {
	case tail = <-s.tailSent:
	default:
		t.Fatal("the tail was never sent")
	}
	if since := time.Since(tail); since < answerTailQuiet-100*time.Millisecond {
		t.Fatalf("probe returned %v after the delegated answer tail, want it to wait for the full tail", since)
	}
	if elapsed := time.Since(begin); elapsed < answerTailQuiet {
		t.Fatalf("probe finished in %v, before a delegated tail could finish", elapsed)
	}
}

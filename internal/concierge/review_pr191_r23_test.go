package concierge

import "testing"

// TestAsksTellsTheOfficeRequestFromTalk is the gate the Supervisor uses after
// a chime whose final held the name but no call (voice.SupervisorConfig.Asks;
// the office, 2026-11-27, dispatch://INFRA-4592): the office request's first
// final asks, and the misheard final and talk that only follows do not.
func TestAsksTellsTheOfficeRequestFromTalk(t *testing.T) {
	for turn, want := range map[string]bool{
		"great. Can you send these notes to to whom?": true,
		"could you make a dispatch issue for Raj":     true,
		"what is happening with the recorder":         true,
		"Whisper. No. Oh, yeah,":                      false,
		"they really are.":                            false,
		"Whisper networks are a problem.":             false,
		"To Raj. On dispatch.":                        false,
	} {
		if got := Asks(turn); got != want {
			t.Errorf("Asks(%q) = %v, want %v", turn, got, want)
		}
	}
}

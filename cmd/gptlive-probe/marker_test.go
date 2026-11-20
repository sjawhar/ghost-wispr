package main

import "testing"

func TestDelegationEvidenceUsesOrderedWholeTokens(t *testing.T) {
	for text, want := range map[string]bool{
		"The verification key is PROBE-42.":    true,
		"The verification key is probe 42.":    true,
		"PROBE-142":                            false,
		"PROBE-420":                            false,
		"42 unrelated things before the probe": false,
		"PROBE then 42":                        false,
	} {
		if got := evidence(text, delegationEvidence); got != want {
			t.Errorf("evidence(%q) = %t, want %t", text, got, want)
		}
	}
}

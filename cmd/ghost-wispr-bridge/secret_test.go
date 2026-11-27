package main

import (
	"encoding/hex"
	"testing"
)

// TestEachBotGetsItsOwnNonce holds that each nonce is 32 random bytes and
// none repeats, so each bot's socket secret is its own: a bot's socket never
// takes another bot's dial, also across restarts of the bridge.
func TestEachBotGetsItsOwnNonce(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		n, err := newNonce()
		if err != nil {
			t.Fatalf("newNonce: %v", err)
		}
		b, err := hex.DecodeString(n)
		if err != nil || len(b) != 32 {
			t.Fatalf("nonce %q is not 32 bytes in hex", n)
		}
		if seen[n] {
			t.Fatalf("nonce %s came twice", n)
		}
		seen[n] = true
	}
}

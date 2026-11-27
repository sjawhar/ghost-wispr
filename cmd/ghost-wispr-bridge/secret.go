package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

// newNonce makes a bot's nonce: 32 random bytes, in hex. The bridge puts it
// in the bot's metadata, which Attendee returns on each read of the bot.
func newNonce() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// socketSecret is a bot's socket secret: hex of HMAC-SHA256 of its nonce
// under the bridge key. It is the last element of the audio socket's path,
// so a restarted bridge, which reads each bot's nonce from Attendee, takes
// the socket Attendee dials again, and nothing else can (spec, "Each bot's
// socket secret").
func socketSecret(bridgeKey []byte, nonce string) string {
	mac := hmac.New(sha256.New, bridgeKey)
	mac.Write([]byte(nonce))
	return hex.EncodeToString(mac.Sum(nil))
}

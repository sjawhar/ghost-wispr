package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/voice/joined"
)

// TestTheProbeReadsTheJoinMessage fails if the acceptance probe cannot read
// the unit's Envoy session, its API and the join line from the join message
// the roster writes, so the probe and the message drift apart.
func TestTheProbeReadsTheJoinMessage(t *testing.T) {
	roster := joined.New(&joined.Config{Session: func() string { return "" }, PublicURL: "https://recorder.example:8443"})
	t.Cleanup(roster.Close)
	roster.SetAgents(&recordingAgents{})
	text, err := roster.Invitation("ABC-12")
	if err != nil {
		t.Fatal(err)
	}
	inv, err := parseInvitation(text)
	if err != nil {
		t.Fatalf("parseInvitation: %v\n%s", err, text)
	}
	want := invitationParts{unit: "unit-session", api: "https://recorder.example:8443", join: "ghost-wispr join ABC-12"}
	if inv != want {
		t.Fatalf("parsed %+v, want %+v", inv, want)
	}
	if _, err := parseInvitation("Join the meeting, please."); err == nil {
		t.Fatal("a message with no session, API or join line parsed")
	}
}

// TestTheProbeReadsADispatchDeliveredJoinMessage fails if the probe does not
// take a person's join message out of the delivery frame Dispatch sends
// through Envoy ({event: {payload: {body}}, delivery}), or changes a plain
// Envoy message's text.
func TestTheProbeReadsADispatchDeliveredJoinMessage(t *testing.T) {
	body := "Join the meeting.\nghost-wispr join ABC-12\n"
	frame, err := json.Marshal(map[string]any{
		"event":    map[string]any{"type": "message.created", "payload": map[string]any{"body": body, "target": "session:probe"}},
		"delivery": map[string]any{"attempt": 1, "mode": "btw"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, fromDispatch := deliveredText(string(frame)); got != body || !fromDispatch {
		t.Fatalf("deliveredText(frame) = %q, %v; want the body, from Dispatch", got, fromDispatch)
	}
	for _, plain := range []string{"ghost-wispr join ABC-12", `{"event": "not a frame"}`, `{"other": 1}`} {
		if got, fromDispatch := deliveredText(plain); got != plain || fromDispatch {
			t.Errorf("deliveredText(%q) = %q, %v; want it as it is", plain, got, fromDispatch)
		}
	}
}

// TestTheProbeNeedsOneInvitationSource fails if the probe runs with no join
// message, or with two, rather than exiting 2 with its usage.
func TestTheProbeNeedsOneInvitationSource(t *testing.T) {
	for _, args := range [][]string{
		{"joined-probe"},
		{"joined-probe", "--invitation", "x", "--wait-invitation"},
	} {
		var stdout, stderr bytes.Buffer
		if code := runJoinedProbeCommand(t.Context(), args, "", &stdout, &stderr); code != 2 || !bytes.Contains(stderr.Bytes(), []byte("usage:")) {
			t.Errorf("%q exited %d with %q, want 2 and the usage", args, code, stderr.String())
		}
	}
}

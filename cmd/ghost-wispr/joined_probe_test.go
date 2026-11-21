package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
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

// probeUnit is the unit's side of a probe run: it takes each message the
// probe sends and answers it, as the roster would, with what reply gives.
type probeUnit struct {
	p     *joinedProbe
	reply func(text, event string) []*envoy.Message
	mu    sync.Mutex
	sent  []string
}

func (u *probeUnit) Send(_ context.Context, request *envoy.SendRequest) (envoy.SendResult, error) {
	u.mu.Lock()
	u.sent = append(u.sent, request.Message)
	event := fmt.Sprintf("event-%d", len(u.sent))
	u.mu.Unlock()
	for _, m := range u.reply(request.Message, event) {
		u.p.messages <- m
	}
	return envoy.SendResult{EventID: event, RecipientState: envoy.StateLive}, nil
}

// sentLine reports whether the probe sent a message that begins with line.
func (u *probeUnit) sentLine(line string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, text := range u.sent {
		if strings.HasPrefix(text, line) {
			return true
		}
	}
	return false
}

// TestTheProbeRefusesAPointerToAnotherMeeting fails if the probe takes a
// pointer that names a recording session other than the one its join's
// answer named, and reads it and asks for the floor as if it were its own
// meeting's, or if it refuses a pointer to its own meeting (the control).
func TestTheProbeRefusesAPointerToAnotherMeeting(t *testing.T) {
	now := time.Now().UTC()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]transcribe.Segment{{Timestamp: now}})
	}))
	t.Cleanup(server.Close)
	invitation := filepath.Join(t.TempDir(), "join.txt")
	text := "Ghost Wispr is the recorder in the meeting room. Its Envoy session is unit-session and its API is " + server.URL + ". Send it each message below.\n\nghost-wispr join scratch-1\n"
	if err := os.WriteFile(invitation, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	meeting := now.Add(-time.Hour).Format("20060102150405")
	for _, tc := range []struct {
		name, pointed string
		refused       bool
	}{
		{"the meeting it joined (control)", meeting, false},
		{"another meeting", now.Add(-25 * time.Hour).Format("20060102150405"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			p := &joinedProbe{out: &out, wait: 5 * time.Second, messages: make(chan *envoy.Message, 64)}
			unit := &probeUnit{p: p, reply: func(text, event string) []*envoy.Message {
				switch {
				case strings.HasPrefix(text, "ghost-wispr join"):
					return []*envoy.Message{
						{SourceSession: "unit-session", EventID: "welcome", InReplyTo: event, Text: "You joined the meeting Ghost Wispr records, as scratch-1's agent. Its recording session is " + meeting + ", recording since " + now.Add(-time.Hour).Format(time.TimeOnly) + " UTC."},
						{SourceSession: "unit-session", EventID: "pointer", Text: fmt.Sprintf("Your work came up in the meeting Ghost Wispr records: recording session %s, %s to %s UTC.",
							tc.pointed, now.Add(-2*time.Second).Format(time.TimeOnly), now.Add(2*time.Second).Format(time.TimeOnly))},
					}
				case strings.HasPrefix(text, "ghost-wispr floor"):
					return []*envoy.Message{{SourceSession: "unit-session", EventID: "outcome", InReplyTo: event, Text: "Your words were spoken in the room."}}
				}
				return nil
			}}
			p.session = unit
			err := p.run(t.Context(), invitation, "The acceptance check.")
			if !tc.refused {
				if err != nil || !unit.sentLine("ghost-wispr floor") {
					t.Fatalf("control: run = %v, floor sent %v; want the floor asked for in its own meeting\n%s", err, unit.sentLine("ghost-wispr floor"), out.String())
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.pointed) || !strings.Contains(err.Error(), meeting) {
				t.Fatalf("run = %v; want an error that names the pointer's meeting %s and the joined one %s\n%s", err, tc.pointed, meeting, out.String())
			}
			if unit.sentLine("ghost-wispr floor") {
				t.Fatal("the probe asked for the floor after a pointer to another meeting")
			}
		})
	}
}

// TestTheProbeDatesAPointersRange fails if the probe counts a pointer's
// segments on the wrong days: a pointer gives its range as clocks only
// (HH:MM:SS UTC). A recording session's id from the recorder is its start
// (YYYYMMDDhhmmss), which dates the range; any other id dates it by today.
// Each case must count the segments in the range and none a day away. The
// controls are ranges within today.
func TestTheProbeDatesAPointersRange(t *testing.T) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	day := 24 * time.Hour
	id := func(at time.Time) string { return at.Format("20060102150405") }
	clock := func(at time.Time) string { return at.Format(time.TimeOnly) }
	for _, tc := range []struct {
		name, session, from, to string
		segments                []time.Time
	}{
		{"a range across midnight", id(today.Add(-10 * time.Second)), "23:59:58", "00:00:02",
			[]time.Time{today.Add(-time.Second), today.Add(time.Second), today.Add(day - time.Second)}},
		{"a range on the day after its meeting began", id(today.Add(-2*day + 10*time.Hour)), "09:00:00", "09:00:04",
			[]time.Time{today.Add(-day + 9*time.Hour + time.Second), today.Add(-day + 9*time.Hour + 3*time.Second), today.Add(9*time.Hour + time.Second)}},
		{"a range that starts a moment before its session's id", id(today.Add(-day + 10*time.Hour)), "09:59:58", "10:00:03",
			[]time.Time{today.Add(-day + 10*time.Hour - time.Second), today.Add(-day + 10*time.Hour + 2*time.Second), today.Add(10*time.Hour + 2*time.Second)}},
		{"a range today (control)", id(today.Add(8 * time.Hour)), "10:05:00", "10:05:04",
			[]time.Time{today.Add(10*time.Hour + 5*time.Minute + time.Second), today.Add(10*time.Hour + 5*time.Minute + 3*time.Second), today.Add(day + 10*time.Hour + 5*time.Minute)}},
		{"another id's range across midnight", "a-meeting", clock(today.Add(-2 * time.Second)), clock(today.Add(2 * time.Second)),
			[]time.Time{today.Add(-time.Second), today.Add(time.Second), today.Add(day + time.Second)}},
		{"another id's range today (control)", "a-meeting", "12:00:00", "12:00:04",
			[]time.Time{today.Add(12*time.Hour + time.Second), today.Add(12*time.Hour + 3*time.Second), today.Add(-day + 12*time.Hour + time.Second)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				segments := make([]transcribe.Segment, len(tc.segments))
				for i, at := range tc.segments {
					segments[i].Timestamp = at
				}
				_ = json.NewEncoder(w).Encode(segments)
			}))
			t.Cleanup(server.Close)
			got, err := (&joinedProbe{}).readTranscript(t.Context(), server.URL, tc.session, tc.from, tc.to)
			if err != nil {
				t.Fatal(err)
			}
			if got["segments_in_range"] != 2 {
				t.Fatalf("session %s, %s to %s UTC: %v segments in range, want the first 2 of %v", tc.session, tc.from, tc.to, got["segments_in_range"], tc.segments)
			}
		})
	}
}

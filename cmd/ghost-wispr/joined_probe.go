package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

const joinedProbeUsage = "usage: ghost-wispr joined-probe [--envoy URL] (--invitation FILE | --wait-invitation) [--api URL] [--floor TEXT] [--timeout SECONDS]"

// joinedProbeFloor is the floor request the probe sends when --floor is not
// set: words for the ear, with no word of a meeting.
const joinedProbeFloor = "This is the acceptance check of joined agents. Nothing needs doing."

// runJoinedProbeCommand is ghost-wispr joined-probe: an agent session for the
// acceptance of joined agents (R17), as the driver and the end-to-end run use
// it. It registers an Envoy session of its own, takes a join message (from a
// file, or delivered to it, as a person's Dispatch message is), and follows
// it as an agent would: it joins, waits for the pointer of its issue coming
// up, reads that part of the transcript from the recorder's API, asks for the
// floor, waits for how its hand came down, and leaves. Each step is one JSON
// line on stdout. A message that may quote the room, a question the voice
// asks, is reported with no text; the roster's messages hold no words of the
// meeting. It exits 0 once it left, 1 when a step failed or timed out, and 2
// on wrong arguments.
func runJoinedProbeCommand(ctx context.Context, args []string, configPath string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("joined-probe", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	envoyURL := flags.String("envoy", "", "")
	invitationFile := flags.String("invitation", "", "")
	waitInvitation := flags.Bool("wait-invitation", false, "")
	// api replaces the API the join message names, for a development run
	// whose unit has no public address of its own (public_url).
	api := flags.String("api", "", "")
	floor := flags.String("floor", joinedProbeFloor, "")
	timeout := flags.Int("timeout", 120, "")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() > 0 || (*invitationFile == "") == !*waitInvitation || *timeout <= 0 {
		_, _ = fmt.Fprintln(stderr, joinedProbeUsage)
		return 2
	}
	p := &joinedProbe{out: stdout, wait: time.Duration(*timeout) * time.Second, messages: make(chan *envoy.Message, 64), api: *api}
	fail := func(err error) int {
		p.emit("failed", map[string]any{"error": err.Error()})
		_, _ = fmt.Fprintf(stderr, "joined-probe: %v\n", err)
		return 1
	}
	if *envoyURL == "" {
		cfgStore, _, err := loadRuntimeConfig(configPath)
		if err != nil {
			return fail(fmt.Errorf("config: %w", err))
		}
		cfg := cfgStore.Get()
		*envoyURL = cfg.Concierge.EnvoyURL
	}
	listener, err := envoy.NewListener(*envoyURL, nil)
	if err != nil {
		return fail(err)
	}
	host, _ := os.Hostname()
	session, err := envoy.OpenSession(ctx, listener, &envoy.SessionConfig{
		SessionID: uuid.NewString(), Title: "ghost-wispr joined probe", Dir: "ghost-wispr", Host: host,
		Plugin: "ghost-wispr-joined-probe", PluginVersion: Version,
		// Dispatch sends a person's message only to a session that
		// advertises its delivery mode.
		Capabilities: []string{"btw"},
		Deliver: func(_ context.Context, m *envoy.Message) error {
			// A person's Dispatch message arrives as a delivery frame; the
			// unit's own messages arrive as their text.
			if body, fromDispatch := deliveredText(m.Text); fromDispatch {
				m.Text = body
			}
			p.messages <- m
			return nil
		},
	})
	if err != nil {
		return fail(fmt.Errorf("open the probe's Envoy session: %w", err))
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = session.Close(closeCtx)
	}()
	p.session = session
	p.emit("registered", map[string]any{"session": session.ID()})
	if err := p.run(ctx, *invitationFile, *floor); err != nil {
		return fail(err)
	}
	return 0
}

// joinedProbe is one probe run.
type joinedProbe struct {
	out      io.Writer
	outMu    sync.Mutex
	wait     time.Duration
	session  *envoy.Session
	messages chan *envoy.Message
	unit     string
	// api replaces the join message's API when it is set.
	api string
}

func (p *joinedProbe) emit(step string, fields map[string]any) {
	line := map[string]any{"step": step, "at": time.Now().UTC().Format(time.RFC3339Nano)}
	for k, v := range fields {
		line[k] = v
	}
	data, _ := json.Marshal(line)
	p.outMu.Lock()
	defer p.outMu.Unlock()
	_, _ = fmt.Fprintf(p.out, "%s\n", data)
}

func (p *joinedProbe) run(ctx context.Context, invitationFile, floor string) error {
	var text, via string
	if invitationFile != "" {
		data, err := os.ReadFile(invitationFile)
		if err != nil {
			return err
		}
		text, via = string(data), "file"
	} else {
		m, err := p.next(ctx, "the join message", func(m *envoy.Message) bool { return strings.Contains(m.Text, "ghost-wispr join") })
		if err != nil {
			return err
		}
		text, via = m.Text, "envoy message from "+m.SourceSession
	}
	inv, err := parseInvitation(text)
	if err != nil {
		return err
	}
	if p.api != "" {
		inv.api = p.api
	}
	p.unit = inv.unit
	p.emit("invited", map[string]any{"via": via, "unit": inv.unit, "api": inv.api, "join": inv.join})

	joinEvent, err := p.send(ctx, inv.join, "join")
	if err != nil {
		return err
	}
	welcome, err := p.next(ctx, "the answer to the join", func(m *envoy.Message) bool { return m.InReplyTo == joinEvent })
	if err != nil {
		return err
	}
	meeting := regexp.MustCompile(`recording session is (\S+?)[,.]`).FindStringSubmatch(welcome.Text)
	if meeting == nil {
		return fmt.Errorf("the join was not taken: %s", welcome.Text)
	}
	p.emit("joined", map[string]any{"recording_session": meeting[1], "text": welcome.Text})

	pointer, err := p.next(ctx, "a pointer", func(m *envoy.Message) bool { return strings.Contains(m.Text, "came up in the meeting") })
	if err != nil {
		return err
	}
	span := regexp.MustCompile(`recording session (\S+), (\d\d:\d\d:\d\d) to (\d\d:\d\d:\d\d) UTC`).FindStringSubmatch(pointer.Text)
	if span == nil {
		return fmt.Errorf("a pointer that names no meeting or time range: %s", pointer.Text)
	}
	p.emit("pointer", map[string]any{"recording_session": span[1], "from": span[2], "to": span[3], "text": pointer.Text})
	// A segment is stored a moment after the words that made it reach the
	// voice, so the read is made again for a few seconds.
	var read map[string]any
	deadline := time.Now().Add(15 * time.Second)
	for {
		read, err = p.readTranscript(ctx, inv.api, span[1], span[2], span[3])
		if err == nil {
			if count, ok := read["segments_in_range"].(int); ok && count > 0 {
				break
			}
		}
		if !time.Now().Before(deadline) {
			if err != nil {
				return fmt.Errorf("read the transcript at the pointer: %w", err)
			}
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	p.emit("transcript_read", read)

	floorEvent, err := p.send(ctx, "ghost-wispr floor\n"+floor, "floor")
	if err != nil {
		return err
	}
	outcome, err := p.next(ctx, "how the hand came down", func(m *envoy.Message) bool { return m.InReplyTo == floorEvent })
	if err != nil {
		return err
	}
	kind := "refused"
	switch {
	case strings.Contains(outcome.Text, "were spoken"):
		kind = "spoken"
	case strings.Contains(outcome.Text, "dropped"):
		kind = "dropped"
	}
	p.emit("hand", map[string]any{"outcome": kind, "text": outcome.Text})

	if _, err := p.send(ctx, "ghost-wispr leave", "leave"); err != nil {
		return err
	}
	return nil
}

// next waits for a message from the unit that want takes, reporting each
// message on the way.
func (p *joinedProbe) next(ctx context.Context, what string, want func(*envoy.Message) bool) (*envoy.Message, error) {
	deadline := time.NewTimer(p.wait)
	defer deadline.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			p.emit("timeout", map[string]any{"waiting_for": what})
			return nil, fmt.Errorf("no %s within %v", what, p.wait)
		case m := <-p.messages:
			fields := map[string]any{"from": m.SourceSession, "event": m.EventID, "in_reply_to": m.InReplyTo}
			if strings.HasPrefix(m.Text, "From the office meeting recorder") {
				fields["kind"] = "question" // it may quote the room
			} else {
				fields["text"] = m.Text
			}
			p.emit("received", fields)
			if want(m) {
				return m, nil
			}
		}
	}
}

// send sends the unit one protocol message, and reports it.
func (p *joinedProbe) send(ctx context.Context, text, step string) (string, error) {
	sendCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	sent, err := p.session.Send(sendCtx, &envoy.SendRequest{Target: p.unit, Message: text, NoReply: true, IdempotencyKey: uuid.NewString()})
	if err != nil {
		return "", fmt.Errorf("send the %s: %w", step, err)
	}
	p.emit(step+"_sent", map[string]any{"event": sent.EventID, "recipient_state": string(sent.RecipientState)})
	return sent.EventID, nil
}

// readTranscript reads the meeting's segments from the recorder's API, and
// reports how many fall in the pointer's time range (today, UTC), with no
// words.
func (p *joinedProbe) readTranscript(ctx context.Context, api, sessionID, from, to string) (map[string]any, error) {
	day := time.Now().UTC().Format("2006-01-02")
	start, err1 := time.Parse(time.RFC3339, day+"T"+from+"Z")
	end, err2 := time.Parse(time.RFC3339, day+"T"+to+"Z")
	if err := errors.Join(err1, err2); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(api, "/")+"/api/sessions/"+sessionID+"/segments", nil)
	if err != nil {
		return nil, err
	}
	if token := os.Getenv("GHOST_WISPR_AUTH_TOKEN"); token != "" {
		request.SetBasicAuth("ghost-wispr", token)
	}
	response, err := (&http.Client{Timeout: 15 * time.Second}).Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET segments: HTTP %d", response.StatusCode)
	}
	var segments []transcribe.Segment
	if err := json.NewDecoder(response.Body).Decode(&segments); err != nil {
		return nil, err
	}
	inRange := 0
	for i := range segments {
		at := segments[i].Timestamp
		if !at.Before(start.Add(-5*time.Second)) && !at.After(end.Add(5*time.Second)) {
			inRange++
		}
	}
	return map[string]any{"status": response.StatusCode, "segments": len(segments), "segments_in_range": inRange}, nil
}

// invitationParts are what a join message gives an agent.
type invitationParts struct {
	unit, api, join string
}

// parseInvitation reads a join message (joined.Roster.Invitation): the unit's
// Envoy session, its API and the join line.
func parseInvitation(text string) (invitationParts, error) {
	var inv invitationParts
	if m := regexp.MustCompile(`Envoy session is (\S+) and its API is (\S+?)\.(\s|$)`).FindStringSubmatch(text); m != nil {
		inv.unit, inv.api = m[1], m[2]
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "ghost-wispr join") {
			inv.join = strings.TrimSpace(line)
			break
		}
	}
	if inv.unit == "" || inv.api == "" || inv.join == "" {
		return inv, errors.New("the join message names no Envoy session, API or join line")
	}
	return inv, nil
}

// deliveredText is what a delivered message says: a Dispatch delivery frame's
// message body ({event: {payload: {body}}, delivery}, how Dispatch hands a
// person's message to a session through Envoy), with true, or any other text
// as it is.
func deliveredText(text string) (string, bool) {
	var frame struct {
		Event struct {
			Payload struct {
				Body *string `json:"body"`
			} `json:"payload"`
		} `json:"event"`
		Delivery json.RawMessage `json:"delivery"`
	}
	if !strings.HasPrefix(strings.TrimSpace(text), "{") || json.Unmarshal([]byte(text), &frame) != nil ||
		frame.Event.Payload.Body == nil || len(frame.Delivery) == 0 {
		return text, false
	}
	return *frame.Event.Payload.Body, true
}

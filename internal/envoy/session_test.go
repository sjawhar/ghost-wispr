package envoy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// fakeListener is an Envoy listener's HTTP API that records each request
// body by path, each request's headers, and answers what the test sets.
type fakeListener struct {
	mu      sync.Mutex
	bodies  map[string][]map[string]any
	answers map[string]func(w http.ResponseWriter, r *http.Request)
	deletes []string
	headers []http.Header
}

func newFakeListener(t *testing.T) (*fakeListener, *httptest.Server) {
	t.Helper()
	fake := &fakeListener{bodies: map[string][]map[string]any{}, answers: map[string]func(http.ResponseWriter, *http.Request){}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		fake.headers = append(fake.headers, r.Header.Clone())
		if r.Method == http.MethodDelete {
			fake.deletes = append(fake.deletes, r.URL.String())
		}
		if r.Body != nil {
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			if json.Unmarshal(raw, &body) == nil {
				fake.bodies[r.URL.Path] = append(fake.bodies[r.URL.Path], body)
			}
		}
		answer := fake.answers[r.URL.Path]
		fake.mu.Unlock()
		if answer == nil {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		answer(w, r)
	}))
	t.Cleanup(server.Close)
	return fake, server
}

func (f *fakeListener) answer(path string, status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[path] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func (f *fakeListener) lastBody(t *testing.T, path string) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	bodies := f.bodies[path]
	if len(bodies) == 0 {
		t.Fatalf("no request to %s", path)
	}
	return bodies[len(bodies)-1]
}

// testSession is a Session on the fake listener with no NATS connection, so
// a test drives registration, sends and frames itself.
func testSession(t *testing.T, server *httptest.Server, deliver func(context.Context, *Message) error, saved *[]ResumePoint) *Session {
	t.Helper()
	listener, err := NewListener(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	session, err := newSession(listener, &SessionConfig{
		SessionID: "pi-session-1", Title: "Office recorder", Dir: "/opt/unit", Host: "unit", Plugin: "ghost-wispr", PluginVersion: "test",
		Deliver: deliver,
		SaveResume: func(point ResumePoint) error {
			if saved != nil {
				*saved = append(*saved, point)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func TestASessionIDMustBeOneEnvoyTakes(t *testing.T) {
	for _, id := range []string{"", "a.b", "a*b", "a>b", "a/b", `a\b`, "a b", "a%2Fb", "a?b", "a#b"} {
		if err := ValidSessionID(id); err == nil {
			t.Errorf("ValidSessionID(%q) = nil, want a refusal", id)
		}
	}
	if err := ValidSessionID("0f9c2d3e-1111-4222-8333-444455556666"); err != nil {
		t.Errorf("a UUID is refused: %v", err)
	}
}

func TestNATSURLComesFromTheListenersHost(t *testing.T) {
	got, err := NATSURLFor("http://listener.example.ts.net:9020")
	if err != nil || got != "nats://listener.example.ts.net:4222" {
		t.Fatalf("NATSURLFor = %q, %v", got, err)
	}
	if _, err := NATSURLFor("not a url"); err == nil {
		t.Error("a listener URL with no host gave a NATS URL")
	}
}

// TestSessionRegistersWithConsumerDelivery (c-identity): a registration asks
// for consumer delivery with this process's epoch, and keeps the floor of the
// resume point its answer names.
func TestSessionRegistersWithConsumerDelivery(t *testing.T) {
	fake, server := newFakeListener(t)
	fake.answer("/v1/interests/subscribe", http.StatusOK, `{"session_id":"pi-session-1","topics":[],"delivery":{"mode":"consumer","subject":"notifications.envoy.deliver.pi-session-1","stream_id":"stream-A","consumer_start":41}}`)
	var saved []ResumePoint
	session := testSession(t, server, nil, &saved)

	if err := session.register(context.Background()); err != nil {
		t.Fatalf("register: %v", err)
	}
	body := fake.lastBody(t, "/v1/interests/subscribe")
	delivery, _ := body["delivery"].(map[string]any)
	if body["session_id"] != "pi-session-1" || body["title"] != "Office recorder" || body["plugin"] != "ghost-wispr" || delivery["consumer"] != true || delivery["epoch"] == "" || delivery["epoch"] == nil {
		t.Fatalf("registration body = %v", body)
	}
	if topics, ok := body["topics"].([]any); !ok || len(topics) != 0 {
		t.Errorf("topics = %#v, want an empty list", body["topics"])
	}
	if _, ok := delivery["resume"]; ok {
		t.Errorf("a first registration with no resume point sent one: %v", delivery)
	}
	if body["client_instance_id"] == "" || body["client_instance_id"] == nil {
		t.Errorf("no client_instance_id: %v", body)
	}
	if len(saved) != 1 || saved[0] != (ResumePoint{StreamID: "stream-A", Seq: 40}) {
		t.Fatalf("saved resume points = %+v, want stream-A at 40 (consumer_start - 1)", saved)
	}

	if err := session.register(context.Background()); err != nil {
		t.Fatal(err)
	}
	delivery, _ = fake.lastBody(t, "/v1/interests/subscribe")["delivery"].(map[string]any)
	resume, _ := delivery["resume"].(map[string]any)
	if resume["stream_id"] != "stream-A" || resume["seq"] != float64(40) {
		t.Errorf("the next registration's resume = %v, want stream-A at 40", delivery["resume"])
	}

	// An opened session subscribes to its delivery subject before it
	// registers, so no frame the registration starts is lost.
	natsServer := newFakeNATS(t)
	subscribedFirst := false
	fake.mu.Lock()
	fake.answers["/v1/interests/subscribe"] = func(w http.ResponseWriter, _ *http.Request) {
		subscribedFirst = slices.Contains(natsServer.subjects(), DeliverSubjectPrefix+"pi-session-1")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"session_id":"pi-session-1","delivery":{"mode":"consumer","stream_id":"stream-A","consumer_start":41}}`))
	}
	fake.mu.Unlock()
	listener, err := NewListener(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	opened, err := OpenSession(context.Background(), listener, &SessionConfig{SessionID: "pi-session-1", NATSURL: natsServer.url(), Plugin: "ghost-wispr"})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer func() { _ = opened.Close(context.Background()) }()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !subscribedFirst {
		t.Fatal("the session registered before it subscribed to its delivery subject")
	}
}

func TestARefusedRegistrationIsAnError(t *testing.T) {
	fake, server := newFakeListener(t)
	fake.answer("/v1/interests/subscribe", http.StatusConflict, `{"error":"no consumer requested","reason":"not_requested"}`)
	session := testSession(t, server, nil, nil)
	err := session.register(context.Background())
	var apiErr *ListenerError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict || !strings.Contains(err.Error(), "no consumer requested") {
		t.Fatalf("register = %v, want the listener's 409", err)
	}
}

func TestSendAsksForAReplyAsThisSession(t *testing.T) {
	fake, server := newFakeListener(t)
	fake.answer("/v1/messages/send", http.StatusOK, `{"event_id":"evt-9","recipient":"agent-7","recipient_state":"live","state":"connected"}`)
	session := testSession(t, server, nil, nil)

	result, err := session.Send(context.Background(), &SendRequest{Target: "agent-7", Message: "What is left?", IdempotencyKey: "key-1", Supersedes: "evt-1"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if result.EventID != "evt-9" || result.RecipientState != "live" || result.State != "connected" {
		t.Errorf("result = %+v", result)
	}
	body := fake.lastBody(t, "/v1/messages/send")
	for field, want := range map[string]any{
		"source": "agent", "source_session": "pi-session-1", "target_session": "agent-7", "message": "What is left?",
		"idempotency_key": "key-1", "expects_reply": "required", "supersedes": "evt-1",
	} {
		if body[field] != want {
			t.Errorf("send %s = %v, want %v", field, body[field], want)
		}
	}
}

func TestASendToAGoneSessionSaysSo(t *testing.T) {
	fake, server := newFakeListener(t)
	fake.answer("/v1/messages/send", http.StatusNotFound, `{"error":"no live session agent-7","state":"gone"}`)
	session := testSession(t, server, nil, nil)
	_, err := session.Send(context.Background(), &SendRequest{Target: "agent-7", Message: "x", IdempotencyKey: "k"})
	var apiErr *ListenerError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound || apiErr.State != "gone" {
		t.Fatalf("Send = %v, want the 404 naming state gone", err)
	}
}

func TestLookupReadsTheRegistry(t *testing.T) {
	fake, server := newFakeListener(t)
	fake.answer("/v1/registry/agent-7", http.StatusOK, `{"session_id":"agent-7","state":"unresponsive","title":"Worker","last_seen":1795066288022}`)
	fake.answer("/v1/registry/nobody", http.StatusNotFound, `{"error":"no live session nobody; this listener does not know when it last registered","state":"gone"}`)
	listener, err := NewListener(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	info, err := listener.Lookup(context.Background(), "agent-7")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if info.State != StateUnresponsive || info.Title != "Worker" || !info.LastSeen.Equal(time.UnixMilli(1795066288022)) {
		t.Errorf("info = %+v", info)
	}
	info, err = listener.Lookup(context.Background(), "nobody")
	if err != nil || info.State != StateGone {
		t.Errorf("Lookup of an unknown session = %+v, %v; want gone", info, err)
	}
	if _, err := listener.Lookup(context.Background(), "a/b"); err == nil {
		t.Error("Lookup sent a session id with a slash in it")
	}
}

// frame builds a relay frame's headers.
func frame(seq, streamID string) nats.Header {
	header := nats.Header{}
	header.Set("Envoy-Subject", "notifications.agent.pi-session-1")
	header.Set("Envoy-Stream-Seq", seq)
	header.Set("Envoy-Stream-Id", streamID)
	header.Set("Envoy-Delivered", "1")
	header.Set("Envoy-Stored-At", "2026-11-19T06:00:00.123456789Z")
	return header
}

// TestFramesAnswered (c-identity): a frame is answered received, then
// progress every progressEvery while the host holds it, then done once it
// wrote it; a probe frame is answered done.
func TestFramesAnswered(t *testing.T) {
	_, server := newFakeListener(t)
	var got []Message
	var saved []ResumePoint
	release := make(chan struct{})
	session := testSession(t, server, func(_ context.Context, message *Message) error {
		got = append(got, *message)
		if message.EventID == "evt-held" {
			<-release
		}
		return nil
	}, &saved)
	session.progressEvery = 20 * time.Millisecond

	var replies []string
	envelope := `{"event_id":"evt-20","source":"agent","source_session":"agent-7","topic":"notifications.agent.pi-session-1","in_reply_to":"evt-9","payload_summary":"First line.","payload":"First line.\nSecond line.","issued_at":1795066288022}`
	session.handle(frame("41", "stream-A"), []byte(envelope), func(body string) { replies = append(replies, body) })
	if strings.Join(replies, ",") != "received,done" {
		t.Fatalf("replies = %q, want received then done", replies)
	}
	if len(got) != 1 || got[0].EventID != "evt-20" || got[0].SourceSession != "agent-7" || got[0].InReplyTo != "evt-9" || got[0].Text != "First line.\nSecond line." {
		t.Fatalf("delivered = %+v", got)
	}
	if len(saved) != 1 || saved[0] != (ResumePoint{StreamID: "stream-A", Seq: 41}) {
		t.Fatalf("resume points = %+v, want stream-A at 41", saved)
	}

	// A one-line message carries its text in the summary alone.
	replies = nil
	session.handle(frame("42", "stream-A"), []byte(`{"event_id":"evt-21","source_session":"agent-7","payload_summary":"pong"}`), func(body string) { replies = append(replies, body) })
	if len(got) != 2 || got[1].Text != "pong" {
		t.Fatalf("one-line message = %+v", got)
	}

	// A frame the host holds is answered progress while it holds it.
	var mu sync.Mutex
	replies = nil
	go func() {
		time.Sleep(110 * time.Millisecond)
		close(release)
	}()
	session.handle(frame("43", "stream-A"), []byte(`{"event_id":"evt-held","payload_summary":"x"}`), func(body string) {
		mu.Lock()
		defer mu.Unlock()
		replies = append(replies, body)
	})
	mu.Lock()
	held := replies
	mu.Unlock()
	if len(held) < 4 || held[0] != "received" || held[len(held)-1] != "done" {
		t.Fatalf("replies to a held frame = %q, want received, progress at least twice, then done", held)
	}
	for _, reply := range held[1 : len(held)-1] {
		if reply != "progress" {
			t.Fatalf("replies to a held frame = %q, want only progress between received and done", held)
		}
	}
	if progressEvery > 20*time.Second {
		t.Errorf("progressEvery = %v, want at most 20 s", progressEvery)
	}

	// A probe frame is answered done.
	probe := nats.Header{}
	probe.Set("Envoy-Probe", "1")
	replies = nil
	session.handle(probe, nil, func(body string) { replies = append(replies, body) })
	if strings.Join(replies, ",") != "done" {
		t.Fatalf("replies to a probe = %q, want done", replies)
	}
}

// TestReRegistersWithin90s (c-identity): the listener calls a session live
// for a while after each registration and asks for one at least every 90 s;
// the heartbeat registers again on every tick.
func TestReRegistersWithin90s(t *testing.T) {
	fake, server := newFakeListener(t)
	session := testSession(t, server, nil, nil)
	if session.heartbeatEvery > 90*time.Second || HeartbeatEvery > 90*time.Second {
		t.Fatalf("a session registers every %v (default %v), want at most 90 s", session.heartbeatEvery, HeartbeatEvery)
	}
	session.heartbeatEvery = 10 * time.Millisecond
	session.stopped.Add(1)
	go session.heartbeat()
	deadline := time.Now().Add(2 * time.Second)
	for {
		fake.mu.Lock()
		registrations := len(fake.bodies["/v1/interests/subscribe"])
		fake.mu.Unlock()
		if registrations >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d registrations in 2 s of 10 ms ticks, want 3", registrations)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestNoEnvoyCredentialSent (c-identity): the listener's API needs only
// reach; no request of a session carries a credential.
func TestNoEnvoyCredentialSent(t *testing.T) {
	fake, server := newFakeListener(t)
	fake.answer("/v1/messages/send", http.StatusOK, `{"event_id":"evt-1","recipient_state":"live","state":"connected"}`)
	session := testSession(t, server, nil, nil)
	if err := session.register(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Send(context.Background(), &SendRequest{Target: "agent-7", Message: "q", IdempotencyKey: "k"}); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Lookup(context.Background(), "agent-7"); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.headers) != 4 {
		t.Fatalf("control: %d requests reached the fake listener, want 4", len(fake.headers))
	}
	for _, header := range fake.headers {
		for name := range header {
			lower := strings.ToLower(name)
			if lower == "authorization" || lower == "cookie" || strings.Contains(lower, "token") || strings.Contains(lower, "key") || strings.Contains(lower, "secret") || strings.Contains(lower, "signature") {
				t.Errorf("a request carries %s", name)
			}
		}
	}
	for path, bodies := range fake.bodies {
		for _, body := range bodies {
			for field := range body {
				if lower := strings.ToLower(field); strings.Contains(lower, "token") || strings.Contains(lower, "secret") || lower == "key" {
					t.Errorf("%s carries the field %s", path, field)
				}
			}
		}
	}
}

// TestANakedFrameHoldsTheResumePointBelowIt: a frame the host could not take
// comes again, so a later frame written before it must not move the resume
// point past it; once it is written, the point moves past both.
func TestANakedFrameHoldsTheResumePointBelowIt(t *testing.T) {
	_, server := newFakeListener(t)
	var saved []ResumePoint
	fail := true
	session := testSession(t, server, func(_ context.Context, message *Message) error {
		if message.EventID == "e5" && fail {
			return errors.New("store unavailable")
		}
		return nil
	}, &saved)
	ignore := func(string) {}
	session.handle(frame("4", "stream-A"), []byte(`{"event_id":"e4","payload_summary":"x"}`), ignore)
	session.handle(frame("5", "stream-A"), []byte(`{"event_id":"e5","payload_summary":"x"}`), ignore)
	session.handle(frame("6", "stream-A"), []byte(`{"event_id":"e6","payload_summary":"x"}`), ignore)
	if last := saved[len(saved)-1]; last != (ResumePoint{StreamID: "stream-A", Seq: 4}) {
		t.Fatalf("resume point with frame 5 held = %+v, want stream-A at 4", last)
	}
	fail = false
	session.handle(frame("5", "stream-A"), []byte(`{"event_id":"e5","payload_summary":"x"}`), ignore)
	if last := saved[len(saved)-1]; last != (ResumePoint{StreamID: "stream-A", Seq: 6}) {
		t.Fatalf("resume point once frame 5 is written = %+v, want stream-A at 6", last)
	}
}

// TestAnAnswerWithNoConsumerStartSetsNoPoint: a listener that names no
// consumer start says nothing of where this session is in the stream, so no
// point is kept (a point at 0 would ask for the whole stream again).
func TestAnAnswerWithNoConsumerStartSetsNoPoint(t *testing.T) {
	fake, server := newFakeListener(t)
	fake.answer("/v1/interests/subscribe", http.StatusOK, `{"session_id":"pi-session-1","delivery":{"mode":"consumer","stream_id":"stream-A"}}`)
	var saved []ResumePoint
	session := testSession(t, server, nil, &saved)
	if err := session.register(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(saved) != 0 {
		t.Fatalf("saved %+v from an answer with no consumer start", saved)
	}
}

func TestAFrameTheHostCannotTakeIsNaked(t *testing.T) {
	_, server := newFakeListener(t)
	var saved []ResumePoint
	session := testSession(t, server, func(context.Context, *Message) error { return errors.New("store unavailable") }, &saved)
	var replies []string
	session.handle(frame("7", "stream-A"), []byte(`{"event_id":"e","payload_summary":"x"}`), func(body string) { replies = append(replies, body) })
	if strings.Join(replies, ",") != "received,nak" {
		t.Fatalf("replies = %q, want received then nak", replies)
	}
	if len(saved) != 0 {
		t.Errorf("a frame that was not written moved the resume point: %+v", saved)
	}
}

func TestAProbeAndABadFrameAreAnsweredDoneAndNotDelivered(t *testing.T) {
	_, server := newFakeListener(t)
	delivered := 0
	session := testSession(t, server, func(context.Context, *Message) error { delivered++; return nil }, nil)

	probe := nats.Header{}
	probe.Set("Envoy-Probe", "1")
	var replies []string
	session.handle(probe, nil, func(body string) { replies = append(replies, body) })
	bad := frame("not-a-number", "stream-A")
	session.handle(bad, []byte(`{}`), func(body string) { replies = append(replies, body) })
	session.handle(frame("9", "stream-A"), []byte(`not json`), func(body string) { replies = append(replies, body) })

	if strings.Join(replies, ",") != "done,done,received,done" {
		t.Fatalf("replies = %q", replies)
	}
	if delivered != 0 {
		t.Errorf("%d bad frames were delivered", delivered)
	}
}

// TestSessionEndsAtShutdown (c-identity): Close removes this process's
// registration, and only it.
func TestSessionEndsAtShutdown(t *testing.T) {
	fake, server := newFakeListener(t)
	session := testSession(t, server, nil, nil)
	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.deletes) != 1 || !strings.HasPrefix(fake.deletes[0], "/v1/sessions/pi-session-1?client_instance_id=") {
		t.Fatalf("deletes = %q", fake.deletes)
	}
}

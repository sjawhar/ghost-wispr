package envoy

// This file is the unit's Envoy session: what lets an agent's reply reach the
// unit. Envoy delivers a reply only to a session, so the unit registers as
// one, as every agent session's plugin does: it subscribes on NATS to its
// relay subject, registers with the listener for consumer delivery, and
// registers again every minute. Each relay frame is answered on its reply
// subject (received, then done or nak). The listener's HTTP API and NATS need
// no credential, only reach. The shapes follow the listener's wire package
// (sjawhar/legion packages/envoy/listenerwire) and the TypeScript client's
// consumer delivery (packages/envoy-client/src/consumer-delivery.ts).

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// DeliverSubjectPrefix is the core NATS subject prefix a session's relay
// frames arrive on, followed by the session id.
const DeliverSubjectPrefix = "notifications.envoy.deliver."

// HeartbeatEvery is how often a session registers again. The listener calls a
// session live for 5 minutes after a registration and asks for one at least
// every 90 seconds.
const HeartbeatEvery = 60 * time.Second

// progressEvery is how often a frame the host still holds is answered
// progress, which restarts the listener's 60 s ack wait (the relay's
// "progress" reply, legion packages/envoy/cmd/listener/session_relay.go).
const progressEvery = 20 * time.Second

// deliverTimeout bounds the host's handling of one frame. progress keeps the
// listener waiting for it meanwhile.
const deliverTimeout = 2 * time.Minute

// SessionState is what a session's last registration says about it.
type SessionState string

const (
	StateLive         SessionState = "live"
	StateUnresponsive SessionState = "unresponsive"
	StateGone         SessionState = "gone"
)

// ValidSessionID refuses an id Envoy refuses or a URL path would read as
// more than one name: empty, or holding . * > / \ % ? # or whitespace. The
// error does not quote the id, which may come from a model's tool call.
func ValidSessionID(id string) error {
	if id == "" || strings.ContainsAny(id, ".*>/\\%?#") || strings.IndexFunc(id, func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' }) >= 0 {
		return errors.New("envoy: no session id: an id is not empty and holds none of . * > / \\ % ? # or whitespace")
	}
	return nil
}

// NATSURLFor is the hub's NATS address for its listener at listenerURL: the
// same host, port 4222.
func NATSURLFor(listenerURL string) (string, error) {
	parsed, err := url.Parse(listenerURL)
	if err != nil || parsed.Hostname() == "" {
		return "", fmt.Errorf("envoy: the listener URL %q names no host", listenerURL)
	}
	return "nats://" + net.JoinHostPort(parsed.Hostname(), "4222"), nil
}

// ListenerError is a listener answer that is no success, with the listener's
// error text and, on a 404 for a session, the state it names.
type ListenerError struct {
	Path    string
	Status  int
	Message string
	Reason  string
	State   string
}

func (e *ListenerError) Error() string {
	text := fmt.Sprintf("envoy: %s answered %d: %s", e.Path, e.Status, e.Message)
	if e.Reason != "" {
		text += " (" + e.Reason + ")"
	}
	return text
}

// Listener calls an Envoy listener's HTTP API.
type Listener struct {
	base *url.URL
	http *http.Client
}

// NewListener returns a client of the listener at baseURL, such as
// http://listener.example:9020. httpClient nil uses one with a 10 s timeout.
func NewListener(baseURL string, httpClient *http.Client) (*Listener, error) {
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("envoy: the listener URL %q names no scheme and host", baseURL)
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &Listener{base: base, http: httpClient}, nil
}

// SessionInfo is the registry's answer for one session.
type SessionInfo struct {
	SessionID string
	State     SessionState
	Title     string
	LastSeen  time.Time
}

// Lookup reads whether session id is live. A session the listener does not
// know, or knows as gone, is StateGone with no error.
func (l *Listener) Lookup(ctx context.Context, id string) (SessionInfo, error) {
	if err := ValidSessionID(id); err != nil {
		return SessionInfo{}, err
	}
	var answer struct {
		SessionID string `json:"session_id"`
		State     string `json:"state"`
		Title     string `json:"title"`
		LastSeen  int64  `json:"last_seen"`
	}
	err := l.do(ctx, http.MethodGet, "/v1/registry/"+id, nil, nil, &answer)
	var listenerErr *ListenerError
	if errors.As(err, &listenerErr) && listenerErr.Status == http.StatusNotFound {
		return SessionInfo{SessionID: id, State: StateGone}, nil
	}
	if err != nil {
		return SessionInfo{}, err
	}
	info := SessionInfo{SessionID: id, State: SessionState(answer.State), Title: answer.Title}
	if answer.LastSeen > 0 {
		info.LastSeen = time.UnixMilli(answer.LastSeen)
	}
	return info, nil
}

func (l *Listener) do(ctx context.Context, method, path string, query url.Values, body, into any) error {
	target := l.base.JoinPath(path)
	target.RawQuery = query.Encode()
	var reader io.Reader = http.NoBody
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("envoy: encode %s: %w", path, err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, target.String(), reader)
	if err != nil {
		return fmt.Errorf("envoy: build %s: %w", path, err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := l.http.Do(request)
	if err != nil {
		return fmt.Errorf("envoy: %s %s: %w", method, path, err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("envoy: read %s: %w", path, err)
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		var refusal struct {
			Error  string `json:"error"`
			Reason string `json:"reason"`
			State  string `json:"state"`
		}
		_ = json.Unmarshal(raw, &refusal)
		return &ListenerError{Path: path, Status: response.StatusCode, Message: refusal.Error, Reason: refusal.Reason, State: refusal.State}
	}
	if into == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("envoy: decode %s: %w", path, err)
	}
	return nil
}

// ResumePoint is a session's position in Envoy's stream: every frame up to
// Seq of stream StreamID is written. A registration sends it, so frames that
// arrived while the unit was down come after a restart.
type ResumePoint struct {
	StreamID string `json:"stream_id"`
	Seq      uint64 `json:"seq"`
}

// Message is one message delivered to the session.
type Message struct {
	EventID       string
	SourceSession string
	Topic         string
	InReplyTo     string
	Supersedes    string
	// Text is the whole message: the payload where the sender's message was
	// longer than its one-line summary, else the summary.
	Text     string
	IssuedAt time.Time
}

// SessionConfig configures the unit's Envoy session.
type SessionConfig struct {
	// SessionID is the unit's own session id, kept across restarts.
	SessionID string
	// NATSURL is the hub's NATS address; empty is the listener's host on
	// port 4222 (NATSURLFor).
	NATSURL                                 string
	Title, Dir, Host, Plugin, PluginVersion string
	Resume                                  *ResumePoint
	// SaveResume keeps the resume point where the unit keeps its state.
	SaveResume func(ResumePoint) error
	// Deliver takes one message. It runs for each frame in stream order; an
	// error answers the frame nak, so the listener delivers it again.
	Deliver func(ctx context.Context, message *Message) error
	Logger  *slog.Logger
}

// Session is the unit's registered Envoy session.
type Session struct {
	*Listener
	cfg              SessionConfig
	logger           *slog.Logger
	clientInstanceID string

	mu    sync.Mutex
	epoch string
	// resume is the point last saved, which a registration sends. stream,
	// written and held are the frames behind it: the stream the relay
	// delivers, the highest sequence answered done (or the consumer's start
	// less one), and the sequences answered nak, which come again.
	resume  *ResumePoint
	stream  string
	written uint64
	held    map[uint64]bool
	conn    *nats.Conn
	sub     *nats.Subscription
	stop    chan struct{}
	stopped sync.WaitGroup
	closed  bool
	// heartbeatEvery and progressEvery are HeartbeatEvery and progressEvery;
	// a test shortens them.
	heartbeatEvery time.Duration
	progressEvery  time.Duration
}

func newSession(listener *Listener, cfg *SessionConfig) (*Session, error) {
	if err := ValidSessionID(cfg.SessionID); err != nil {
		return nil, err
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	session := &Session{
		Listener:         listener,
		cfg:              *cfg,
		logger:           logger,
		clientInstanceID: randomID(),
		epoch:            randomID(),
		stop:             make(chan struct{}),
		heartbeatEvery:   HeartbeatEvery,
		progressEvery:    progressEvery,
	}
	if cfg.Resume != nil && cfg.Resume.StreamID != "" {
		resume := *cfg.Resume
		session.resume = &resume
		session.stream, session.written = resume.StreamID, resume.Seq
	}
	return session, nil
}

// OpenSession connects to NATS, subscribes to the session's relay subject,
// registers, and keeps registering every HeartbeatEvery until Close. It
// fails when the first registration fails: a session the listener refused
// receives nothing.
func OpenSession(ctx context.Context, listener *Listener, cfg *SessionConfig) (*Session, error) {
	session, err := newSession(listener, cfg)
	if err != nil {
		return nil, err
	}
	natsURL := cfg.NATSURL
	if natsURL == "" {
		if natsURL, err = NATSURLFor(listener.base.String()); err != nil {
			return nil, err
		}
	}
	conn, err := nats.Connect(natsURL,
		nats.Name("ghost-wispr "+cfg.SessionID),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
		nats.ReconnectHandler(func(*nats.Conn) {
			// A new connection is a new epoch, registered at once so the
			// listener relays to it.
			session.mu.Lock()
			session.epoch = randomID()
			session.mu.Unlock()
			go session.registerLogged()
		}),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				session.logger.Warn("envoy session lost its NATS connection", "error", err)
			}
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("envoy: connect to NATS at %s: %w", natsURL, err)
	}
	sub, err := conn.Subscribe(DeliverSubjectPrefix+cfg.SessionID, func(msg *nats.Msg) {
		session.handle(msg.Header, msg.Data, func(body string) {
			if err := msg.Respond([]byte(body)); err != nil {
				session.logger.Warn("envoy session could not answer a relay frame", "answer", body, "error", err)
			}
		})
	})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("envoy: subscribe to the relay subject: %w", err)
	}
	// The flush and the first registration bound themselves: NATS refuses a
	// flush on a context with no deadline, and a caller need not give one.
	openCtx, cancel := context.WithTimeout(ctx, openTimeout)
	defer cancel()
	if err := conn.FlushWithContext(openCtx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("envoy: flush the relay subscription: %w", err)
	}
	session.mu.Lock()
	session.conn, session.sub = conn, sub
	session.mu.Unlock()
	if err := session.register(openCtx); err != nil {
		conn.Close()
		return nil, err
	}
	session.stopped.Add(1)
	go session.heartbeat()
	return session, nil
}

// openTimeout bounds OpenSession's flush of the relay subscription and its
// first registration.
const openTimeout = 15 * time.Second

// ID is the session's id.
func (s *Session) ID() string { return s.cfg.SessionID }

func (s *Session) heartbeat() {
	defer s.stopped.Done()
	ticker := time.NewTicker(s.heartbeatEvery)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.registerLogged()
		}
	}
}

func (s *Session) registerLogged() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.register(ctx); err != nil {
		s.logger.Warn("envoy session registration failed; the next one is in a minute", "error", err)
	}
}

// register sends one registration and keeps the floor of the resume point
// its answer names.
func (s *Session) register(ctx context.Context) error {
	s.mu.Lock()
	delivery := map[string]any{"consumer": true, "epoch": s.epoch}
	if s.resume != nil {
		delivery["resume"] = map[string]any{"stream_id": s.resume.StreamID, "seq": s.resume.Seq}
	}
	s.mu.Unlock()
	body := map[string]any{
		"session_id":         s.cfg.SessionID,
		"dir":                s.cfg.Dir,
		"topics":             []string{},
		"title":              s.cfg.Title,
		"capabilities":       []string{},
		"host":               s.cfg.Host,
		"plugin":             s.cfg.Plugin,
		"plugin_version":     s.cfg.PluginVersion,
		"client_instance_id": s.clientInstanceID,
		"delivery":           delivery,
	}
	var answer struct {
		Delivery *struct {
			StreamID      string `json:"stream_id"`
			ConsumerStart uint64 `json:"consumer_start"`
			Gap           *bool  `json:"gap"`
		} `json:"delivery"`
	}
	if err := s.do(ctx, http.MethodPost, "/v1/interests/subscribe", nil, body, &answer); err != nil {
		return err
	}
	if answer.Delivery == nil || answer.Delivery.StreamID == "" {
		return nil
	}
	if answer.Delivery.Gap != nil && *answer.Delivery.Gap {
		s.logger.Warn("envoy says messages sent to this session while it was away are gone")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// An answer with no consumer start, an older listener's, says nothing
	// of where this session is in the stream, so it moves nothing.
	if answer.Delivery.ConsumerStart == 0 {
		return nil
	}
	floor := answer.Delivery.ConsumerStart - 1
	if s.switchStream(answer.Delivery.StreamID) || floor > s.written {
		s.written = max(s.written, floor)
		s.save()
	}
	return nil
}

// switchStream makes id the stream the session is in, and reports whether it
// was another: nothing of the old stream counts then. s.mu is held.
func (s *Session) switchStream(id string) bool {
	if s.stream == id {
		return false
	}
	s.stream, s.written, s.held = id, 0, nil
	return true
}

// save keeps the resume point when it changed: below the lowest frame held,
// else the highest written. s.mu is held.
func (s *Session) save() {
	point := ResumePoint{StreamID: s.stream, Seq: s.written}
	for seq := range s.held {
		point.Seq = min(point.Seq, seq-1)
	}
	if s.resume != nil && *s.resume == point {
		return
	}
	s.resume = &point
	if s.cfg.SaveResume == nil {
		return
	}
	if err := s.cfg.SaveResume(point); err != nil {
		s.logger.Warn("envoy session could not save its resume point", "error", err)
	}
}

// handle answers one relay message: a probe done at once, a frame that does
// not decode done (a redelivery cannot fix it), and a frame received, then
// progress every progressEvery while Deliver holds it, then done once Deliver
// wrote it or nak when it could not. respond is called from one goroutine at
// a time.
func (s *Session) handle(header nats.Header, data []byte, respond func(string)) {
	if header.Get("Envoy-Probe") == "1" {
		respond("done")
		return
	}
	streamID := header.Get("Envoy-Stream-Id")
	seq, err := strconv.ParseUint(header.Get("Envoy-Stream-Seq"), 10, 64)
	if err != nil || seq == 0 || streamID == "" || header.Get("Envoy-Subject") == "" {
		s.logger.Warn("envoy session dropped a relay frame with bad headers")
		respond("done")
		return
	}
	respond("received")
	var envelope struct {
		EventID        string `json:"event_id"`
		SourceSession  string `json:"source_session"`
		Topic          string `json:"topic"`
		InReplyTo      string `json:"in_reply_to"`
		Supersedes     string `json:"supersedes"`
		PayloadSummary string `json:"payload_summary"`
		Payload        string `json:"payload"`
		IssuedAt       int64  `json:"issued_at"`
		Sender         *struct {
			SessionID string `json:"session_id"`
		} `json:"sender"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil || envelope.EventID == "" {
		s.logger.Warn("envoy session dropped a relay frame that carries no envelope", "seq", seq)
		respond("done")
		s.wrote(streamID, seq)
		return
	}
	message := Message{
		EventID: envelope.EventID, SourceSession: envelope.SourceSession, Topic: envelope.Topic,
		InReplyTo: envelope.InReplyTo, Supersedes: envelope.Supersedes, Text: envelope.PayloadSummary,
	}
	if envelope.Payload != "" {
		message.Text = envelope.Payload
	}
	if message.SourceSession == "" && envelope.Sender != nil {
		message.SourceSession = envelope.Sender.SessionID
	}
	if envelope.IssuedAt > 0 {
		message.IssuedAt = time.UnixMilli(envelope.IssuedAt)
	}
	if s.cfg.Deliver != nil {
		ctx, cancel := context.WithTimeout(context.Background(), deliverTimeout)
		held, progressed := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(progressed)
			ticker := time.NewTicker(s.progressEvery)
			defer ticker.Stop()
			for {
				select {
				case <-held:
					return
				case <-ticker.C:
					respond("progress")
				}
			}
		}()
		err := s.cfg.Deliver(ctx, &message)
		close(held)
		<-progressed
		cancel()
		if err != nil {
			s.logger.Warn("envoy session could not take a message; it comes again", "event_id", message.EventID, "error", err)
			s.hold(streamID, seq)
			respond("nak")
			return
		}
	}
	respond("done")
	s.wrote(streamID, seq)
}

// hold records a frame answered nak: it comes again, so the resume point
// stays below it.
func (s *Session) hold(streamID string, seq uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.switchStream(streamID)
	if s.held == nil {
		s.held = map[uint64]bool{}
	}
	s.held[seq] = true
}

// wrote moves the resume point past a frame the session answered done.
func (s *Session) wrote(streamID string, seq uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.switchStream(streamID)
	delete(s.held, seq)
	s.written = max(s.written, seq)
	s.save()
}

// SendRequest is a message to one session that asks for a reply.
type SendRequest struct {
	Target  string
	Message string
	// NoReply sends a notice that asks no reply (expects_reply false).
	NoReply bool
	// IdempotencyKey names this message: a resend under it is dropped as a
	// repeat.
	IdempotencyKey string
	InReplyTo      string
	// Supersedes is the event id of an earlier message this one replaces.
	Supersedes string
}

// SendResult is the listener's answer to a send.
type SendResult struct {
	EventID        string
	RecipientState SessionState
	// State is connected or stopped: whether the recipient's process held
	// its subscription when the message was stored.
	State string
	// Duplicate: Envoy already held a message under the request's
	// idempotency key, so it stored nothing, and EventID names no message.
	Duplicate bool
}

// Send sends a message from this session that requires a reply, or, with
// NoReply, a notice that asks none.
func (s *Session) Send(ctx context.Context, request *SendRequest) (SendResult, error) {
	if err := ValidSessionID(request.Target); err != nil {
		return SendResult{}, err
	}
	body := map[string]any{
		"source":          "agent",
		"source_session":  s.cfg.SessionID,
		"target_session":  request.Target,
		"message":         request.Message,
		"idempotency_key": request.IdempotencyKey,
		"expects_reply":   "required",
		"urgency":         "high",
	}
	if request.NoReply {
		// The listener reads the JSON literal false, never the string.
		body["expects_reply"], body["urgency"] = false, "med"
	}
	if request.InReplyTo != "" {
		body["in_reply_to"] = request.InReplyTo
	}
	if request.Supersedes != "" {
		body["supersedes"] = request.Supersedes
	}
	var answer struct {
		EventID        string `json:"event_id"`
		RecipientState string `json:"recipient_state"`
		State          string `json:"state"`
		Duplicate      bool   `json:"duplicate"`
	}
	if err := s.do(ctx, http.MethodPost, "/v1/messages/send", nil, body, &answer); err != nil {
		return SendResult{}, err
	}
	if answer.EventID == "" {
		return SendResult{}, errors.New("envoy: the send answered no event id")
	}
	return SendResult{EventID: answer.EventID, RecipientState: SessionState(answer.RecipientState), State: answer.State, Duplicate: answer.Duplicate}, nil
}

// Close stops the heartbeat, removes this process's registration and closes
// the NATS connection.
func (s *Session) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.stop)
	conn := s.conn
	s.mu.Unlock()
	s.stopped.Wait()
	err := s.do(ctx, http.MethodDelete, "/v1/sessions/"+s.cfg.SessionID, url.Values{"client_instance_id": {s.clientInstanceID}}, nil, nil)
	if conn != nil {
		conn.Close()
	}
	return err
}

func randomID() string {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}

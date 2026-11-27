package call

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"
)

// The control messages' types, one per row of C2's table.
const (
	TypeHello     = "hello"      // both sides, the first message of a link
	TypeState     = "state"      // Pi
	TypePost      = "post"       // Pi
	TypeCancel    = "cancel"     // Pi
	TypePing      = "ping"       // Pi, every 250 ms
	TypePong      = "pong"       // bridge
	TypeCall      = "call"       // bridge
	TypeChatCheck = "chat_check" // bridge
	TypePause     = "pause"      // bridge
	TypeResume    = "resume"     // bridge
	TypeBusy      = "busy"       // bridge
	TypeRateFault = "rate_fault" // bridge
	TypeRoster    = "roster"     // bridge, at each gate opening
	TypeJoin      = "join"       // bridge
	TypeLeave     = "leave"      // bridge
	TypeChat      = "chat"       // bridge
)

// Texts is the Pi's hello's "texts" object: the bot's name and the keys of
// C5's call.notices, so the bridge holds no text of its own.
type Texts struct {
	BotName      string `json:"bot_name"`
	Consent      string `json:"consent"`
	PausedOpen   string `json:"paused_open"`
	PausedClosed string `json:"paused_closed"`
	StartsPaused string `json:"starts_paused"`
	StartsClosed string `json:"starts_closed"`
	ResumeHint   string `json:"resume_hint"`
	OfficeOnly   string `json:"office_only"`
	Resumed      string `json:"resumed"`
	Lost         string `json:"lost"`
}

// Event is C2's event object; its times are written in RFC 3339.
type Event struct {
	ID        string    `json:"id"`
	Organizer string    `json:"organizer"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
}

// State is the Pi's state: the body of a state message, and the Pi's hello's
// "state" object.
type State struct {
	State       string `json:"state"`        // recording | listening | closed
	Closings    int    `json:"closings"`     // the capture gate's count of closings
	PauseSource string `json:"pause_source"` // none | office_control | office_close | chat
	HearsRoom   bool   `json:"hears_room"`
}

// Hello is each side's first message. A hello that sets any of the Pi's
// fields (Texts, JitterTargetMS, State) is the Pi's: it is written with
// build, link_versions, texts (null when Texts is nil), jitter_target_ms and
// state, which it must have. Any other hello is the bridge's: it is written
// with build, link_versions, paused, pause_chat_time, event (null when Event
// is nil) and bot_state. MarshalJSON refuses a hello that sets fields of both
// sides, so neither side's fields are dropped unseen. Each hello names its
// build and at least one link version, each from 1. One that names a version
// this build accepts holds that version's values; one that names none is
// checked, and read, for its build and its versions alone.
type Hello struct {
	Build        string // BuildRelease | BuildCallfault
	LinkVersions []int
	// The Pi's hello:
	Texts          *Texts
	JitterTargetMS int
	State          *State
	// The bridge's hello:
	Paused        bool
	PauseChatTime string // RFC 3339, or "" when not paused from the chat
	Event         *Event // JSON null when none
	BotState      string
}

// helloWire reads either side's hello. Its fields are Hello's, so a *helloWire
// converts to a *Hello.
type helloWire struct {
	Build          string `json:"build"`
	LinkVersions   []int  `json:"link_versions"`
	Texts          *Texts `json:"texts"`
	JitterTargetMS int    `json:"jitter_target_ms"`
	State          *State `json:"state"`
	Paused         bool   `json:"paused"`
	PauseChatTime  string `json:"pause_chat_time"`
	Event          *Event `json:"event"`
	BotState       string `json:"bot_state"`
}

type piHello struct {
	Build          string `json:"build"`
	LinkVersions   []int  `json:"link_versions"`
	Texts          *Texts `json:"texts"`
	JitterTargetMS int    `json:"jitter_target_ms"`
	State          *State `json:"state"`
}

type bridgeHello struct {
	Build         string `json:"build"`
	LinkVersions  []int  `json:"link_versions"`
	Paused        bool   `json:"paused"`
	PauseChatTime string `json:"pause_chat_time"`
	Event         *Event `json:"event"`
	BotState      string `json:"bot_state"`
}

// helloVersions is what a side reads of a hello that names no link version
// it accepts.
type helloVersions struct {
	Build        string `json:"build"`
	LinkVersions []int  `json:"link_versions"`
}

// Post asks the bridge to post a notice in the chat.
type Post struct {
	Key string `json:"key"` // starts_paused | starts_closed
}

// Cancel stops an input's frames up to a cancel generation.
type Cancel struct {
	Input      string `json:"input"` // voice | chime
	Generation uint16 `json:"generation"`
}

// Ping is the Pi's keepalive.
type Ping struct {
	Seq       uint64 `json:"seq"`
	PiClockUS int64  `json:"pi_clock_us"`
}

// Pong answers a ping, and gives the bridge's pause state.
type Pong struct {
	Seq           uint64 `json:"seq"`
	PiClockUS     int64  `json:"pi_clock_us"` // the ping's, echoed
	BridgeClockUS int64  `json:"bridge_clock_us"`
	Paused        bool   `json:"paused"`
	PauseChatTime string `json:"pause_chat_time"`
}

// Call gives the call's event and the bot's state.
type Call struct {
	Event    Event  `json:"event"`
	BotState string `json:"bot_state"` // joining | admitted | in_call | left
}

// ChatCheck is the chat check's result for an event.
type ChatCheck struct {
	Result string `json:"result"` // passed | failed
	Event  string `json:"event"`  // the event's id
}

// ChatTime is the body of pause and resume: the time of the chat line.
type ChatTime struct {
	ChatTime string `json:"chat_time"`
}

// Busy names an event whose end passed while another call held the bot.
type Busy struct {
	Event Event `json:"event"`
}

// RateFault reports the sample rate the bridge counted on Attendee's socket.
type RateFault struct {
	SamplesPerSecond int `json:"samples_per_second"`
}

// Participant is one remote participant: the body of join and leave, and an
// item of a roster.
type Participant struct {
	Index uint8  `json:"index"` // the index of its frames on StreamParticipant
	ID    string `json:"id"`    // Meet's participant id
	Name  string `json:"name"`
}

// Roster is the call's participants at a gate opening. A nil list is written
// as an empty one.
type Roster struct {
	Participants []Participant `json:"participants"`
}

// Chat is one chat line of the call.
type Chat struct {
	SenderID string `json:"sender_id"`
	Text     string `json:"text"`
	ChatTime string `json:"chat_time"`
}

// Control is one control message: Type names its row, and exactly that row's
// field is set.
type Control struct {
	Type      string
	Hello     *Hello
	State     *State
	Post      *Post
	Cancel    *Cancel
	Ping      *Ping
	Pong      *Pong
	Call      *Call
	ChatCheck *ChatCheck
	Pause     *ChatTime
	Resume    *ChatTime
	Busy      *Busy
	RateFault *RateFault
	Roster    *Roster
	Join      *Participant
	Leave     *Participant
	Chat      *Chat
}

// row returns the value of c's row, after it checks that Type names a row,
// that its field, and no other, is set, and that the row holds values C2's
// table allows (check).
func (c *Control) row() (any, error) {
	rows := [...]struct {
		typ string
		set bool
		v   checker
	}{
		{TypeHello, c.Hello != nil, c.Hello},
		{TypeState, c.State != nil, c.State},
		{TypePost, c.Post != nil, c.Post},
		{TypeCancel, c.Cancel != nil, c.Cancel},
		{TypePing, c.Ping != nil, c.Ping},
		{TypePong, c.Pong != nil, c.Pong},
		{TypeCall, c.Call != nil, c.Call},
		{TypeChatCheck, c.ChatCheck != nil, c.ChatCheck},
		{TypePause, c.Pause != nil, c.Pause},
		{TypeResume, c.Resume != nil, c.Resume},
		{TypeBusy, c.Busy != nil, c.Busy},
		{TypeRateFault, c.RateFault != nil, c.RateFault},
		{TypeRoster, c.Roster != nil, c.Roster},
		{TypeJoin, c.Join != nil, c.Join},
		{TypeLeave, c.Leave != nil, c.Leave},
		{TypeChat, c.Chat != nil, c.Chat},
	}
	var row checker
	set := 0
	for _, r := range &rows {
		if r.set {
			set++
		}
		if r.typ == c.Type {
			if !r.set {
				return nil, fmt.Errorf("call: a %q control without its row", c.Type)
			}
			row = r.v
		}
	}
	if row == nil {
		return nil, fmt.Errorf("call: unknown control type %q", c.Type)
	}
	if set != 1 {
		return nil, fmt.Errorf("call: a %q control with %d rows set; want its own alone", c.Type, set)
	}
	if err := row.check(); err != nil {
		return nil, fmt.Errorf("call: %s control: %w", c.Type, err)
	}
	return row, nil
}

// MarshalJSON writes c as one flat JSON object: "type", then its row's fields
// under C2's names, zero values included. It refuses a row with a value C2's
// table does not allow, so a side writes nothing that ParseControl refuses.
//
//nolint:gocritic // hugeParam: the contract passes Control by value; json.Marshal of a value must use MarshalJSON.
func (c Control) MarshalJSON() ([]byte, error) {
	row, err := c.row()
	if err != nil {
		return nil, err
	}
	switch c.Type {
	case TypeHello:
		row = c.Hello.wire()
	case TypeRoster:
		if c.Roster.Participants == nil {
			row = Roster{Participants: []Participant{}}
		}
	}
	body, err := json.Marshal(row)
	if err != nil {
		return nil, err
	}
	// body is a JSON object of at least one field, as every row has fields.
	// "type" goes in as its first field; Type is one of the constants above,
	// which need no escaping.
	out := make([]byte, 0, len(`{"type":"",`)+len(c.Type)+len(body)-1)
	out = append(out, `{"type":"`...)
	out = append(out, c.Type...)
	out = append(out, `",`...)
	return append(out, body[1:]...), nil
}

// sides says whether h sets any of the Pi's fields, and any of the bridge's.
func (h *Hello) sides() (pi, bridge bool) {
	return h.Texts != nil || h.JitterTargetMS != 0 || h.State != nil,
		h.Paused || h.PauseChatTime != "" || h.Event != nil || h.BotState != ""
}

// wire returns the side's form of h, a hello that check found to be one
// side's.
func (h *Hello) wire() any {
	if pi, _ := h.sides(); pi {
		return piHello{
			Build: h.Build, LinkVersions: h.LinkVersions,
			Texts: h.Texts, JitterTargetMS: h.JitterTargetMS, State: h.State,
		}
	}
	return bridgeHello{
		Build: h.Build, LinkVersions: h.LinkVersions,
		Paused: h.Paused, PauseChatTime: h.PauseChatTime, Event: h.Event, BotState: h.BotState,
	}
}

// ParseControl reads one control message. It refuses an unknown type, a
// body that does not decode into its row, a row that lacks one of its fields
// or holds null for one (a pointer's may be null), and a row with a value
// C2's table does not allow. It takes fields it does not know, and reads a
// hello that names no link version this build accepts for its build and its
// versions alone, so a side still reads the hello of a later link version
// and learns which versions the other side accepts.
func ParseControl(b []byte) (Control, error) {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(b, &head); err != nil {
		return Control{}, fmt.Errorf("call: control message: %w", err)
	}
	c := Control{Type: head.Type}
	var err error
	switch head.Type {
	case TypeHello:
		c.Hello, err = parseHello(b)
	case TypeState:
		c.State, err = decodeRow[State](b)
	case TypePost:
		c.Post, err = decodeRow[Post](b)
	case TypeCancel:
		c.Cancel, err = decodeRow[Cancel](b)
	case TypePing:
		c.Ping, err = decodeRow[Ping](b)
	case TypePong:
		c.Pong, err = decodeRow[Pong](b)
	case TypeCall:
		c.Call, err = decodeRow[Call](b)
	case TypeChatCheck:
		c.ChatCheck, err = decodeRow[ChatCheck](b)
	case TypePause:
		c.Pause, err = decodeRow[ChatTime](b)
	case TypeResume:
		c.Resume, err = decodeRow[ChatTime](b)
	case TypeBusy:
		c.Busy, err = decodeRow[Busy](b)
	case TypeRateFault:
		c.RateFault, err = decodeRow[RateFault](b)
	case TypeRoster:
		c.Roster, err = decodeRow[Roster](b)
	case TypeJoin:
		c.Join, err = decodeRow[Participant](b)
	case TypeLeave:
		c.Leave, err = decodeRow[Participant](b)
	case TypeChat:
		c.Chat, err = decodeRow[Chat](b)
	default:
		return Control{}, fmt.Errorf("call: unknown control type %q", head.Type)
	}
	if err != nil {
		return Control{}, fmt.Errorf("call: %s control: %w", head.Type, err)
	}
	if _, err := c.row(); err != nil {
		return Control{}, err
	}
	return c, nil
}

// parseHello reads a hello. One that names a link version this build
// accepts is that version's, with each field of its side's hello. One that
// names none is read for its build and versions alone, which is all this
// side needs of it, whatever the later version changed of the rest.
func parseHello(b []byte) (*Hello, error) {
	v, err := decodeRow[helloVersions](b)
	if err != nil {
		return nil, err
	}
	if _, ok := Choose(Accepted, v.LinkVersions); !ok {
		return &Hello{Build: v.Build, LinkVersions: v.LinkVersions}, nil
	}
	w := new(helloWire)
	if err := json.Unmarshal(b, w); err != nil {
		return nil, err
	}
	h := (*Hello)(w)
	if err := requireFields(b, reflect.TypeOf(h.wire())); err != nil {
		return nil, err
	}
	return h, nil
}

// decodeRow decodes a flat control message into its row, which must have
// each of its fields; the "type" field is not a field of any row, so it is
// passed over.
func decodeRow[T any](b []byte) (*T, error) {
	v := new(T)
	if err := json.Unmarshal(b, v); err != nil {
		return nil, err
	}
	if err := requireFields(b, reflect.TypeFor[T]()); err != nil {
		return nil, err
	}
	return v, nil
}

// unmarshaler is the type of a value that reads its own JSON, as a
// time.Time does: requireFields does not look inside one.
var unmarshaler = reflect.TypeFor[json.Unmarshaler]()

// requireFields checks that raw, JSON that decoded as a t, has each field of
// t: each field of an object, of each object it holds, and of each object in
// a list, none of them null unless its Go field is a pointer, which C2 writes
// as null when there is none. json.Unmarshal leaves a field it does not find
// at its zero value, which C2 gives a meaning: a pong without paused would
// read as not paused.
func requireFields(raw json.RawMessage, t reflect.Type) error {
	if reflect.PointerTo(t).Implements(unmarshaler) {
		return nil
	}
	switch t.Kind() {
	case reflect.Pointer:
		if string(raw) == "null" {
			return nil
		}
		return requireFields(raw, t.Elem())
	case reflect.Slice:
		if k := t.Elem().Kind(); k != reflect.Struct && k != reflect.Pointer && k != reflect.Slice {
			return nil
		}
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return err
		}
		for i, item := range items {
			if err := requireFields(item, t.Elem()); err != nil {
				return fmt.Errorf("item %d: %w", i, err)
			}
		}
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return err
		}
		for i := range t.NumField() {
			f := t.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			v, ok := fields[name]
			switch {
			case !ok:
				return fmt.Errorf("no %s", name)
			case string(v) == "null" && f.Type.Kind() != reflect.Pointer:
				return fmt.Errorf("%s is null", name)
			}
			if err := requireFields(v, f.Type); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
	}
	return nil
}

// A checker is a row of C2's table: check says whether each of its values is
// one the table allows, each enum's, each count's bound, each time's form,
// and each id that names something.
type checker interface{ check() error }

// The values of C2's enums.
var (
	builds       = []string{BuildRelease, BuildCallfault}
	gateStates   = []string{"recording", "listening", "closed"}
	pauseSources = []string{"none", "office_control", "office_close", "chat"}
	postKeys     = []string{"starts_paused", "starts_closed"}
	inputs       = []string{"voice", "chime"}
	botStates    = []string{"joining", "admitted", "in_call", "left"}
	results      = []string{"passed", "failed"}
)

// oneOf checks that field holds one of values.
func oneOf(field, v string, values []string) error {
	if slices.Contains(values, v) {
		return nil
	}
	return fmt.Errorf("%s %q is not one of %s", field, v, strings.Join(values, ", "))
}

// rfc3339 checks that field holds an RFC 3339 time.
func rfc3339(field, v string) error {
	if _, err := time.Parse(time.RFC3339, v); err != nil {
		return fmt.Errorf("%s %q is not an RFC 3339 time", field, v)
	}
	return nil
}

// check checks a hello: each hello's build and versions, that it holds one
// side's fields, and, when it names a version this build accepts, the
// values of that side's fields.
func (h *Hello) check() error {
	pi, bridge := h.sides()
	switch {
	case pi && bridge:
		return errors.New("a hello with both the Pi's and the bridge's fields")
	case pi && h.State == nil:
		return errors.New("the Pi's hello has no state")
	case len(h.LinkVersions) == 0:
		return errors.New("a hello names the link versions its side accepts")
	}
	if err := oneOf("build", h.Build, builds); err != nil {
		return err
	}
	for _, v := range h.LinkVersions {
		if v < 1 {
			return fmt.Errorf("link version %d is not a whole number from 1", v)
		}
	}
	if _, ok := Choose(Accepted, h.LinkVersions); !ok {
		return nil // a later version's hello: this side reads its versions alone
	}
	if pi {
		if h.JitterTargetMS <= 0 {
			return fmt.Errorf("jitter_target_ms %d is not above 0", h.JitterTargetMS)
		}
		if err := h.State.check(); err != nil {
			return fmt.Errorf("state: %w", err)
		}
		return nil
	}
	if h.PauseChatTime != "" {
		if err := rfc3339("pause_chat_time", h.PauseChatTime); err != nil {
			return err
		}
	}
	if h.Event != nil {
		if err := h.Event.check(); err != nil {
			return fmt.Errorf("event: %w", err)
		}
	}
	if h.BotState != "" {
		return oneOf("bot_state", h.BotState, botStates)
	}
	return nil
}

func (s *State) check() error {
	if err := oneOf("state", s.State, gateStates); err != nil {
		return err
	}
	if s.Closings < 0 {
		return fmt.Errorf("closings %d is below 0", s.Closings)
	}
	return oneOf("pause_source", s.PauseSource, pauseSources)
}

func (p *Post) check() error { return oneOf("key", p.Key, postKeys) }

func (c *Cancel) check() error { return oneOf("input", c.Input, inputs) }

// check allows every ping: C2 bounds neither its seq nor its clock.
func (*Ping) check() error { return nil }

func (p *Pong) check() error {
	if p.PauseChatTime == "" {
		return nil
	}
	return rfc3339("pause_chat_time", p.PauseChatTime)
}

func (c *Call) check() error {
	if err := c.Event.check(); err != nil {
		return fmt.Errorf("event: %w", err)
	}
	return oneOf("bot_state", c.BotState, botStates)
}

func (c *ChatCheck) check() error {
	if err := oneOf("result", c.Result, results); err != nil {
		return err
	}
	if c.Event == "" {
		return errors.New("a chat_check that names no event")
	}
	return nil
}

func (c *ChatTime) check() error { return rfc3339("chat_time", c.ChatTime) }

func (b *Busy) check() error {
	if err := b.Event.check(); err != nil {
		return fmt.Errorf("event: %w", err)
	}
	return nil
}

func (r *RateFault) check() error {
	if r.SamplesPerSecond < 0 {
		return fmt.Errorf("samples_per_second %d is below 0", r.SamplesPerSecond)
	}
	return nil
}

// check checks each participant, and that no two share an index, which maps
// a participant's frames to its id.
func (r *Roster) check() error {
	var taken [256]bool
	for i := range r.Participants {
		p := &r.Participants[i]
		if err := p.check(); err != nil {
			return fmt.Errorf("participant %d: %w", i, err)
		}
		if taken[p.Index] {
			return fmt.Errorf("participant %d: index %d is another participant's", i, p.Index)
		}
		taken[p.Index] = true
	}
	return nil
}

func (p *Participant) check() error {
	if p.ID == "" {
		return errors.New("a participant with no id")
	}
	return nil
}

func (c *Chat) check() error {
	if c.SenderID == "" {
		return errors.New("a chat line with no sender_id")
	}
	return rfc3339("chat_time", c.ChatTime)
}

// check checks an event: its id and organizer, which name it, and a start
// and an end, the end not before the start.
func (e *Event) check() error {
	switch {
	case e.ID == "":
		return errors.New("an event with no id")
	case e.Organizer == "":
		return errors.New("an event with no organizer")
	case e.Start.IsZero() || e.End.IsZero():
		return errors.New("an event with no start or no end")
	case e.End.Before(e.Start):
		return errors.New("an event that ends before it starts")
	}
	return nil
}

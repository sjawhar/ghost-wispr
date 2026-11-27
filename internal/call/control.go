package call

import (
	"encoding/json"
	"errors"
	"fmt"
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
// sides, so neither side's fields are dropped unseen.
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

// row returns the value of c's row, after it checks that Type names a row and
// that its field, and no other, is set.
func (c *Control) row() (any, error) {
	rows := [...]struct {
		typ string
		set bool
		v   any
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
	var row any
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
	return row, nil
}

// MarshalJSON writes c as one flat JSON object: "type", then its row's fields
// under C2's names, zero values included.
//
//nolint:gocritic // hugeParam: the contract passes Control by value; json.Marshal of a value must use MarshalJSON.
func (c Control) MarshalJSON() ([]byte, error) {
	row, err := c.row()
	if err != nil {
		return nil, err
	}
	switch c.Type {
	case TypeHello:
		if row, err = c.Hello.wire(); err != nil {
			return nil, err
		}
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

// wire returns the side's form of h.
func (h *Hello) wire() (any, error) {
	pi := h.Texts != nil || h.JitterTargetMS != 0 || h.State != nil
	bridge := h.Paused || h.PauseChatTime != "" || h.Event != nil || h.BotState != ""
	switch {
	case pi && bridge:
		return nil, errors.New("call: a hello with both the Pi's and the bridge's fields")
	case !pi:
		return bridgeHello{
			Build: h.Build, LinkVersions: h.LinkVersions,
			Paused: h.Paused, PauseChatTime: h.PauseChatTime, Event: h.Event, BotState: h.BotState,
		}, nil
	case h.State == nil:
		return nil, errors.New("call: the Pi's hello has no state")
	}
	return piHello{
		Build: h.Build, LinkVersions: h.LinkVersions,
		Texts: h.Texts, JitterTargetMS: h.JitterTargetMS, State: h.State,
	}, nil
}

// ParseControl reads one control message. It refuses an unknown type and a
// body that does not decode into its row. It takes fields it does not know,
// so a side still reads the hello of a later link version and learns which
// versions the other side accepts.
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
		var w *helloWire
		w, err = decodeRow[helloWire](b)
		c.Hello = (*Hello)(w)
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
	return c, nil
}

// decodeRow decodes a flat control message into its row; the "type" field is
// not a field of any row, so it is passed over.
func decodeRow[T any](b []byte) (*T, error) {
	v := new(T)
	if err := json.Unmarshal(b, v); err != nil {
		return nil, err
	}
	return v, nil
}

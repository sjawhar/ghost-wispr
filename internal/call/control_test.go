package call

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// decodeExact decodes JSON into maps, slices and json.Numbers, so two
// documents compare by their fields and exact numbers, not by key order.
func decodeExact(t *testing.T, b []byte) any {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return v
}

var (
	testEvent = Event{
		ID:        "evt-0001",
		Organizer: "lead@example.com",
		Start:     time.Date(2026, 11, 26, 15, 0, 0, 0, time.UTC),
		End:       time.Date(2026, 11, 26, 16, 0, 0, 0, time.UTC),
	}
	testEventJSON = `{"id":"evt-0001","organizer":"lead@example.com","start":"2026-11-26T15:00:00Z","end":"2026-11-26T16:00:00Z"}`
)

// TestEachControlRowRoundTrips holds C2's table: each row's control message is
// one flat JSON object, its "type" and exactly that row's fields under C2's
// names, zero values included; the Pi's hello and the bridge's hello each carry
// only their own side's fields; and ParseControl gives back the control that
// was written. It fails if a field is renamed, dropped by omitempty, added to
// the wrong side's hello, or nested where C2 has it flat.
func TestEachControlRowRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    Control
		want string
		back *Control // what ParseControl gives, when not c itself
	}{
		{
			name: "the Pi's hello",
			c: Control{Type: TypeHello, Hello: &Hello{
				Build: BuildRelease, LinkVersions: []int{1},
				Texts: &Texts{
					BotName: "Ghost Wispr test (recording)", Consent: "consent text", PausedOpen: "paused open text",
					PausedClosed: "paused closed text", StartsPaused: "starts paused text", StartsClosed: "starts closed text",
					ResumeHint: "resume hint text", OfficeOnly: "office only text", Resumed: "resumed text", Lost: "lost text",
				},
				JitterTargetMS: 80,
				State:          &State{State: "recording", Closings: 3, PauseSource: "none", HearsRoom: true},
			}},
			want: `{"type":"hello","build":"release","link_versions":[1],
				"texts":{"bot_name":"Ghost Wispr test (recording)","consent":"consent text","paused_open":"paused open text",
					"paused_closed":"paused closed text","starts_paused":"starts paused text","starts_closed":"starts closed text",
					"resume_hint":"resume hint text","office_only":"office only text","resumed":"resumed text","lost":"lost text"},
				"jitter_target_ms":80,
				"state":{"state":"recording","closings":3,"pause_source":"none","hears_room":true}}`,
		},
		{
			name: "the Pi's hello before its texts",
			c: Control{Type: TypeHello, Hello: &Hello{
				Build: BuildRelease, LinkVersions: []int{1}, JitterTargetMS: 80,
				State: &State{State: "listening", PauseSource: "office_control"},
			}},
			want: `{"type":"hello","build":"release","link_versions":[1],"texts":null,"jitter_target_ms":80,
				"state":{"state":"listening","closings":0,"pause_source":"office_control","hears_room":false}}`,
		},
		{
			name: "the bridge's hello with no event",
			c:    Control{Type: TypeHello, Hello: &Hello{Build: BuildCallfault, LinkVersions: []int{1, 2}}},
			want: `{"type":"hello","build":"callfault","link_versions":[1,2],"paused":false,"pause_chat_time":"","event":null,"bot_state":""}`,
		},
		{
			name: "the bridge's hello in a paused call",
			c: Control{Type: TypeHello, Hello: &Hello{
				Build: BuildRelease, LinkVersions: []int{1}, Paused: true, PauseChatTime: "2026-11-26T15:04:05Z",
				Event: &testEvent, BotState: "in_call",
			}},
			want: `{"type":"hello","build":"release","link_versions":[1],"paused":true,"pause_chat_time":"2026-11-26T15:04:05Z",
				"event":` + testEventJSON + `,"bot_state":"in_call"}`,
		},
		{
			name: "state",
			c:    Control{Type: TypeState, State: &State{State: "listening", Closings: 0, PauseSource: "office_control", HearsRoom: false}},
			want: `{"type":"state","state":"listening","closings":0,"pause_source":"office_control","hears_room":false}`,
		},
		{
			name: "post",
			c:    Control{Type: TypePost, Post: &Post{Key: "starts_closed"}},
			want: `{"type":"post","key":"starts_closed"}`,
		},
		{
			name: "cancel",
			c:    Control{Type: TypeCancel, Cancel: &Cancel{Input: "chime", Generation: 65535}},
			want: `{"type":"cancel","input":"chime","generation":65535}`,
		},
		{
			name: "ping at its extremes",
			c:    Control{Type: TypePing, Ping: &Ping{Seq: 18446744073709551615, PiClockUS: -9223372036854775808}},
			want: `{"type":"ping","seq":18446744073709551615,"pi_clock_us":-9223372036854775808}`,
		},
		{
			name: "pong",
			c: Control{Type: TypePong, Pong: &Pong{
				Seq: 0, PiClockUS: 9223372036854775807, BridgeClockUS: 1_790_000_000_123_456, Paused: false, PauseChatTime: "",
			}},
			want: `{"type":"pong","seq":0,"pi_clock_us":9223372036854775807,"bridge_clock_us":1790000000123456,"paused":false,"pause_chat_time":""}`,
		},
		{
			name: "call",
			c:    Control{Type: TypeCall, Call: &Call{Event: testEvent, BotState: "joining"}},
			want: `{"type":"call","event":` + testEventJSON + `,"bot_state":"joining"}`,
		},
		{
			name: "chat_check",
			c:    Control{Type: TypeChatCheck, ChatCheck: &ChatCheck{Result: "failed", Event: "evt-0001"}},
			want: `{"type":"chat_check","result":"failed","event":"evt-0001"}`,
		},
		{
			name: "pause",
			c:    Control{Type: TypePause, Pause: &ChatTime{ChatTime: "2026-11-26T15:10:00.123Z"}},
			want: `{"type":"pause","chat_time":"2026-11-26T15:10:00.123Z"}`,
		},
		{
			name: "resume",
			c:    Control{Type: TypeResume, Resume: &ChatTime{ChatTime: "2026-11-26T15:12:00Z"}},
			want: `{"type":"resume","chat_time":"2026-11-26T15:12:00Z"}`,
		},
		{
			name: "busy",
			c:    Control{Type: TypeBusy, Busy: &Busy{Event: testEvent}},
			want: `{"type":"busy","event":` + testEventJSON + `}`,
		},
		{
			name: "rate_fault",
			c:    Control{Type: TypeRateFault, RateFault: &RateFault{SamplesPerSecond: 22050}},
			want: `{"type":"rate_fault","samples_per_second":22050}`,
		},
		{
			name: "roster",
			c: Control{Type: TypeRoster, Roster: &Roster{Participants: []Participant{
				{Index: 0, ID: "spaces/a/devices/1", Name: "Ada"},
				{Index: 255, ID: "spaces/a/devices/2", Name: `Zoë <Ops> & "Co"`},
			}}},
			want: `{"type":"roster","participants":[{"index":0,"id":"spaces/a/devices/1","name":"Ada"},
				{"index":255,"id":"spaces/a/devices/2","name":"Zoë <Ops> & \"Co\""}]}`,
		},
		{
			name: "an empty roster is an empty list",
			c:    Control{Type: TypeRoster, Roster: &Roster{}},
			want: `{"type":"roster","participants":[]}`,
			back: &Control{Type: TypeRoster, Roster: &Roster{Participants: []Participant{}}},
		},
		{
			name: "join",
			c:    Control{Type: TypeJoin, Join: &Participant{Index: 3, ID: "spaces/a/devices/3", Name: "Grace"}},
			want: `{"type":"join","index":3,"id":"spaces/a/devices/3","name":"Grace"}`,
		},
		{
			name: "leave",
			c:    Control{Type: TypeLeave, Leave: &Participant{Index: 3, ID: "spaces/a/devices/3", Name: "Grace"}},
			want: `{"type":"leave","index":3,"id":"spaces/a/devices/3","name":"Grace"}`,
		},
		{
			name: "chat",
			c: Control{Type: TypeChat, Chat: &Chat{
				SenderID: "spaces/a/devices/3", Text: "wispr pause — «ok» <b>&</b>\u2028next", ChatTime: "2026-11-26T15:10:00Z",
			}},
			want: `{"type":"chat","sender_id":"spaces/a/devices/3","text":"wispr pause — «ok» <b>&</b>\u2028next","chat_time":"2026-11-26T15:10:00Z"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.c.MarshalJSON()
			if err != nil {
				t.Fatalf("MarshalJSON: %v", err)
			}
			if !reflect.DeepEqual(decodeExact(t, got), decodeExact(t, []byte(tc.want))) {
				t.Errorf("MarshalJSON wrote\n%s\nwant\n%s", got, tc.want)
			}
			viaJSON, err := json.Marshal(tc.c)
			if err != nil || !bytes.Equal(viaJSON, got) {
				t.Errorf("json.Marshal wrote %s, %v; want MarshalJSON's %s", viaJSON, err, got)
			}
			back, err := ParseControl(got)
			if err != nil {
				t.Fatalf("ParseControl(%s): %v", got, err)
			}
			want := tc.c
			if tc.back != nil {
				want = *tc.back
			}
			if !reflect.DeepEqual(back, want) {
				t.Errorf("ParseControl gave back\n%+v\nwant\n%+v", back, want)
			}
		})
	}
}

// TestParseControlRefusesAnUnknownType holds that a message whose type is not
// one of C2's rows is refused rather than passed on as an empty control. It
// fails if ParseControl returns a Control for a type it has no row for.
func TestParseControlRefusesAnUnknownType(t *testing.T) {
	for _, b := range []string{
		`{"type":"telemetry","seq":1}`,
		`{"type":"HELLO","build":"release","link_versions":[1]}`,
		`{"type":"","seq":1}`,
		`{"seq":1,"pi_clock_us":2}`,
	} {
		if c, err := ParseControl([]byte(b)); err == nil {
			t.Errorf("ParseControl(%s) = %+v, nil; want an error", b, c)
		}
	}
	// The control: a known type with the same fields is taken.
	if _, err := ParseControl([]byte(`{"type":"ping","seq":1,"pi_clock_us":2}`)); err != nil {
		t.Errorf("ParseControl of a ping: %v", err)
	}
}

// TestParseControlRefusesABodyThatDoesNotDecode holds that a known type whose
// fields have the wrong JSON types, or a message that is not a JSON object, is
// refused. It fails if ParseControl ignores the decode error of the row.
func TestParseControlRefusesABodyThatDoesNotDecode(t *testing.T) {
	for _, b := range []string{
		`not json`,
		`null`,
		`[{"type":"ping"}]`,
		`{"type":"ping","seq":"one","pi_clock_us":2}`,
		`{"type":"ping","seq":-1,"pi_clock_us":2}`,
		`{"type":"cancel","input":"voice","generation":65536}`,
		`{"type":"join","index":256,"id":"p","name":"n"}`,
		`{"type":"roster","participants":{"index":0}}`,
		`{"type":"hello","build":"release","link_versions":"1"}`,
		`{"type":"call","event":{"id":"e","start":"yesterday"},"bot_state":"joining"}`,
		`{"type":"state","state":"recording","closings":1,"pause_source":"none","hears_room":"yes"}`,
	} {
		if c, err := ParseControl([]byte(b)); err == nil {
			t.Errorf("ParseControl(%s) = %+v, nil; want an error", b, c)
		}
	}
}

// TestAHelloOfALaterLinkVersionStillParses holds R15's upgrade path: a side
// reads the other side's hello even when a later link version added fields to
// it, so it learns the other side's versions and can report that none is
// common. It fails if ParseControl refuses fields it does not know.
func TestAHelloOfALaterLinkVersionStillParses(t *testing.T) {
	c, err := ParseControl([]byte(`{"type":"hello","build":"release","link_versions":[2,3],"codec":"opus",
		"paused":false,"pause_chat_time":"","event":null,"bot_state":"","resume_token":"x"}`))
	if err != nil {
		t.Fatalf("ParseControl of a later version's hello: %v", err)
	}
	if c.Type != TypeHello || c.Hello == nil || !reflect.DeepEqual(c.Hello.LinkVersions, []int{2, 3}) {
		t.Fatalf("ParseControl gave %+v; want a hello naming link versions [2 3]", c)
	}
	if _, ok := Choose(Accepted, c.Hello.LinkVersions); ok {
		t.Errorf("Choose(%v, [2 3]) found a common version; this test needs a hello this build cannot link with", Accepted)
	}
}

// TestAControlWithoutExactlyItsRowIsNotWritten holds Control's rule that Type
// names its row and exactly that row's field is set, that the Pi's hello
// carries its state, and that a hello carries one side's fields alone. It
// fails if MarshalJSON writes a message with no fields for its type, drops a
// second row or the other side's hello fields silently, or writes an unknown
// type.
func TestAControlWithoutExactlyItsRowIsNotWritten(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    Control
	}{
		{"no type", Control{}},
		{"a type with no row", Control{Type: TypeState}},
		{"a hello with no row", Control{Type: TypeHello}},
		{"a second row", Control{Type: TypeState, State: &State{State: "recording"}, Ping: &Ping{Seq: 1}}},
		{"the row of another type", Control{Type: TypePong, Ping: &Ping{Seq: 1}}},
		{"an unknown type", Control{Type: "telemetry", State: &State{State: "recording"}}},
		{"the Pi's hello with no state", Control{Type: TypeHello, Hello: &Hello{
			Build: BuildRelease, LinkVersions: []int{1}, Texts: &Texts{BotName: "Ghost Wispr test (recording)"}, JitterTargetMS: 80,
		}}},
		{"the Pi's hello with the bridge's event", Control{Type: TypeHello, Hello: &Hello{
			Build: BuildRelease, LinkVersions: []int{1}, Texts: &Texts{BotName: "Ghost Wispr test (recording)"},
			State: &State{State: "recording"}, Event: &testEvent,
		}}},
		{"the bridge's hello with the Pi's state", Control{Type: TypeHello, Hello: &Hello{
			Build: BuildRelease, LinkVersions: []int{1}, BotState: "joining", State: &State{State: "recording"},
		}}},
		{"the bridge's hello with the Pi's jitter target", Control{Type: TypeHello, Hello: &Hello{
			Build: BuildRelease, LinkVersions: []int{1}, Paused: true, JitterTargetMS: 80,
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if b, err := tc.c.MarshalJSON(); err == nil {
				t.Errorf("MarshalJSON wrote %s; want an error", b)
			}
		})
	}
}

// From Sol's review of PR 202 (TestReviewControlRejectsMalformedKnownRows):
// a known row that decodes but breaks C2's table is refused, and a valid one,
// the control, is taken.
func TestParseControlRefusesAKnownRowOutsideC2(t *testing.T) {
	if _, err := ParseControl([]byte(`{"type":"state","state":"listening","closings":0,"pause_source":"chat","hears_room":false}`)); err != nil {
		t.Fatalf("valid state control refused: %v", err)
	}
	for _, body := range []string{
		`{"type":"state"}`,
		`{"type":"state","state":"unknown","closings":-1,"pause_source":"chat","hears_room":false}`,
		`{"type":"cancel","input":"office","generation":0}`,
		`{"type":"chat_check","result":"maybe","event":"event1"}`,
		`{"type":"pause","chat_time":"not-a-time"}`,
		`{"type":"hello","build":"unknown","link_versions":[0,-1]}`,
	} {
		t.Run(body, func(t *testing.T) {
			if c, err := ParseControl([]byte(body)); err == nil {
				t.Fatalf("malformed known control accepted: %+v", c)
			}
		})
	}
}

// The parts of the messages below that C2's table allows.
const (
	validState  = `"state":{"state":"listening","closings":0,"pause_source":"office_control","hears_room":false}`
	validBridge = `"paused":false,"pause_chat_time":"","event":null,"bot_state":""`
	validTexts  = `"texts":{"bot_name":"b","consent":"c","paused_open":"po","paused_closed":"pc","starts_paused":"sp",` +
		`"starts_closed":"sc","resume_hint":"rh","office_only":"oo","resumed":"r","lost":"l"}`
)

// TestParseControlRefusesEachRowOutsideC2 holds C2's table for each row: a
// field it lacks, a null where C2 has a value, a value outside an enum, a
// count below its bound, a time that is not RFC 3339, an empty id, and an
// event that ends before it starts are each refused. json.Unmarshal takes
// each of them, so it fails if ParseControl checks no more than the JSON
// types: a pong without paused, for one, would read as not paused.
func TestParseControlRefusesEachRowOutsideC2(t *testing.T) {
	// The controls: each part above is taken.
	for _, body := range []string{
		`{"type":"hello","build":"release","link_versions":[1],` + validTexts + `,"jitter_target_ms":80,` + validState + `}`,
		`{"type":"hello","build":"callfault","link_versions":[1,2],` + validBridge + `}`,
	} {
		if _, err := ParseControl([]byte(body)); err != nil {
			t.Fatalf("ParseControl(%s): %v", body, err)
		}
	}
	for _, tc := range []struct{ name, body string }{
		{"a hello with no build", `{"type":"hello","link_versions":[1],` + validBridge + `}`},
		{"a hello of another build", `{"type":"hello","build":"debug","link_versions":[1],` + validBridge + `}`},
		{"a hello with no link versions", `{"type":"hello","build":"release","link_versions":[],` + validBridge + `}`},
		{"a hello whose link versions are null", `{"type":"hello","build":"release","link_versions":null,` + validBridge + `}`},
		{"a hello with link version 0", `{"type":"hello","build":"release","link_versions":[0,1],` + validBridge + `}`},
		{"a hello with both sides' fields", `{"type":"hello","build":"release","link_versions":[1],"texts":null,"jitter_target_ms":80,` + validState +
			`,"paused":false,"pause_chat_time":"","event":null,"bot_state":"in_call"}`},
		{"the Pi's hello with no texts", `{"type":"hello","build":"release","link_versions":[1],"jitter_target_ms":80,` + validState + `}`},
		{"the Pi's hello whose texts lack a notice", `{"type":"hello","build":"release","link_versions":[1],"jitter_target_ms":80,` + validState + `,` +
			`"texts":{"bot_name":"b","consent":"c","paused_open":"po","paused_closed":"pc","starts_paused":"sp","starts_closed":"sc","resume_hint":"rh","office_only":"oo","resumed":"r"}}`},
		{"the Pi's hello with no jitter target", `{"type":"hello","build":"release","link_versions":[1],"texts":null,` + validState + `}`},
		{"the Pi's hello with a jitter target of 0", `{"type":"hello","build":"release","link_versions":[1],"texts":null,"jitter_target_ms":0,` + validState + `}`},
		{"the Pi's hello whose state is null", `{"type":"hello","build":"release","link_versions":[1],"texts":null,"jitter_target_ms":80,"state":null}`},
		{"the Pi's hello in a state outside C2", `{"type":"hello","build":"release","link_versions":[1],"texts":null,"jitter_target_ms":80,` +
			`"state":{"state":"muted","closings":0,"pause_source":"none","hears_room":true}}`},
		{"the bridge's hello with no paused", `{"type":"hello","build":"release","link_versions":[1],"pause_chat_time":"","event":null,"bot_state":""}`},
		{"the bridge's hello with a pause time that is no time", `{"type":"hello","build":"release","link_versions":[1],"paused":true,"pause_chat_time":"15:04","event":null,"bot_state":""}`},
		{"the bridge's hello with a bot state outside C2", `{"type":"hello","build":"release","link_versions":[1],"paused":false,"pause_chat_time":"","event":` + testEventJSON + `,"bot_state":"waiting"}`},
		{"the bridge's hello with an event with no id", `{"type":"hello","build":"release","link_versions":[1],"paused":false,"pause_chat_time":"",` +
			`"event":{"id":"","organizer":"lead@example.com","start":"2026-11-26T15:00:00Z","end":"2026-11-26T16:00:00Z"},"bot_state":"in_call"}`},
		{"state with no closings", `{"type":"state","state":"recording","pause_source":"none","hears_room":true}`},
		{"state whose hears_room is null", `{"type":"state","state":"recording","closings":0,"pause_source":"none","hears_room":null}`},
		{"state with closings below 0", `{"type":"state","state":"recording","closings":-1,"pause_source":"none","hears_room":true}`},
		{"state in a state outside C2", `{"type":"state","state":"paused","closings":0,"pause_source":"none","hears_room":true}`},
		{"state with a pause source outside C2", `{"type":"state","state":"listening","closings":0,"pause_source":"phone","hears_room":true}`},
		{"post with no key", `{"type":"post"}`},
		{"post with a key outside C2", `{"type":"post","key":"starts_open"}`},
		{"cancel with no generation", `{"type":"cancel","input":"voice"}`},
		{"cancel of an input outside C2", `{"type":"cancel","input":"office","generation":1}`},
		{"ping with no clock", `{"type":"ping","seq":1}`},
		{"ping whose seq is null", `{"type":"ping","seq":null,"pi_clock_us":2}`},
		{"pong with no paused", `{"type":"pong","seq":1,"pi_clock_us":2,"bridge_clock_us":3,"pause_chat_time":""}`},
		{"pong with a pause time that is no time", `{"type":"pong","seq":1,"pi_clock_us":2,"bridge_clock_us":3,"paused":true,"pause_chat_time":"later"}`},
		{"call with a bot state outside C2", `{"type":"call","event":` + testEventJSON + `,"bot_state":"gone"}`},
		{"call whose event is null", `{"type":"call","event":null,"bot_state":"joining"}`},
		{"call whose event has no end", `{"type":"call","event":{"id":"evt-0001","organizer":"lead@example.com","start":"2026-11-26T15:00:00Z"},"bot_state":"joining"}`},
		{"call whose event has no organizer", `{"type":"call","event":{"id":"evt-0001","organizer":"","start":"2026-11-26T15:00:00Z","end":"2026-11-26T16:00:00Z"},"bot_state":"joining"}`},
		{"call whose event ends before it starts", `{"type":"call","event":{"id":"evt-0001","organizer":"lead@example.com","start":"2026-11-26T16:00:00Z","end":"2026-11-26T15:00:00Z"},"bot_state":"joining"}`},
		{"chat_check with no event", `{"type":"chat_check","result":"passed"}`},
		{"chat_check naming no event", `{"type":"chat_check","result":"passed","event":""}`},
		{"chat_check with a result outside C2", `{"type":"chat_check","result":"maybe","event":"evt-0001"}`},
		{"pause with no time", `{"type":"pause"}`},
		{"pause with an empty time", `{"type":"pause","chat_time":""}`},
		{"resume with a time that is not RFC 3339", `{"type":"resume","chat_time":"2026-11-26 15:12:00"}`},
		{"busy whose event has no id", `{"type":"busy","event":{"organizer":"lead@example.com","start":"2026-11-26T15:00:00Z","end":"2026-11-26T16:00:00Z"}}`},
		{"busy whose event has no start", `{"type":"busy","event":{"id":"evt-0001","organizer":"lead@example.com","start":"0001-01-01T00:00:00Z","end":"2026-11-26T16:00:00Z"}}`},
		{"rate_fault with no rate", `{"type":"rate_fault"}`},
		{"rate_fault below 0", `{"type":"rate_fault","samples_per_second":-1}`},
		{"roster whose participants are null", `{"type":"roster","participants":null}`},
		{"roster with a participant with no name", `{"type":"roster","participants":[{"index":0,"id":"spaces/a/devices/1"}]}`},
		{"roster with a participant with no id", `{"type":"roster","participants":[{"index":0,"id":"","name":"Ada"}]}`},
		{"roster with one index twice", `{"type":"roster","participants":[{"index":3,"id":"spaces/a/devices/1","name":"Ada"},{"index":3,"id":"spaces/a/devices/2","name":"Grace"}]}`},
		{"join with no index", `{"type":"join","id":"spaces/a/devices/3","name":"Grace"}`},
		{"join with no id", `{"type":"join","index":3,"id":"","name":"Grace"}`},
		{"leave with no id", `{"type":"leave","index":3,"name":"Grace"}`},
		{"chat with no text", `{"type":"chat","sender_id":"spaces/a/devices/3","chat_time":"2026-11-26T15:10:00Z"}`},
		{"chat with no sender", `{"type":"chat","sender_id":"","text":"hi","chat_time":"2026-11-26T15:10:00Z"}`},
		{"chat with a time that is no time", `{"type":"chat","sender_id":"spaces/a/devices/3","text":"hi","chat_time":"yesterday"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if c, err := ParseControl([]byte(tc.body)); err == nil {
				t.Errorf("ParseControl(%s) = %+v, nil; want an error", tc.body, c)
			}
		})
	}
}

// TestMarshalJSONRefusesWhatParseControlRefuses holds that a side writes no
// control message the other side refuses: each value outside C2's table is
// refused by MarshalJSON too. It fails if MarshalJSON checks only which row
// is set.
func TestMarshalJSONRefusesWhatParseControlRefuses(t *testing.T) {
	piHello := func(s State, jitter int) Control {
		return Control{Type: TypeHello, Hello: &Hello{Build: BuildRelease, LinkVersions: []int{1}, JitterTargetMS: jitter, State: &s}}
	}
	bridgeHello := func(h Hello) Control {
		h.Build, h.LinkVersions = BuildRelease, []int{1}
		return Control{Type: TypeHello, Hello: &h}
	}
	recording := State{State: "recording", PauseSource: "none"}
	noID := testEvent
	noID.ID = ""
	backwards := testEvent
	backwards.Start, backwards.End = testEvent.End, testEvent.Start
	for _, tc := range []struct {
		name string
		c    Control
	}{
		{"a hello of no build", Control{Type: TypeHello, Hello: &Hello{LinkVersions: []int{1}}}},
		{"a hello with link version 0", Control{Type: TypeHello, Hello: &Hello{Build: BuildRelease, LinkVersions: []int{0}}}},
		{"the Pi's hello with a jitter target below 0", piHello(recording, -1)},
		{"the Pi's hello in a state outside C2", piHello(State{State: "muted", PauseSource: "none"}, 80)},
		{"the bridge's hello with a pause time that is no time", bridgeHello(Hello{Paused: true, PauseChatTime: "now"})},
		{"the bridge's hello with a bot state outside C2", bridgeHello(Hello{Event: &testEvent, BotState: "waiting"})},
		{"the bridge's hello with an event with no id", bridgeHello(Hello{Event: &noID, BotState: "in_call"})},
		{"state with closings below 0", Control{Type: TypeState, State: &State{State: "closed", Closings: -1, PauseSource: "office_close"}}},
		{"state with no pause source", Control{Type: TypeState, State: &State{State: "recording"}}},
		{"post with no key", Control{Type: TypePost, Post: &Post{}}},
		{"cancel of no input", Control{Type: TypeCancel, Cancel: &Cancel{Generation: 1}}},
		{"pong with a pause time that is no time", Control{Type: TypePong, Pong: &Pong{Paused: true, PauseChatTime: "later"}}},
		{"call with no bot state", Control{Type: TypeCall, Call: &Call{Event: testEvent}}},
		{"call whose event ends before it starts", Control{Type: TypeCall, Call: &Call{Event: backwards, BotState: "joining"}}},
		{"chat_check with a result outside C2", Control{Type: TypeChatCheck, ChatCheck: &ChatCheck{Result: "maybe", Event: "evt-0001"}}},
		{"chat_check naming no event", Control{Type: TypeChatCheck, ChatCheck: &ChatCheck{Result: "failed"}}},
		{"pause with no time", Control{Type: TypePause, Pause: &ChatTime{}}},
		{"resume with a time that is no time", Control{Type: TypeResume, Resume: &ChatTime{ChatTime: "x"}}},
		{"busy of the zero event", Control{Type: TypeBusy, Busy: &Busy{}}},
		{"rate_fault below 0", Control{Type: TypeRateFault, RateFault: &RateFault{SamplesPerSecond: -1}}},
		{"roster with a participant with no id", Control{Type: TypeRoster, Roster: &Roster{Participants: []Participant{{Index: 0, Name: "Ada"}}}}},
		{"roster with one index twice", Control{Type: TypeRoster, Roster: &Roster{Participants: []Participant{
			{Index: 3, ID: "spaces/a/devices/1", Name: "Ada"}, {Index: 3, ID: "spaces/a/devices/2", Name: "Grace"},
		}}}},
		{"join with no id", Control{Type: TypeJoin, Join: &Participant{Index: 3, Name: "Grace"}}},
		{"leave with no id", Control{Type: TypeLeave, Leave: &Participant{Index: 3}}},
		{"chat with no sender", Control{Type: TypeChat, Chat: &Chat{Text: "hi", ChatTime: "2026-11-26T15:10:00Z"}}},
		{"chat with a time that is no time", Control{Type: TypeChat, Chat: &Chat{SenderID: "spaces/a/devices/3", Text: "hi", ChatTime: "yesterday"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if b, err := tc.c.MarshalJSON(); err == nil {
				t.Errorf("MarshalJSON wrote %s; want an error", b)
			}
		})
	}
}

// TestALaterVersionsHelloIsReadForItsVersionsAlone holds R15's upgrade path
// when a later link version changed a field of the hello: a hello that names
// no version this build accepts is read for its build and its versions
// alone, so this side still learns which versions the other accepts. Its
// control, the same hello naming version 1 as well, is version 1's, and is
// refused. It fails if ParseControl reads a later version's fields as
// version 1's.
func TestALaterVersionsHelloIsReadForItsVersionsAlone(t *testing.T) {
	const later = `{"type":"hello","build":"release","link_versions":[2],"texts":{"bot_name":7},"jitter_target_ms":"80ms","state":{"state":"muted"}}`
	c, err := ParseControl([]byte(later))
	if err != nil {
		t.Fatalf("ParseControl of a later version's hello: %v", err)
	}
	if want := (Control{Type: TypeHello, Hello: &Hello{Build: BuildRelease, LinkVersions: []int{2}}}); !reflect.DeepEqual(c, want) {
		t.Errorf("ParseControl gave %+v; want %+v, the build and the versions alone", c.Hello, want.Hello)
	}
	if c, err := ParseControl([]byte(strings.Replace(later, `[2]`, `[1,2]`, 1))); err == nil {
		t.Errorf("ParseControl of a hello that names version 1 with fields version 1 does not allow = %+v, nil; want an error", c.Hello)
	}
}

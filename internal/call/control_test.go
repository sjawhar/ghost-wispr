package call

import (
	"bytes"
	"encoding/json"
	"reflect"
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

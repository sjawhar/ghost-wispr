package call

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testPiHello is a Pi's hello as the Pi's writer sends it.
func testPiHello() *Control {
	return &Control{Type: TypeHello, Hello: &Hello{
		Build: BuildRelease, LinkVersions: []int{1},
		Texts:          &Texts{BotName: "Ghost Wispr test (recording)", Consent: "consent text"},
		JitterTargetMS: 80,
		State:          &State{State: "recording", Closings: 2, PauseSource: "none", HearsRoom: true},
	}}
}

// testBridgeHello is a bridge's hello as the bridge sends it through Send.
func testBridgeHello() *Control {
	return &Control{Type: TypeHello, Hello: &Hello{
		Build: BuildCallfault, LinkVersions: []int{1, 2}, BotState: "joining", Event: &testEvent,
	}}
}

// serve runs q's writer on end until the test ends, as a side's link does,
// and fails the test if the writer ended before the test did. At the test's
// end it ends the writer's context and closes the link, as the side closes
// its connection, so a writer blocked on a write after a failed check ends.
func serve(t *testing.T, q *SendQueue, end *fakeEnd) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ended := make(chan error, 1)
	go func() { ended <- q.Serve(ctx, end) }()
	t.Cleanup(func() {
		select {
		case err := <-ended:
			t.Errorf("the writer ended before the test did: %v", err)
			return
		default:
		}
		cancel()
		end.close()
		<-ended
	})
}

func mustQueue(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// TestEachSideNamesItsLinkVersionsFirst holds R15 and C2 for both sides'
// writers: the first message of a link is the side's hello naming the link
// versions it accepts; a control message and a frame queued before the hello
// wait for it; and the waiting control message leaves before the frame that
// was queued ahead of it. It fails if the queue writes anything before its
// hello, writes the hello after what was queued first, or keeps arrival order
// between control messages and frames.
func TestEachSideNamesItsLinkVersionsFirst(t *testing.T) {
	piEnd, bridgeEnd := newFakeTransport(t)
	piQueue, bridgeQueue := NewSendQueue(8), NewSendQueue(8)

	office := Frame{Stream: StreamOffice, Seq: 1, ClockUS: 10, PCM: pcmOf(320)}
	ping := Control{Type: TypePing, Ping: &Ping{Seq: 1, PiClockUS: 10}}
	mix := Frame{Stream: StreamCallMix, Seq: 1, ClockUS: 20, PCM: pcmOf(480)}
	check := Control{Type: TypeChatCheck, ChatCheck: &ChatCheck{Result: "passed", Event: "evt-0001"}}
	mustQueue(t, piQueue.SendAudio(office))
	mustQueue(t, piQueue.Send(&ping))
	mustQueue(t, bridgeQueue.SendAudio(mix))
	mustQueue(t, bridgeQueue.Send(&check))

	serve(t, piQueue, piEnd)
	serve(t, bridgeQueue, bridgeEnd)
	bridgeEnd.quiet(t, 50*time.Millisecond) // the Pi's writer has no hello yet
	piEnd.quiet(t, 50*time.Millisecond)     // nor has the bridge's

	mustQueue(t, piQueue.Send(testPiHello()))
	mustQueue(t, bridgeQueue.Send(testBridgeHello()))

	for _, side := range []struct {
		name     string
		reader   *fakeEnd
		versions []int
		control  Control
		frame    Frame
	}{
		{"the Pi", bridgeEnd, []int{1}, ping, office},
		{"the bridge", piEnd, []int{1, 2}, check, mix},
	} {
		first := side.reader.read(t)
		if first.Control == nil || first.Control.Type != TypeHello || !slices.Equal(first.Control.Hello.LinkVersions, side.versions) {
			t.Fatalf("%s's first message is %+v; want its hello naming link versions %v", side.name, first, side.versions)
		}
		if second := side.reader.read(t); second.Control == nil || !reflect.DeepEqual(*second.Control, side.control) {
			t.Errorf("%s's second message is %+v; want the %s it queued before its hello", side.name, second, side.control.Type)
		}
		if third := side.reader.read(t); third.Frame == nil || !reflect.DeepEqual(*third.Frame, side.frame) {
			t.Errorf("%s's third message is %+v; want the frame it queued first", side.name, third)
		}
	}
}

// TestAWaitingControlLeavesBeforeAnyFrame holds C2's writer rule on an open
// link: when the writer picks its next message, a waiting control message
// goes before every waiting frame, and frames keep their order. It fails if
// the queue writes frames and control messages in the order they came.
func TestAWaitingControlLeavesBeforeAnyFrame(t *testing.T) {
	piEnd, bridgeEnd := newFakeTransport(t)
	q := NewSendQueue(8)
	mustQueue(t, q.Send(testPiHello()))
	serve(t, q, piEnd)
	if hello := bridgeEnd.read(t); hello.Control == nil || hello.Control.Type != TypeHello {
		t.Fatalf("the first message is %+v; want the hello", hello)
	}

	// The writer can take frame 1 before the state is queued, and then waits
	// until it is read; frames 2 and 3 wait with the state.
	state := Control{Type: TypeState, State: &State{State: "closed", Closings: 3, PauseSource: "office_close"}}
	for seq := uint32(1); seq <= 3; seq++ {
		mustQueue(t, q.SendAudio(Frame{Stream: StreamWisprVoice, Seq: seq, PCM: pcmOf(480)}))
	}
	mustQueue(t, q.Send(&state))

	var order []string
	for range 4 {
		m := bridgeEnd.read(t)
		switch {
		case m.Control != nil:
			order = append(order, m.Control.Type)
		case m.Frame != nil:
			order = append(order, []string{"", "frame 1", "frame 2", "frame 3"}[m.Frame.Seq])
		}
	}
	if !slices.Equal(order, []string{"state", "frame 1", "frame 2", "frame 3"}) &&
		!slices.Equal(order, []string{"frame 1", "state", "frame 2", "frame 3"}) {
		t.Errorf("the writer wrote %v; want the state before frames 2 and 3, and the frames in order", order)
	}
}

// TestAFullQueueDropsItsOldestFrame holds the queue's bound: it holds at most
// maxFrames frames, drops the oldest to take a new one, and counts each drop,
// so a stalled link holds no more than that much old audio. It fails if the
// queue grows past its bound, drops the newest, or does not count a drop.
func TestAFullQueueDropsItsOldestFrame(t *testing.T) {
	piEnd, bridgeEnd := newFakeTransport(t)
	q := NewSendQueue(2)
	for seq := uint32(1); seq <= 3; seq++ {
		mustQueue(t, q.SendAudio(Frame{Stream: StreamOffice, Seq: seq, PCM: pcmOf(320)}))
	}
	if got := q.Dropped(); got != 1 {
		t.Errorf("Dropped() = %d after three frames into room for two; want 1", got)
	}
	mustQueue(t, q.Send(testPiHello()))
	serve(t, q, piEnd)
	bridgeEnd.read(t) // the hello
	for _, want := range []uint32{2, 3} {
		if m := bridgeEnd.read(t); m.Frame == nil || m.Frame.Seq != want {
			t.Fatalf("got %+v; want frame %d", m, want)
		}
	}
	bridgeEnd.quiet(t, 50*time.Millisecond)
}

// TestAQueueRefusesWhatCannotGoOnTheLink holds the queue's checks: a hello
// that names no link version, a second hello, a control message without its
// row, and a frame with another length than its stream's 20 ms are refused
// and never written. It fails if any of them is queued.
func TestAQueueRefusesWhatCannotGoOnTheLink(t *testing.T) {
	piEnd, bridgeEnd := newFakeTransport(t)
	q := NewSendQueue(4)

	noVersions := testPiHello()
	noVersions.Hello.LinkVersions = nil
	if err := q.Send(noVersions); err == nil {
		t.Error("Send took a hello that names no link version")
	}
	mustQueue(t, q.Send(testPiHello()))
	if err := q.Send(testPiHello()); err == nil {
		t.Error("Send took a second hello for one link")
	}
	if err := q.Send(&Control{Type: TypeState}); err == nil {
		t.Error("Send took a state message without its state")
	}
	for _, f := range []Frame{
		{Stream: 9, PCM: pcmOf(480)},
		{Stream: StreamOffice, PCM: pcmOf(480)},
		{Stream: StreamCallMix, PCM: pcmOf(479)},
	} {
		if err := q.SendAudio(f); !errors.Is(err, ErrBadFrame) {
			t.Errorf("SendAudio of stream %d with %d samples = %v; want ErrBadFrame", f.Stream, len(f.PCM), err)
		}
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("NewSendQueue(0) made a queue with no room for a frame")
			}
		}()
		NewSendQueue(0)
	}()

	serve(t, q, piEnd)
	if m := bridgeEnd.read(t); m.Control == nil || m.Control.Type != TypeHello {
		t.Fatalf("the first message is %+v; want the hello", m)
	}
	bridgeEnd.quiet(t, 50*time.Millisecond)
}

type failingWriter struct{ err error }

func (w failingWriter) WriteMessage(int, []byte) error { return w.err }

// TestAQueueRefusesMessagesAfterItsWriterEnds holds that a queue serves one
// link: once its writer ends, by a failed write or by its context, the
// queue refuses every message with ErrQueueClosed, so a sender learns the
// link is down and holds nothing for a link that is gone. It fails if a
// closed queue keeps taking messages, or Serve hides why it ended.
func TestAQueueRefusesMessagesAfterItsWriterEnds(t *testing.T) {
	broken := errors.New("connection reset")
	failed := NewSendQueue(4)
	mustQueue(t, failed.Send(testPiHello()))
	if err := failed.Serve(context.Background(), failingWriter{broken}); !errors.Is(err, broken) {
		t.Errorf("Serve over a failing write = %v; want the write's error", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled := NewSendQueue(4)
	if err := cancelled.Serve(ctx, failingWriter{broken}); !errors.Is(err, context.Canceled) {
		t.Errorf("Serve with an ended context = %v; want context.Canceled", err)
	}

	for name, q := range map[string]*SendQueue{"after a failed write": failed, "after its context ended": cancelled} {
		if err := q.Send(&Control{Type: TypePing, Ping: &Ping{Seq: 2}}); !errors.Is(err, ErrQueueClosed) {
			t.Errorf("%s: Send = %v; want ErrQueueClosed", name, err)
		}
		if err := q.SendAudio(Frame{Stream: StreamOffice, PCM: pcmOf(320)}); !errors.Is(err, ErrQueueClosed) {
			t.Errorf("%s: SendAudio = %v; want ErrQueueClosed", name, err)
		}
		if err := q.Serve(context.Background(), failingWriter{nil}); !errors.Is(err, ErrQueueClosed) {
			t.Errorf("%s: a second Serve = %v; want ErrQueueClosed", name, err)
		}
	}
	if err := cancelled.Send(testPiHello()); !errors.Is(err, ErrQueueClosed) {
		t.Errorf("a hello after the writer ended = %v; want ErrQueueClosed", err)
	}
}

// holdingWriter holds its writes until release is closed, records the
// control message each write carries (none for a frame), and counts a write
// that starts while another is in progress, so a test sees two writers on
// one link.
type holdingWriter struct {
	release    chan struct{}
	inWrite    atomic.Bool
	overlapped atomic.Int64
	mu         sync.Mutex
	wrote      []Message
}

func (w *holdingWriter) WriteMessage(_ int, b []byte) error {
	if w.inWrite.Swap(true) {
		w.overlapped.Add(1)
	}
	defer w.inWrite.Store(false)
	<-w.release
	var m Message
	if c, err := ParseControl(b); err == nil {
		m.Control = &c
	}
	w.mu.Lock()
	w.wrote = append(w.wrote, m)
	w.mu.Unlock()
	return nil
}

func (w *holdingWriter) written() []Message {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.wrote)
}

// TestASecondServeIsRefusedWhileTheFirstRuns holds the queue's one writer:
// of several Serve calls made at once on one queue, one writes the link and
// every other returns at once with ErrQueueServing, writing nothing, so the
// link's order holds. It fails if a second Serve enters the writer loop
// beside the first, which then waits for a message to take or writes one.
func TestASecondServeIsRefusedWhileTheFirstRuns(t *testing.T) {
	q := NewSendQueue(4)
	mustQueue(t, q.Send(testPiHello()))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const callers = 8
	w := &holdingWriter{release: make(chan struct{})}
	start := make(chan struct{})
	results := make(chan error, callers)
	for range callers {
		go func() {
			<-start
			results <- q.Serve(ctx, w)
		}()
	}
	close(start)
	for i := range callers - 1 {
		select {
		case err := <-results:
			if !errors.Is(err, ErrQueueServing) {
				t.Errorf("a Serve beside a running one = %v; want ErrQueueServing", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%d of %d Serve calls returned in 2 s; want each call but the serving one refused at once", i, callers-1)
		}
	}

	pings := []Control{{Type: TypePing, Ping: &Ping{Seq: 1}}, {Type: TypePing, Ping: &Ping{Seq: 2}}}
	for i := range pings {
		mustQueue(t, q.Send(&pings[i]))
	}
	close(w.release)
	clockDeadline := time.Now().Add(2 * time.Second)
	for len(w.written()) < 3 && time.Now().Before(clockDeadline) {
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-results:
		t.Fatalf("the serving call ended before its context: %v", err)
	default:
	}
	cancel()
	if err := <-results; !errors.Is(err, context.Canceled) {
		t.Errorf("the serving call = %v; want context.Canceled", err)
	}

	got := w.written()
	if len(got) != 3 || got[0].Control == nil || got[0].Control.Type != TypeHello ||
		!reflect.DeepEqual(*got[1].Control, pings[0]) || !reflect.DeepEqual(*got[2].Control, pings[1]) {
		t.Errorf("the link got %d messages %+v; want the hello, then the two pings in order", len(got), got)
	}
	if n := w.overlapped.Load(); n != 0 {
		t.Errorf("%d writes started while another was in progress; want one writer", n)
	}
	if err := q.Serve(context.Background(), w); !errors.Is(err, ErrQueueClosed) {
		t.Errorf("a Serve after the writer ended = %v; want ErrQueueClosed", err)
	}
}

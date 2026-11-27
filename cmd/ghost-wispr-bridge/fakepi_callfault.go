//go:build callfault

package main

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/call"
)

// fakePi is fake_pi's stand-in for the Pi inside the test bridge (plan C8):
// it accepts link version 1, gives neutral test texts, reports recording,
// sends no audio, and drops all the audio the bridge sends, which it counts.
// It links to no Pi and carries no office audio, so a run with it may log
// more than ERROR (spec, "Logs").
type fakePi struct {
	hello call.Hello
	recv  chan call.Message

	mu          sync.Mutex
	up          bool
	callMix     int
	participant int
	controls    int
}

// fakePiLinkVersions are the link versions the fake Pi accepts.
var fakePiLinkVersions = []int{1}

// fakePiCountEvery is how often the fake Pi logs its counts.
const fakePiCountEvery = time.Minute

var errFakePiDown = errors.New("fake pi: the link is down: the bridge named no link version the fake Pi accepts")

// fakePiHello is the fake Pi's hello: neutral test texts, with no company
// name, and the state recording.
func fakePiHello() call.Hello {
	return call.Hello{
		Build:        call.BuildCallfault,
		LinkVersions: fakePiLinkVersions,
		Texts: &call.Texts{
			BotName:      "Ghost Wispr test (recording)",
			Consent:      "Ghost Wispr test is in this call for a test. It keeps nothing it hears. To try the pause, type 'wispr pause' here.",
			PausedOpen:   "Ghost Wispr test is paused. The call still hears the test office.",
			PausedClosed: "Ghost Wispr test is paused. The test office's microphone is closed.",
			StartsPaused: "The test office is paused, so the call does not hear it.",
			StartsClosed: "The test office's microphone is closed, so the call does not hear it. Only the test office can open it.",
			ResumeHint:   "Type 'wispr resume' to resume.",
			OfficeOnly:   "Only the test office can open its microphone.",
			Resumed:      "Ghost Wispr test is recording again.",
			Lost:         "Ghost Wispr test lost its test office and left the call.",
		},
		JitterTargetMS: 80,
		State:          &call.State{State: "recording", PauseSource: "none", HearsRoom: true},
	}
}

// newFakePi links at once: its hello is the first message on Recv, without
// waiting for the bridge's (contract A1). It logs its counts every minute
// until ctx ends.
func newFakePi(ctx context.Context, clk clock, log *slog.Logger) *fakePi {
	p := &fakePi{hello: fakePiHello(), recv: make(chan call.Message, 1), up: true}
	p.recv <- call.Message{Control: &call.Control{Type: call.TypeHello, Hello: &p.hello}}
	go p.logCounts(ctx, clk, log)
	return p
}

func (p *fakePi) Hello() call.Hello { return p.hello }

// Generation is 1: the fake Pi links once, at its start, and a link it takes
// down stays down.
func (p *fakePi) Generation() uint64 { return 1 }

// Send takes the bridge's control messages and drops them. A bridge hello
// that names no version the fake Pi accepts takes the link down, as the
// Pi's close with link_version does.
//
//nolint:gocritic // hugeParam: the contract passes call.Control by value
func (p *fakePi) Send(c call.Control) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.up {
		return errFakePiDown
	}
	p.controls++
	if c.Type == call.TypeHello && c.Hello != nil {
		if _, ok := call.Choose(fakePiLinkVersions, c.Hello.LinkVersions); !ok {
			p.up = false
		}
	}
	return nil
}

// SendAudio drops f and counts it.
func (p *fakePi) SendAudio(f call.Frame) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.up {
		return errFakePiDown
	}
	switch f.Stream {
	case call.StreamCallMix:
		p.callMix++
	case call.StreamParticipant:
		p.participant++
	}
	return nil
}

func (p *fakePi) Recv() <-chan call.Message { return p.recv }

func (p *fakePi) Up() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.up
}

// logCounts logs, every minute, the frames and control messages the fake
// Pi took so far: in a test run they stand for the link's counters.
func (p *fakePi) logCounts(ctx context.Context, clk clock, log *slog.Logger) {
	for {
		select {
		case <-clk.After(fakePiCountEvery):
		case <-ctx.Done():
			return
		}
		p.mu.Lock()
		line := "FAKE-PI call_mix_frames=" + strconv.Itoa(p.callMix) +
			" participant_frames=" + strconv.Itoa(p.participant) +
			" controls=" + strconv.Itoa(p.controls)
		p.mu.Unlock()
		log.Info(line)
	}
}

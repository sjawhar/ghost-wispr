//go:build callfault

package main

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/call"
)

// faultBridgeHello is the bridge's hello naming versions.
func faultBridgeHello(versions ...int) call.Control {
	return call.Control{Type: call.TypeHello, Hello: &call.Hello{Build: call.BuildCallfault, LinkVersions: versions}}
}

// faultFrame is one 20 ms frame of stream.
func faultFrame(stream uint8, seq uint32) call.Frame {
	return call.Frame{Stream: stream, Seq: seq, PCM: make([]int16, 480)}
}

// fake_pi stands in for the Pi: its first message is a hello that accepts
// link version 1 and gives neutral test texts and the state recording; it
// takes a bridge that names version 1, drops every audio frame the bridge
// sends and counts it, and sends no audio back.
func TestFakePiAcceptsVersionOneAndDropsAudio(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clk := newFakeClock(faultStart)
	log := &faultLog{}
	pi := newFakePi(ctx, clk, log.logger())

	if !pi.Up() {
		t.Fatal("the fake Pi is not up")
	}
	first := <-pi.Recv()
	if first.Control == nil || first.Control.Type != call.TypeHello || first.Control.Hello == nil {
		t.Fatalf("the first message = %+v, want the Pi's hello", first)
	}
	h := first.Control.Hello
	if len(h.LinkVersions) != 1 || h.LinkVersions[0] != 1 {
		t.Fatalf("link versions = %v, want [1]", h.LinkVersions)
	}
	if h.Texts == nil || h.Texts.BotName != "Ghost Wispr test (recording)" || h.Texts.Consent == "" || h.Texts.Lost == "" {
		t.Fatalf("texts = %+v, want the test bot name and every notice", h.Texts)
	}
	if h.State == nil || h.State.State != "recording" {
		t.Fatalf("state = %+v, want recording", h.State)
	}
	if got := pi.Hello(); got.Texts == nil || got.Texts.BotName != h.Texts.BotName {
		t.Fatalf("Hello() = %+v, want the hello it sent", got)
	}

	if err := pi.Send(faultBridgeHello(1, 2)); err != nil || !pi.Up() {
		t.Fatalf("a bridge that names [1 2]: Send = %v, up %v; want nil, true", err, pi.Up())
	}
	for i := range 50 {
		if err := pi.SendAudio(faultFrame(call.StreamCallMix, uint32(i))); err != nil {
			t.Fatalf("SendAudio: %v", err)
		}
	}
	for i := range 3 {
		if err := pi.SendAudio(faultFrame(call.StreamParticipant, uint32(i))); err != nil {
			t.Fatalf("SendAudio: %v", err)
		}
	}
	if err := pi.Send(call.Control{Type: call.TypeCall, Call: &call.Call{BotState: "in_call"}}); err != nil {
		t.Fatalf("Send(call): %v", err)
	}
	select {
	case m := <-pi.Recv():
		t.Fatalf("the fake Pi sent %+v after its hello; it sends nothing more", m)
	default:
	}

	clk.BlockUntil(t, 1)
	clk.Advance(time.Minute)
	lines := log.waitLines(t, "FAKE-PI ", 1)
	if want := "FAKE-PI call_mix_frames=50 participant_frames=3 controls=2"; lines[0].Message != want {
		t.Fatalf("count line = %q, want %q", lines[0].Message, want)
	}

	other := newFakePi(ctx, clk, log.logger())
	<-other.Recv()
	if err := other.Send(faultBridgeHello(2)); err != nil {
		t.Fatalf("Send(hello [2]) = %v; the hello itself goes out", err)
	}
	if other.Up() {
		t.Fatal("the fake Pi is up with a bridge that names only version 2")
	}
	if err := other.SendAudio(faultFrame(call.StreamCallMix, 0)); err == nil {
		t.Fatal("SendAudio on a link with no common version: no error")
	}
	if err := other.Send(call.Control{Type: call.TypeCall, Call: &call.Call{}}); err == nil {
		t.Fatal("Send on a link with no common version: no error")
	}
}

// The fault fake_pi links the bridge to a fake Pi; without it the bridge's
// link stays what main gave (none in slice 3).
func TestTheFakePiFaultLinksTheBridgeToAFakePi(t *testing.T) {
	end := faultStart.Add(time.Hour)
	spec := faultSpec(end)
	spec["fault"] = "fake_pi" // plan C8's form: one name
	r := faultApply(t, &settings{RunSpecFile: faultWriteSpec(t, spec)}, faultEnv{})
	faultAccepted(t, r)
	pi, ok := r.deps.Link.(*fakePi)
	if !ok || !pi.Up() || pi.Hello().Texts == nil || pi.Hello().Texts.BotName != "Ghost Wispr test (recording)" {
		t.Fatalf("link = %#v, want an up fake Pi with the test texts", r.deps.Link)
	}

	without := faultApply(t, &settings{RunSpecFile: faultWriteSpec(t, faultSpec(end, "long_lines"))}, faultEnv{})
	faultAccepted(t, without)
	if without.deps.Link != nil {
		t.Fatalf("link without fake_pi = %#v, want none", without.deps.Link)
	}
}

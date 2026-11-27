package main

import (
	"testing"
	"time"
)

// TestAPausedUnitStaysPausedWhenTheJabraReportsMuteOnItsOwn fails if a Phone
// Mute the Jabra sends by itself, not a person's tap, resumes a paused unit,
// or if the light is written while a microphone stream is open.
//
// On the office Pi (Jabra Speak2 75, release a889869, 2026-11-17), the Jabra
// sent Phone Mute (MSC_SCAN 0xb002f, KEY_MICMUTE 1 then 0) with nobody at it,
// 12-15 ms after its microphone stream closed, whenever its mute light had
// changed while that stream was open:
//
//	06:02:46.849Z LED_MUTE value=0        (light written while arecord held the microphone)
//	06:02:47.899Z LED_MUTE value=1
//	06:02:49.245Z arecord ended
//	06:02:49.257Z MSC_SCAN value=720943
//	06:02:49.257Z KEY_MICMUTE value=1
//	06:02:49.257Z KEY_MICMUTE value=0
//
// The same light written with no stream open got no key back. Written during
// a playback stream, it got no key back at once, but the Jabra sent Phone Mute
// by itself at the next microphone close (section 9.7, restart 1), so the
// light waits for playback too (TestNoJabraLightWriteWhileTTSPlaybackIsOpen).
// a889869 wrote the light at connect while a
// microphone stream was open on the Jabra (PortAudio opens one as the app
// starts, and a pause left it open), so at 05:50:48Z a restart of a paused
// unit read the Jabra's own report as a tap and resumed: capture_state
// "resuming", the blink(1) red, the Jabra's microphone open, for 30 s until the
// unit was paused again by hand. A pause must hold through a restart
// (INFRA-1027 R9), and a resume must come from a person.
//
// Here a microphone stream is open as the button connects to a paused unit.
// The light must wait for the stream to close; the Jabra's own report 15 ms
// after the close must leave the unit paused; a person's tap after that must
// still resume it.
func TestAPausedUnitStaysPausedWhenTheJabraReportsMuteOnItsOwn(t *testing.T) {
	runParallel(t)
	rig := newJabraRig(true) // the unit starts paused, as restorePause leaves it
	rig.openMic(t)           // a microphone stream is open on the Jabra as the button connects
	rig.start(t)
	device := rig.plugIn()
	waitForHardware(t, "the Jabra to connect", rig.attached)
	time.Sleep(20 * time.Millisecond) // about twenty light checks
	if device.reportsSent() != 0 {
		t.Fatal("the light was written while a microphone stream was open")
	}
	if !rig.capture.IsPaused() {
		t.Fatal("control: the unit is not paused before the Jabra's own report")
	}

	// The microphone stream closes, the light is written, and the Jabra
	// reports Phone Mute by itself about 15 ms after the close, as measured on
	// the unit.
	rig.closeMic(t)
	if !device.shows() {
		t.Fatal("the Jabra light does not show the pause once the microphone closed")
	}
	time.Sleep(15 * time.Millisecond)
	device.press(t)

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if !rig.capture.IsPaused() {
			t.Fatal("the Jabra's own Phone Mute, sent as its microphone stream closed, resumed the paused unit")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Control: a person's tap well after the close resumes the unit.
	device.press(t)
	waitForHardware(t, "a person's tap to resume capture", func() bool { return !rig.capture.IsPaused() })
}

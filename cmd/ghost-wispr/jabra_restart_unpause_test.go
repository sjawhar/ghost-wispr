package main

import (
	"testing"
	"time"
)

// TestAPausedUnitStaysPausedWhenTheJabraReportsMuteOnItsOwn fails if a Phone
// Mute the Jabra sends by itself, not a person's tap, resumes a paused unit.
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
// The same light written with no microphone stream open, or during a playback
// stream, got no key back. Ghost Wispr writes the light at connect
// (ResendMuteLight) while a microphone stream is open on the Jabra: PortAudio
// opens it as the app starts, and a pause leaves it open
// (TestAPauseReleasesTheCaptureDevice). So at 05:50:48Z a restart of a paused
// unit read the Jabra's own report as a tap and resumed: capture_state
// "resuming", the blink(1) red, the Jabra's microphone open, for 30 s until
// the unit was paused again by hand. A pause must hold through a restart
// (INFRA-1027 R9), and a resume must come from a person.
func TestAPausedUnitStaysPausedWhenTheJabraReportsMuteOnItsOwn(t *testing.T) {
	rig := newJabraRig(true) // the unit starts paused, as restorePause leaves it
	rig.mic.open.Store(true) // a microphone stream is open on the Jabra as the button connects
	rig.start(t)
	device := rig.plugIn()
	waitForHardware(t, "the Jabra light to show the pause at connect", device.shows)
	if !rig.capture.IsPaused() {
		t.Fatal("control: the unit is not paused before the Jabra's own report")
	}

	// The microphone stream closes, and the Jabra reports Phone Mute by itself
	// about 12 ms later, as measured on the unit.
	rig.mic.open.Store(false)
	time.Sleep(15 * time.Millisecond)
	device.press(t)

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if !rig.capture.IsPaused() {
			t.Fatal("the Jabra's own Phone Mute, sent as its microphone stream closed after a light change, resumed the paused unit")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

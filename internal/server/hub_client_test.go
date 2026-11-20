package server

import (
	"encoding/json"
	"testing"
	"time"
)

// statusOf decodes a component_status payload's component, status and seq;
// ok is false for any other event.
func statusOf(t *testing.T, payload []byte) (component, status string, seq uint64, ok bool) {
	t.Helper()
	var event struct {
		Type      string `json:"type"`
		Component string `json:"component"`
		Status    string `json:"status"`
		Seq       uint64 `json:"seq"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatalf("decode %s: %v", payload, err)
	}
	return event.Component, event.Status, event.Seq, event.Type == "component_status"
}

// TestASlowSubscriberEndsOnEachComponentsLatestStatus fails if a subscriber
// whose channel is full loses a component's latest status, or gets a
// component's statuses out of order. Before, a full channel dropped the event:
// with one free slot, a subscriber got "reconnecting" and lost "draining" and
// "connected", and held "reconnecting" for as long as nothing else changed,
// while the client and every other subscriber read connected. Ordinary events
// may still be dropped; a status is coalesced to the newest.
func TestASlowSubscriberEndsOnEachComponentsLatestStatus(t *testing.T) {
	hub := NewHub()
	fast, slow := hub.Subscribe(), hub.Subscribe()
	defer hub.Unsubscribe(fast)
	defer hub.Unsubscribe(slow)
	// The slow subscriber is one event short of full, as after a burst of
	// live transcript; the fast one keeps up.
	for range subscriberQueue - 1 {
		hub.Broadcast([]byte(`{"type":"live_transcript_interim"}`))
		<-fast
	}
	hub.BroadcastComponentStatusSeq(t.Context(), "deepgram", "reconnecting", "Deepgram reconnecting", 1)
	hub.BroadcastComponentStatusSeq(t.Context(), "deepgram", "draining", "Deepgram draining", 2)
	hub.BroadcastComponentStatus("mic", "connected", "Microphone open at 16000 Hz")
	hub.BroadcastComponentStatusSeq(t.Context(), "deepgram", "connected", "Deepgram connected", 3)
	hub.Broadcast([]byte(`{"type":"live_transcript_interim"}`)) // dropped for the slow one: it is full

	read := func(ch chan []byte, name string) (last map[string]string) {
		t.Helper()
		last = map[string]string{}
		var lastSeq uint64
		for {
			select {
			case payload := <-ch:
				component, status, seq, ok := statusOf(t, payload)
				if !ok {
					continue
				}
				if seq != 0 {
					if seq <= lastSeq {
						t.Fatalf("%s subscriber got %s %q with seq %d after seq %d", name, component, status, seq, lastSeq)
					}
					lastSeq = seq
				}
				last[component] = status
			case <-time.After(300 * time.Millisecond):
				return last
			}
		}
	}
	for name, ch := range map[string]chan []byte{"fast": fast, "slow": slow} {
		last := read(ch, name)
		if last["deepgram"] != "connected" || last["mic"] != "connected" {
			t.Errorf("the %s subscriber ends on deepgram %q and mic %q, want connected and connected", name, last["deepgram"], last["mic"])
		}
	}
}

// TestANumberedStatusHeardOfLateIsDropped fails if a numbered status older
// than the one the hub last announced replaces it, for a page that connects
// (the snapshot) or one that is open.
func TestANumberedStatusHeardOfLateIsDropped(t *testing.T) {
	hub := NewHub()
	ch := hub.Subscribe()
	defer hub.Unsubscribe(ch)
	hub.BroadcastComponentStatusSeq(t.Context(), "deepgram", "connected", "Deepgram connected", 7)
	hub.BroadcastComponentStatusSeq(t.Context(), "deepgram", "draining", "Deepgram draining", 6)

	first := <-ch
	if _, status, seq, _ := statusOf(t, first); status != "connected" || seq != 7 {
		t.Fatalf("first event %s, want connected with seq 7", first)
	}
	select {
	case got := <-ch:
		t.Fatalf("the older status was sent: %s", got)
	case <-time.After(100 * time.Millisecond):
	}
	for _, event := range hub.SnapshotComponentStatuses() {
		if event.Component == "deepgram" && (event.Status != "connected" || event.Seq != 7) {
			t.Fatalf("snapshot holds deepgram %q seq %d, want connected seq 7", event.Status, event.Seq)
		}
	}
	// An unnumbered status (a pause's "paused") still applies, and the next
	// numbered one after it.
	hub.BroadcastComponentStatus("deepgram", "paused", "Deepgram connects when capture resumes")
	hub.BroadcastComponentStatusSeq(t.Context(), "deepgram", "reconnecting", "Deepgram reconnecting", 8)
	for _, want := range []string{"paused", "reconnecting"} {
		if _, status, _, _ := statusOf(t, <-ch); status != want {
			t.Fatalf("got %q, want %q", status, want)
		}
	}
}

// TestUnsubscribeStopsAWaitingStatus fails if a subscriber's waiting status is
// sent after it unsubscribed (a send on a closed channel panics), or if
// Unsubscribe waits on a subscriber that no longer reads.
func TestUnsubscribeStopsAWaitingStatus(t *testing.T) {
	hub := NewHub()
	ch := hub.Subscribe()
	for range subscriberQueue {
		hub.Broadcast([]byte(`{"type":"live_transcript_interim"}`))
	}
	hub.BroadcastComponentStatusSeq(t.Context(), "deepgram", "connected", "Deepgram connected", 1) // waits: ch is full
	done := make(chan struct{})
	go func() { hub.Unsubscribe(ch); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Unsubscribe waited on a subscriber that does not read")
	}
	hub.BroadcastComponentStatusSeq(t.Context(), "deepgram", "draining", "Deepgram draining", 2)
	n := 0
	for range ch {
		n++
	}
	if n != subscriberQueue {
		t.Fatalf("the closed channel held %d events, want the %d that fitted", n, subscriberQueue)
	}
}

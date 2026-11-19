package concierge

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// TestStopEndsTheConciergesWork (PR #89's review, round 3): the voice turned
// off stops its Concierge, and each turn-on starts a new one; Stop ends the
// goroutine that offers answers, and a rewrite for the ear still running, so
// nothing is left behind once it returns.
func TestStopEndsTheConciergesWork(t *testing.T) {
	r := newRig(t)
	r.ask(t, "agent-7", "is the tap merged", "ABC-12")
	r.model.block = make(chan struct{})
	defer close(r.model.block)
	if err := r.concierge.Deliver(context.Background(), &envoy.Message{EventID: "r", SourceSession: "agent-7", InReplyTo: "evt-1", Text: "It merged."}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the rewrite for the ear started", func() bool {
		r.model.mu.Lock()
		defer r.model.mu.Unlock()
		return len(r.model.ctxs) == 1
	})
	stopped := make(chan struct{})
	go func() {
		r.concierge.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop did not return")
	}
	r.model.mu.Lock()
	rewrite := r.model.ctxs[0]
	r.model.mu.Unlock()
	if rewrite.Err() == nil {
		t.Fatal("the rewrite for the ear goes on after Stop")
	}
	select {
	case <-r.concierge.done:
	default:
		t.Fatal("the goroutine that offers answers goes on after Stop")
	}
	r.concierge.Stop() // a second Stop does nothing
}

// TestANewConciergeCancelsTheQuestionsOfEndedMeetings (PR #89's review,
// round 3): a meeting that ended while the voice was off, so with no
// Concierge to tell, keeps no working question: the next Concierge cancels
// them as it starts, and tells the page. A meeting still recording keeps its
// own.
func TestANewConciergeCancelsTheQuestionsOfEndedMeetings(t *testing.T) {
	r := newRig(t)
	r.ask(t, "agent-7", "is the tap merged", "ABC-12")
	if err := r.store.CreateSession("meeting-2", testNow); err != nil {
		t.Fatal(err)
	}
	if err := r.store.CreateVoiceTask(&storage.VoiceTask{ID: "evt-live", SessionID: "meeting-2", AskedAt: testNow, Holder: "session:agent-9"}); err != nil {
		t.Fatal(err)
	}
	r.concierge.Stop()
	if err := r.store.EndSession("meeting-1", testNow.Add(time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	var changed []string
	next, err := New(&Config{Generator: r.model, Store: r.store, Now: r.concierge.now, Changed: func(id string) { changed = append(changed, id) }})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Stop()
	if task, _ := r.store.VoiceTask("evt-1"); task.State != storage.VoiceTaskCancelled {
		t.Fatalf("the ended meeting's question = %+v, want cancelled", task)
	}
	if task, _ := r.store.VoiceTask("evt-live"); task.State != storage.VoiceTaskWorking {
		t.Fatalf("the recording meeting's question = %+v, want working", task)
	}
	if len(changed) != 1 || changed[0] != "meeting-1" {
		t.Fatalf("pages told = %q, want meeting-1", changed)
	}
}

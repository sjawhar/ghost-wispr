package concierge

import (
	"context"
	"slices"
	"strings"
	"sync"
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
	if err := r.store.EndSession("meeting-1", testNow.Add(time.Minute), "", ""); err != nil {
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
	// PR #89's review, round 5: the holder is told, once the unit's Envoy
	// session is open; the recording meeting's holder is not.
	next.SetAgents(r.agents)
	eventually(t, "the ended meeting's holder was told", func() bool { return notices(r, "agent-7") == 1 })
	if n := notices(r, "agent-9"); n != 0 {
		t.Fatalf("notices to the recording meeting's holder = %d, want 0", n)
	}
}

// notices counts the notices, asking no reply, sent to agent.
func notices(r *rig, agent string) int {
	r.agents.mu.Lock()
	defer r.agents.mu.Unlock()
	n := 0
	for _, sent := range r.agents.sent {
		if sent.NoReply && sent.Target == agent {
			n++
		}
	}
	return n
}

// TestAMeetingsEndTellsTheHolder (PR #89's review, round 5): a meeting's end
// cancels its questions and tells each holder not to answer, by time and
// issue, never the question's words.
func TestAMeetingsEndTellsTheHolder(t *testing.T) {
	r := newRig(t)
	r.ask(t, "agent-7", "is the tap merged", "ABC-12")
	if err := r.concierge.SessionEnded("meeting-1"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the holder was told", func() bool { return notices(r, "agent-7") == 1 })
	r.agents.mu.Lock()
	last := r.agents.sent[len(r.agents.sent)-1]
	r.agents.mu.Unlock()
	if !strings.Contains(last.Message, "about ABC-12") || strings.Contains(last.Message, "tap") {
		t.Fatalf("notice = %q, want the issue and none of the question's words", last.Message)
	}
}

// TestTurningTheVoiceOffCancelsItsQuestions (PR #89's review, round 4):
// turning the voice off cancels every question still working, in a meeting
// still recording too, and tells each page; else a late reply plays the
// answer sound once the voice is back on. As after a meeting's end, the late
// reply still lands in the ledger, for the page, and is not announced. A stop
// alone, at shutdown, keeps them: the unit's Envoy session survives a
// restart, and the reply lands then.
func TestTurningTheVoiceOffCancelsItsQuestions(t *testing.T) {
	r := newRig(t)
	r.ask(t, "agent-7", "is the tap merged", "ABC-12")
	r.concierge.Stop()
	if task, _ := r.store.VoiceTask("evt-1"); task.State != storage.VoiceTaskWorking {
		t.Fatalf("a stop at shutdown: the question = %+v, want working", task)
	}

	r = newRig(t)
	var mu sync.Mutex
	var told []string
	r.concierge.changed = func(sessionID string) {
		mu.Lock()
		defer mu.Unlock()
		told = append(told, sessionID)
	}
	r.ask(t, "agent-7", "is the tap merged", "ABC-12")
	mu.Lock()
	told = nil
	mu.Unlock()
	r.concierge.VoiceOff()
	select {
	case <-r.concierge.done:
	default:
		t.Fatal("VoiceOff did not stop the Concierge")
	}
	if task, _ := r.store.VoiceTask("evt-1"); task.State != storage.VoiceTaskCancelled {
		t.Fatalf("the question after the voice went off = %+v, want cancelled", task)
	}
	mu.Lock()
	pages := slices.Clone(told)
	mu.Unlock()
	if !slices.Equal(pages, []string{"meeting-1"}) {
		t.Fatalf("pages told = %q, want meeting-1", pages)
	}
	// PR #89's review, round 5: the holder is told not to answer.
	if n := notices(r, "agent-7"); n != 1 {
		t.Fatalf("notices to the holder = %d, want 1", n)
	}

	// The voice is on again: a new Concierge on the same ledger.
	next, err := New(&Config{Generator: r.model, Store: r.store, Now: r.concierge.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(next.Stop)
	next.SetAgents(r.agents)
	r.concierge = next
	if err := next.Deliver(context.Background(), &envoy.Message{EventID: "late", SourceSession: "agent-7", InReplyTo: "evt-1", Text: "Merged."}); err != nil {
		t.Fatal(err)
	}
	if task, _ := r.store.VoiceTask("evt-1"); task.State != storage.VoiceTaskCancelled || task.AnswerWritten != "Merged." {
		t.Fatalf("the late reply = %+v, want it kept and the question cancelled", task)
	}
	noAnnouncement(t, r, 400*time.Millisecond, "a reply to a question asked before the voice went off")
}

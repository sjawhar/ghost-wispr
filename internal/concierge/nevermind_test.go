package concierge

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// TestNeverMindCancelsTheNewestWorkingTask: "Wispr, never mind" cancels the
// meeting's newest working question, and tells its holder in a short message
// that asks no reply. A second cancels the one before it; with none left,
// nothing is sent.
func TestNeverMindCancelsTheNewestWorkingTask(t *testing.T) {
	r := newRig(t)
	logs := r.captureLogs()
	r.ask(t, "agent-7", "first question", "ABC-12")
	r.ask(t, "agent-8", "second question", "")

	if err := r.concierge.NeverMind(context.Background(), "meeting-1"); err != nil {
		t.Fatalf("NeverMind: %v", err)
	}
	newest, _ := r.store.VoiceTask("evt-2")
	older, _ := r.store.VoiceTask("evt-1")
	if newest.State != storage.VoiceTaskCancelled || older.State != storage.VoiceTaskWorking {
		t.Fatalf("after one never mind: evt-2 %s, evt-1 %s; want cancelled and working", newest.State, older.State)
	}
	if len(r.agents.sent) != 3 {
		t.Fatalf("sent %d messages, want the two questions and one cancel", len(r.agents.sent))
	}
	cancel := r.agents.sent[2]
	if cancel.Target != "agent-8" || cancel.InReplyTo != "evt-2" || !cancel.NoReply || cancel.IdempotencyKey == "" {
		t.Fatalf("cancel message = %+v", cancel)
	}
	if !strings.Contains(cancel.Message, "do not answer") || strings.Contains(cancel.Message, "second question") {
		t.Fatalf("cancel text = %q, want a short notice that quotes nothing", cancel.Message)
	}
	if !strings.Contains(logs.String(), `msg="concierge told an agent its question was cancelled" task=evt-2 request=evt-3`) {
		t.Fatalf("the log lacks the cancel line:\n%s", logs)
	}

	if err := r.concierge.NeverMind(context.Background(), "meeting-1"); err != nil {
		t.Fatal(err)
	}
	if older, _ := r.store.VoiceTask("evt-1"); older.State != storage.VoiceTaskCancelled {
		t.Fatalf("after a second never mind: evt-1 %s, want cancelled", older.State)
	}
	if err := r.concierge.NeverMind(context.Background(), "meeting-1"); err != nil {
		t.Fatal(err)
	}
	if len(r.agents.sent) != 4 {
		t.Fatalf("sent %d messages, want no cancel once none is working", len(r.agents.sent))
	}
}

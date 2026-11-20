package concierge

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// TestNeverMindCancelsTheNewestWorkingTask: "Wispr, never mind" cancels the
// meeting's newest working question, tells its holder in a short message
// that asks no reply, and reports that it cancelled one. A second cancels the
// one before it; with none left, nothing is sent and it reports none, so the
// voice drops its newest held answer instead.
func TestNeverMindCancelsTheNewestWorkingTask(t *testing.T) {
	r := newRig(t)
	logs := r.captureLogs()
	r.ask(t, "agent-7", "first question", "ABC-12")
	r.ask(t, "agent-8", "second question", "")

	if cancelled, err := r.concierge.NeverMind(context.Background(), "meeting-1"); err != nil || !cancelled {
		t.Fatalf("NeverMind = %v, %v; want a working question cancelled", cancelled, err)
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

	if cancelled, err := r.concierge.NeverMind(context.Background(), "meeting-1"); err != nil || !cancelled {
		t.Fatalf("second NeverMind = %v, %v; want the older question cancelled", cancelled, err)
	}
	if older, _ := r.store.VoiceTask("evt-1"); older.State != storage.VoiceTaskCancelled {
		t.Fatalf("after a second never mind: evt-1 %s, want cancelled", older.State)
	}
	if cancelled, err := r.concierge.NeverMind(context.Background(), "meeting-1"); err != nil || cancelled {
		t.Fatalf("NeverMind with no working question = %v, %v; want none cancelled", cancelled, err)
	}
	if len(r.agents.sent) != 4 {
		t.Fatalf("sent %d messages, want no cancel once none is working", len(r.agents.sent))
	}
}

// answeredMeanwhile is the ledger with the first task NeverMind tries to
// cancel answered just before the cancel lands, as when its holder's answer
// arrives during the never mind.
type answeredMeanwhile struct {
	Store
	answered bool
}

func (s *answeredMeanwhile) EndVoiceTask(id string, state storage.VoiceTaskState) (bool, error) {
	if !s.answered {
		s.answered = true
		if _, err := s.AnswerVoiceTask(id, "It ships tomorrow."); err != nil {
			return false, err
		}
	}
	return s.Store.EndVoiceTask(id, state)
}

// TestNeverMindCancelsTheNextWorkingTaskWhenTheNewestIsAnsweredMeanwhile fails
// if a never mind whose newest working question was answered just before its
// cancel reports no working question while an older one still works: the
// voice would then drop a held answer and leave that question working. It
// cancels the newest question still working.
func TestNeverMindCancelsTheNextWorkingTaskWhenTheNewestIsAnsweredMeanwhile(t *testing.T) {
	r := newRig(t)
	r.ask(t, "agent-7", "first question", "ABC-12")
	r.ask(t, "agent-8", "second question", "")
	r.concierge.store = &answeredMeanwhile{Store: r.store}

	if cancelled, err := r.concierge.NeverMind(context.Background(), "meeting-1"); err != nil || !cancelled {
		t.Fatalf("NeverMind = %v, %v; want the older working question cancelled", cancelled, err)
	}
	if newest, _ := r.store.VoiceTask("evt-2"); newest.State != storage.VoiceTaskDone {
		t.Fatalf("evt-2 %s, want done: it was answered", newest.State)
	}
	if older, _ := r.store.VoiceTask("evt-1"); older.State != storage.VoiceTaskCancelled {
		t.Fatalf("evt-1 %s, want cancelled", older.State)
	}
}

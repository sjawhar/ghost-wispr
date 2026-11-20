package concierge

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

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

	if cancelled, err := r.concierge.NeverMind(context.Background(), "meeting-1", r.concierge.QuestionsAsked()); err != nil || !cancelled {
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

	if cancelled, err := r.concierge.NeverMind(context.Background(), "meeting-1", r.concierge.QuestionsAsked()); err != nil || !cancelled {
		t.Fatalf("second NeverMind = %v, %v; want the older question cancelled", cancelled, err)
	}
	if older, _ := r.store.VoiceTask("evt-1"); older.State != storage.VoiceTaskCancelled {
		t.Fatalf("after a second never mind: evt-1 %s, want cancelled", older.State)
	}
	if cancelled, err := r.concierge.NeverMind(context.Background(), "meeting-1", r.concierge.QuestionsAsked()); err != nil || cancelled {
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

	if cancelled, err := r.concierge.NeverMind(context.Background(), "meeting-1", r.concierge.QuestionsAsked()); err != nil || !cancelled {
		t.Fatalf("NeverMind = %v, %v; want the older working question cancelled", cancelled, err)
	}
	if newest, _ := r.store.VoiceTask("evt-2"); newest.State != storage.VoiceTaskDone {
		t.Fatalf("evt-2 %s, want done: it was answered", newest.State)
	}
	if older, _ := r.store.VoiceTask("evt-1"); older.State != storage.VoiceTaskCancelled {
		t.Fatalf("evt-1 %s, want cancelled", older.State)
	}
}

// TestNeverMindCancelsOnlyAQuestionAskedBeforeIt fails if a never mind
// cancels a question asked after it was said, as one asked while an earlier
// never mind still told its holder: it cancels the newest working question
// asked before it.
func TestNeverMindCancelsOnlyAQuestionAskedBeforeIt(t *testing.T) {
	r := newRig(t)
	r.ask(t, "agent-7", "first question", "ABC-12")
	said := r.concierge.QuestionsAsked() // the never mind is said
	r.ask(t, "agent-8", "later question", "")

	if cancelled, err := r.concierge.NeverMind(context.Background(), "meeting-1", said); err != nil || !cancelled {
		t.Fatalf("NeverMind = %v, %v; want the question asked before it cancelled", cancelled, err)
	}
	if task, _ := r.store.VoiceTask("evt-2"); task.State != storage.VoiceTaskWorking {
		t.Fatalf("the question asked after the never mind is %s, want working", task.State)
	}
	if task, _ := r.store.VoiceTask("evt-1"); task.State != storage.VoiceTaskCancelled {
		t.Fatalf("evt-1 %s, want cancelled", task.State)
	}
}

// steppedClock is a wall clock a test sets, as one that steps back.
type steppedClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *steppedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *steppedClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// TestNeverMindsKeepTheirOrderWhenTheClockStepsBack fails if two never minds
// said after one question, with the wall clock stepped back before a second
// question is asked, cancel that second question: it was asked after both,
// whatever its time says. The first cancels the earlier question; the second
// finds none it may cancel.
func TestNeverMindsKeepTheirOrderWhenTheClockStepsBack(t *testing.T) {
	r := newRig(t)
	clock := &steppedClock{now: testNow}
	r.concierge.now = clock.Now
	r.ask(t, "agent-7", "first question", "ABC-12")
	first, second := r.concierge.QuestionsAsked(), r.concierge.QuestionsAsked() // two never minds said
	clock.set(testNow.Add(-4 * time.Second))                                    // the wall clock steps back
	r.ask(t, "agent-8", "second question", "")

	if cancelled, err := r.concierge.NeverMind(context.Background(), "meeting-1", first); err != nil || !cancelled {
		t.Fatalf("first NeverMind = %v, %v; want the first question cancelled", cancelled, err)
	}
	if cancelled, err := r.concierge.NeverMind(context.Background(), "meeting-1", second); err != nil || cancelled {
		t.Fatalf("second NeverMind = %v, %v; want nothing it may cancel", cancelled, err)
	}
	if task, _ := r.store.VoiceTask("evt-1"); task.State != storage.VoiceTaskCancelled {
		t.Fatalf("evt-1 %s, want cancelled", task.State)
	}
	if task, _ := r.store.VoiceTask("evt-2"); task.State != storage.VoiceTaskWorking {
		t.Fatalf("the question asked after both never minds is %s, want working", task.State)
	}
}

// TestNeverMindWithAStoppedVoiceCancelsNothing fails if a never mind whose
// context the voice's shutdown cancelled still cancels a question: one that
// raced the shutdown must leave the ledger as it is.
func TestNeverMindWithAStoppedVoiceCancelsNothing(t *testing.T) {
	r := newRig(t)
	r.ask(t, "agent-7", "first question", "ABC-12")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if cancelled, err := r.concierge.NeverMind(ctx, "meeting-1", r.concierge.QuestionsAsked()); err == nil || cancelled {
		t.Fatalf("NeverMind with a cancelled context = %v, %v; want nothing cancelled and its error", cancelled, err)
	}
	if task, _ := r.store.VoiceTask("evt-1"); task.State != storage.VoiceTaskWorking {
		t.Fatalf("evt-1 %s, want working", task.State)
	}
}

// neverMindInRekey is the ledger that runs a never mind right after a
// question's task takes its Envoy event id, before the Concierge goes on.
type neverMindInRekey struct {
	Store
	run func()
}

func (s *neverMindInRekey) RekeyVoiceTask(id, newID string) error {
	if err := s.Store.RekeyVoiceTask(id, newID); err != nil {
		return err
	}
	if run := s.run; run != nil {
		s.run = nil
		run()
	}
	return nil
}

// TestANeverMindDuringAQuestionsRekeyLeavesThatQuestion fails if a never mind
// said before a question, whose Concierge call runs just as that question's
// task takes its Envoy event id, cancels that later question: the task's
// place among the questions goes with its row, in the rekey's own statement.
func TestANeverMindDuringAQuestionsRekeyLeavesThatQuestion(t *testing.T) {
	r := newRig(t)
	r.ask(t, "agent-7", "first question", "ABC-12")
	said := r.concierge.QuestionsAsked() // the never mind is said
	var cancelled bool
	var neverMindErr error
	r.concierge.store = &neverMindInRekey{Store: r.store, run: func() {
		cancelled, neverMindErr = r.concierge.NeverMind(context.Background(), "meeting-1", said)
	}}
	r.ask(t, "agent-8", "later question", "")

	if neverMindErr != nil || !cancelled {
		t.Fatalf("NeverMind during the later question's rekey = %v, %v; want the first question cancelled", cancelled, neverMindErr)
	}
	if task, _ := r.store.VoiceTask("evt-2"); task.State != storage.VoiceTaskWorking {
		t.Fatalf("the question asked after the never mind is %s, want working", task.State)
	}
	if task, _ := r.store.VoiceTask("evt-1"); task.State != storage.VoiceTaskCancelled {
		t.Fatalf("evt-1 %s, want cancelled", task.State)
	}
}

// TestNeverMindCancelsTheNewestQuestionWhenTheClockStepsBackBetweenThem fails
// if a never mind cancels the older of two working questions because the
// wall clock stepped back between them, which gives the newer one the
// earlier time: the newest question is the one asked last, by its place.
func TestNeverMindCancelsTheNewestQuestionWhenTheClockStepsBackBetweenThem(t *testing.T) {
	r := newRig(t)
	clock := &steppedClock{now: testNow}
	r.concierge.now = clock.Now
	r.ask(t, "agent-7", "first question", "ABC-12")
	clock.set(testNow.Add(-4 * time.Second)) // the wall clock steps back
	r.ask(t, "agent-8", "second question", "")

	if cancelled, err := r.concierge.NeverMind(context.Background(), "meeting-1", r.concierge.QuestionsAsked()); err != nil || !cancelled {
		t.Fatalf("NeverMind = %v, %v; want the newest question cancelled", cancelled, err)
	}
	if task, _ := r.store.VoiceTask("evt-2"); task.State != storage.VoiceTaskCancelled {
		t.Fatalf("evt-2, asked last, %s; want cancelled", task.State)
	}
	if task, _ := r.store.VoiceTask("evt-1"); task.State != storage.VoiceTaskWorking {
		t.Fatalf("evt-1, asked first, %s; want working", task.State)
	}
}

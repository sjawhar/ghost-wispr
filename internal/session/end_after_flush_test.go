package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// orderedStore is storeMock with the order of its segment writes and session
// ends, and a first segment write held until release closes.
type orderedStore struct {
	*storeMock
	mu       sync.Mutex
	events   []string
	entered  chan struct{}
	release  chan struct{}
	holdOnce sync.Once
}

func (s *orderedStore) note(event string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

func (s *orderedStore) order() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

func (s *orderedStore) EndSession(id string, endedAt time.Time, audioPath, queueEvent string) error {
	s.note("end")
	return s.storeMock.EndSession(id, endedAt, audioPath, queueEvent)
}

func newOrderedStore() *orderedStore {
	s := &orderedStore{storeMock: newStoreMock(), entered: make(chan struct{}), release: make(chan struct{})}
	s.appendHook = func() {
		s.holdOnce.Do(func() {
			close(s.entered)
			<-s.release
		})
		s.note("append")
	}
	return s
}

// TestASessionEndsOnlyAfterTheWordsAnotherEndIsFlushing fails if a session
// end (the stop's ForceEndSession) ends the session while another end of it
// (a pause's settlement) is still storing the words it took from the buffer.
// The session would end, and its summary start, without its last words, which
// land after the end; and at a shutdown the summary that end starts is in the
// store when the deadline closes it (cmd/ghost-wispr's
// TestAPausesSettlementIsWorkTheStopWaitsFor failed so, 1 run in 200).
func TestASessionEndsOnlyAfterTheWordsAnotherEndIsFlushing(t *testing.T) {
	for _, outlast := range []bool{false, true} {
		name := "the flush ends within the second end's deadline"
		if outlast {
			name = "the flush outlasts the second end's deadline"
		}
		t.Run(name, func(t *testing.T) {
			store := newOrderedStore()
			manager := NewManager(store, nil, nil, nil, NewDetector(time.Hour), 0)
			if _, err := manager.ManualStartSession(context.Background(), ""); err != nil {
				t.Fatal(err)
			}
			if _, err := manager.MessageKept(liveResult(t, true, false, "Last", "words.")); err != nil {
				t.Fatal(err)
			}
			first := make(chan error, 1)
			go func() { first <- manager.ForceEndSession(context.Background()) }() // the pause's settlement
			select {
			case <-store.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("control: the first end did not flush the buffered words")
			}

			deadline := 200 * time.Millisecond
			if !outlast {
				deadline = 5 * time.Second
				time.AfterFunc(100*time.Millisecond, func() { close(store.release) })
			}
			ctx, cancel := context.WithTimeout(context.Background(), deadline)
			defer cancel()
			err := manager.ForceEndSession(ctx) // the stop's
			if outlast {
				if got := store.order(); len(got) != 0 {
					t.Fatalf("the second end, whose deadline passed while the first end's words were still being stored, did %v; want nothing", got)
				}
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("the second end = %v, want its deadline", err)
				}
				close(store.release)
			}
			select {
			case err := <-first:
				if err != nil && !errors.Is(err, ErrNoActiveSession) {
					t.Errorf("the first end = %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the first end did not return")
			}
			if got := store.order(); len(got) != 2 || got[0] != "append" || got[1] != "end" {
				t.Fatalf("store writes %v; want the words appended, then the session ended once", got)
			}
		})
	}
}

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

// TestAnEndUnderWayWaitsForTheWordsAFlushTookDuringIt fails if an end that
// has already claimed the session (here, saving its recording) writes the
// session's end, and starts its summary, while a flush that took words after
// its claim (a pause's, of a final that came during the save) is still
// storing them: the words would land in an ended session, missing from its
// summary. GPT-6.1 Sol's reverse-order probe at dbbe4476, which fails on main
// too.
func TestAnEndUnderWayWaitsForTheWordsAFlushTookDuringIt(t *testing.T) {
	store := newOrderedStore()
	recorder := &recorderMock{}
	enteredSave, releaseSave := blockFirstEnd(recorder)
	manager := NewManager(store, recorder, nil, nil, NewDetector(time.Hour), 0)
	if _, err := manager.ManualStartSession(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	first := make(chan error, 1)
	go func() { first <- manager.ForceEndSession(context.Background()) }() // claims, then saves the recording
	waitClosed(t, enteredSave, "the first end to claim the session and save its recording")
	if _, err := manager.MessageKept(liveResult(t, true, false, "Last", "words.")); err != nil {
		t.Fatal(err)
	}
	manager.PauseCapture()
	second := make(chan error, 1)
	go func() { second <- manager.ForceEndSession(context.Background()) }() // the pause's: flushes, then waits
	waitClosed(t, store.entered, "the pause's flush to store the words")
	close(releaseSave)
	select {
	case err := <-first:
		t.Fatalf("the first end returned (%v) while the words it must hold were still being stored; store writes %v", err, store.order())
	case <-time.After(300 * time.Millisecond):
	}
	if got := store.order(); len(got) != 0 {
		t.Fatalf("store writes %v while the pause's flush held its words; want none", got)
	}
	close(store.release)
	for _, ch := range []chan error{first, second} {
		select {
		case err := <-ch:
			if err != nil && !errors.Is(err, ErrNoActiveSession) {
				t.Errorf("an end = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("an end did not return once the words were stored")
		}
	}
	if got := store.order(); len(got) != 2 || got[0] != "append" || got[1] != "end" {
		t.Fatalf("store writes %v; want the words appended, then the session ended once", got)
	}
}

// TestWordsAFlushFindsAfterTheEndSealedGoToNoEndedSession fails if a flush
// that starts once an end has begun writing the session's end (sealed it)
// stores its words in that session: after the write they would land in an
// ended session, which a pause or a stop has closed. The flush leaves them in
// the buffer, for the end to sort: words captured before the recording
// closed are dropped, as words that come after the end are, and later words
// open the next session.
func TestWordsAFlushFindsAfterTheEndSealedGoToNoEndedSession(t *testing.T) {
	store := newOrderedStore()
	holdEnd := make(chan struct{})
	writingEnd := make(chan struct{})
	var once sync.Once
	store.appendHook = func() { store.note("append") }
	endStore := &endHeldStore{orderedStore: store, writing: writingEnd, release: holdEnd, once: &once}
	manager := NewManager(endStore, &recorderMock{}, nil, nil, NewDetector(time.Hour), 0)
	ended, err := manager.ManualStartSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	first := make(chan error, 1)
	go func() { first <- manager.ForceEndSession(context.Background()) }()
	waitClosed(t, writingEnd, "the end to write the session's end")
	if _, err := manager.MessageKept(liveResult(t, true, true, "After", "the", "seal.")); err != nil {
		t.Fatal(err)
	}
	close(holdEnd)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if text := segmentText(t, store.storeMock, ended); text != "" {
		t.Fatalf("the ended session %s got %q, which a flush took after its end began writing", ended, text)
	}
}

// endHeldStore holds the first EndSession until release closes, after
// closing writing.
type endHeldStore struct {
	*orderedStore
	writing, release chan struct{}
	once             *sync.Once
}

func (s *endHeldStore) EndSession(id string, endedAt time.Time, audioPath, queueEvent string) error {
	s.once.Do(func() {
		close(s.writing)
		<-s.release
	})
	return s.orderedStore.EndSession(id, endedAt, audioPath, queueEvent)
}

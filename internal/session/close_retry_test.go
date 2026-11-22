package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// closeRefusingStore refuses the first RecordSessionClose, as a store that
// cannot write would, and keeps the close time each later one stores and the
// end time EndSession stores.
type closeRefusingStore struct {
	*storeMock
	mu       sync.Mutex
	refused  bool
	closedAt []time.Time
	endedAt  []time.Time
}

func (s *closeRefusingStore) RecordSessionClose(_ string, closedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.refused {
		s.refused = true
		return errors.New("planted: the store refused the close time")
	}
	s.closedAt = append(s.closedAt, closedAt)
	return nil
}

func (s *closeRefusingStore) EndSession(id string, endedAt time.Time, audioPath, queueEvent string) error {
	s.mu.Lock()
	s.endedAt = append(s.endedAt, endedAt)
	s.mu.Unlock()
	return s.storeMock.EndSession(id, endedAt, audioPath, queueEvent)
}

// TestAnEndWhoseCloseTimeWasRefusedLeavesTheRetryItsOwnTime fails if an end
// whose close-time write the store refused, which returns before it closes
// the recording (so the recording goes on), hands that attempt's time to the
// retry: the retry would store a close and an end before the audio the
// recording took since (GPT-6.1 Sol's probe at 6f49011e: 202 ms before the
// raw recording's last write, growing while it records). The retry takes the
// time it closes the recording.
func TestAnEndWhoseCloseTimeWasRefusedLeavesTheRetryItsOwnTime(t *testing.T) {
	store := &closeRefusingStore{storeMock: newStoreMock()}
	recorder := &recorderMock{}
	manager := NewManager(store, recorder, nil, nil, NewDetector(time.Hour), 0)
	if _, err := manager.ManualStartSession(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if err := manager.ForceEndSession(context.Background()); err == nil {
		t.Fatal("control: the end whose close-time write was refused succeeded")
	}
	recorder.mu.Lock()
	closed := recorder.ended
	recorder.mu.Unlock()
	if closed != 0 {
		t.Fatalf("control: the recorder closed %d times after a refused close-time write; want none, the recording goes on", closed)
	}
	time.Sleep(50 * time.Millisecond) // the recording takes audio meanwhile
	retried := time.Now().UTC()
	if err := manager.ForceEndSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.closedAt) != 1 || store.closedAt[0].Before(retried) {
		t.Errorf("the retry stored the close times %v; want one, no earlier than the retry at %s", store.closedAt, retried.Format(time.RFC3339Nano))
	}
	if len(store.endedAt) != 1 || store.endedAt[0].Before(retried) {
		t.Errorf("the session ended at %v; want once, no earlier than the retry at %s", store.endedAt, retried.Format(time.RFC3339Nano))
	}
}

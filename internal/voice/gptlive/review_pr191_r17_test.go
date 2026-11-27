package gptlive

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// TestAStaleRoomSnapshotCompletesSilently (Sol's seventeenth review of #191,
// P2 4): two delegations read Room concurrently. The older captures an
// earlier revision and is suspended; the newer reads a later one after a
// replacement and answers first. The older must not then run its stale
// snapshot through the Concierge or rewind the applied revision: it completes
// silently (Main). Control: an unchanged revision runs the older normally.
func TestAStaleRoomSnapshotCompletesSilently(t *testing.T) {
	for _, changed := range []bool{false, true} {
		name := "control: unchanged revision"
		if changed {
			name = "a newer revision applied first"
		}
		t.Run(name, func(t *testing.T) {
			srv := newLiveServer(t)
			tools := newFakeTools(voice.AnswerResult{Say: "The answer is available."})
			x := testExchange(newFakeOutput(), tools)
			var mu sync.Mutex
			room := voice.RoomView{Live: true, Asker: "what about ABC-42?", Request: "what about ABC-42?", Revision: 1}
			reads := 0
			captured := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			x.Room = func() voice.RoomView {
				mu.Lock()
				v := room
				reads++
				n := reads
				mu.Unlock()
				// Read 1 is newSession's opening revision, read 2 the older
				// delegation's arrival (onDelegation), read 3 its worker's.
				if n == 3 {
					close(captured)
					<-release
				}
				return v
			}
			sess, c, _ := openSession(t, srv, x)
			s := sess.(*session)
			c.heard("what about ABC-42?")
			c.delegate("older")
			select {
			case <-captured:
			case <-time.After(testWait):
				t.Fatal("older delegation never captured Room")
			}
			if changed {
				mu.Lock()
				room = voice.RoomView{Live: true, Asker: "what about ABC-12?", Request: "what about ABC-12?", Revision: 2, TakeBacks: 1}
				mu.Unlock()
			}
			c.delegate("newer")
			newer := tools.call(t)
			var args voice.AnswerArgs
			if err := json.Unmarshal(newer.Args, &args); err != nil {
				t.Fatal(err)
			}
			if newer.ID != "newer" {
				t.Fatalf("first call was %s, want newer", newer.ID)
			}
			if changed && args.Question != "what about ABC-12?" {
				t.Fatalf("newer question %q", args.Question)
			}
			once.Do(func() { close(release) })
			s.mu.Lock()
			applied := s.lastAppliedRevision
			s.mu.Unlock()
			if changed {
				// The stale older snapshot must run no tool and leave the
				// applied revision at the newer value.
				select {
				case call := <-tools.calls:
					t.Errorf("stale snapshot ran a tool: %s", string(call.Args))
				case <-time.After(300 * time.Millisecond):
				}
				if applied < 2 {
					t.Errorf("applied revision rewound to %d", applied)
				}
				return
			}
			older := tools.call(t)
			if err := json.Unmarshal(older.Args, &args); err != nil {
				t.Fatal(err)
			}
			if args.Question != "what about ABC-42?" {
				t.Errorf("unchanged older question %q", args.Question)
			}
		})
	}
}

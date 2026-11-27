package gptlive

import (
	"sync"

	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// roomStub is a settable RoomView, so a test can drive the room's current
// question the way the Supervisor does: the room is the only source of a
// delegation's question text (Main, after Sol's eighteenth review of #191).
type roomStub struct {
	mu   sync.Mutex
	view voice.RoomView
}

//nolint:gocritic // The stub keeps a copy of the view, as Exchange.Room returns one by value.
func (r *roomStub) set(v voice.RoomView) {
	r.mu.Lock()
	r.view = v
	r.mu.Unlock()
}

func (r *roomStub) get() voice.RoomView {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.view
}

// withRoom gives x a settable Room starting at a live view with asker and
// revision 1, and returns the stub.
func withRoom(x *voice.Exchange, asker string) *roomStub {
	r := &roomStub{view: voice.RoomView{Live: true, Asker: asker, Request: asker, Revision: 1}}
	x.Room = r.get
	return r
}

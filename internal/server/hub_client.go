package server

import "sync"

// subscriberQueue is the channel buffer of each subscriber (Hub.Subscribe).
const subscriberQueue = 64

// hubClient is one subscriber. An ordinary event that finds its channel full
// is dropped, as a page that falls that far behind can reload. A component's
// status is never dropped that way, since a page or the light keeps the last
// status it got: one that finds the channel full waits in pending, which keeps
// only the newest status of each component, and pump sends it once the
// subscriber reads. While a component's status waits, a newer one replaces it
// rather than overtaking it, so each subscriber gets a component's statuses
// in order and ends on its latest.
type hubClient struct {
	ch chan []byte

	mu      sync.Mutex
	pending map[string]pendingStatus
	gen     uint64

	kick   chan struct{}
	done   chan struct{}
	pumped chan struct{}
}

// pendingStatus is a status waiting for room, and gen tells it from a newer
// status of the same component that replaced it while pump was sending it.
type pendingStatus struct {
	payload []byte
	gen     uint64
}

func newHubClient() *hubClient {
	c := &hubClient{
		ch:      make(chan []byte, subscriberQueue),
		pending: make(map[string]pendingStatus),
		kick:    make(chan struct{}, 1),
		done:    make(chan struct{}),
		pumped:  make(chan struct{}),
	}
	go c.pump()
	return c
}

// send offers an ordinary event, and drops it if the channel is full.
func (c *hubClient) send(payload []byte) {
	select {
	case c.ch <- payload:
	default:
	}
}

// sendLatest offers component's status: at once if nothing of that component
// waits and the channel has room, else as the component's pending status.
func (c *hubClient) sendLatest(component string, payload []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, waiting := c.pending[component]; !waiting {
		select {
		case c.ch <- payload:
			return
		default:
		}
	}
	c.gen++
	c.pending[component] = pendingStatus{payload: payload, gen: c.gen}
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// pump sends the pending statuses as the subscriber makes room, until stop.
func (c *hubClient) pump() {
	defer close(c.pumped)
	for {
		select {
		case <-c.done:
			return
		case <-c.kick:
		}
		for {
			c.mu.Lock()
			var component string
			var next pendingStatus
			found := false
			for key, status := range c.pending {
				if !found || status.gen < next.gen {
					component, next, found = key, status, true
				}
			}
			c.mu.Unlock()
			if !found {
				break
			}
			select {
			case c.ch <- next.payload:
			case <-c.done:
				return
			}
			c.mu.Lock()
			if c.pending[component].gen == next.gen {
				delete(c.pending, component)
			}
			c.mu.Unlock()
		}
	}
}

// stop ends pump; the hub closes ch only after, so pump never sends on it
// closed.
func (c *hubClient) stop() {
	close(c.done)
	<-c.pumped
}

package main

import (
	"context"
	"slices"
	"sync"
	"time"
)

// projectKeysEvery is how often the unit reads Dispatch's project keys again.
const projectKeysEvery = 24 * time.Hour

// projectKeys holds Dispatch's project keys, read at start-up and each day:
// keyterms of the live transcript, and the projects whose spoken keys the
// voice writes as keys (R4).
type projectKeys struct {
	mu   sync.RWMutex
	keys []string
}

// Get returns the keys known, nil before the first read answered.
func (p *projectKeys) Get() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.keys
}

func (p *projectKeys) set(keys []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keys = slices.Clone(keys)
}

// watchProjectKeys reads the keys at once, within a few seconds, then every
// every in the background until ctx ends. A read that fails keeps the keys
// known.
func watchProjectKeys(ctx context.Context, read func(context.Context) ([]string, error), every time.Duration, keys *projectKeys, logf func(string, ...any)) {
	refresh := func() {
		readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		got, err := read(readCtx)
		if err != nil {
			logf("warning: read Dispatch's project keys, for the voice's keyterms: %v", err)
			return
		}
		keys.set(got)
	}
	refresh()
	go func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refresh()
			}
		}
	}()
}

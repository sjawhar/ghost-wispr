package main

import (
	"maps"
	"slices"
	"sync"
)

// warningsRegistry holds independently-owned startup and hardware warnings for
// GET /api/status without sharing mutable slices between goroutines.
type warningsRegistry struct {
	mu      sync.RWMutex
	entries map[string]string
}

func newWarningsRegistry() *warningsRegistry {
	return &warningsRegistry{entries: make(map[string]string)}
}

func (r *warningsRegistry) Set(key, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[key] = text
}

func (r *warningsRegistry) Clear(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, key)
}

func (r *warningsRegistry) List() []string {
	r.mu.RLock()
	entries := maps.Clone(r.entries)
	r.mu.RUnlock()

	keys := slices.Sorted(maps.Keys(entries))
	warnings := make([]string, 0, len(keys))
	for _, key := range keys {
		warnings = append(warnings, entries[key])
	}
	return warnings
}

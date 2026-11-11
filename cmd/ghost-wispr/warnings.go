package main

import (
	"maps"
	"slices"
	"strings"
	"sync"
)

// warningsRegistry holds independently-owned startup and hardware warnings for
// GET /api/status without sharing mutable slices between goroutines.
type warningsRegistry struct {
	mu                sync.RWMutex
	recordingMismatch sync.Mutex
	entries           map[string]string
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

// KeysWithPrefix returns a sorted snapshot of keys beginning with prefix.
func (r *warningsRegistry) KeysWithPrefix(prefix string) []string {
	r.mu.RLock()
	keys := make([]string, 0, len(r.entries))
	for key := range r.entries {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	r.mu.RUnlock()
	slices.Sort(keys)
	return keys
}

// WithRecordingMismatch serializes a mismatch result with a lifecycle change
// that clears its warning.
func (r *warningsRegistry) WithRecordingMismatch(fn func()) {
	r.recordingMismatch.Lock()
	defer r.recordingMismatch.Unlock()
	fn()
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

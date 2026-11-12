package main

import (
	"sync"
	"testing"
)

// TestWarningsRegistryIsRaceFree exercises concurrent hardware updates and
// status reads; it must be run with -race to catch unsynchronized access.
func TestWarningsRegistryIsRaceFree(t *testing.T) {
	registry := newWarningsRegistry()
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 100 {
				registry.Set("hardware", "Recording light not found; the unit records without its light")
				_ = registry.List()
				_ = registry.KeysWithPrefix("hardware")
				registry.Clear("hardware")
			}
		}()
	}
	workers.Wait()
	if warnings := registry.List(); len(warnings) != 0 {
		t.Fatalf("warnings after final clear = %v, want none", warnings)
	}
}

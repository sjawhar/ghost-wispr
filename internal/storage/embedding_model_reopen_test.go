package storage

import (
	"path/filepath"
	"testing"
	"time"
)

// TestANewEmbeddingModelKeepsItsVendorPathOnReopen catches the legacy model
// migration running again on gateway models written by the current build.
func TestANewEmbeddingModelKeepsItsVendorPathOnReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession("s", time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreEmbedding("s", 0, []float32{1}, "hash", "openai/text-embedding-3-small"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		store, err = NewSQLiteStore(path)
		if err != nil {
			t.Fatal(err)
		}
		stored, readErr := store.GetEmbeddings("s")
		closeErr := store.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("read: %v; close: %v", readErr, closeErr)
		}
		if len(stored) != 1 || stored[0].Model != "openai/text-embedding-3-small" {
			t.Fatalf("after reopen: %+v; want the gateway model's complete vendor path", stored)
		}
	}
}

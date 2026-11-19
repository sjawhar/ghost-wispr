package concierge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/genaiconfig"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// earReply is an invented agent reply of the ear corpus, with the issue key
// its question named.
type earReply struct {
	Key   string `json:"key"`
	Reply string `json:"reply"`
}

func readEarReplies(t *testing.T) []earReply {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "ear_replies.json"))
	if err != nil {
		t.Fatal(err)
	}
	var replies []earReply
	if err := json.Unmarshal(raw, &replies); err != nil {
		t.Fatal(err)
	}
	return replies
}

// TestEarFormOnSavedOutputs (c-ear, R8): the live model's rewrites of 40
// invented agent replies, saved by TestLiveEarCorpus, each hold at most 3
// sentences, 50 words and 20 words a sentence, and pass the check against
// their reply.
func TestEarFormOnSavedOutputs(t *testing.T) {
	replies := readEarReplies(t)
	raw, err := os.ReadFile(filepath.Join("testdata", "ear_rewrites.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rewrites []string
	if err := json.Unmarshal(raw, &rewrites); err != nil {
		t.Fatal(err)
	}
	if len(replies) != 40 || len(rewrites) != len(replies) {
		t.Fatalf("%d replies and %d saved rewrites, want 40 of each", len(replies), len(rewrites))
	}
	for i, rewrite := range rewrites {
		if reason := earForm(rewrite); reason != "" {
			t.Errorf("rewrite %d %q: %s", i, rewrite, reason)
		}
		if reason := earCheck(rewrite, replies[i].Reply, replies[i].Key); reason != "" {
			t.Errorf("rewrite %d %q of %q: %s", i, rewrite, replies[i].Reply, reason)
		}
	}
}

// TestLiveEarCorpus rewrites the corpus's replies with the live model, as
// forEar does for a holder's answer, and saves the rewrites where
// GHOST_WISPR_LIVE_EAR_OUT names. It runs only when GHOST_WISPR_LIVE_EAR=1
// and GHOST_WISPR_LIVE_GCP_PROJECT names a project (Google's default
// credentials, Vertex in GHOST_WISPR_LIVE_GCP_LOCATION, global by default).
func TestLiveEarCorpus(t *testing.T) {
	if os.Getenv("GHOST_WISPR_LIVE_EAR") != "1" {
		t.Skip("set GHOST_WISPR_LIVE_EAR=1 to rewrite the ear corpus with the live model")
	}
	project, out := os.Getenv("GHOST_WISPR_LIVE_GCP_PROJECT"), os.Getenv("GHOST_WISPR_LIVE_EAR_OUT")
	if project == "" || out == "" {
		t.Fatal("GHOST_WISPR_LIVE_GCP_PROJECT and GHOST_WISPR_LIVE_EAR_OUT must be set")
	}
	location := os.Getenv("GHOST_WISPR_LIVE_GCP_LOCATION")
	if location == "" {
		location = "global"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	generator, err := NewGenerator(ctx, &genaiconfig.Options{Backend: genaiconfig.BackendVertex, Project: project, Location: location})
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ear.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	c, err := New(&Config{Generator: generator, Store: store, Model: os.Getenv("GHOST_WISPR_LIVE_MODEL")})
	if err != nil {
		t.Fatal(err)
	}
	replies := readEarReplies(t)
	rewrites := make([]string, len(replies))
	for i, reply := range replies {
		started := time.Now()
		rewrites[i] = c.forEar(ctx, reply.Reply, reply.Key)
		t.Logf("%d (%s): %q", i, time.Since(started).Round(time.Millisecond), rewrites[i])
	}
	encoded, err := json.MarshalIndent(rewrites, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

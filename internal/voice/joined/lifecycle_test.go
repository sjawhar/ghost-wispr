package joined

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/earcheck"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// TestNoPointerOnceItsAgentLeftOrItsMeetingWent fails if a pointer still
// being prepared when its agent leaves, its meeting ends or its recording
// session is deleted is sent all the same. The delete goes through the
// store's own delete of the recording session and the roster's hook for it,
// as a session delete does.
func TestNoPointerOnceItsAgentLeftOrItsMeetingWent(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(t *testing.T, r *rig, store *storage.SQLiteStore)
	}{
		{"the agent leaves", func(t *testing.T, r *rig, _ *storage.SQLiteStore) {
			r.deliver(t, "agent-1", "ghost-wispr leave")
		}},
		{"the meeting ends", func(t *testing.T, r *rig, store *storage.SQLiteStore) {
			if err := store.EndSession("sess-1", r.clock.Now(), ""); err != nil {
				t.Fatal(err)
			}
			r.setSession("")
			r.roster.SessionEnded("sess-1")
		}},
		{"the recording session is deleted", func(t *testing.T, r *rig, store *storage.SQLiteStore) {
			if err := store.EndSession("sess-1", r.clock.Now(), ""); err != nil {
				t.Fatal(err)
			}
			r.setSession("")
			if _, err := store.DeleteSession("sess-1", t.TempDir()); err != nil {
				t.Fatal(err)
			}
			if _, err := store.GetSession("sess-1"); err == nil {
				t.Fatal("control: the delete left the recording session")
			}
			r.roster.SessionsDeleted([]string{"sess-1"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "unit.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			if err := store.CreateSession("sess-1", meetingStart); err != nil {
				t.Fatal(err)
			}
			if err := store.AppendSegment("sess-1", transcribe.Segment{Text: "ABC-12 is late", StartTime: 297, EndTime: 300, Timestamp: meetingStart.Add(5 * time.Minute)}); err != nil {
				t.Fatal(err)
			}
			// The pointer is held while it reads when its meeting started.
			entered, release := make(chan struct{}), make(chan struct{})
			var holding atomic.Bool
			r := newRig(t, func(cfg *Config) {
				cfg.Started = func(id string) time.Time {
					session, err := store.GetSession(id)
					if holding.CompareAndSwap(true, false) {
						close(entered)
						<-release
					}
					if err != nil {
						return time.Time{}
					}
					return session.StartedAt
				}
			})
			r.join(t, "agent-1", "ABC-12")
			holding.Store(true)
			done := make(chan struct{})
			go func() {
				defer close(done)
				r.roster.OnSegment(r.segment("ABC-12 is late", false))
			}()
			<-entered
			tc.end(t, r, store)
			if got := r.roster.Joined(); len(got) != 0 {
				t.Fatalf("control: the roster still lists %+v", got)
			}
			close(release)
			<-done
			r.roster.wg.Wait() // every send the roster started, none cancelled by the test
			if pointers := r.pointers("agent-1"); len(pointers) != 0 {
				t.Fatalf("a pointer was sent after %s: %q", tc.name, pointers[0].Message)
			}
		})
	}
}

// TestFloorTextIsCheckedWithItsAttribution fails if a floor request whose
// words pass the spoken-text check alone, but not once the voice names the
// agent first, is taken; or if one that passes with its attribution is
// refused.
func TestFloorTextIsCheckedWithItsAttribution(t *testing.T) {
	sentence := func(words int) string { return strings.TrimSpace(strings.Repeat("word ", words)) + "." }
	for _, tc := range []struct {
		name, draft string
		taken       bool
	}{
		{"a 20-word sentence becomes 23", sentence(20), false},
		{"48 words become 51", sentence(17) + " " + sentence(16) + " " + sentence(15), false},
		{"47 words become 50", sentence(17) + " " + sentence(15) + " " + sentence(15), true},
		{"a 17-word sentence becomes 20", sentence(17), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if reason := earcheck.Form(tc.draft); reason != "" {
				t.Fatalf("control: the draft alone fails the check: %s", reason)
			}
			r := newRig(t, nil)
			r.join(t, "agent-1", "ABC-12")
			before := len(r.agents.to("agent-1"))
			r.deliver(t, "agent-1", "ghost-wispr floor\n"+tc.draft)
			hands := r.floor.hands()
			if !tc.taken {
				if len(hands) != 0 {
					t.Fatalf("raised %q, which the voice cannot say", hands[0].Text)
				}
				eventually(t, "the refusal", func() bool { return len(r.agents.to("agent-1")) > before })
				if got := r.agents.to("agent-1")[before].Message; !strings.Contains(got, "ABC-12's agent says:") || !strings.Contains(got, "47 words") {
					t.Fatalf("refusal %q, want it to say the room hears the attribution and how many words are left", got)
				}
				return
			}
			if len(hands) != 1 {
				t.Fatalf("raised %d hands, want 1", len(hands))
			}
			if reason := earcheck.Form(hands[0].Text); reason != "" {
				t.Fatalf("raised %q, which fails the spoken-text check: %s", hands[0].Text, reason)
			}
		})
	}

	// Through a real Supervisor: what the voice says passes the check.
	sup := newSupervisorRig(t)
	r := newRig(t, func(cfg *Config) {
		cfg.Now = nil
		cfg.Floor = sup.s
		cfg.Session = func() string { return "sess-1" }
	})
	r.clock.set(time.Now())
	r.join(t, "agent-1", "ABC-12")
	r.deliver(t, "agent-1", "ghost-wispr floor\n"+sentence(17))
	sup.heard("Wispr, go ahead.")
	if spoken := sup.said(t); earcheck.Form(spoken) != "" {
		t.Fatalf("the voice said %q, which fails the spoken-text check: %s", spoken, earcheck.Form(spoken))
	}
}

// TestARepeatedJoinKeepsItsNotLiveDeadline fails if a join of an agent
// already on the roster, its redelivery among them, starts its 10 minutes not
// live in Envoy again; only Envoy reading it live does.
func TestARepeatedJoinKeepsItsNotLiveDeadline(t *testing.T) {
	r := newRig(t, nil)
	join := r.deliver(t, "agent-1", "ghost-wispr join ABC-12")
	r.agents.setState("agent-1", envoy.SessionInfo{SessionID: "agent-1", State: envoy.StateUnresponsive})
	r.roster.Sweep(context.Background())
	r.clock.advance(9 * time.Minute)
	if handled, err := r.roster.Deliver(context.Background(), join); !handled || err != nil {
		t.Fatalf("redelivered join: %v %v", handled, err)
	}
	r.deliver(t, "agent-1", "ghost-wispr join ABC-12")
	r.clock.advance(time.Minute)
	r.roster.Sweep(context.Background())
	if got := r.roster.Joined(); len(got) != 0 {
		t.Fatalf("still joined after 10 minutes not live: %+v", got)
	}

	// Envoy reading it live does start the 10 minutes again.
	r.join(t, "agent-2", "ABC-12")
	r.agents.setState("agent-2", envoy.SessionInfo{SessionID: "agent-2", State: envoy.StateUnresponsive})
	r.roster.Sweep(context.Background())
	r.clock.advance(9 * time.Minute)
	r.agents.setState("agent-2", envoy.SessionInfo{SessionID: "agent-2", State: envoy.StateLive})
	r.roster.Sweep(context.Background())
	r.agents.setState("agent-2", envoy.SessionInfo{SessionID: "agent-2", State: envoy.StateUnresponsive})
	r.roster.Sweep(context.Background())
	r.clock.advance(time.Minute)
	r.roster.Sweep(context.Background())
	if got := r.roster.Joined(); len(got) != 1 {
		t.Fatalf("an agent Envoy read live a minute ago left: %+v", got)
	}
}

// TestAJoinFromAGoneSessionIsRefused fails if a join from a session Envoy
// lists as gone is taken, or one from a session Envoy lists as unresponsive,
// not yet gone, is refused.
func TestAJoinFromAGoneSessionIsRefused(t *testing.T) {
	r := newRig(t, nil)
	r.agents.setState("gone-1", envoy.SessionInfo{SessionID: "gone-1", State: envoy.StateGone})
	r.deliver(t, "gone-1", "ghost-wispr join ABC-12")
	if got := r.roster.Joined(); len(got) != 0 {
		t.Fatalf("a session Envoy lists as gone joined: %+v", got)
	}
	eventually(t, "the refusal", func() bool { return len(r.agents.to("gone-1")) == 1 })
	if got := r.agents.to("gone-1")[0].Message; !strings.Contains(got, "gone") {
		t.Fatalf("refusal %q, want it to say Envoy lists the session as gone", got)
	}
	r.agents.setState("slow-1", envoy.SessionInfo{SessionID: "slow-1", State: envoy.StateUnresponsive})
	r.join(t, "slow-1", "ABC-12")
}

// TestTitleWordsAreR17s fails if two words of the agent's issue title of 5
// letters or more, whatever the words, send no pointer; or if one such word,
// another form of one (a plural), or shorter words do.
func TestTitleWordsAreR17s(t *testing.T) {
	for _, tc := range []struct {
		said string
		want bool
	}{
		{"We can talk after or before.", true},
		{"We can talk after lunch.", true},
		{"We can talk after it.", false},
		{"The records come after.", false},
		{"Lunch is at noon.", false},
	} {
		t.Run(tc.said, func(t *testing.T) {
			r := newRig(t, func(cfg *Config) {
				cfg.Title = func(context.Context, string) (string, error) { return "Record after lunch before meetings", nil }
			})
			r.join(t, "agent-1", "ABC-12")
			r.roster.OnSegment(r.segment(tc.said, false))
			if tc.want {
				eventually(t, "a pointer", func() bool { return len(r.pointers("agent-1")) == 1 })
				return
			}
			never(t, "a pointer for fewer than two title words", 200*time.Millisecond, func() bool { return len(r.pointers("agent-1")) > 0 })
		})
	}
}

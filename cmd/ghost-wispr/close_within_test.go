package main

import (
	"context"
	"testing"
	"time"
)

// TestANetworkCloseHoldsTheShutdownOnlyUntilItsDeadline fails if
// closeWithin, which runs the shutdown's network closes (the Deepgram
// connection's), waits past its context's end for a close that waits on its
// peer, or reports a close that ended as not ended. The control's close
// ends at once.
func TestANetworkCloseHoldsTheShutdownOnlyUntilItsDeadline(t *testing.T) {
	for _, stuck := range []bool{false, true} {
		name := "control: the close ends at once"
		if stuck {
			name = "the close waits on its peer"
		}
		t.Run(name, func(t *testing.T) {
			release := make(chan struct{})
			defer close(release)
			ended := make(chan struct{})
			closeConn := func() {
				defer close(ended)
				if stuck {
					<-release
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			returned := make(chan bool, 1)
			go func() { returned <- closeWithin(ctx, "the test's connection", closeConn) }()
			select {
			case done := <-returned:
				if done == stuck {
					t.Errorf("closeWithin reported the close ended: %t, want %t", done, !stuck)
				}
			case <-time.After(time.Second):
				t.Fatal("closeWithin waited past its deadline for a close that waits on its peer")
			}
			if !stuck {
				select {
				case <-ended:
				default:
					t.Error("control: the close had not run when closeWithin returned")
				}
			}
		})
	}
}

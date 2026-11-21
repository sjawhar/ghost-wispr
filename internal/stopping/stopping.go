// Package stopping holds the cause the app's work is cancelled with when the
// app stops. Work cut off with it did not fail: a writer that finds it as its
// context's cause leaves what it was doing as an exit inside it would, for the
// next start to take up, and records no failure.
package stopping

import (
	"context"
	"errors"
)

// ErrStopped is the cause of the context of work the app's stop cut off: the
// session manager's background work (session.Manager.StopBackground) and the
// app's own context at shutdown.
var ErrStopped = errors.New("the app is stopping")

// CutOff reports whether ctx was ended by the app's stop.
func CutOff(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), ErrStopped)
}

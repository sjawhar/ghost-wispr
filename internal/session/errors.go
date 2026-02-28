package session

import "errors"

// ErrNoActiveSession is returned by ForceEndSession when no session is active.
var ErrNoActiveSession = errors.New("no active session")

package session

import "errors"

// ErrNoActiveSession is returned by ForceEndSession when no session is active.
var ErrNoActiveSession = errors.New("no active session")

// ErrSessionAlreadyActive is returned by ManualStartSession when a session is already active.
var ErrSessionAlreadyActive = errors.New("session already active")

package session

import "errors"

// ErrNoActiveSession is returned by ForceEndSession when no session is active.
var ErrNoActiveSession = errors.New("no active session")

// ErrSessionAlreadyActive is returned by ManualStartSession when a session is already active.
var ErrSessionAlreadyActive = errors.New("session already active")

// ErrCapturePaused is returned when a session would start while microphone
// capture is paused.
var ErrCapturePaused = errors.New("capture is paused")

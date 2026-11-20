---
title: "A stop gate reads the source context, not a context derived from it"
category: general
tags:
  - concurrency
  - context
  - cancellation
  - voice
  - flaky-test
date: 2026-11-20
status: active
module: internal/voice/tts
problem_type: runtime_error
component: internal/voice/tts
severity: medium
root_cause: concurrency
resolution_type: code_fix
symptoms:
  - "TestInterruptCancelsQueuedSays fails about once in 130 runs: streamed [\"first\" \"queued\"]"
  - "after \"Wispr, stop\" the speech engine is asked to say a sentence that was only queued"
  - "a queued Say returns nil after a stop: its first frame reached the Output"
---

# A stop gate reads the source context, not a context derived from it

## Problem

Voice A (the streaming text-to-speech voice) gives each Say two contexts derived from a source: the generation context, which an Interrupt cancels, and the caller's context, the exchange's, which a stop cancels. The synthesis stage decided whether a queued Say may start by reading the Say's own derived contexts. Go cancels a context's children one at a time, in map order, after it sets the parent's error. So the Say in flight could end first, free the synthesis stage, and let it start a queued Say whose own context was not cancelled yet. The engine got the text after the stop; on the stop path a frame reached the Output too.

## What Didn't Work

- Sleeping in the test before the Interrupt: the race is not about the Say reaching its queue, it is inside the Interrupt.
- Widening the window by hanging many child contexts off the Say in flight: the synthesis stage reads that context's `Err`, which waits for its children to be cancelled, so the window closed.
- Widening it by queueing 100 Says behind the one in flight: the old gate still passed 3 of 200 runs at 2 CPUs. A test that only makes a race likely can pass on broken code.

## Solution

One gate, `session.stoppedLocked`, read under the session lock by the synthesis start, by each frame write, and by the choice between "a stop" and "the Output failed":

```go
if r.gen != s.gen { // Interrupt moves the generation under the same lock
	return ErrInterrupted
}
if r.caller.Err() != nil { // set before any context derived from it is cancelled
	return context.Cause(r.caller)
}
return nil
```

It holds only while every Say of one exchange gets the exchange's context itself; a child per Say would be cancelled one by one again.

## Testing it deterministically

A test can own the stop's cancellation order. Go's `context` package hands the cancellation of a derived context to a parent that has an `AfterFunc(func()) func() bool` method. `steppedContext` in `internal/voice/tts/tts_test.go` keeps those functions, sets its error, and cancels only the in-flight Say's derived contexts, which is the state the race needs. The stop test passes one as the exchange context. The Interrupt test, which is in the package, swaps the session's generation context and its cancel function for one: `Interrupt` calls that function under the session lock. Both tests make the old gate start the queued Say on every run.

## Prevention

- To ask "did a stop come?", read the context the stop cancels, or a counter moved under the lock the stop holds, never a context derived from it.
- Break a gate on purpose before trusting its test: remove the caller check and the stop test must fail; restore the old synthesis gate and the Interrupt test must fail.

## Related Issues

- CI failure on #109, run 44686215362, which does not touch the voice.

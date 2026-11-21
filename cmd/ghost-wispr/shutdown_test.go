package main

import (
	"context"
	"slices"
	"testing"
	"time"
)

// TestTheShutdownRunsItsStepsInOrder fails if main's shutdown
// (shutdownSteps.run) runs its steps out of order: the capture gate's
// openings must end (captureGate.Stop) before the app's context does, the
// light must go off only once the mic loops are joined, and the store must
// close last. It also fails if the steps that wait for the store's work, and
// the store's close, do not share one deadline of the given wait, or if the
// devices' stop is not told whether the mic loops ended.
func TestTheShutdownRunsItsStepsInOrder(t *testing.T) {
	for _, loopsEnd := range []bool{true, false} {
		var ran []string
		var ctxs []context.Context
		var devicesTold *bool
		step := func(name string) func() { return func() { ran = append(ran, name) } }
		withCtx := func(name string) func(context.Context) {
			return func(ctx context.Context) {
				ran = append(ran, name)
				ctxs = append(ctxs, ctx)
			}
		}
		steps := shutdownSteps{
			closeVoice:         step("close the voice"),
			stopCapture:        step("stop the gate's openings"),
			stopApp:            step("end the app's context"),
			stopStartPublishes: step("stop session_started sends"),
			joinMicLoops: func() bool {
				ran = append(ran, "join the mic loops")
				return loopsEnd
			},
			lightOff:      step("turn the light off"),
			stopServing:   withCtx("stop serving HTTP"),
			stopStoreWork: withCtx("stop the store's work"),
			stopDevices: func(ctx context.Context, micLoopsEnded bool) {
				withCtx("stop the devices")(ctx)
				devicesTold = &micLoopsEnded
			},
			closeStore: withCtx("close the store"),
		}
		started := time.Now()
		steps.run(time.Minute)
		want := []string{
			"close the voice",
			"stop the gate's openings",
			"end the app's context",
			"stop session_started sends",
			"join the mic loops",
			"turn the light off",
			"stop serving HTTP",
			"stop the store's work",
			"stop the devices",
			"close the store",
		}
		if !slices.Equal(ran, want) {
			t.Fatalf("the shutdown ran\n%q\nwant\n%q", ran, want)
		}
		for i, ctx := range ctxs {
			if ctx != ctxs[0] {
				t.Errorf("step %d of the deadline's steps has a context of its own", i)
			}
		}
		if deadline, ok := ctxs[0].Deadline(); !ok || deadline.Before(started.Add(time.Minute)) || deadline.After(time.Now().Add(time.Minute)) {
			t.Errorf("the deadline's steps have deadline %v (set: %t), want a minute from the shutdown's start", deadline, ok)
		}
		if devicesTold == nil || *devicesTold != loopsEnd {
			t.Errorf("the devices' stop was told the mic loops ended: %v, want %t", devicesTold, loopsEnd)
		}
	}
}

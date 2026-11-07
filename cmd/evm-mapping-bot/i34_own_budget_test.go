package main

import (
	"context"
	"testing"
	"time"
)

// OQ-11 (I-34): a step run with ownBudget after the tick's context ran out
// still has time; a stop (the parent cancelled) still ends it.
func TestOQ11_AStepAfterASlowTickGetsItsOwnBudget(t *testing.T) {
	parent, stop := context.WithCancel(context.Background())
	tick, tickCancel := context.WithTimeout(parent, time.Millisecond)
	defer tickCancel()
	<-tick.Done() // the scan overran the tick's budget
	ran := false
	ownBudget(parent, time.Minute, func(c context.Context) {
		ran = c.Err() == nil
	})
	if !ran {
		t.Fatal("the step after a slow tick got an expired context")
	}
	stop()
	ownBudget(parent, time.Minute, func(c context.Context) {
		if c.Err() == nil {
			t.Fatal("a stop did not reach the step")
		}
	})
}

package agent

import (
	"sync"
	"testing"
)

// A stop is either seen by the work's cleanup or refused: never accepted
// after the cleanup has looked.
func TestStopAndFinishAreOrdered(t *testing.T) {
	f := &inFlight{}
	if f.finish() {
		t.Fatal("unstopped work reported stopped")
	}
	ran := false
	if f.tryStop(func() { ran = true }) || ran || f.stopped.Load() {
		t.Fatal("a stop was accepted after the work finished")
	}

	g := &inFlight{}
	if !g.tryStop(nil) || !g.finish() {
		t.Fatal("a stop before the end wasn't seen by finish")
	}

	// Racing: whatever the order, an accepted stop is always seen.
	for range 200 {
		h := &inFlight{}
		var wg sync.WaitGroup
		var accepted, seen bool
		wg.Add(2)
		go func() { defer wg.Done(); accepted = h.tryStop(nil) }()
		go func() { defer wg.Done(); seen = h.finish() }()
		wg.Wait()
		if accepted && !seen {
			t.Fatal("a stop was accepted but the cleanup missed it")
		}
	}
}

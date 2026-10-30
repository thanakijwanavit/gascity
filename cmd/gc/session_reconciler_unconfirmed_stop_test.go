package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// g1-c42b7: the controller re-queues the async drain-ack stop every tick.
// When the provider cannot confirm the stop, the line is logged once per
// backend state (1003 identical lines in the G1 supervisor log before), a
// changed state is logged again, and no attempt pokes the controller: the
// runtime may still be alive.
// Not parallel — modifies the package-level drainAckAsyncStopPokeController seam.
func TestQueueDrainAckAsyncStopLogsUnconfirmedStopOncePerState(t *testing.T) {
	var pokeCalls int
	var pokeMu sync.Mutex
	old := drainAckAsyncStopPokeController
	drainAckAsyncStopPokeController = func(string) error {
		pokeMu.Lock()
		pokeCalls++
		pokeMu.Unlock()
		return nil
	}
	t.Cleanup(func() { drainAckAsyncStopPokeController = old })

	store := beads.NewMemStore()
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ambiguous := "acp backend: stop succeeded without liveness confirmation while default backend still reports the session running (launch records: acp=none, default=none)"
	survived := "default backend: stop returned but the runtime it started is still running (the configured backend holds no launch record)"
	var stderr synchronizedBuffer
	for i, stopErr := range []error{
		&runtime.StopUnconfirmedError{Name: "worker", Reason: ambiguous},
		&runtime.StopUnconfirmedError{Name: "worker", Reason: ambiguous, Repeat: true},
		&runtime.StopUnconfirmedError{Name: "worker", Reason: ambiguous, Repeat: true},
		&runtime.StopUnconfirmedError{Name: "worker", Reason: survived},
	} {
		sp.StopErrors = map[string]error{"worker": stopErr}
		tracker := &asyncStartTracker{}
		queueDrainAckAsyncStop("", store, sp, &config.City{}, "gc-worker", "worker", "", nil, tracker, &stderr)
		if !tracker.wait(time.Second) {
			t.Fatalf("async drain-ack stop #%d did not complete", i+1)
		}
	}

	out := stderr.String()
	if got := strings.Count(out, "async drain-ack stop worker: "); got != 2 {
		t.Fatalf("logged %d drain-ack stop failures, want 2 (one per state):\n%s", got, out)
	}
	if strings.Count(out, ambiguous) != 1 || strings.Count(out, survived) != 1 {
		t.Fatalf("stderr = %q, want each state logged once", out)
	}
	if !sp.IsRunning("worker") {
		t.Fatal("the fake runtime should still be running: every stop was unconfirmed")
	}
	pokeMu.Lock()
	got := pokeCalls
	pokeMu.Unlock()
	if got != 0 {
		t.Fatalf("poke count = %d, want 0 (an unconfirmed stop must not poke)", got)
	}
}

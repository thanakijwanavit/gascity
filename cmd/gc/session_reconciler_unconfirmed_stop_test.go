package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

const (
	unconfirmedAmbiguous = "acp backend: stop succeeded without liveness confirmation while default backend still reports the session running (launch records: acp=none, default=none)"
	unconfirmedSurvived  = "default backend: stop returned but the runtime it started is still running (the configured backend holds no launch record)"
)

func newUnconfirmedStop(name, reason string) error {
	return &runtime.StopUnconfirmedError{Name: name, Reason: reason}
}

func repeatedUnconfirmedStop(name, reason string) error {
	return &runtime.StopUnconfirmedError{Name: name, Reason: reason, Repeat: true}
}

// queuedStopFake answers each Stop with the next queued error and keeps the
// runtime running, the way the auto composite answers a stop it cannot
// confirm; the last error repeats once the queue is drained.
type queuedStopFake struct {
	*runtime.Fake
	mu    sync.Mutex
	errs  []error
	stops int
}

func (f *queuedStopFake) Stop(string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	err := f.errs[0]
	if len(f.errs) > 1 {
		f.errs = f.errs[1:]
	}
	return err
}

func (f *queuedStopFake) stopCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stops
}

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
	ambiguous, survived := unconfirmedAmbiguous, unconfirmedSurvived
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

// The confirm-dead loop re-kills a survivor every poll; an unconfirmed stop in
// the same state is logged once, while the re-kills themselves go on.
func TestConfirmDrainAckRuntimeDeadLogsUnconfirmedReKillOncePerState(t *testing.T) {
	store := beads.NewMemStore()
	sp := &queuedStopFake{Fake: runtime.NewFake(), errs: []error{
		newUnconfirmedStop("worker", unconfirmedAmbiguous),
		repeatedUnconfirmedStop("worker", unconfirmedAmbiguous),
	}}
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var stderr synchronizedBuffer
	if confirmDrainAckRuntimeDead("", store, sp, &config.City{}, "worker", "", nil, &stderr, 200*time.Millisecond, 10*time.Millisecond) {
		t.Fatal("confirm-dead reported a surviving runtime dead")
	}
	if got := sp.stopCount(); got < 3 {
		t.Fatalf("re-kills = %d, want the loop to keep retrying until its deadline", got)
	}
	out := stderr.String()
	if got := strings.Count(out, "async drain-ack stop worker re-kill: "); got != 1 {
		t.Fatalf("logged %d re-kill failures, want 1 for one unchanged state:\n%s", got, out)
	}
	if !strings.Contains(out, "runtime still alive after confirm-dead deadline") {
		t.Fatalf("stderr = %q, want the deadline diagnostic", out)
	}
}

// The drain-deadline stop is retried every tick while the seat stays open; an
// unconfirmed stop in the same state is logged once, a changed state again.
func TestReconcileSessionBeads_DrainDeadlineLogsUnconfirmedStopOncePerState(t *testing.T) {
	env := poolSeatEnv()
	seat := stuckDrainedPoolSeat(t, env, "drained", poolSlotDrainRetireDeadline+time.Minute)
	const line = "session reconciler: drain-deadline stop of " + poolSeatName + ": "

	for i, stopErr := range []error{
		newUnconfirmedStop(poolSeatName, unconfirmedAmbiguous),
		repeatedUnconfirmedStop(poolSeatName, unconfirmedAmbiguous),
		repeatedUnconfirmedStop(poolSeatName, unconfirmedAmbiguous),
		newUnconfirmedStop(poolSeatName, unconfirmedSurvived),
	} {
		env.sp.StopErrors = map[string]error{poolSeatName: stopErr}
		current, err := env.store.Get(seat.ID)
		if err != nil {
			t.Fatalf("Get(%s): %v", seat.ID, err)
		}
		env.reconcile([]beads.Bead{current})
		if current, err = env.store.Get(seat.ID); err != nil {
			t.Fatalf("Get(%s): %v", seat.ID, err)
		}
		if current.Status == "closed" {
			t.Fatalf("tick %d closed the seat over an unconfirmed stop", i+1)
		}
	}

	out := env.stderr.String()
	if got := strings.Count(out, line); got != 2 {
		t.Fatalf("logged %d drain-deadline stop failures, want 2 (one per state):\n%s", got, out)
	}
	if strings.Count(out, unconfirmedAmbiguous) != 1 || strings.Count(out, unconfirmedSurvived) != 1 {
		t.Fatalf("stderr = %q, want each state logged once", out)
	}
	if !env.sp.IsRunning(poolSeatName) {
		t.Fatal("the seat runtime should still be running")
	}
}

// The stale reset-pending eviction is attempted on every overdue tick; an
// unconfirmed stop in the same state is logged once.
func TestReconcileSessionBeads_ResetStallEvictionLogsUnconfirmedStopOncePerState(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(2)}},
		Session:   config.SessionConfig{StartupTimeout: "60s"},
	}
	env.desiredState["worker"] = TemplateParams{
		Command:      "test-cmd",
		SessionName:  "worker",
		TemplateName: "worker",
		Hints:        agent.StartupHints{ProcessNames: []string{"test-cmd"}},
	}
	if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("starting fake session: %v", err)
	}
	env.sp.Zombies["worker"] = true
	session := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&session, map[string]string{
		"continuation_reset_pending":   "true",
		sessionpkg.ResetCommittedAtKey: env.clk.Now().Add(-75 * time.Second).UTC().Format(time.RFC3339),
	})
	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	const evictionLine = "session reconciler: evicting stale reset-pending runtime worker:"

	for _, stopErr := range []error{
		newUnconfirmedStop("worker", unconfirmedAmbiguous),
		repeatedUnconfirmedStop("worker", unconfirmedAmbiguous),
		repeatedUnconfirmedStop("worker", unconfirmedAmbiguous),
	} {
		env.sp.StopErrors = map[string]error{"worker": stopErr}
		reconcileSessionBeads(
			context.Background(), []beads.Bead{session}, env.desiredState, cfgNames,
			env.cfg, env.sp, env.store, nil, nil, nil, env.dt, map[string]int{"worker": 0}, false, nil, "test-city",
			nil, env.clk, env.rec, env.cfg.Session.StartupTimeoutDuration(), 0, &env.stdout, &env.stderr,
		)
	}

	if got := strings.Count(env.stderr.String(), evictionLine); got != 1 {
		t.Fatalf("logged %d eviction failures over 3 overdue ticks in one state, want 1; stderr: %s", got, env.stderr.String())
	}
	if !env.sp.IsRunning("worker") {
		t.Fatal("stale runtime should still be running after unconfirmed stops")
	}
}

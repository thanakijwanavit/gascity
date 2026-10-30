package auto

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// startedBackendKey spells StartedBackendMetaKey as a literal so these tests
// state the launch-record contract independently of the constant.
const startedBackendKey = "GC_RUNTIME_BACKEND"

// startBeforeLaunchRecord starts name directly on sp the way a backend
// launched it before the composite wrote launch records: only the backend's
// own launch metadata (seeded from the start env) names the session. This is
// the G1 refinery case (g1-c42b7): a Claude pane on herdr whose sidecar holds
// GC_SESSION_NAME and GC_PROVIDER=claude, while city config now says ACP.
func startBeforeLaunchRecord(t *testing.T, sp *runtime.Fake, name string) {
	t.Helper()
	if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
		t.Fatalf("Start(%q): %v", name, err)
	}
	if err := sp.SetMeta(name, "GC_SESSION_NAME", name); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_NAME): %v", err)
	}
	if err := sp.SetMeta(name, "GC_PROVIDER", "claude"); err != nil {
		t.Fatalf("SetMeta(GC_PROVIDER): %v", err)
	}
}

func countCalls(sp *runtime.Fake, method, name string) int {
	n := 0
	for _, c := range sp.Calls {
		if c.Method == method && c.Name == name {
			n++
		}
	}
	return n
}

func TestStartRecordsTheBackendThatLaunchedTheSession(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	p := New(defaultSP, acpSP)
	p.RouteACP("acp-agent")

	for _, name := range []string{"pane-agent", "acp-agent"} {
		if err := p.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatalf("Start(%q): %v", name, err)
		}
	}

	for _, tc := range []struct {
		sp   *runtime.Fake
		name string
		want string
	}{
		{defaultSP, "pane-agent", "default"},
		{acpSP, "acp-agent", "acp"},
		{acpSP, "pane-agent", ""},
		{defaultSP, "acp-agent", ""},
	} {
		got, err := tc.sp.GetMeta(tc.name, startedBackendKey)
		if err != nil {
			t.Fatalf("GetMeta(%q): %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("launch record for %q = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestStartFailureWritesNoLaunchRecord(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	defaultSP.StartErrors["agent"] = errors.New("boom")
	p := New(defaultSP, acpSP)

	if err := p.Start(context.Background(), "agent", runtime.Config{}); err == nil {
		t.Fatal("Start should fail")
	}
	if got, _ := defaultSP.GetMeta("agent", startedBackendKey); got != "" {
		t.Fatalf("launch record after failed Start = %q, want none", got)
	}
}

// claude on herdr -> codex/kimi on acp: config now routes the session to ACP,
// but its live runtime is the pane the default backend started.
func TestStopAfterProviderChangeStopsTheStartingBackend(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	p := New(defaultSP, acpSP)
	if err := p.Start(context.Background(), "refinery", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	p.RouteACP("refinery") // provider changed in config after launch

	if err := p.Stop("refinery"); err != nil {
		t.Fatalf("Stop = %v, want nil: the default backend started this runtime", err)
	}
	if defaultSP.IsRunning("refinery") {
		t.Fatal("runtime the default backend started is still running after Stop")
	}
}

// Sessions launched before the launch record existed: the configured ACP
// backend holds no runtime and no launch metadata, the default backend runs
// the session and its own launch metadata names it.
func TestStopAfterProviderChangeFindsOwnerByLaunchMetadata(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	startBeforeLaunchRecord(t, defaultSP, "refinery")
	p := New(defaultSP, acpSP)
	p.RouteACP("refinery")

	if err := p.Stop("refinery"); err != nil {
		t.Fatalf("Stop = %v, want nil: default backend launched this runtime", err)
	}
	if defaultSP.IsRunning("refinery") {
		t.Fatal("old pane still running after Stop")
	}
	if got := p.route("refinery"); got != defaultSP {
		t.Fatal("route entry should be cleaned up after a confirmed stop")
	}
}

// acp -> claude: config routes back to the default backend while the live
// runtime is still the ACP one the composite started.
func TestStopAfterTransportChangeBackToDefaultStopsTheACPRuntime(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	p := New(defaultSP, acpSP)
	p.RouteACP("agent")
	if err := p.Start(context.Background(), "agent", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	p.Unroute("agent") // config now selects the default backend

	if err := p.Stop("agent"); err != nil {
		t.Fatalf("Stop = %v, want nil", err)
	}
	if acpSP.IsRunning("agent") {
		t.Fatal("ACP runtime still running after Stop")
	}
}

// Neither backend's launch metadata names the session: ownership is unknown,
// so the default backend's same-named runtime stays and the error says why.
func TestStopWithNoLaunchRecordRefusesWithReason(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	if err := defaultSP.Start(context.Background(), "agent", runtime.Config{}); err != nil {
		t.Fatalf("default Start: %v", err)
	}
	p := New(defaultSP, acpSP)
	p.RouteACP("agent")

	err := p.Stop("agent")
	if err == nil {
		t.Fatal("Stop = nil, want an error: no backend recorded this launch")
	}
	if runtime.IsSessionGone(err) {
		t.Fatalf("Stop error %q reads as session-gone; callers would treat a live runtime as stopped", err)
	}
	if !strings.Contains(err.Error(), "launch record") {
		t.Fatalf("Stop error = %q, want the launch-record state that blocked it", err)
	}
	if !defaultSP.IsRunning("agent") {
		t.Fatal("unowned same-named runtime must stay running")
	}
	if got := p.route("agent"); got != acpSP {
		t.Fatal("route should be preserved when the stop is refused")
	}
}

// The configured backend keeps a launch record for the session (it may still
// own a runtime its liveness probe misses): ambiguous, keep refusing.
func TestStopKeepsRefusingWhenConfiguredBackendHoldsLaunchRecord(t *testing.T) {
	defaultSP := runtime.NewFake()
	startBeforeLaunchRecord(t, defaultSP, "agent")
	acpFake := runtime.NewFake()
	if err := acpFake.SetMeta("agent", "GC_SESSION_NAME", "agent"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	acpSP := &falseNegativeStopProvider{Fake: acpFake}
	p := New(defaultSP, acpSP)
	p.RouteACP("agent")

	err := p.Stop("agent")
	if err == nil {
		t.Fatal("Stop = nil, want an error while the ACP backend may still own the session")
	}
	if runtime.IsSessionGone(err) {
		t.Fatalf("Stop error %q reads as session-gone", err)
	}
	if !defaultSP.IsRunning("agent") {
		t.Fatal("same-named default runtime must stay running while ownership is ambiguous")
	}
}

// The starting backend's Stop returns nil but its runtime survives: the stop
// is not confirmed and must not be reported as done.
func TestStopOnStartingBackendIsConfirmedByLiveness(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	startBeforeLaunchRecord(t, defaultSP, "agent")
	defaultSP.StopLeavesRunning["agent"] = true
	p := New(defaultSP, acpSP)
	p.stopConfirmTimeout, p.stopConfirmPoll = 30*time.Millisecond, 5*time.Millisecond
	p.RouteACP("agent")

	err := p.Stop("agent")
	if err == nil {
		t.Fatal("Stop = nil, want an error: the starting backend still reports the runtime")
	}
	if !errors.Is(err, runtime.ErrStopUnconfirmed) {
		t.Fatalf("Stop error = %v, want runtime.ErrStopUnconfirmed", err)
	}
	if n := countCalls(defaultSP, "IsRunning", "agent"); n < 3 {
		t.Fatalf("starting backend liveness probed %d times, want polling until the confirm window ends", n)
	}
	if runtime.IsSessionGone(err) {
		t.Fatalf("Stop error %q reads as session-gone", err)
	}
	if !strings.Contains(err.Error(), "stop returned") {
		t.Fatalf("Stop error = %q, want the unconfirmed stop on the default backend", err)
	}
	if countCalls(defaultSP, "Stop", "agent") == 0 {
		t.Fatal("the starting backend was never asked to stop its runtime")
	}
	if got := p.route("agent"); got != acpSP {
		t.Fatal("route should be preserved when the stop is not confirmed")
	}
}

// Normal case: the configured backend runs its own session; the other
// backend is not touched.
func TestStopNormalCaseStopsOnlyTheConfiguredBackend(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	p := New(defaultSP, acpSP)
	p.RouteACP("acp-agent")
	for _, name := range []string{"pane-agent", "acp-agent"} {
		if err := p.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatalf("Start(%q): %v", name, err)
		}
	}

	if err := p.Stop("acp-agent"); err != nil {
		t.Fatalf("Stop(acp-agent): %v", err)
	}
	if err := p.Stop("pane-agent"); err != nil {
		t.Fatalf("Stop(pane-agent): %v", err)
	}
	if acpSP.IsRunning("acp-agent") || defaultSP.IsRunning("pane-agent") {
		t.Fatal("sessions should be stopped on their own backends")
	}
	if n := countCalls(defaultSP, "Stop", "acp-agent"); n != 0 {
		t.Fatalf("default backend Stop(acp-agent) called %d times, want 0", n)
	}
	if n := countCalls(acpSP, "Stop", "pane-agent"); n != 0 {
		t.Fatalf("acp backend Stop(pane-agent) called %d times, want 0", n)
	}
}

// Liveness after a provider change: the routed ACP backend holds nothing, so
// the agent-process check must reach the backend that runs the session, as
// IsRunning already does. Otherwise IsRunning=true with ProcessAlive=false
// reads the live pane as a zombie.
func TestProcessAliveReachesTheStartingBackendAfterProviderChange(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	p := New(defaultSP, acpSP)
	if err := p.Start(context.Background(), "agent", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	p.RouteACP("agent")

	if !p.IsRunning("agent") {
		t.Fatal("IsRunning = false, want true from the default backend")
	}
	if !p.ProcessAlive("agent", []string{"claude"}) {
		t.Fatal("ProcessAlive = false, want true from the backend that runs the session")
	}
	defaultSP.Zombies["agent"] = true
	if p.ProcessAlive("agent", []string{"claude"}) {
		t.Fatal("ProcessAlive = true for a zombie on the starting backend")
	}
}

// A stop retried every tick in an unchanged state is flagged Repeat so the
// worker event and the reconciler log fire once per state change; a changed
// state, or a confirmed stop, starts over.
func TestUnconfirmedStopIsFlaggedRepeatUntilTheStateChanges(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	if err := defaultSP.Start(context.Background(), "agent", runtime.Config{}); err != nil {
		t.Fatalf("default Start: %v", err)
	}
	p := New(defaultSP, acpSP)
	p.RouteACP("agent")

	first := p.Stop("agent")
	if !errors.Is(first, runtime.ErrStopUnconfirmed) || runtime.IsRepeatedStopUnconfirmed(first) {
		t.Fatalf("first Stop = %v, want a new unconfirmed stop", first)
	}
	second := p.Stop("agent")
	if !runtime.IsRepeatedStopUnconfirmed(second) {
		t.Fatalf("second Stop = %v, want it flagged as a repeat", second)
	}
	if second.Error() != first.Error() {
		t.Fatalf("repeat reason %q differs from %q", second, first)
	}

	// The ACP backend now holds a launch record: a different state.
	if err := acpSP.SetMeta("agent", "GC_SESSION_NAME", "agent"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	changed := p.Stop("agent")
	if !errors.Is(changed, runtime.ErrStopUnconfirmed) || runtime.IsRepeatedStopUnconfirmed(changed) {
		t.Fatalf("Stop after a state change = %v, want a new unconfirmed stop", changed)
	}
	if !strings.Contains(changed.Error(), "acp=yes") {
		t.Fatalf("Stop reason = %q, want the ACP launch record in it", changed)
	}
	if err := acpSP.RemoveMeta("agent", "GC_SESSION_NAME"); err != nil {
		t.Fatalf("RemoveMeta: %v", err)
	}

	// The default backend's launch metadata now names the session: confirmed.
	if err := defaultSP.SetMeta("agent", "GC_SESSION_NAME", "agent"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if err := p.Stop("agent"); err != nil {
		t.Fatalf("Stop = %v, want nil once the default backend's launch record names the session", err)
	}

	// A confirmed stop clears the state: the next refusal is new again.
	if err := defaultSP.Start(context.Background(), "agent", runtime.Config{}); err != nil {
		t.Fatalf("default Start: %v", err)
	}
	if err := defaultSP.RemoveMeta("agent", "GC_SESSION_NAME"); err != nil {
		t.Fatalf("RemoveMeta: %v", err)
	}
	p.RouteACP("agent")
	again := p.Stop("agent")
	if !errors.Is(again, runtime.ErrStopUnconfirmed) || runtime.IsRepeatedStopUnconfirmed(again) {
		t.Fatalf("Stop after a confirmed stop = %v, want a new unconfirmed stop", again)
	}
}

// A metadata read failure on either backend leaves ownership unknown: no
// cross-backend stop, and the reason says the record was unreadable.
func TestStopWithUnreadableLaunchRecordRefuses(t *testing.T) {
	defaultSP := runtime.NewFake()
	startBeforeLaunchRecord(t, defaultSP, "agent")
	acpSP := &metaErrorProvider{Fake: runtime.NewFake()}
	p := New(defaultSP, acpSP)
	p.RouteACP("agent")

	err := p.Stop("agent")
	if !errors.Is(err, runtime.ErrStopUnconfirmed) {
		t.Fatalf("Stop = %v, want an unconfirmed stop while the ACP record is unreadable", err)
	}
	if !strings.Contains(err.Error(), "acp=unreadable") {
		t.Fatalf("Stop reason = %q, want acp=unreadable", err)
	}
	if strings.Contains(err.Error(), "not found") {
		t.Fatalf("Stop reason %q echoes the backend error", err)
	}
	if !defaultSP.IsRunning("agent") {
		t.Fatal("default runtime must stay running while ownership is unknown")
	}
}

// Start on a backend that cannot store metadata still starts the session.
func TestStartSucceedsWhenTheLaunchRecordCannotBeWritten(t *testing.T) {
	defaultSP := &metaErrorProvider{Fake: runtime.NewFake()}
	p := New(defaultSP, runtime.NewFake())
	if err := p.Start(context.Background(), "agent", runtime.Config{}); err != nil {
		t.Fatalf("Start = %v, want nil: the launch record is best-effort", err)
	}
	if !defaultSP.IsRunning("agent") {
		t.Fatal("session should be running")
	}
}

// metaErrorProvider fails every metadata call with an error whose text would
// read as session-gone if it were echoed into a stop error.
type metaErrorProvider struct {
	*runtime.Fake
}

func (p *metaErrorProvider) GetMeta(string, string) (string, error) {
	return "", errors.New("meta store: key not found on unreadable volume")
}

func (p *metaErrorProvider) SetMeta(string, string, string) error {
	return errors.New("meta store unavailable")
}

// The default backend's launch metadata names a different session (a reused
// runtime name): it is not this session's launch record, so no cross-backend
// stop.
func TestStopRefusesWhenTheLaunchMetadataNamesAnotherSession(t *testing.T) {
	defaultSP := runtime.NewFake()
	if err := defaultSP.Start(context.Background(), "agent", runtime.Config{}); err != nil {
		t.Fatalf("default Start: %v", err)
	}
	if err := defaultSP.SetMeta("agent", "GC_SESSION_NAME", "agent-old"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	p := New(defaultSP, runtime.NewFake())
	p.RouteACP("agent")

	if err := p.Stop("agent"); !errors.Is(err, runtime.ErrStopUnconfirmed) {
		t.Fatalf("Stop = %v, want an unconfirmed stop", err)
	}
	if !defaultSP.IsRunning("agent") {
		t.Fatal("a runtime whose launch metadata names another session must stay running")
	}
}

// When the routed backend runs the session itself it is stopped there, even
// without a launch record, and the other backend's same-named runtime is left
// to the existing stale-route rules.
func TestStopPrefersTheRunningConfiguredBackend(t *testing.T) {
	defaultSP := runtime.NewFake()
	startBeforeLaunchRecord(t, defaultSP, "agent")
	acpSP := runtime.NewFake()
	if err := acpSP.Start(context.Background(), "agent", runtime.Config{}); err != nil {
		t.Fatalf("acp Start: %v", err)
	}
	p := New(defaultSP, acpSP)
	p.RouteACP("agent")

	if err := p.Stop("agent"); err != nil {
		t.Fatalf("Stop = %v, want nil", err)
	}
	if acpSP.IsRunning("agent") {
		t.Fatal("the routed backend's own runtime should be stopped")
	}
	if n := countCalls(defaultSP, "Stop", "agent"); n != 0 {
		t.Fatalf("default backend Stop called %d times, want 0 while the routed backend runs the session", n)
	}
}

// A warm-box relaunch on the routed backend refreshes the launch record.
func TestRelaunchRecordsTheBackendThatRelaunchedTheSession(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	p := New(defaultSP, acpSP)
	p.RouteACP("agent")
	if err := acpSP.Start(context.Background(), "agent", runtime.Config{}); err != nil {
		t.Fatalf("acp Start: %v", err)
	}

	if err := p.Relaunch(context.Background(), "agent", runtime.Config{}); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	if got, _ := acpSP.GetMeta("agent", startedBackendKey); got != "acp" {
		t.Fatalf("launch record after Relaunch = %q, want %q", got, "acp")
	}
}

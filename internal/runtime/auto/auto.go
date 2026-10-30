// Package auto provides a composite [runtime.Provider] that routes
// sessions to a default backend (typically tmux) or ACP based on
// per-session registration. Sessions are registered as ACP via
// [Provider.RouteACP] before [Provider.Start] is called. Unregistered
// sessions route to the default backend.
package auto

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// StartedBackendMetaKey names the runtime metadata entry Start writes on the
// backend that launched a session: "default" or "acp". Stop reads it to find
// the backend that owns a live runtime after the session's configured
// transport changed (for example claude on herdr -> codex on acp).
const StartedBackendMetaKey = "GC_RUNTIME_BACKEND"

const (
	defaultLabel = "default"
	acpLabel     = "acp"
)

// Default bounds for confirming a stop on the backend that started the
// runtime. A backend may answer liveness from a short-lived cache (herdr
// reuses its agent list for up to a second), so one probe right after the
// stop can still see the runtime.
const (
	defaultStopConfirmTimeout = 3 * time.Second
	defaultStopConfirmPoll    = 200 * time.Millisecond
)

// Provider routes session operations to a default or ACP backend
// based on per-session registration.
type Provider struct {
	defaultSP runtime.Provider
	acpSP     runtime.Provider

	mu     sync.RWMutex
	routes map[string]bool // true = ACP
	// unconfirmed holds, per session, the last unconfirmed-stop reason, so a
	// stop retried every tick in the same state is flagged as a repeat.
	unconfirmed map[string]string

	stopConfirmTimeout time.Duration
	stopConfirmPoll    time.Duration
}

var (
	_ runtime.Provider                      = (*Provider)(nil)
	_ runtime.DeadRuntimeSessionChecker     = (*Provider)(nil)
	_ runtime.InteractionProvider           = (*Provider)(nil)
	_ runtime.IdleSnapshotProvider          = (*Provider)(nil)
	_ runtime.InterruptBoundaryWaitProvider = (*Provider)(nil)
	_ runtime.InterruptedTurnResetProvider  = (*Provider)(nil)
	_ runtime.InputClearProvider            = (*Provider)(nil)
	_ runtime.TransportCapabilityProvider   = (*Provider)(nil)
	_ runtime.RelaunchProvider              = (*Provider)(nil)
	_ runtime.LivenessObserver              = (*Provider)(nil)
	_ runtime.LivenessObserverWithError     = (*Provider)(nil)
	_ runtime.SessionEventProvider          = (*Provider)(nil)
)

// New creates a composite provider. defaultSP handles sessions not
// registered as ACP. acpSP handles sessions registered via RouteACP.
func New(defaultSP, acpSP runtime.Provider) *Provider {
	return &Provider{
		defaultSP:          defaultSP,
		acpSP:              acpSP,
		routes:             make(map[string]bool),
		unconfirmed:        make(map[string]string),
		stopConfirmTimeout: defaultStopConfirmTimeout,
		stopConfirmPoll:    defaultStopConfirmPoll,
	}
}

// RouteACP registers a session name to use the ACP backend.
// Must be called before Start for that session.
func (p *Provider) RouteACP(name string) {
	p.mu.Lock()
	p.routes[name] = true
	p.mu.Unlock()
}

// Unroute removes a session's routing entry. Called on Stop to avoid
// leaking entries for destroyed sessions.
func (p *Provider) Unroute(name string) {
	p.mu.Lock()
	delete(p.routes, name)
	p.mu.Unlock()
}

func (p *Provider) route(name string) runtime.Provider {
	p.mu.RLock()
	isACP := p.routes[name]
	p.mu.RUnlock()
	if isACP {
		return p.acpSP
	}
	return p.defaultSP
}

// backends returns the routed backend and the other one, with their labels.
func (p *Provider) backends(name string) (primary runtime.Provider, primaryLabel string, other runtime.Provider, otherLabel string) {
	p.mu.RLock()
	isACP := p.routes[name]
	p.mu.RUnlock()
	if isACP {
		return p.acpSP, acpLabel, p.defaultSP, defaultLabel
	}
	return p.defaultSP, defaultLabel, p.acpSP, acpLabel
}

// SupportsTransport reports whether this provider can route the requested
// session transport.
func (p *Provider) SupportsTransport(transport string) bool {
	if transport != "acp" {
		return true
	}
	if provider, ok := p.acpSP.(runtime.TransportCapabilityProvider); ok {
		return provider.SupportsTransport(transport)
	}
	return false
}

// DetectTransport reports the backend currently hosting the named session.
// It returns "acp" for ACP-backed sessions and "" for default or unknown.
func (p *Provider) DetectTransport(name string) string {
	if p.defaultSP.IsRunning(name) {
		return ""
	}
	if p.acpSP.IsRunning(name) {
		return "acp"
	}
	return ""
}

// Start delegates to the routed backend and, once the runtime is up, records
// on that backend which backend launched it (StartedBackendMetaKey), so Stop
// still finds the runtime after the session's configured transport changes.
// The record is best-effort: Stop falls back to the backend's own launch
// metadata when it is missing.
func (p *Provider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	sp, label, _, _ := p.backends(name)
	if err := sp.Start(ctx, name, cfg); err != nil {
		return err
	}
	_ = sp.SetMeta(name, StartedBackendMetaKey, label)
	p.forgetUnconfirmedStop(name)
	return nil
}

// Stop delegates to the routed backend and cleans up the route entry
// only on success. If the routed backend fails, tries the other backend
// to handle stale/missing route entries (e.g., after controller restart).
//
// When the routed backend holds neither the runtime nor a launch record for
// the session, while the other backend's launch record names the session,
// the configured transport changed after launch (claude on herdr -> codex on
// acp): the runtime is stopped on the backend that started it, and the stop
// counts only once that backend no longer reports it.
func (p *Provider) Stop(name string) error {
	primary, primaryLabel, other, otherLabel := p.backends(name)
	primaryRunning := primary.IsRunning(name)
	if !primaryRunning && launchedOnOtherBackend(name, primary, primaryLabel, other, otherLabel) {
		return p.stopOnStartingBackend(other, otherLabel, name)
	}
	p.mu.RLock()
	primaryExplicitRoute := p.routes[name]
	p.mu.RUnlock()
	err := primary.Stop(name)
	if err == nil && primaryRunning {
		p.stopped(name)
		return nil
	}
	// Fall through to the other backend in case the route is stale.
	p.mu.RLock()
	if p.routes[name] {
		primaryLabel, otherLabel = acpLabel, defaultLabel
		other = p.defaultSP
	} else {
		primaryLabel, otherLabel = defaultLabel, acpLabel
		other = p.acpSP
	}
	p.mu.RUnlock()
	otherRunning := other.IsRunning(name)
	if err == nil {
		if primaryExplicitRoute {
			if otherRunning {
				return p.unconfirmedStop(name, fmt.Sprintf(
					"%s backend: stop succeeded without liveness confirmation while %s backend still reports the session running (launch records: %s=%s, %s=%s)",
					primaryLabel, otherLabel,
					primaryLabel, describeLaunchRecord(primary, primaryLabel, name),
					otherLabel, describeLaunchRecord(other, otherLabel, name)))
			}
			p.stopped(name)
			return nil
		}
		err = fmt.Errorf("%w: %q", runtime.ErrSessionNotFound, name)
	}
	otherErr := other.Stop(name)
	if otherErr == nil {
		if !otherRunning {
			otherErr = fmt.Errorf("%w: %q", runtime.ErrSessionNotFound, name)
		} else if (primaryRunning || primaryExplicitRoute) && !runtime.IsSessionGone(err) {
			return fmt.Errorf("%s backend: %w", primaryLabel, err)
		}
	}
	mergedErr := runtime.MergeBackendStopErrors(
		runtime.BackendError{Label: primaryLabel, Err: err},
		runtime.BackendError{Label: otherLabel, Err: otherErr},
	)
	if mergedErr == nil {
		p.stopped(name)
		return nil
	}
	return mergedErr
}

// launchedOnOtherBackend reports whether other, not the routed primary,
// launched name's runtime: other's launch record names the session, while
// primary holds neither the runtime nor a launch record for it. Other's
// runtime need not be running: an agent that exited still leaves its pane
// (herdr reads a pane at a bare shell prompt as not running), and stopping
// on the launching backend is what closes it. A metadata read error leaves
// ownership unknown (false), so Stop keeps its refusal to touch a same-named
// runtime it cannot attribute.
func launchedOnOtherBackend(name string, primary runtime.Provider, primaryLabel string, other runtime.Provider, otherLabel string) bool {
	if launched, err := launchRecorded(other, otherLabel, name); err != nil || !launched {
		return false
	}
	launched, err := launchRecorded(primary, primaryLabel, name)
	return err == nil && !launched
}

// launchRecorded reports whether sp's runtime metadata records launching
// name: Start's record names sp, or, for runtimes started before that record
// existed, sp's own launch metadata (seeded from the start env) carries the
// session name.
func launchRecorded(sp runtime.Provider, label, name string) (bool, error) {
	backend, err := sp.GetMeta(name, StartedBackendMetaKey)
	if err != nil {
		return false, err
	}
	if backend = strings.TrimSpace(backend); backend != "" {
		return backend == label, nil
	}
	sessionName, err := sp.GetMeta(name, "GC_SESSION_NAME")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(sessionName) == name, nil
}

// describeLaunchRecord renders sp's launch record for name in an unconfirmed
// stop reason. It never echoes a backend error: some read like "not found",
// which runtime.IsSessionGone would take for a stopped session.
func describeLaunchRecord(sp runtime.Provider, label, name string) string {
	launched, err := launchRecorded(sp, label, name)
	switch {
	case err != nil:
		return "unreadable"
	case launched:
		return "yes"
	default:
		return "none"
	}
}

// stopOnStartingBackend stops name on the backend that launched it and
// reports success only once that backend no longer reports the runtime.
func (p *Provider) stopOnStartingBackend(sp runtime.Provider, label, name string) error {
	if err := sp.Stop(name); err != nil && !runtime.IsSessionGone(err) {
		return fmt.Errorf("%s backend: %w", label, err)
	}
	if !p.waitStopped(sp, name) {
		return p.unconfirmedStop(name, fmt.Sprintf(
			"%s backend: stop returned but the runtime it started is still running (the configured backend holds no launch record)", label))
	}
	p.stopped(name)
	return nil
}

// waitStopped polls sp until it no longer reports name running, for at most
// stopConfirmTimeout.
func (p *Provider) waitStopped(sp runtime.Provider, name string) bool {
	deadline := time.Now().Add(p.stopConfirmTimeout)
	for {
		if !sp.IsRunning(name) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(p.stopConfirmPoll)
	}
}

// stopped records a confirmed stop: the route entry and any earlier
// unconfirmed-stop state for name are dropped.
func (p *Provider) stopped(name string) {
	p.Unroute(name)
	p.forgetUnconfirmedStop(name)
}

// unconfirmedStop returns the error for a stop that could not be confirmed,
// flagged Repeat when the same reason was already reported for name, so a
// caller retrying every tick logs it once per state change.
func (p *Provider) unconfirmedStop(name, reason string) error {
	p.mu.Lock()
	repeat := p.unconfirmed[name] == reason
	p.unconfirmed[name] = reason
	p.mu.Unlock()
	return &runtime.StopUnconfirmedError{Name: name, Reason: reason, Repeat: repeat}
}

func (p *Provider) forgetUnconfirmedStop(name string) {
	p.mu.Lock()
	delete(p.unconfirmed, name)
	p.mu.Unlock()
}

// Interrupt delegates to the routed backend.
func (p *Provider) Interrupt(name string) error {
	return p.route(name).Interrupt(name)
}

// IsRunning checks the routed backend first. If it reports not running,
// falls through to the other backend to handle route table inconsistencies.
func (p *Provider) IsRunning(name string) bool {
	if p.route(name).IsRunning(name) {
		return true
	}
	// Fall through: check the other backend in case routing is stale.
	p.mu.RLock()
	isACP := p.routes[name]
	p.mu.RUnlock()
	if isACP {
		return p.defaultSP.IsRunning(name)
	}
	return p.acpSP.IsRunning(name)
}

// IsDeadRuntimeSession checks both backends for a positive dead-artifact
// report because ListRunning is also merged across both backends.
func (p *Provider) IsDeadRuntimeSession(name string) (bool, error) {
	primary := p.route(name)
	if dead, err := providerDeadRuntimeSession(primary, name); dead || err != nil {
		return dead, err
	}
	p.mu.RLock()
	isACP := p.routes[name]
	p.mu.RUnlock()
	if isACP {
		return providerDeadRuntimeSession(p.defaultSP, name)
	}
	return providerDeadRuntimeSession(p.acpSP, name)
}

func providerDeadRuntimeSession(sp runtime.Provider, name string) (bool, error) {
	checker, ok := sp.(runtime.DeadRuntimeSessionChecker)
	if !ok {
		return false, nil
	}
	return checker.IsDeadRuntimeSession(name)
}

// IsAttached delegates to the routed backend.
func (p *Provider) IsAttached(name string) bool {
	return p.route(name).IsAttached(name)
}

// Attach delegates to the routed backend. ACP sessions return an error.
func (p *Provider) Attach(name string) error {
	p.mu.RLock()
	isACP := p.routes[name]
	p.mu.RUnlock()
	if isACP {
		return fmt.Errorf("agent %q uses ACP transport (no terminal to attach to)", name)
	}
	return p.defaultSP.Attach(name)
}

// ProcessAlive asks the backend that runs the session: the routed one, or,
// like IsRunning, the other one when the routed backend does not run it
// (stale route, or a provider change since launch). Otherwise a live runtime
// would read as running with a dead agent just because config now names
// another backend.
func (p *Provider) ProcessAlive(name string, processNames []string) bool {
	primary, _, other, _ := p.backends(name)
	if !primary.IsRunning(name) && other.IsRunning(name) {
		return other.ProcessAlive(name, processNames)
	}
	return primary.ProcessAlive(name, processNames)
}

// ObserveLiveness delegates to the routed backend through runtime.ObserveLiveness
// so the backend's native LivenessObserver fast-path is preserved — e.g. herdr's
// agent-status liveness. Without this, wrapping a LivenessObserver backend in an
// auto router would silently collapse it to the generic IsRunning+ProcessAlive
// fold (the fragile process-table walk), reintroducing the singleton
// restart-loop for any city that also routes some sessions to ACP.
func (p *Provider) ObserveLiveness(name string, processNames []string) runtime.Liveness {
	primary := runtime.ObserveLiveness(p.route(name), name, processNames)
	if primary.Running {
		return primary
	}
	// Fall through: check the other backend in case routing is stale
	// (e.g. after a controller restart clears the in-memory route table),
	// matching IsRunning's recovery so a live ACP singleton on a
	// herdr-default city is not misread as dead.
	p.mu.RLock()
	isACP := p.routes[name]
	p.mu.RUnlock()
	other := p.acpSP
	if isACP {
		other = p.defaultSP
	}
	return runtime.ObserveLiveness(other, name, processNames)
}

// ObserveLivenessWithError preserves routed-backend observation failures. A
// confirmed absence still falls through to the other backend so stale route
// recovery matches IsRunning and ObserveLiveness without collapsing an
// unavailable primary into absence.
func (p *Provider) ObserveLivenessWithError(name string, processNames []string) (runtime.Liveness, error) {
	primary, err := runtime.ObserveLivenessWithError(p.route(name), name, processNames)
	if err != nil || primary.Running {
		return primary, err
	}
	p.mu.RLock()
	isACP := p.routes[name]
	p.mu.RUnlock()
	other := p.acpSP
	if isACP {
		other = p.defaultSP
	}
	return runtime.ObserveLivenessWithError(other, name, processNames)
}

// Nudge delegates to the routed backend.
func (p *Provider) Nudge(name string, content []runtime.ContentBlock) error {
	return p.route(name).Nudge(name, content)
}

// WaitForIdle delegates to the routed backend when it supports explicit
// idle-boundary waiting.
func (p *Provider) WaitForIdle(ctx context.Context, name string, timeout time.Duration) error {
	if wp, ok := p.route(name).(runtime.IdleWaitProvider); ok {
		return wp.WaitForIdle(ctx, name, timeout)
	}
	return runtime.ErrInteractionUnsupported
}

// SnapshotIdle delegates to the routed backend when it can take a
// point-in-time idle observation. Without this the composite would hide a
// tmux-backed session's SnapshotIdle from every caller as soon as any agent in
// the city selects the ACP transport, because this Provider enumerates the
// optional interfaces it forwards rather than embedding a backend.
func (p *Provider) SnapshotIdle(name string) (bool, error) {
	if sp, ok := p.route(name).(runtime.IdleSnapshotProvider); ok {
		return sp.SnapshotIdle(name)
	}
	return false, runtime.ErrInteractionUnsupported
}

// NudgeNow delegates to the routed backend when it supports immediate
// injection without an internal wait-idle step.
func (p *Provider) NudgeNow(name string, content []runtime.ContentBlock) error {
	if np, ok := p.route(name).(runtime.ImmediateNudgeProvider); ok {
		return np.NudgeNow(name, content)
	}
	return p.route(name).Nudge(name, content)
}

// ResetInterruptedTurn delegates to the routed backend when it supports
// provider-native interrupted-turn discard semantics.
func (p *Provider) ResetInterruptedTurn(ctx context.Context, name string) error {
	if rp, ok := p.route(name).(runtime.InterruptedTurnResetProvider); ok {
		return rp.ResetInterruptedTurn(ctx, name)
	}
	return runtime.ErrInteractionUnsupported
}

// ClearInput delegates to the routed backend when it can empty a session's
// input box and confirm it is empty.
func (p *Provider) ClearInput(ctx context.Context, name string, restoreWindow time.Duration) error {
	if cp, ok := p.route(name).(runtime.InputClearProvider); ok {
		return cp.ClearInput(ctx, name, restoreWindow)
	}
	return runtime.ErrInteractionUnsupported
}

// Relaunch forwards a warm-box agent relaunch to the routed backend when it
// supports one, so the reconciler's RelaunchProvider type-assert is not masked
// by the auto router.
func (p *Provider) Relaunch(ctx context.Context, name string, cfg runtime.Config) error {
	sp, label, _, _ := p.backends(name)
	rp, ok := sp.(runtime.RelaunchProvider)
	if !ok {
		return runtime.ErrRelaunchUnsupported
	}
	if err := rp.Relaunch(ctx, name, cfg); err != nil {
		return err
	}
	_ = sp.SetMeta(name, StartedBackendMetaKey, label)
	return nil
}

// WaitForInterruptBoundary delegates to the routed backend when it can confirm
// a provider-native interrupt boundary before the next turn is injected.
func (p *Provider) WaitForInterruptBoundary(ctx context.Context, name string, since time.Time, timeout time.Duration) error {
	if wp, ok := p.route(name).(runtime.InterruptBoundaryWaitProvider); ok {
		return wp.WaitForInterruptBoundary(ctx, name, since, timeout)
	}
	return runtime.ErrInteractionUnsupported
}

// Pending delegates to the routed backend when it supports structured
// interactions.
func (p *Provider) Pending(name string) (*runtime.PendingInteraction, error) {
	if ip, ok := p.route(name).(runtime.InteractionProvider); ok {
		return ip.Pending(name)
	}
	return nil, runtime.ErrInteractionUnsupported
}

// Respond delegates to the routed backend when it supports structured
// interactions.
func (p *Provider) Respond(name string, response runtime.InteractionResponse) error {
	if ip, ok := p.route(name).(runtime.InteractionProvider); ok {
		return ip.Respond(name, response)
	}
	return runtime.ErrInteractionUnsupported
}

// SetMeta delegates to the routed backend.
func (p *Provider) SetMeta(name, key, value string) error {
	return p.route(name).SetMeta(name, key, value)
}

// GetMeta delegates to the routed backend.
func (p *Provider) GetMeta(name, key string) (string, error) {
	return p.route(name).GetMeta(name, key)
}

// RemoveMeta delegates to the routed backend.
func (p *Provider) RemoveMeta(name, key string) error {
	return p.route(name).RemoveMeta(name, key)
}

// Peek delegates to the routed backend.
func (p *Provider) Peek(name string, lines int) (string, error) {
	return p.route(name).Peek(name, lines)
}

// ListRunning queries both backends and returns best-effort results plus a
// partial-list error when one backend fails.
func (p *Provider) ListRunning(prefix string) ([]string, error) {
	defaultList, dErr := p.defaultSP.ListRunning(prefix)
	acpList, aErr := p.acpSP.ListRunning(prefix)
	return runtime.MergeBackendListResults(
		runtime.BackendListResult{Label: "default", Names: defaultList, Err: dErr},
		runtime.BackendListResult{Label: "acp", Names: acpList, Err: aErr},
	)
}

// GetLastActivity delegates to the routed backend.
func (p *Provider) GetLastActivity(name string) (time.Time, error) {
	return p.route(name).GetLastActivity(name)
}

// ClearScrollback delegates to the routed backend.
func (p *Provider) ClearScrollback(name string) error {
	return p.route(name).ClearScrollback(name)
}

// CopyTo delegates to the routed backend.
func (p *Provider) CopyTo(name, src, relDst string) error {
	return p.route(name).CopyTo(name, src, relDst)
}

// SendKeys delegates to the routed backend.
func (p *Provider) SendKeys(name string, keys ...string) error {
	return p.route(name).SendKeys(name, keys...)
}

// RunLive delegates to the routed backend.
func (p *Provider) RunLive(name string, cfg runtime.Config) error {
	return p.route(name).RunLive(name, cfg)
}

// Capabilities returns the intersection of both backends' capabilities.
// A capability is reported only if both default and ACP support it.
// NeedsClaimBackstop is a need, not an ability, so it unions instead: if
// either backend requires the stalled-claim backstop, the composite does too.
func (p *Provider) Capabilities() runtime.ProviderCapabilities {
	dc := p.defaultSP.Capabilities()
	ac := p.acpSP.Capabilities()
	return runtime.ProviderCapabilities{
		CanReportAttachment: dc.CanReportAttachment && ac.CanReportAttachment,
		CanReportActivity:   dc.CanReportActivity && ac.CanReportActivity,
		CanStream:           dc.CanStream && ac.CanStream,
		CanAttachTTY:        dc.CanAttachTTY && ac.CanAttachTTY,
		NeedsClaimBackstop:  dc.NeedsClaimBackstop || ac.NeedsClaimBackstop,
	}
}

// SleepCapability reports idle sleep capability for the routed backend.
func (p *Provider) SleepCapability(name string) runtime.SessionSleepCapability {
	if scp, ok := p.route(name).(runtime.SleepCapabilityProvider); ok {
		return scp.SleepCapability(name)
	}
	return runtime.SessionSleepCapabilityDisabled
}

// SubscribeSessionEvents forwards the session-event stream of whichever
// backend implements runtime.SessionEventProvider. Today only herdr does, so
// without this method, wrapping an event-capable default backend (e.g.
// herdr) behind auto for ACP routing would fail the
// runtime.SessionEventProvider type assertion in cmd/gc's
// sessionEventPump.restart and silently drop the whole event-driven
// reconcile poke, falling back to patrol polling with no underlying
// capability loss to explain it.
func (p *Provider) SubscribeSessionEvents(ctx context.Context) (<-chan runtime.SessionEvent, error) {
	dSEP, dok := p.defaultSP.(runtime.SessionEventProvider)
	aSEP, aok := p.acpSP.(runtime.SessionEventProvider)
	switch {
	case dok:
		return dSEP.SubscribeSessionEvents(ctx)
	case aok:
		return aSEP.SubscribeSessionEvents(ctx)
	default:
		return nil, fmt.Errorf("neither default nor ACP backend implements SubscribeSessionEvents")
	}
}

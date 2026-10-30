package herdr

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/shellquote"
)

// ── the pane's foreground: who may receive typed text ────────────────────────
//
// herdr delivers text by typing it into a pane's terminal, and whatever holds
// the pane's foreground reads it. When that is the session's agent the text is
// a prompt; when it is a shell, every line is a command. On 29 Oct 2026 G1
// rebooted, herdr restored the agent panes with a fresh shell and typed
// `claude --resume <id>` into each, the resume failed, and gc went on pasting
// nudges (mail and room-line previews, which can carry a customer's words)
// into bash and pressing Enter (g1-j3yf1). So every text delivery first reads
// the pane's foreground process group (`pane process-info`, the call liveness
// already makes) and types only when the session's agent is in it. Nothing is
// cached: a pane's processes change under a reboot, a crash or an operator.

// Sidecar keys Start writes for each life, beside the pane binding.
const (
	// metaLaunchedCommand records whether this life launched a command ("true")
	// or runs the pane's own shell as the session ("false"). The pane of a
	// launched command back at a bare interactive prompt means the command is
	// gone (see resolveBinding).
	metaLaunchedCommand = "GC_HERDR_LAUNCHED_COMMAND"
	// metaAgentProcesses holds the session's configured process names, one per
	// line, for the type check (see agentProcessNames).
	metaAgentProcesses = "GC_HERDR_AGENT_PROCESSES"
)

// interactiveShellFlags are the only arguments a shell waiting at its prompt
// is started with. A shell given anything else (-c and its command, a script)
// is running something, not waiting for input.
var interactiveShellFlags = map[string]bool{
	"-i": true, "-l": true, "-il": true, "-li": true,
	"--login": true, "--interactive": true, "--norc": true, "--noprofile": true,
}

// shellBase is the program name in an argv[0] or a process name, without the
// leading "-" a login shell carries.
func shellBase(s string) string {
	return strings.TrimPrefix(filepath.Base(s), "-")
}

// interactiveShellArgv reports whether argv starts an interactive shell and
// nothing else: a shell program followed only by interactive-shell flags.
func interactiveShellArgv(argv []string) bool {
	if len(argv) == 0 || !interactiveShells[shellBase(argv[0])] {
		return false
	}
	for _, a := range argv[1:] {
		if !interactiveShellFlags[a] {
			return false
		}
	}
	return true
}

// barePromptShell reports the shell a pane idles in when its foreground is
// nothing but the pane's root shell waiting at an interactive prompt: every
// foreground process is the root (pid == shellPID), the root is a shell by
// name, and its argv is an interactive shell's. herdr 0.9.3 reported exactly
// this for G1's failed-resume pane (bash, argv [bash]). A live raw-launched
// session's root is its `/bin/sh -c <command>` wrapper instead, which is not at
// a prompt even when it is alone in the foreground between two commands. An
// argv herdr did not report proves nothing, so it is no bare prompt.
func barePromptShell(shellPID int, fg []proc) (string, bool) {
	if shellPID == 0 || len(fg) == 0 {
		return "", false
	}
	for _, pr := range fg {
		if pr.PID != shellPID {
			return "", false
		}
	}
	name := shellBase(fg[0].Name)
	if !interactiveShells[name] || !interactiveShellArgv(fg[0].Argv) {
		return "", false
	}
	return name, true
}

// agentInForeground reports whether the pane's foreground process group holds
// one of the agent processes in want, matched exactly on a process's name or on
// the program name of its argv[0] (claude ≥2.1.x can run under a name that is
// its version string, argv[0] still "claude"). A shell is never the agent,
// whatever its argv[0] claims: a shell is what an exited agent leaves behind.
// Nothing matches by substring or on arguments, so `bash -c claude` and
// `claudex` are not claude.
func agentInForeground(fg []proc, want []string) bool {
	for _, pr := range fg {
		if pr.PID == 0 || interactiveShells[shellBase(pr.Name)] {
			continue
		}
		if slices.Contains(want, pr.Name) || len(pr.Argv) > 0 && slices.Contains(want, filepath.Base(pr.Argv[0])) {
			return true
		}
	}
	return false
}

// agentProcessNames is what the type check accepts as the session's agent: its
// configured process names less any shell, or, when none remain (nothing was
// configured, or the binding predates metaAgentProcesses), the agent kinds
// herdr launches. Either way a shell never qualifies, so a provider that lists
// one (zcode-repl's profile lists bash) is matched on its other names only.
func agentProcessNames(configured []string) []string {
	var want []string
	for _, n := range configured {
		if n = strings.TrimSpace(n); n != "" && !interactiveShells[shellBase(n)] {
			want = append(want, n)
		}
	}
	if len(want) > 0 {
		return want
	}
	for kind := range herdrAgentKinds {
		want = append(want, kind)
	}
	sort.Strings(want)
	return want
}

// launchedCommand reports whether session name's life launched a command,
// rather than running the pane's own shell as the session, as Start recorded
// it. A binding written before Start recorded it falls back to whether the
// session env named a provider (GC_PROVIDER, seeded into the sidecar at
// Start): a session gc started for a provider launched that provider's
// command. G1's sessions bound by gc 1.5.1-brru read this way until their
// next Start. known is false when the sidecar could not be read: each caller
// then takes its own safe side (see shellIsSession and lookupOps).
func (p *Provider) launchedCommand(name string) (launched, known bool) {
	v, err := p.GetMeta(name, metaLaunchedCommand)
	if err != nil {
		return false, false
	}
	switch strings.TrimSpace(v) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	provider, err := p.GetMeta(name, "GC_PROVIDER")
	if err != nil {
		return false, false
	}
	return strings.TrimSpace(provider) != "", true
}

// shellIsSession reports whether session name's pane shell is the session
// itself (its life launched no command), the one case the type check lets a
// bare prompt take text. A sidecar that cannot be read does not say so, so
// the check stays strict.
func (p *Provider) shellIsSession(name string) bool {
	launched, known := p.launchedCommand(name)
	return known && !launched
}

// sessionAgentProcessNames is agentProcessNames for the names Start recorded
// for session name. A sidecar that cannot be read leaves the agent kinds, which
// still refuse every shell.
func (p *Provider) sessionAgentProcessNames(name string) []string {
	v, _ := p.GetMeta(name, metaAgentProcesses)
	return agentProcessNames(strings.Split(v, "\n"))
}

// recordLaunch persists what liveness and the type check need to know about
// this life's launch. Start calls it before the launch, so a reconcile tick
// during the launch reads this life's facts, never a crashed prior life's.
func (p *Provider) recordLaunch(name string, spec launchSpec, processNames []string) error {
	if err := p.SetMeta(name, metaLaunchedCommand, strconv.FormatBool(launchesCommand(spec))); err != nil {
		return err
	}
	var names []string
	for _, n := range processNames {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return p.RemoveMeta(name, metaAgentProcesses)
	}
	return p.SetMeta(name, metaAgentProcesses, strings.Join(names, "\n"))
}

// launchesCommand reports whether spec runs a command in the pane rather than
// leaving the pane's own shell as the session: an empty command does not, and
// neither does a raw command that is itself an interactive shell (`bash -l`),
// whose prompt is the session.
func launchesCommand(spec launchSpec) bool {
	if spec.Kind != "" {
		return true
	}
	return spec.Raw != "" && !interactiveShellArgv(shellquote.Split(spec.Raw))
}

// typeCheck confirms, immediately before a verb types text into paneID, that
// the pane may receive it. A non-nil error means nothing is typed.
type typeCheck func(ctx context.Context, paneID string) error

// typeCheckFor is the check every text delivery to session name runs right
// before it types: the pane's foreground process group must hold the session's
// agent (one of want). A shell, any other program, or a foreground that cannot
// be read gets nothing typed and a *paneNotAgentError. The one shell that may
// take text is the session itself: when its life launched no command
// (shellIsSession: an empty command, or a command that is an interactive
// shell), the pane's bare prompt is where its input goes, as the runtime
// contract's nudge to a running shell session expects.
func (p *Provider) typeCheckFor(name string, want []string, shellIsSession bool) typeCheck {
	return func(ctx context.Context, paneID string) error {
		shellPID, fg, err := p.c.processInfo(ctx, paneID)
		if err != nil {
			return &paneNotAgentError{session: name, pane: paneID, want: want, cause: err}
		}
		if agentInForeground(fg, want) {
			return nil
		}
		shell, bare := barePromptShell(shellPID, fg)
		if bare && shellIsSession {
			return nil
		}
		return &paneNotAgentError{session: name, pane: paneID, want: want, shell: shell, fg: foregroundNames(fg)}
	}
}

// paneNotAgentError is a text delivery refused because the pane's foreground
// is not the session's agent; nothing was typed. It names the session, the pane
// and the pane's process names, never the text, so callers can log it as is.
type paneNotAgentError struct {
	session, pane string
	want          []string // the agent process names the check accepts
	shell         string   // the shell the pane idles in, when it sits at a bare prompt
	fg            []string // the foreground's process names otherwise
	cause         error    // why the foreground could not be read, when it could not
}

func (e *paneNotAgentError) Error() string {
	var what string
	switch {
	case e.cause != nil:
		what = fmt.Sprintf("the foreground of pane %s could not be read, so the session's agent is unconfirmed (%v)", e.pane, e.cause)
	case e.shell != "":
		what = fmt.Sprintf("pane runs a shell (%s in pane %s), not the session's agent", e.shell, e.pane)
	case len(e.fg) == 0:
		what = fmt.Sprintf("pane %s shows no foreground process, so the session's agent is unconfirmed", e.pane)
	default:
		what = fmt.Sprintf("pane runs %s (pane %s), not the session's agent", strings.Join(e.fg, ", "), e.pane)
	}
	return fmt.Sprintf("herdr: nothing typed for session %q: %s (agent: %s)", e.session, what, describeAgentNames(e.want))
}

func (e *paneNotAgentError) Unwrap() error { return e.cause }

// describeAgentNames renders the accepted agent names for a refusal.
func describeAgentNames(want []string) string {
	if len(want) > 4 {
		return fmt.Sprintf("any of %d herdr agent kinds", len(want))
	}
	return strings.Join(want, ", ")
}

// foregroundNames lists the foreground's process names once each, in order.
func foregroundNames(fg []proc) []string {
	var names []string
	for _, pr := range fg {
		if pr.Name != "" && !slices.Contains(names, pr.Name) {
			names = append(names, pr.Name)
		}
	}
	return names
}

// agentForegroundWait bounds how long a startup turn waits for the launched
// agent to reach the pane's foreground. A kind launch returns only once herdr
// has detected the agent; a raw launch's wrapper forks the agent just after
// waitPaneLaunched sees the wrapper, so the wait is normally one probe.
const agentForegroundWait = 15 * time.Second

// awaitAgentForeground runs check until it passes, the pane is at a bare shell
// prompt (its agent is gone, not coming: no point waiting), or
// agentForegroundWait elapses, and returns the last verdict.
func awaitAgentForeground(ctx context.Context, paneID string, check typeCheck) error {
	deadline := time.Now().Add(agentForegroundWait)
	for {
		err := check(ctx, paneID)
		var refused *paneNotAgentError
		if err == nil || !errors.As(err, &refused) || refused.shell != "" || !time.Now().Before(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(200 * time.Millisecond):
		}
	}
}

package herdr

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ── g1-j3yf1: nothing is typed into a shell, and a shell pane is no live session ──
//
// On 29 Oct 2026 G1 rebooted. herdr restored the agent panes and typed
// `claude --resume <id>` into each; the resume found no conversation and
// exited, leaving bash at its prompt. gc still called those sessions active (a
// raw-launched session read as running for as long as its pane existed) and
// pasted its nudges, mail and room-line previews that can carry a customer's
// words, into bash and pressed Enter, so every line ran as a shell command.
//
// These tests pin both halves of the fix against the fake herdr. Text goes only
// to a pane whose foreground process group holds the session's agent: a shell,
// anything else, or a foreground that cannot be read gets nothing typed and an
// error. And a raw session whose pane is back at a bare interactive prompt reads
// not running, so the controller restarts it. The process shapes are the ones
// herdr 0.9.3 reported on G1 for a live claude pane and for the failed-resume
// pane.

const guardSession = "mayor"

// guardNudge reads like a room-line preview carrying shell syntax: a bash prompt
// would run it if it were ever typed there.
const guardNudge = `Room: "ok" > /tmp/g1-j3yf1-pwned; $(touch /tmp/g1-j3yf1-pwned)`

func fgProc(pid int, name string, argv ...string) proc {
	return proc{PID: pid, Name: name, Argv: argv}
}

// What herdr 0.9.3 reported on G1: the failed-resume pane (bash at its prompt,
// nothing else) and a live raw-launched claude (the `exec /bin/sh -c` wrapper as
// the pane's root, claude and its MCP helpers below it).
var (
	bareBashPID   = 304098
	bareBash      = []proc{fgProc(304098, "bash", "bash")}
	liveClaudePID = 980276
	liveClaude    = []proc{
		fgProc(980276, "sh", "/bin/sh", "-c", "CLAUDE_CONFIG_DIR=/cfg claude --resume k1"),
		fgProc(980346, "claude", "claude", "--resume", "k1"),
		fgProc(982912, "npm exec @playw", "mcp@latest"),
		fgProc(984392, "MainThread", "node", "/srv/mcp.js"),
	}
)

// setForeground makes the fake herdr answer every `pane process-info` with
// shellPID and fg until the test sets another.
func setForeground(t *testing.T, state string, shellPID int, fg ...proc) {
	t.Helper()
	var reply struct {
		Result struct {
			ProcessInfo struct {
				ShellPID   int    `json:"shell_pid"`
				Foreground []proc `json:"foreground_processes"`
			} `json:"process_info"`
		} `json:"result"`
	}
	reply.Result.ProcessInfo.ShellPID = shellPID
	reply.Result.ProcessInfo.Foreground = append([]proc{}, fg...)
	b, err := json.Marshal(reply)
	if err != nil {
		t.Fatal(err)
	}
	setForegroundReply(t, state, string(b))
}

// setForegroundReply makes the fake herdr print reply, as it is, for every
// `pane process-info`.
func setForegroundReply(t *testing.T, state, reply string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(state, "foreground.json"), []byte(reply), 0o644); err != nil {
		t.Fatal(err)
	}
}

// legacyRawBinding seeds the sidecar the way the binary installed on G1 left it
// for a raw-launched claude session: shell mode, the pane, the exact name, and
// the GC_PROVIDER the session env carried, with none of the keys a later
// binary adds.
func legacyRawBinding(t *testing.T, p *Provider, name string) {
	t.Helper()
	bindTestPane(t, p, name, bindModeShell)
	for k, v := range map[string]string{metaBoundName: name, "GC_PROVIDER": "claude"} {
		if err := p.SetMeta(name, k, v); err != nil {
			t.Fatal(err)
		}
	}
}

// rawClaudeCommand is a claude launch the way G1 writes it: the env assignment
// rules out a kind launch, so Start execs it through the pane's shell.
const rawClaudeCommand = "CLAUDE_CONFIG_DIR=/cfg claude --resume k1"

// startSession starts name with cfg against the fake herdr.
func startSession(t *testing.T, p *Provider, name string, cfg runtime.Config) {
	t.Helper()
	if err := p.Start(context.Background(), name, cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
}

// assertNothingTyped fails if any verb that types into a pane appears in the
// fake herdr's calls after offset from.
func assertNothingTyped(t *testing.T, state string, from int) {
	t.Helper()
	calls := fakeCalls(t, state)[from:]
	for _, verb := range []string{"agent prompt", "pane run", "pane send-keys"} {
		if strings.Contains(calls, verb) {
			t.Fatalf("%q was issued, but nothing may be typed into this pane:\n%s", verb, calls)
		}
	}
}

// assertRefused fails unless err refuses the delivery, names the session and
// carries none of the nudge text; wants are further phrases it must contain.
func assertRefused(t *testing.T, err error, wants ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("the delivery returned nil, so a refused delivery would read as a success")
	}
	msg := err.Error()
	if !strings.Contains(msg, guardSession) {
		t.Errorf("the refusal does not name the session %q: %v", guardSession, err)
	}
	if strings.Contains(msg, "pwned") || strings.Contains(msg, "Room:") {
		t.Errorf("the refusal carries the nudge text, which must never reach a log: %v", err)
	}
	for _, w := range wants {
		if !strings.Contains(msg, w) {
			t.Errorf("the refusal does not say %q: %v", w, err)
		}
	}
}

// The incident itself: a nudge to a session whose pane is back at a shell
// prompt is refused with nothing typed, whether the binding was written by the
// binary G1 runs today or by this one, and whichever shell the pane idles in.
func TestNudgeIsNeverTypedIntoAShell(t *testing.T) {
	shells := []struct {
		name     string
		shellPID int
		fg       []proc
	}{
		{"bash after a failed claude --resume (G1 wF5:p1)", bareBashPID, bareBash},
		{"login zsh", 100, []proc{fgProc(100, "-zsh", "-zsh")}},
		{"sh", 100, []proc{fgProc(100, "sh", "/bin/sh")}},
		{"fish login shell", 100, []proc{fgProc(100, "fish", "/usr/bin/fish", "-l")}},
	}
	sessions := []struct {
		name  string
		setup func(t *testing.T, p *Provider)
	}{
		{"binding left by the installed binary", func(t *testing.T, p *Provider) { legacyRawBinding(t, p, guardSession) }},
		{"raw session started by this binary", func(t *testing.T, p *Provider) {
			startSession(t, p, guardSession, runtime.Config{Command: rawClaudeCommand, ProcessNames: []string{"node", "claude"}})
		}},
		{"kind-launched agent that exited", func(t *testing.T, p *Provider) { startSession(t, p, guardSession, runtime.Config{Command: "claude"}) }},
	}
	for _, s := range sessions {
		for _, sh := range shells {
			t.Run(s.name+"/"+sh.name, func(t *testing.T) {
				p, state := newFakeHerdrProvider(t)
				listenHerdrSocket(t, p)
				s.setup(t, p)
				setForeground(t, state, sh.shellPID, sh.fg...)
				from := len(fakeCalls(t, state))
				err := p.Nudge(guardSession, runtime.TextContent(guardNudge))
				assertRefused(t, err, "pane runs a shell")
				assertNothingTyped(t, state, from)
			})
		}
	}
}

// Fail closed: a foreground that cannot be read or shows no process is not a
// confirmed agent, so nothing is typed. The session is kind-launched, so the
// registry resolves its pane and no binding probe refuses first.
func TestNudgeRefusesAForegroundItCannotRead(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply string
	}{
		{"process-info fails", `{"error":{"code":"internal_error","message":"process table unavailable"}}`},
		{"no foreground process reported", `{"result":{"process_info":{"shell_pid":4242,"foreground_processes":[]}}}`},
		{"no shell pid and no foreground", `{"result":{"process_info":{"shell_pid":0,"foreground_processes":[]}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, state := newFakeHerdrProvider(t)
			listenHerdrSocket(t, p)
			startSession(t, p, guardSession, runtime.Config{Command: "claude"})
			setForegroundReply(t, state, tc.reply)
			from := len(fakeCalls(t, state))
			assertRefused(t, p.Nudge(guardSession, runtime.TextContent(guardNudge)))
			assertNothingTyped(t, state, from)
		})
	}
}

// Only the agent's own process counts, matched on its exact name: a shell that
// mentions the agent, a shell that renames itself, a name or path that merely
// contains the agent's, and any other program all get nothing typed.
func TestNudgeRefusesAForegroundThatIsNotTheAgent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		shellPID int
		fg       []proc
	}{
		{"bash -c claude names claude only in its arguments", 100, []proc{fgProc(100, "bash", "bash", "-c", "claude")}},
		{"a shell whose argv[0] claims to be claude", 100, []proc{fgProc(100, "bash", "claude")}},
		{"a name that only contains claude", 100, []proc{fgProc(100, "sh", "/bin/sh", "-c", "claudex"), fgProc(101, "claudex", "claudex")}},
		{"a path that only contains claude", 100, []proc{fgProc(101, "my-claude", "/opt/bin/my-claude")}},
		{"an editor", 100, []proc{fgProc(101, "vim", "vim", "notes.md")}},
		{"a python REPL", 100, []proc{fgProc(101, "python3", "python3")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, state := newFakeHerdrProvider(t)
			listenHerdrSocket(t, p)
			startSession(t, p, guardSession, runtime.Config{Command: "claude"})
			setForeground(t, state, tc.shellPID, tc.fg...)
			from := len(fakeCalls(t, state))
			assertRefused(t, p.Nudge(guardSession, runtime.TextContent(guardNudge)))
			assertNothingTyped(t, state, from)
		})
	}
}

// The guard must not cost a live agent its nudges: a G1 raw claude takes the
// paste fallback, a claude whose process name is its version string is still
// claude by its argv[0], and a registered agent takes `agent prompt`.
func TestNudgeReachesTheAgentInTheForeground(t *testing.T) {
	t.Run("raw claude on G1, delivered by paste", func(t *testing.T) {
		p, state := newFakeHerdrProvider(t)
		legacyRawBinding(t, p, guardSession)
		setForeground(t, state, liveClaudePID, liveClaude...)
		if err := p.Nudge(guardSession, runtime.TextContent(guardNudge)); err != nil {
			t.Fatalf("Nudge to a live claude: %v", err)
		}
		calls := fakeCalls(t, state)
		if !strings.Contains(calls, "pane run %5 "+guardNudge+"\n") || !strings.Contains(calls, "pane send-keys %5 Enter") {
			t.Fatalf("the nudge did not reach the live claude by paste and Enter:\n%s", calls)
		}
	})
	t.Run("claude whose process name is its version", func(t *testing.T) {
		p, state := newFakeHerdrProvider(t)
		legacyRawBinding(t, p, guardSession)
		setForeground(t, state, liveClaudePID, liveClaude[0], fgProc(980346, "2.1.216", "claude", "--resume", "k1"))
		if err := p.Nudge(guardSession, runtime.TextContent("You have mail.")); err != nil {
			t.Fatalf("Nudge to a claude named by its version: %v", err)
		}
		if calls := fakeCalls(t, state); !strings.Contains(calls, "pane run %5 You have mail.\n") {
			t.Fatalf("the nudge did not reach the claude named by its version:\n%s", calls)
		}
	})
	t.Run("registered agent, delivered by agent prompt", func(t *testing.T) {
		p, state := newFakeHerdrProvider(t)
		listenHerdrSocket(t, p)
		startSession(t, p, guardSession, runtime.Config{Command: "claude"})
		if err := p.Nudge(guardSession, runtime.TextContent("You have mail.")); err != nil {
			t.Fatalf("Nudge to a registered claude: %v", err)
		}
		if calls := fakeCalls(t, state); !strings.Contains(calls, "agent prompt %5 You have mail.\n") {
			t.Fatalf("the nudge did not reach the registered claude:\n%s", calls)
		}
	})
}

// "The session's provider, as configured": a codex session's pane running
// claude is not running its agent, and a shell never is, even where a
// provider's own process names list one (zcode-repl's profile lists bash).
func TestNudgeAcceptsOnlyTheSessionsConfiguredAgent(t *testing.T) {
	t.Run("codex session, claude in the foreground", func(t *testing.T) {
		p, state := newFakeHerdrProvider(t)
		listenHerdrSocket(t, p)
		startSession(t, p, guardSession, runtime.Config{Command: "codex", ProcessNames: []string{"codex"}})
		setForeground(t, state, 4242, fgProc(4243, "claude", "claude"))
		from := len(fakeCalls(t, state))
		assertRefused(t, p.Nudge(guardSession, runtime.TextContent(guardNudge)))
		assertNothingTyped(t, state, from)

		setForeground(t, state, 4242, fgProc(4243, "codex", "codex"))
		if err := p.Nudge(guardSession, runtime.TextContent("You have mail.")); err != nil {
			t.Fatalf("Nudge to the codex it was configured for: %v", err)
		}
		if calls := fakeCalls(t, state); !strings.Contains(calls, "agent prompt %5 You have mail.\n") {
			t.Fatalf("the nudge did not reach codex:\n%s", calls)
		}
	})
	t.Run("a shell listed among the process names", func(t *testing.T) {
		p, state := newFakeHerdrProvider(t)
		listenHerdrSocket(t, p)
		startSession(t, p, guardSession, runtime.Config{Command: "zcode-repl", ProcessNames: []string{"bash", "node"}})
		setForeground(t, state, bareBashPID, bareBash...)
		from := len(fakeCalls(t, state))
		assertRefused(t, p.Nudge(guardSession, runtime.TextContent(guardNudge)), "pane runs a shell")
		assertNothingTyped(t, state, from)
	})
}

// Liveness: a raw session whose pane is back at a bare interactive prompt is not
// running, so the reconciler restarts it the way it restarts an exited agent.
// Nothing reaps the pane here: it still resolves, so Peek reads it and Stop
// closes it, and Start recycles its tab.
func TestRawSessionAtAShellPromptIsNotRunning(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, p *Provider)
	}{
		{"binding left by the installed binary", func(t *testing.T, p *Provider) { legacyRawBinding(t, p, guardSession) }},
		{"raw session started by this binary", func(t *testing.T, p *Provider) {
			startSession(t, p, guardSession, runtime.Config{Command: rawClaudeCommand, ProcessNames: []string{"node", "claude"}})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, state := newFakeHerdrProvider(t)
			listenHerdrSocket(t, p)
			tc.setup(t, p)

			setForeground(t, state, liveClaudePID, liveClaude...)
			if got := p.ObserveLiveness(guardSession, nil); !got.Running || !got.Alive {
				t.Fatalf("ObserveLiveness = %+v for a live claude; want Running and Alive", got)
			}

			setForeground(t, state, bareBashPID, bareBash...)
			if got := p.ObserveLiveness(guardSession, []string{"node", "claude"}); got.Running || got.Alive {
				t.Fatalf("ObserveLiveness = %+v for a pane at a bash prompt; want not running, so the controller restarts it", got)
			}
			if p.IsRunning(guardSession) {
				t.Fatal("IsRunning = true for a pane at a bash prompt")
			}
			names, err := p.ListRunning("")
			if err != nil {
				t.Fatalf("ListRunning: %v", err)
			}
			for _, n := range names {
				if n == guardSession {
					t.Fatalf("ListRunning = %v lists a session whose pane is at a bash prompt", names)
				}
			}
			if calls := fakeCalls(t, state); strings.Contains(calls, "pane close") || strings.Contains(calls, "tab close") {
				t.Fatalf("a liveness read closed the pane; only Stop and Start's recycle may:\n%s", calls)
			}
			if _, err := p.Peek(guardSession, 10); err != nil {
				t.Fatalf("Peek no longer reaches the pane: %v", err)
			}
			if err := p.Stop(guardSession); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			if calls := fakeCalls(t, state); !strings.Contains(calls, "pane close %5") {
				t.Fatalf("Stop did not close the pane:\n%s", calls)
			}
		})
	}
}

// What must keep running at a prompt: a session whose shell is the session
// (empty command, or a command that is itself an interactive shell), a binding
// with nothing saying an agent was launched, and a live raw session whose
// foreground momentarily shows only its `sh -c` wrapper.
func TestSessionsWhoseShellIsTheSessionStayRunning(t *testing.T) {
	for _, tc := range []struct {
		name     string
		setup    func(t *testing.T, p *Provider)
		shellPID int
		fg       []proc
	}{
		{"empty command", func(t *testing.T, p *Provider) { startSession(t, p, guardSession, runtime.Config{}) }, bareBashPID, bareBash},
		{"command that is an interactive shell", func(t *testing.T, p *Provider) { startSession(t, p, guardSession, runtime.Config{Command: "bash -l"}) }, 4242, []proc{fgProc(4242, "bash", "bash", "-l")}},
		{"binding with no provider, left by the installed binary", func(t *testing.T, p *Provider) {
			bindTestPane(t, p, guardSession, bindModeShell)
			if err := p.SetMeta(guardSession, metaBoundName, guardSession); err != nil {
				t.Fatal(err)
			}
		}, bareBashPID, bareBash},
		{"raw session between two commands", func(t *testing.T, p *Provider) { legacyRawBinding(t, p, guardSession) }, liveClaudePID, liveClaude[:1]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, state := newFakeHerdrProvider(t)
			listenHerdrSocket(t, p)
			tc.setup(t, p)
			setForeground(t, state, tc.shellPID, tc.fg...)
			if !p.IsRunning(guardSession) {
				t.Fatal("IsRunning = false; this session runs at its prompt, and reading it dead restarts it every tick")
			}
			if got := p.ObserveLiveness(guardSession, nil); !got.Running || !got.Alive {
				t.Fatalf("ObserveLiveness = %+v; want Running and Alive", got)
			}
		})
	}
}

// The startup turn takes the same guard: a kind-launched claude that exited at
// once (a resume that found no conversation) leaves bash in the pane, and the
// first turn must not be typed there. Start still succeeds (the reconciler
// owns the restart) and the strand is recorded with the reason.
func TestStartupTurnIsNeverTypedIntoAShell(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	setForeground(t, state, bareBashPID, bareBash...)

	cfg := runtime.Config{Command: "claude", Nudge: "Run gc hook --claim --json now."}
	if err := p.Start(context.Background(), guardSession, cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	if strings.Contains(calls, "agent prompt %5 Run gc hook") || strings.Contains(calls, "pane run %5 Run gc hook") {
		t.Fatalf("the startup turn was typed into a pane at a bash prompt:\n%s", calls)
	}
	v, err := p.GetMeta(guardSession, metaStartupUnconfirmed)
	if err != nil || !strings.Contains(v, "pane runs a shell") {
		t.Fatalf("startup marker = %q, %v; want the strand recorded as a shell pane", v, err)
	}
}
